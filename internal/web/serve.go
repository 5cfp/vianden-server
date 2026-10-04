// Package web runs the HTTP(S) listeners in the configured TLS mode:
//
//   - plain:       HTTP only (development, or behind a reverse proxy).
//   - autocert:    HTTPS with a free Let's Encrypt certificate, renewed automatically,
//     plus a port-80 listener for Let's Encrypt's domain check and HTTP->HTTPS redirects.
//   - self-signed: HTTPS with a certificate the server makes itself (see selfsigned.go).
package web

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/5cfp/vianden-server/internal/config"
)

// Serve runs the listeners until ctx is cancelled, then shuts them down gracefully.
// onShutdown runs when shutdown starts (used to close WebSocket connections).
func Serve(ctx context.Context, cfg config.Config, handler http.Handler, onShutdown func(), logger *slog.Logger) error {
	switch cfg.TLSMode {
	case config.TLSAutocert:
		return serveAutocert(ctx, cfg, handler, onShutdown, logger)
	case config.TLSSelfSigned:
		cert, err := loadOrCreateSelfSigned(cfg, logger)
		if err != nil {
			return err
		}
		tlsCfg := baseTLSConfig()
		tlsCfg.Certificates = []tls.Certificate{cert}
		return run(ctx, onShutdown, newServer(cfg.HTTPSAddr, handler, tlsCfg))
	default:
		return run(ctx, onShutdown, newServer(cfg.ListenAddr, handler, nil))
	}
}

func serveAutocert(ctx context.Context, cfg config.Config, handler http.Handler, onShutdown func(), logger *slog.Logger) error {
	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS, // accepts Let's Encrypt's terms of service
		// Only ever request a certificate for OUR domain. Without this, anyone could point
		// random names at the server and make it request certificates (and hit rate limits).
		HostPolicy: autocert.HostWhitelist(cfg.Domain),
		// Certificates and the account key are stored here, so restarts do not request new ones.
		Cache: autocert.DirCache(filepath.Join(cfg.DataDir, "autocert")),
		Email: cfg.ACMEEmail,
	}
	logger.Info("HTTPS with Let's Encrypt", "domain", cfg.Domain, "https", cfg.HTTPSAddr, "http", cfg.HTTPAddr)

	tlsCfg := baseTLSConfig()
	tlsCfg.GetCertificate = m.GetCertificate
	tlsCfg.NextProtos = append(tlsCfg.NextProtos, "h2", "http/1.1", "acme-tls/1")

	// Port 80: Let's Encrypt's "HTTP-01" domain check, and a redirect to HTTPS for everything else.
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           m.HTTPHandler(redirectToHTTPS(cfg.Domain, handler)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return run(ctx, onShutdown, newServer(cfg.HTTPSAddr, hsts(handler), tlsCfg), httpSrv)
}

// baseTLSConfig: TLS 1.2 or newer only. Go's default cipher choices are already safe.
func baseTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// hsts tells browsers to only use HTTPS for this domain from now on. Harmless for the
// app, useful if someone opens the server in a browser.
func hsts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

func newServer(addr string, handler http.Handler, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: tlsCfg,
		// Slowloris defense: a client that sends its headers very slowly
		// cannot hold a connection open forever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// run starts every server, waits until ctx is cancelled or one of them fails, then shuts all down.
func run(ctx context.Context, onShutdown func(), servers ...*http.Server) error {
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			shutdownAll(servers)
			return err // e.g. port already in use, or no permission for ports below 1024
		}
		go func() {
			if srv.TLSConfig != nil {
				errCh <- srv.ServeTLS(ln, "", "") // certificates come from TLSConfig
			} else {
				errCh <- srv.Serve(ln)
			}
		}()
	}

	select {
	case err := <-errCh:
		shutdownAll(servers)
		return err
	case <-ctx.Done():
		if onShutdown != nil {
			onShutdown()
		}
		return shutdownAll(servers)
	}
}

// shutdownAll stops the servers, giving in-flight requests up to 10 seconds to finish.
func shutdownAll(servers []*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// isLoopback reports whether a request comes from this machine (127.0.0.1 or ::1).
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectToHTTPS answers plain-HTTP requests with a permanent redirect to the same path on
// https://domain. The target uses the CONFIGURED domain, never the request's Host header,
// so the redirect cannot be abused to send people to another site ("open redirect").
// Exception: the local health check is answered directly (from this machine only).
func redirectToHTTPS(domain string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" && isLoopback(r.RemoteAddr) {
			handler.ServeHTTP(w, r)
			return
		}
		// #nosec G710 -- the host is always the configured domain; only the path comes from the request.
		http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}
