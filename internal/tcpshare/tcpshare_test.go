package tcpshare

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func setup(t *testing.T) (addr string, httpLn net.Listener, q *Queue) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	q = NewQueue(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 443})
	httpLn = Split(ln, q)
	t.Cleanup(func() { httpLn.Close(); q.Close() })
	return ln.Addr().String(), httpLn, q
}

func accept(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("nothing accepted")
		return nil
	}
}

func TestHTTPGoesToTheWebServer(t *testing.T) {
	addr, httpLn, _ := setup(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "web") })}
	go srv.Serve(httpLn)
	defer srv.Close()

	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "web" {
		t.Errorf("got %q", body)
	}
}

func TestICEGoesToTheQueueWithItsFirstBytes(t *testing.T) {
	addr, _, q := setup(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	packet := []byte{0x00, 0x04, 'S', 'T', 'U', 'N'} // length 4, then the payload
	c.Write(packet)

	got := accept(t, q)
	buf := make([]byte, len(packet))
	if _, err := io.ReadFull(got, buf); err != nil || string(buf) != string(packet) {
		t.Errorf("voice side read %q, %v; want the whole packet incl. the sniffed byte", buf, err)
	}
	if q.Addr().String() != "203.0.113.5:443" {
		t.Errorf("queue address %s: must be the public one we were given", q.Addr())
	}
}

func TestTLSGoesToTheWebServer(t *testing.T) {
	addr, httpLn, _ := setup(t)
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	c.Write([]byte{0x16, 0x03, 0x01}) // start of a TLS ClientHello
	got := accept(t, httpLn)
	b, _ := bufio.NewReader(got).Peek(3)
	if b[0] != 0x16 {
		t.Errorf("web side got %x", b)
	}
}

func TestSilentConnectionsDoNotBlockOthers(t *testing.T) {
	addr, _, q := setup(t)
	silent, _ := net.Dial("tcp", addr) // sends nothing
	defer silent.Close()

	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	c.Write([]byte{0x00, 0x01, 'x'})
	accept(t, q) // still works while the silent one waits for its first byte
}

func TestClosedQueueRefusesConnections(t *testing.T) {
	q := NewQueue(&net.TCPAddr{Port: 443})
	q.Close()
	if _, err := q.Accept(); err == nil {
		t.Error("Accept on a closed queue succeeded")
	}
	a, b := net.Pipe()
	defer b.Close()
	q.deliver(a) // must close it, not block
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("connection delivered to a closed queue is still open")
	}
}
