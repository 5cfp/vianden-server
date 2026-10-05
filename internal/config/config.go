// Package config loads the server configuration from environment variables.
//
// For local development, values can also come from a .env file in the current
// folder. Real environment variables always win over values in .env.
// Every variable is listed, with an explanation, in .env.example.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// MaxServerNameLength limits the server name shown to clients.
const MaxServerNameLength = 64

// TLSMode says how the server provides HTTPS.
type TLSMode string

const (
	// TLSPlain: no HTTPS. For local development, or behind a reverse proxy that does HTTPS.
	TLSPlain TLSMode = "plain"
	// TLSAutocert: a free, trusted certificate from Let's Encrypt, renewed automatically.
	// Needs a domain name pointing to this server, and ports 80 and 443 reachable from the internet.
	TLSAutocert TLSMode = "autocert"
	// TLSSelfSigned: the server makes its own certificate. For servers without a domain
	// (IP only). Clients cannot verify it automatically, so they use "trust on first use".
	TLSSelfSigned TLSMode = "self-signed"
)

// Config holds every setting the server needs.
type Config struct {
	// ServerName is shown to clients in GET /api/v1/info.
	ServerName string
	// DatabaseURL is the PostgreSQL connection string. It contains the DB password: never log it.
	DatabaseURL string

	TLSMode TLSMode
	// ListenAddr is where the server listens in plain mode, e.g. "127.0.0.1:8080".
	ListenAddr string
	// HTTPSAddr is where the server listens for HTTPS (autocert and self-signed modes).
	HTTPSAddr string
	// HTTPAddr is used in autocert mode only: Let's Encrypt checks the domain over plain HTTP
	// on port 80, and every other plain-HTTP request is redirected to HTTPS.
	HTTPAddr string
	// Domain is the public name of the server, e.g. "chat.example.com". Required for autocert;
	// for self-signed it is added to the certificate.
	Domain string
	// ACMEEmail is optional: Let's Encrypt uses it to warn about certificate problems.
	ACMEEmail string
	// DataDir holds files the server creates: certificates, uploads, avatars.
	DataDir string

	// Voice (M7). VoiceUDPPort is the one UDP port for all voice audio (0 = voice off).
	VoiceUDPPort int
	// VoicePublicAddresses are the IPs or host names clients use to reach voice: the
	// public IP behind a router, optionally also the LAN IP (for people at home). Host
	// names are looked up again regularly (dynamic home IPs). Empty = the machine's own
	// addresses (fine on a LAN or a VPS with a public IP on its network card).
	VoicePublicAddresses []string
	// VoiceTCPPort is the PUBLIC port of the voice TCP fallback, which shares the main
	// HTTP(S) port. Docker maps 443 -> 8443, so there it must be set to 443.
	VoiceTCPPort int
}

// Load reads the configuration. A missing .env file is fine; an unreadable one is an error.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("reading .env: %w", err)
	}
	return fromEnv(os.Getenv)
}

// fromEnv builds and checks the config from a getenv function (tests pass a fake one).
func fromEnv(getenv func(string) string) (Config, error) {
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	cfg := Config{
		ServerName:  get("VIANDEN_SERVER_NAME", "Vianden Server"),
		DatabaseURL: get("VIANDEN_DATABASE_URL", ""),
		TLSMode:     TLSMode(strings.ToLower(get("VIANDEN_TLS_MODE", string(TLSPlain)))),
		ListenAddr:  get("VIANDEN_LISTEN_ADDR", "127.0.0.1:8080"),
		HTTPSAddr:   get("VIANDEN_HTTPS_ADDR", ":443"),
		HTTPAddr:    get("VIANDEN_HTTP_ADDR", ":80"),
		Domain:      strings.ToLower(get("VIANDEN_DOMAIN", "")),
		ACMEEmail:   get("VIANDEN_ACME_EMAIL", ""),
		DataDir:     get("VIANDEN_DATA_DIR", "./data"),
	}
	if err := voiceFromEnv(&cfg, get); err != nil {
		return Config{}, err
	}

	// No default on purpose: a default would mean a default password.
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("VIANDEN_DATABASE_URL is not set (copy .env.example to .env)")
	}
	if len(cfg.ServerName) > MaxServerNameLength {
		return Config{}, fmt.Errorf("VIANDEN_SERVER_NAME is longer than %d characters", MaxServerNameLength)
	}

	switch cfg.TLSMode {
	case TLSPlain, TLSSelfSigned:
	case TLSAutocert:
		if cfg.Domain == "" {
			return Config{}, errors.New("VIANDEN_TLS_MODE=autocert needs VIANDEN_DOMAIN (e.g. chat.example.com)")
		}
		if net.ParseIP(cfg.Domain) != nil {
			return Config{}, errors.New("VIANDEN_DOMAIN must be a domain name, not an IP address (Let's Encrypt only certifies names; use self-signed for IP-only servers)")
		}
	default:
		return Config{}, fmt.Errorf("VIANDEN_TLS_MODE must be plain, autocert, or self-signed (got %q)", cfg.TLSMode)
	}
	if cfg.Domain != "" && !validHostname(cfg.Domain) {
		return Config{}, fmt.Errorf("VIANDEN_DOMAIN %q is not a valid host name", cfg.Domain)
	}
	return cfg, nil
}

// validHostname accepts names like "chat.example.com" (letters, digits, hyphens, dots) or an IP.
func validHostname(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) > 253 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// voiceFromEnv reads the voice settings (M7).
func voiceFromEnv(cfg *Config, get func(key, fallback string) string) error {
	port, err := strconv.Atoi(get("VIANDEN_VOICE_UDP_PORT", "50000"))
	if err != nil || port < 0 || port > 65535 {
		return errors.New("VIANDEN_VOICE_UDP_PORT must be a port number (0 turns voice off)")
	}
	cfg.VoiceUDPPort = port

	// Default: the domain, if there is one (its DNS name points at the public IP).
	addrs := get("VIANDEN_VOICE_PUBLIC_ADDRESS", cfg.Domain)
	for _, a := range strings.Split(addrs, ",") {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if !validHostname(a) {
			return fmt.Errorf("VIANDEN_VOICE_PUBLIC_ADDRESS: %q is not an IP address or host name", a)
		}
		cfg.VoicePublicAddresses = append(cfg.VoicePublicAddresses, a)
	}

	// The TCP fallback shares the main port; by default its public port is that port.
	main := cfg.ListenAddr
	if cfg.TLSMode != TLSPlain {
		main = cfg.HTTPSAddr
	}
	_, mainPort, _ := net.SplitHostPort(main)
	tcp, err := strconv.Atoi(get("VIANDEN_VOICE_TCP_PORT", mainPort))
	if err != nil || tcp < 1 || tcp > 65535 {
		return errors.New("VIANDEN_VOICE_TCP_PORT must be a port number")
	}
	cfg.VoiceTCPPort = tcp
	return nil
}
