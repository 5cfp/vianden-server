package web

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/config"
	"github.com/5cfp/vianden-server/internal/tcpshare"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

// freePort returns a localhost address with a free port.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startServe runs Serve in the background and returns a function that stops it.
func startServe(t *testing.T, cfg config.Config) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, ok, nil, nil, discard) }()
	time.Sleep(300 * time.Millisecond) // let it start listening
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not stop")
			return nil
		}
	}
}

func TestPlainMode(t *testing.T) {
	addr := freePort(t)
	stop := startServe(t, config.Config{TLSMode: config.TLSPlain, ListenAddr: addr})

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Errorf("graceful shutdown: %v", err)
	}
}

func TestSelfSignedModeServesTLSWithAStableCertificate(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)
	cfg := config.Config{TLSMode: config.TLSSelfSigned, HTTPSAddr: addr, DataDir: dir, Domain: "chat.example.com"}

	fingerprint := func() string {
		stop := startServe(t, cfg)
		defer stop()
		// Like the app's trust-on-first-use: accept the certificate, but read its fingerprint.
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		state := conn.ConnectionState()
		if state.Version < tls.VersionTLS12 {
			t.Errorf("TLS version %x is too old", state.Version)
		}
		leaf := state.PeerCertificates[0]
		if leaf.VerifyHostname("chat.example.com") != nil || leaf.VerifyHostname("localhost") != nil {
			t.Error("certificate does not cover the configured domain and localhost")
		}
		return Fingerprint(leaf.Raw)
	}

	first := fingerprint()
	second := fingerprint() // a restart must reuse the same certificate (TOFU pins it!)
	if first != second {
		t.Errorf("certificate changed after restart: %s -> %s", first, second)
	}

	if runtime.GOOS != "windows" { // Windows does not use Unix permission bits
		info, err := os.Stat(filepath.Join(dir, "self-signed", "key.pem"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("private key permissions = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestSelfSignedIsNotTrustedByDefault(t *testing.T) {
	addr := freePort(t)
	stop := startServe(t, config.Config{TLSMode: config.TLSSelfSigned, HTTPSAddr: addr, DataDir: t.TempDir()})
	defer stop()

	// A normal client must refuse it: that is exactly why the app asks the user (TOFU).
	_, err := http.Get("https://" + addr + "/")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate verification error", err)
	}
}

func TestOldTLSVersionsAreRefused(t *testing.T) {
	addr := freePort(t)
	stop := startServe(t, config.Config{TLSMode: config.TLSSelfSigned, HTTPSAddr: addr, DataDir: t.TempDir()})
	defer stop()

	_, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11})
	if err == nil {
		t.Error("a TLS 1.1 connection was accepted")
	}
}

func TestFingerprintFormat(t *testing.T) {
	der := []byte("certificate bytes")
	fp := Fingerprint(der)
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(fp) {
		t.Errorf("fingerprint %q is not 32 colon-separated hex bytes", fp)
	}
	sum := sha256.Sum256(der)
	if want := fmt.Sprintf("%02X:%02X:", sum[0], sum[1]); !strings.HasPrefix(fp, want) {
		t.Errorf("fingerprint %q should start with %q (SHA-256 of the input)", fp, want)
	}
}

func TestRedirectToHTTPS(t *testing.T) {
	h := redirectToHTTPS("chat.example.com", ok)

	req := httptest.NewRequest("GET", "http://evil.example/api/v1/channels?x=1", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect {
		t.Errorf("status %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://chat.example.com/api/v1/channels?x=1" {
		t.Errorf("Location = %q: must use the configured domain, never the Host header", loc)
	}
}

func TestHealthOverPort80OnlyFromThisMachine(t *testing.T) {
	h := redirectToHTTPS("chat.example.com", ok)

	local := httptest.NewRequest("GET", "/api/v1/health", nil)
	local.RemoteAddr = "127.0.0.1:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, local)
	if rec.Code != http.StatusOK {
		t.Errorf("local health check: %d, want 200", rec.Code)
	}

	remote := httptest.NewRequest("GET", "/api/v1/health", nil)
	remote.RemoteAddr = "198.51.100.4:4000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote)
	if rec.Code != http.StatusPermanentRedirect {
		t.Errorf("remote health check over plain HTTP: %d, want a redirect to HTTPS", rec.Code)
	}
}

func TestPortInUseIsReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = Serve(context.Background(), config.Config{TLSMode: config.TLSPlain, ListenAddr: ln.Addr().String()}, ok, nil, nil, discard)
	if err == nil {
		t.Error("expected an error for a port that is already in use")
	}
}

func TestRedirectCannotLeaveOurDomain(t *testing.T) {
	h := redirectToHTTPS("chat.example.com", ok)
	for _, path := range []string{"//evil.example/x", `/\evil.example`, "/%2F%2Fevil.example", "/@evil.example"} {
		req := httptest.NewRequest("GET", "http://chat.example.com"+path, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Host != "chat.example.com" {
			t.Errorf("path %q redirects to host %q (Location %q)", path, loc.Host, rec.Header().Get("Location"))
		}
	}
}

// The main port is shared with voice's TCP fallback (M7): HTTP still works, and a
// connection starting with byte 0x00 goes to voice instead.
func TestMainPortIsSharedWithVoice(t *testing.T) {
	addr := freePort(t)
	q := tcpshare.NewQueue(&net.TCPAddr{Port: 443})
	defer q.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, config.Config{TLSMode: config.TLSPlain, ListenAddr: addr}, ok, nil, q, discard)
	}()
	time.Sleep(300 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x00, 0x01, 'x'})
	got := make(chan net.Conn, 1)
	go func() {
		if vc, err := q.Accept(); err == nil {
			got <- vc
		}
	}()
	select {
	case vc := <-got:
		vc.Close()
	case <-time.After(3 * time.Second):
		t.Error("the voice connection did not reach voice")
	}
	cancel()
	<-done
}
