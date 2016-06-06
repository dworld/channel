package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
)

var (
	// Mode is the server work mode, client or proxy
	Mode string
	// LAddr is the local address
	LAddr string
	// PAddr is the proxy address
	PAddr string
	// RAddr is the real address
	RAddr string

	showHelp bool
)

var (
	defaultDialer *Dialer
	proxyConnID   int32
)

func init() {
	flag.StringVar(&LAddr, "laddr", "127.0.0.1:7001", "the local address")
	flag.StringVar(&PAddr, "paddr", "127.0.0.1:7002", "the proxy address")
	flag.StringVar(&RAddr, "raddr", "www.qq.com:80", "the real address")
	flag.StringVar(&Mode, "mode", "client", "worker mode, client or proxy")
	flag.BoolVar(&showHelp, "help", false, "show this help")
}

func main() {
	flag.Parse()
	if showHelp {
		flag.Usage()
		return
	}
	if Mode != "client" && Mode != "proxy" {
		log.Fatalf("invlaid mode, %s", Mode)
		return
	}
	if Mode == "client" {
		go serve(LAddr, "CLIENT", handleClientConn)
		serve(PAddr, "PROXY", handleClientProxyConn)
		return
	}
	serveProxy()
}

func serve(addr string, serviceName string, handler func(net.Conn)) {
	log.Printf("Listen %s at %s\n", serviceName, addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept: %s\n", err)
			continue
		}
		go handler(conn)
	}
}

func serveProxy() {
	for {
		log.Printf("dial to %s\n", PAddr)
		conn, err := net.Dial("tcp", PAddr)
		if err != nil {
			log.Printf("Dial: %s\n", err)
			continue
		}
		handleProxy(conn)
	}
}

func closeConn(name string, conn net.Conn) {
	log.Printf("close %s conn %v\n", name, conn)
	conn.Close()
}

func handleProxy(conn net.Conn) {
	log.Printf("handle PROXY conn %v\n", conn)
	defer closeConn("PROXY", conn)
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		handleOneProxy(r, w)
	}
}

func handleOneProxy(r *bufio.Reader, w *bufio.Writer) {
	line, err := r.ReadString('\n')
	if err != nil {
		log.Printf("ReadLine: %s\n", err)
		return
	}
	log.Printf("REQ: %s", line)
	if len(line) <= 5 {
		log.Printf("invalid request, %s\n", line)
		return
	}
	raddr := string(line[5:])
	log.Printf("dial to %s\n", PAddr)
	proxyConn, err := net.Dial("tcp", PAddr)
	if err != nil {
		log.Printf("Dial: %s\n", line)
		return
	}
	log.Printf("dial to %s\n", raddr)
	rconn, err := net.Dial("tcp", raddr)
	if err != nil {
		log.Printf("Dial: %s\n", line)
		return
	}

	connID := atomic.AddInt32(&proxyConnID, 1)
	rsp := fmt.Sprintf("%d\n", connID)
	log.Printf("RSP: %s", line)
	proxyConn.Write([]byte(rsp))
	preader := bufio.NewReader(proxyConn)
	_, err = preader.ReadString('\n')
	if err != nil {
		log.Printf("ReadLine: %s\n", err)
		return
	}

	w.WriteString(rsp)
	log.Printf("construct connection %d\n", connID)
	w.Flush()

	go pipeRemote(rconn, proxyConn)
}

func pipeRemote(rconn, proxyConn net.Conn) {
	defer closeConn("REMOTE", rconn)
	defer closeConn("PROXY", proxyConn)
	go copyWithError(rconn, proxyConn)
	copyWithError(proxyConn, rconn)
}

func copyWithError(dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Printf("Copy: %s\n", err)
	}
}

func handleClientConn(conn net.Conn) {
	log.Printf("handle CLIENT conn %v\n", conn)
	defer closeConn("CLIENT", conn)
	if defaultDialer == nil {
		log.Printf("dialer is not inited\n")
		return
	}
	// defaultDialer.Lock()
	// defer defaultDialer.Unlock()
	rconn, err := defaultDialer.Dial(RAddr)
	if err != nil {
		log.Printf("Dial error, %s\n", err)
		return
	}
	defer closeConn("PROXY", rconn)
	go copyWithError(conn, rconn)
	copyWithError(rconn, conn)
}

func handleClientProxyConn(conn net.Conn) {
	log.Printf("handle CLIENT_PROXY conn %v\n", conn)
	if defaultDialer == nil {
		defaultDialer = NewDialer(conn)
		return
	}
	// defaultDialer.Lock()
	// defer defaultDialer.Unlock()
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		log.Printf("ReadString: %s", err)
		return
	}
	connID, err := strconv.Atoi(line[:len(line)-1])
	if err != nil {
		log.Printf("Atoi: %s", err)
		return
	}
	defaultDialer.setProxyConn(int32(connID), conn)
	conn.Write([]byte("ok\n"))
}

// Dialer construct connection used by client request
type Dialer struct {
	sync.Mutex
	conn   net.Conn
	writer *bufio.Writer
	reader *bufio.Reader

	conns map[int32]net.Conn
}

// NewDialer create new dialer
func NewDialer(conn net.Conn) *Dialer {
	r := &Dialer{conns: map[int32]net.Conn{}}
	r.setConn(conn)
	return r
}

// Dial construct connection used by client request
func (dialer *Dialer) Dial(addr string) (net.Conn, error) {
	log.Printf("dial to %s", addr)
	w := dialer.writer
	r := dialer.reader
	req := fmt.Sprintf("dial:%s\n", addr)
	log.Printf("REQ: %s", req)
	_, err := w.WriteString(req)
	if err != nil {
		return nil, err
	}
	w.Flush()
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	log.Printf("RSP: %s", string(line))
	connID, err := strconv.Atoi(string(line[:len(line)-1]))
	if err != nil {
		return nil, err
	}
	conn := dialer.conns[int32(connID)]
	if conn == nil {
		return nil, errors.New("can't get conn")
	}
	return conn, nil
}

func (dialer *Dialer) setConn(conn net.Conn) {
	if dialer.conn != nil {
		closeConn("PROXY", dialer.conn)
	}
	dialer.conn = conn
	dialer.writer = bufio.NewWriter(dialer.conn)
	dialer.reader = bufio.NewReader(dialer.conn)
}

func (dialer *Dialer) setProxyConn(connID int32, conn net.Conn) {
	log.Printf("set proxy conn %d, %v\n", connID, conn)
	dialer.conns[connID] = conn
}
