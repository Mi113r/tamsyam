package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"
)

// basicCreds protects an HTTP tunnel with basic auth (ts http --auth user:pass).
// Only hashes are kept, so the password does not linger in memory as text.
type basicCreds struct {
	user, pass [sha256.Size]byte
}

func parseBasicCreds(v string) (*basicCreds, error) {
	user, pass, ok := strings.Cut(v, ":")
	if !ok || user == "" || pass == "" {
		return nil, errors.New("auth must look like user:password")
	}
	return &basicCreds{user: sha256.Sum256([]byte(user)), pass: sha256.Sum256([]byte(pass))}, nil
}

func (c *basicCreds) match(user, pass string) bool {
	u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	return subtle.ConstantTimeCompare(u[:], c.user[:])&subtle.ConstantTimeCompare(p[:], c.pass[:]) == 1
}

// checkAuth enforces the tunnel's basic auth. It strips the credentials so
// they reach neither the local service nor the inspector, and blocks IPs that
// keep guessing (per tunnel, so it never locks anyone out of the admin panel).
func (t *Tunnel) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if t.auth == nil {
		return true
	}
	ip := clientIP(r)
	if wait := t.authFails.blocked(ip); wait > 0 {
		tooManyAttempts(w, wait)
		return false
	}
	user, pass, ok := r.BasicAuth()
	if !ok || !t.auth.match(user, pass) {
		if ok {
			t.authFails.fail(ip)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="tamsyam tunnel", charset="UTF-8"`)
		renderError(w, http.StatusUnauthorized, "Login required", "This tunnel is protected with a password.")
		return false
	}
	t.authFails.succeed(ip)
	r.Header.Del("Authorization")
	return true
}

const (
	tunnelAuthMaxFailures = 10
	tunnelAuthWindow      = 15 * time.Minute
)
