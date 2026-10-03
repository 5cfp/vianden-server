package api

import (
	"testing"
	"time"
)

// newTestLimiter returns a limiter with a fake clock that tests can move forward.
func newTestLimiter(perMinute float64, burst int) (*ipRateLimiter, *time.Time) {
	l := newIPRateLimiter(perMinute, burst)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestRateLimitAllowsBurstThenBlocks(t *testing.T) {
	l, _ := newTestLimiter(10, 3)

	for i := range 3 {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d was blocked, want allowed (within burst)", i+1)
		}
	}
	ok, wait := l.allow("1.2.3.4")
	if ok {
		t.Fatal("4th request allowed, want blocked")
	}
	if wait <= 0 || wait > 6*time.Second {
		t.Errorf("wait = %v, want up to 6s (10 per minute)", wait)
	}
}

func TestRateLimitRefillsOverTime(t *testing.T) {
	l, now := newTestLimiter(10, 1)

	l.allow("1.2.3.4")
	if ok, _ := l.allow("1.2.3.4"); ok {
		t.Fatal("second request allowed immediately")
	}
	*now = now.Add(6 * time.Second) // 10 per minute = one every 6 seconds
	if ok, _ := l.allow("1.2.3.4"); !ok {
		t.Error("request after 6s was blocked, want allowed")
	}
}

func TestRefusedRequestsDoNotExtendTheWait(t *testing.T) {
	l, now := newTestLimiter(10, 1)
	l.allow("1.2.3.4")

	for range 50 { // hammering while blocked must not push the next slot further away
		l.allow("1.2.3.4")
	}
	*now = now.Add(6 * time.Second)
	if ok, _ := l.allow("1.2.3.4"); !ok {
		t.Error("blocked after waiting the full interval")
	}
}

func TestRateLimitIsPerClient(t *testing.T) {
	l, _ := newTestLimiter(10, 1)

	l.allow("1.2.3.4")
	if ok, _ := l.allow("5.6.7.8"); !ok {
		t.Error("a different IP was blocked by someone else's requests")
	}
}

func TestOldClientsAreForgotten(t *testing.T) {
	l, now := newTestLimiter(10, 1)
	l.allow("1.2.3.4")

	*now = now.Add(11 * time.Minute)
	l.allow("5.6.7.8") // triggers the cleanup sweep

	if _, found := l.clients["1.2.3.4"]; found {
		t.Error("idle client was not removed; the map would grow forever")
	}
}

func TestRateLimitKey(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":                   "1.2.3.4",
		"2001:db8:1:2:aaaa::1":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::9": "2001:db8:1:2::/64", // same /64 as above: same bucket
		"::1":                       "::/64",
		"not-an-ip":                 "not-an-ip",
	}
	for in, want := range cases {
		if got := rateLimitKey(in); got != want {
			t.Errorf("rateLimitKey(%q) = %q, want %q", in, got, want)
		}
	}
}
