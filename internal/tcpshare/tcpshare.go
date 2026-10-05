// Package tcpshare lets the voice fallback (ICE-TCP) share one TCP port with HTTPS.
//
// Some networks (schools, offices, hotels) block UDP, so voice audio cannot use its UDP
// port there. WebRTC can then send the audio over TCP instead ("ICE-TCP"). Such networks
// usually allow TCP port 443, because that is HTTPS, so we accept ICE-TCP on the same
// port as the website and tell the two apart by the FIRST BYTE of each new connection:
//
//   - HTTPS starts with a TLS handshake record: byte 0x16.
//   - Plain HTTP (development) starts with a method name: a letter like 'G' or 'P'.
//   - ICE-TCP (RFC 4571) starts with a 2-byte packet length, and its first packet is a
//     small STUN message (far below 256 bytes), so its first byte is 0x00.
//
// Nothing is decrypted or interpreted here: the byte is only looked at, then the whole
// connection (including that byte) is handed to the HTTP server or to the voice service.
package tcpshare

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"
)

// sniffTimeout: a new connection must send its first byte within this time, or it is
// closed. Otherwise idle connections could pile up before the HTTP server's own
// timeouts even start.
const sniffTimeout = 10 * time.Second

// Queue is a net.Listener that receives connections from a Splitter instead of a socket.
// The voice service listens on it. It lives as long as the server, even when the web
// listener is restarted.
type Queue struct {
	conns  chan net.Conn
	addr   net.Addr
	once   sync.Once
	closed chan struct{}
}

// NewQueue makes a Queue that reports addr as its address. Voice tells clients this
// address, so it must be the PUBLIC port (e.g. 443 when Docker maps 443 -> 8443).
func NewQueue(addr *net.TCPAddr) *Queue {
	return &Queue{conns: make(chan net.Conn, 16), addr: addr, closed: make(chan struct{})}
}

func (q *Queue) Accept() (net.Conn, error) {
	select {
	case c := <-q.conns:
		return c, nil
	case <-q.closed:
		return nil, net.ErrClosed
	}
}

func (q *Queue) Close() error {
	q.once.Do(func() { close(q.closed) })
	return nil
}

func (q *Queue) Addr() net.Addr { return q.addr }

// deliver hands a connection to the queue, or closes it if the queue is closed or full
// (full = the voice service is not keeping up; better to drop than to block HTTPS).
func (q *Queue) deliver(c net.Conn) {
	// Checked first on purpose: when several select cases are ready, Go picks one at
	// random, so a closed queue with room left could otherwise still take the connection.
	select {
	case <-q.closed:
		c.Close()
		return
	default:
	}
	select {
	case q.conns <- c:
	default:
		c.Close()
	}
}

// Split wraps a TCP listener: ICE-TCP connections go to q, everything else is returned
// by the wrapper's Accept (for the HTTP server). Closing the wrapper closes ln (not q).
func Split(ln net.Listener, q *Queue) net.Listener {
	s := &splitter{inner: ln, queue: q, conns: make(chan net.Conn), done: make(chan struct{})}
	go s.acceptLoop()
	return s
}

type splitter struct {
	inner net.Listener
	queue *Queue
	conns chan net.Conn // for the HTTP server
	done  chan struct{}
	once  sync.Once
	err   error // why acceptLoop stopped (read after done is closed)
}

func (s *splitter) acceptLoop() {
	for {
		c, err := s.inner.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue // temporary: keep accepting
			}
			// err is only written here, inside the Once, before done is closed: readers
			// look at it only after seeing done closed, so they never race with this write.
			s.once.Do(func() {
				s.err = err
				close(s.done)
			})
			return
		}
		go s.sniff(c) // never block accepting on a slow client
	}
}

// sniff reads the first byte and sends the connection where it belongs.
func (s *splitter) sniff(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	r := bufio.NewReaderSize(c, 512)
	first, err := r.Peek(1) // looks without consuming: the byte is replayed below
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: c, r: r}

	if first[0] == 0x00 {
		s.queue.deliver(pc)
		return
	}
	select {
	case s.conns <- pc:
	case <-s.done:
		c.Close()
	}
}

func (s *splitter) Accept() (net.Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case <-s.done:
		if s.err != nil {
			return nil, s.err
		}
		return nil, net.ErrClosed
	}
}

func (s *splitter) Close() error {
	err := s.inner.Close()
	s.once.Do(func() { close(s.done) })
	return err
}

func (s *splitter) Addr() net.Addr { return s.inner.Addr() }

// peekedConn is a connection whose first bytes were already read into r: reads come
// from r first, so nothing is lost.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }
