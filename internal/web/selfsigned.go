package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/5cfp/vianden-server/internal/config"
)

// loadOrCreateSelfSigned loads the server's own certificate, or makes one on first start.
//
// A self-signed certificate still ENCRYPTS everything, but no authority vouches for it,
// so a client cannot tell it apart from an attacker's certificate the first time.
// That is why clients use "trust on first use" (TOFU): they show the certificate's
// fingerprint once, the user compares it with the one the server owner shares (printed
// below at every start), and the client remembers it and warns loudly if it ever changes.
func loadOrCreateSelfSigned(cfg config.Config, logger *slog.Logger) (tls.Certificate, error) {
	dir := filepath.Join(cfg.DataDir, "self-signed")
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if errors.Is(err, fs.ErrNotExist) {
		if err := createSelfSigned(dir, certFile, keyFile, cfg.Domain); err != nil {
			return tls.Certificate{}, fmt.Errorf("creating self-signed certificate: %w", err)
		}
		logger.Info("created a new self-signed certificate", "file", certFile)
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading self-signed certificate: %w", err)
	}

	fp := Fingerprint(cert.Certificate[0])
	fmt.Println()
	fmt.Println("  HTTPS with a self-signed certificate. Its SHA-256 fingerprint is:")
	fmt.Println("    " + fp)
	fmt.Println("  Share it with your users: the app asks them to compare it the first time they connect.")
	fmt.Println()
	logger.Info("HTTPS with a self-signed certificate", "https", cfg.HTTPSAddr, "fingerprint", fp)
	return cert, nil
}

func createSelfSigned(dir, certFile, keyFile, domain string) error {
	// ECDSA P-256: a small, fast, modern key type, supported by every TLS client.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Vianden self-signed"},
		NotBefore:    time.Now().Add(-time.Hour),
		// Long validity on purpose: with TOFU, a NEW certificate means every user sees a
		// "certificate changed" warning, so it should not change often.
		NotAfter:    time.Now().AddDate(10, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if domain != "" {
		if ip := net.ParseIP(domain); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, domain)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The private key is readable by the server's user only (0600): with it, anyone could
	// impersonate the server.
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// Fingerprint is the SHA-256 of a certificate (DER bytes), as uppercase hex pairs joined by
// colons: "AB:CD:...". Clients compute the same value to pin the certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
