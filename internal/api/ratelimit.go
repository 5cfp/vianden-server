package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipRateLimiter limits how often each client IP address may call an endpoint.
//
// It uses a "token bucket": each IP has a bucket holding up to `burst` tokens.
// Every request takes one token; tokens refill at `perSecond`. An empty bucket means 429.
// This slows down password guessing without bothering normal users.
//
// Only in memory: limits reset when the server restarts, which is fine for one server.
type ipRateLimiter struct {
	perSecond rate.Limit
	burst     int
	now       func() time.Time // replaceable in tests

	mu        sync.Mutex
	clients   map[string]*client
	lastSweep time.Time
}

type client struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(perMinute float64, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		perSecond: rate.Limit(perMinute / 60),
		burst:     burst,
		now:       time.Now,
		clients:   make(map[string]*client),
	}
}

// allow reports whether ip may make a request now. If not, it also returns how long to wait.
func (l *ipRateLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Forget clients not seen for 10 minutes (their bucket is full again anyway),
	// so the map cannot grow forever.
	if now.Sub(l.lastSweep) > time.Minute {
		for k, c := range l.clients {
			if now.Sub(c.lastSeen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.lastSweep = now
	}

	c, ok := l.clients[ip]
	if !ok {
		c = &client{limiter: rate.NewLimiter(l.perSecond, l.burst)}
		l.clients[ip] = c
	}
	c.lastSeen = now

	r := c.limiter.ReserveN(now, 1)
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now) // do not count a refused request
		return false, delay
	}
	return true, 0
}

// limit wraps a handler: requests over the limit get 429 Too Many Requests.
func (l *ipRateLimiter) limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(rateLimitKey(clientIP(r))); !ok {
			// Retry-After tells well-behaved clients how many seconds to wait.
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
			return
		}
		next(w, r)
	}
}

// clientIP returns the IP address of the connection.
//
// It does NOT trust headers like X-Forwarded-For: anyone can send those, so trusting them
// would let an attacker pick a new fake IP for every request and bypass the limit.
// (Running behind a reverse proxy needs a "trusted proxy" setting; see M4.)
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitKey groups IPv6 addresses by their /64 network. One home connection usually gets
// a whole /64 (billions of addresses), so limiting single IPv6 addresses would let an
// attacker use a fresh address for every request. IPv4 addresses are used as they are.
func rateLimitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
