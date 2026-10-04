package config

import (
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := fromEnv(env(map[string]string{"VIANDEN_DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSMode != TLSPlain || cfg.ListenAddr != "127.0.0.1:8080" || cfg.HTTPSAddr != ":443" || cfg.HTTPAddr != ":80" || cfg.DataDir != "./data" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestAutocertNeedsADomain(t *testing.T) {
	base := map[string]string{"VIANDEN_DATABASE_URL": "postgres://x", "VIANDEN_TLS_MODE": "autocert"}
	if _, err := fromEnv(env(base)); err == nil || !strings.Contains(err.Error(), "VIANDEN_DOMAIN") {
		t.Errorf("autocert without domain: err = %v", err)
	}

	base["VIANDEN_DOMAIN"] = "203.0.113.7"
	if _, err := fromEnv(env(base)); err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Errorf("autocert with an IP: err = %v", err)
	}

	base["VIANDEN_DOMAIN"] = "  Chat.Example.COM "
	cfg, err := fromEnv(env(base))
	if err != nil || cfg.Domain != "chat.example.com" {
		t.Errorf("valid domain: %+v, %v (should be trimmed and lowercased)", cfg.Domain, err)
	}
}

func TestTLSModeValues(t *testing.T) {
	for _, mode := range []string{"plain", "autocert", "self-signed", "SELF-SIGNED"} {
		vars := map[string]string{"VIANDEN_DATABASE_URL": "postgres://x", "VIANDEN_TLS_MODE": mode, "VIANDEN_DOMAIN": "chat.example.com"}
		if _, err := fromEnv(env(vars)); err != nil {
			t.Errorf("%q: %v", mode, err)
		}
	}
	vars := map[string]string{"VIANDEN_DATABASE_URL": "postgres://x", "VIANDEN_TLS_MODE": "https"}
	if _, err := fromEnv(env(vars)); err == nil {
		t.Error(`unknown mode "https" accepted`)
	}
}

func TestDomainValidation(t *testing.T) {
	for _, d := range []string{"chat.example.com", "a-b.example.org", "localhost", "192.168.1.10", "::1"} {
		if !validHostname(d) {
			t.Errorf("%q should be valid", d)
		}
	}
	for _, d := range []string{"-bad.com", "bad-.com", "a..b", ".com", "x.com.", "under_score.com", "evil.com/path", "space here.com", strings.Repeat("a", 64) + ".com"} {
		if validHostname(d) {
			t.Errorf("%q should be invalid", d)
		}
	}
}

func TestRequiredDatabaseURL(t *testing.T) {
	if _, err := fromEnv(env(map[string]string{})); err == nil {
		t.Error("missing database URL accepted")
	}
}
