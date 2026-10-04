package main

import "testing"

func TestLoopback(t *testing.T) {
	cases := map[string]string{
		":8443":            "127.0.0.1:8443",
		"0.0.0.0:80":       "127.0.0.1:80",
		"[::]:443":         "127.0.0.1:443",
		"127.0.0.1:8080":   "127.0.0.1:8080",
		"192.168.1.5:8080": "192.168.1.5:8080",
	}
	for in, want := range cases {
		if got := loopback(in); got != want {
			t.Errorf("loopback(%q) = %q, want %q", in, got, want)
		}
	}
}
