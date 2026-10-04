package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/5cfp/vianden-server/internal/config"
)

// healthcheck asks the server running on THIS machine (or container) for /api/v1/health.
// Docker runs it every 30 seconds; the image has no curl or shell, so the server checks itself.
// Returns the process exit code: 0 = healthy, 1 = not.
func healthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	client := &http.Client{Timeout: 5 * time.Second}
	var url string
	switch cfg.TLSMode {
	case config.TLSAutocert:
		// Over the port-80 listener, which answers /api/v1/health to loopback requests.
		// (Checking over HTTPS would make the server try to get a certificate on every check.)
		url = "http://" + loopback(cfg.HTTPAddr) + "/api/v1/health"
	case config.TLSSelfSigned:
		// The certificate is not verified here: this request never leaves the machine.
		// #nosec G402 -- loopback-only request to this same server; nothing to verify against.
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		url = "https://" + loopback(cfg.HTTPSAddr) + "/api/v1/health"
	default:
		url = "http://" + loopback(cfg.ListenAddr) + "/api/v1/health"
	}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}

// loopback turns a listen address like ":8443" or "0.0.0.0:8443" into "127.0.0.1:8443".
func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
