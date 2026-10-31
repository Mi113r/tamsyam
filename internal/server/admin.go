package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Mi113r/tamsyam/internal/protocol"
	"github.com/Mi113r/tamsyam/internal/store"
)

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/logo.svg
var logoSVG []byte

func (s *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Revalidate on every load, so an upgraded server is not shown an old UI.
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(adminHTML)
	})
	mux.HandleFunc("GET /api/info", s.apiInfo)
	mux.HandleFunc("GET /api/tunnels", s.apiListTunnels)
	mux.HandleFunc("DELETE /api/tunnels/{key}", s.apiKickTunnel)
	mux.HandleFunc("GET /api/tunnels/{key}/requests", s.apiTunnelRequests)
	mux.HandleFunc("GET /api/tunnels/{key}/requests/{id}", s.apiRequestDetail)
	mux.HandleFunc("POST /api/tunnels/{key}/requests/{id}/replay", s.apiReplayRequest)
	mux.HandleFunc("GET /api/tunnels/{key}/connections", s.apiTunnelConnections)
	mux.HandleFunc("GET /api/stats", s.apiStats)
	mux.HandleFunc("GET /api/reservations", s.apiListReservations)
	mux.HandleFunc("DELETE /api/reservations/{name}", s.apiRelease)
	mux.HandleFunc("GET /api/tokens", s.apiListTokens)
	mux.HandleFunc("POST /api/tokens", s.apiCreateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.apiRevokeToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(logoSVG)
	})
	return s.basicAuth(mux)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/favicon.svg" {
			next.ServeHTTP(w, r)
			return
		}
		if s.fromPrivateNetwork(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if wait := s.limiter.blocked(ip); wait > 0 {
			tooManyAttempts(w, wait)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) != 1 {
			// A request without credentials is the browser asking for the
			// login prompt, not a guess.
			if ok {
				s.authFailed(ip, "admin password")
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="tamsyam admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.limiter.succeed(ip)
		next.ServeHTTP(w, r)
	})
}

// fromPrivateNetwork reports whether r comes from TAMSYAM_PRIVATE_NETWORKS.
// Requests forwarded by an unknown proxy never qualify: their address is the
// proxy's, and a proxy inside the LAN would otherwise open the panel to everyone.
func (s *Server) fromPrivateNetwork(r *http.Request) bool {
	if len(s.cfg.PrivateNetworks) == 0 || r.Context().Value(unknownProxyKey{}) != nil {
		return false
	}
	return containsAddr(s.cfg.PrivateNetworks, peerAddr(r.RemoteAddr))
}

func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	tcp := ""
	if s.cfg.TCPPortMin > 0 {
		tcp = fmt.Sprintf("%d-%d", s.cfg.TCPPortMin, s.cfg.TCPPortMax)
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"domain":    s.cfg.Domain,
		"server":    s.cfg.PublicScheme + "://" + r.Host,
		"version":   protocol.Version,
		"tcp_ports": tcp,
	})
}

func (s *Server) apiListTunnels(w http.ResponseWriter, r *http.Request) {
	list := s.registry.List()
	sort.Slice(list, func(i, j int) bool { return list[i].ConnectedAt.After(list[j].ConnectedAt) })
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiKickTunnel(w http.ResponseWriter, r *http.Request) {
	t := s.registry.Get(r.PathValue("key"))
	if t == nil {
		writeError(w, http.StatusNotFound, "tunnel not found")
		return
	}
	t.Close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiTunnelRequests(w http.ResponseWriter, r *http.Request) {
	t := s.registry.Get(r.PathValue("key"))
	if t == nil {
		writeError(w, http.StatusNotFound, "tunnel not found")
		return
	}
	writeJSON(w, http.StatusOK, t.RequestLog())
}

func (s *Server) apiRequestDetail(w http.ResponseWriter, r *http.Request) {
	t, id, ok := s.tunnelRequest(w, r)
	if !ok {
		return
	}
	d, err := t.RequestDetail(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) apiReplayRequest(w http.ResponseWriter, r *http.Request) {
	t, id, ok := s.tunnelRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	newID, err := t.Replay(ctx, id)
	switch {
	case errors.Is(err, errRequestNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errNotReplayable):
		writeError(w, http.StatusUnprocessableEntity, "WebSocket upgrades and truncated bodies cannot be replayed")
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]uint64{"id": newID})
	}
}

func (s *Server) tunnelRequest(w http.ResponseWriter, r *http.Request) (*Tunnel, uint64, bool) {
	t := s.registry.Get(r.PathValue("key"))
	if t == nil {
		writeError(w, http.StatusNotFound, "tunnel not found")
		return nil, 0, false
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad id")
		return nil, 0, false
	}
	return t, id, true
}

func (s *Server) apiListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListTokens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	tok, plain, err := s.store.CreateToken(strings.TrimSpace(body.Name))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "secret": plain})
}

func (s *Server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.RevokeToken(id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Revocation also drops the token's live tunnels.
	for _, t := range s.registry.List() {
		if t.TokenID == id {
			if live := s.registry.Get(t.Key); live != nil {
				live.Close()
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) apiTunnelConnections(w http.ResponseWriter, r *http.Request) {
	t := s.registry.Get(r.PathValue("key"))
	if t == nil {
		writeError(w, http.StatusNotFound, "tunnel not found")
		return
	}
	writeJSON(w, http.StatusOK, t.ConnLog())
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	rep, err := s.statsReport(r.URL.Query().Get("range"), r.URL.Query().Get("tunnel"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) apiListReservations(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListReservations()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// apiRelease frees a reserved TCP port (numeric name) or subdomain.
func (s *Server) apiRelease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var err error
	if port, perr := strconv.Atoi(name); perr == nil {
		err = s.store.ReleasePort(port)
	} else {
		err = s.store.ReleaseSubdomain(strings.ToLower(name))
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not reserved")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
