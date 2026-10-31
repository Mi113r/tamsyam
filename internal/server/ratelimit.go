package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxTrackedClients bounds the limiter's memory; expired entries are swept
// when it fills up.
const maxTrackedClients = 10000

// authLimiter blocks a client IP after too many failed logins (admin password
// or client token) within a window, so secrets cannot be brute-forced.
type authLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	now     func() time.Time
	clients map[string]*authFailures
}

type authFailures struct {
	count int
	since time.Time // first failure of the current window
}

func newAuthLimiter(maxFailures int, window time.Duration) *authLimiter {
	return &authLimiter{max: maxFailures, window: window, now: time.Now, clients: map[string]*authFailures{}}
}

// blocked returns how long ip must wait before trying again; zero means it may try.
func (l *authLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.clients[ip]
	if f == nil || f.count < l.max {
		return 0
	}
	return max(f.since.Add(l.window).Sub(l.now()), 0)
}

// fail records a failed attempt and reports whether it got ip blocked.
func (l *authLimiter) fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.clients[ip]
	if f == nil || now.Sub(f.since) >= l.window {
		if f == nil && len(l.clients) >= maxTrackedClients {
			l.sweep(now)
			if len(l.clients) >= maxTrackedClients {
				return false
			}
		}
		f = &authFailures{since: now}
		l.clients[ip] = f
	}
	f.count++
	return f.count == l.max
}

// succeed forgets ip's failures after a successful login.
func (l *authLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, ip)
}

func (l *authLimiter) sweep(now time.Time) {
	for ip, f := range l.clients {
		if now.Sub(f.since) >= l.window {
			delete(l.clients, ip)
		}
	}
}

func tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, fmt.Sprintf("too many failed login attempts, retry in %d s", secs), http.StatusTooManyRequests)
}
