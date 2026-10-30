package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
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

func (s *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminHTML)
	})
	mux.HandleFunc("GET /api/info", s.apiInfo)
	mux.HandleFunc("GET /api/tunnels", s.apiListTunnels)
	mux.HandleFunc("DELETE /api/tunnels/{sub}", s.apiKickTunnel)
	mux.HandleFunc("GET /api/tunnels/{sub}/requests", s.apiTunnelRequests)
	mux.HandleFunc("GET /api/tunnels/{sub}/requests/{id}", s.apiRequestDetail)
	mux.HandleFunc("POST /api/tunnels/{sub}/requests/{id}/replay", s.apiReplayRequest)
	mux.HandleFunc("GET /api/tokens", s.apiListTokens)
	mux.HandleFunc("POST /api/tokens", s.apiCreateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.apiRevokeToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return s.basicAuth(mux)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="tamsyam admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"domain":  s.cfg.Domain,
		"server":  s.cfg.PublicScheme + "://" + r.Host,
		"version": protocol.Version,
	})
}

func (s *Server) apiListTunnels(w http.ResponseWriter, r *http.Request) {
	list := s.registry.List()
	sort.Slice(list, func(i, j int) bool { return list[i].ConnectedAt.After(list[j].ConnectedAt) })
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiKickTunnel(w http.ResponseWriter, r *http.Request) {
	t := s.registry.Get(r.PathValue("sub"))
	if t == nil {
		writeError(w, http.StatusNotFound, "tunnel not found")
		return
	}
	t.Close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiTunnelRequests(w http.ResponseWriter, r *http.Request) {
	t := s.registry.Get(r.PathValue("sub"))
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
	t := s.registry.Get(r.PathValue("sub"))
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
			if live := s.registry.Get(t.Subdomain); live != nil {
				live.Close()
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
