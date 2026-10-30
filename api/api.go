// Package api exposes an Engine over HTTP/JSON: the native API under /v1
// and a Mem0-compatible API under /mem0. The handler is a plain
// http.Handler, so it runs in a server, behind a Lambda adapter, or inside
// another application's router.
//
// Every request is authenticated to an Identity (tenant + principal). A key
// bound to a user or agent can only act as that user or agent; admin keys
// can act for anyone in their tenant. See docs/api.md.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/metrics"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Identity is who a request acts as.
type Identity struct {
	Tenant    string           `json:"tenant"`
	Principal memory.Principal `json:"principal"`
}

// Authenticator resolves a request to an Identity.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// ErrUnauthorized is returned by authenticators for missing or bad credentials.
var ErrUnauthorized = errors.New("unauthorized")

// errForbidden is returned when an identity acts outside its binding.
var errForbidden = errors.New("forbidden")

// Key configures one API key.
type Key struct {
	// Key is the secret, or SHA256 its hex SHA-256 (preferred in config files).
	Key    string   `json:"key,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Tenant string   `json:"tenant"`
	User   string   `json:"user_id,omitempty"`
	Agent  string   `json:"agent_id,omitempty"`
	Roles  []string `json:"roles,omitempty"`
	Admin  bool     `json:"admin,omitempty"`
}

// Keys authenticates "Authorization: Bearer <key>" or "X-API-Key: <key>"
// against hashed keys.
type Keys struct{ byHash map[[32]byte]Identity }

// NewKeys validates and hashes key definitions.
func NewKeys(keys []Key) (*Keys, error) {
	k := &Keys{byHash: map[[32]byte]Identity{}}
	for i, def := range keys {
		var h [32]byte
		switch {
		case def.SHA256 != "":
			b, err := hex.DecodeString(def.SHA256)
			if err != nil || len(b) != 32 {
				return nil, fmt.Errorf("api key %d: bad sha256", i)
			}
			copy(h[:], b)
		case len(def.Key) >= 16:
			h = sha256.Sum256([]byte(def.Key))
		default:
			return nil, fmt.Errorf("api key %d: key must have at least 16 characters", i)
		}
		if def.Tenant == "" {
			return nil, fmt.Errorf("api key %d: tenant is required", i)
		}
		k.byHash[h] = Identity{Tenant: def.Tenant, Principal: memory.Principal{User: def.User, Agent: def.Agent, Roles: def.Roles, Admin: def.Admin}}
	}
	return k, nil
}

// Authenticate looks up the presented key by its hash.
func (k *Keys) Authenticate(r *http.Request) (Identity, error) {
	key := r.Header.Get("X-API-Key")
	if a := r.Header.Get("Authorization"); key == "" && len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		key = strings.TrimSpace(a[7:])
	}
	if a := r.Header.Get("Authorization"); key == "" && len(a) > 6 && strings.EqualFold(a[:6], "token ") {
		key = strings.TrimSpace(a[6:]) // Mem0 clients send "Token <key>"
	}
	if key == "" {
		return Identity{}, ErrUnauthorized
	}
	h := sha256.Sum256([]byte(key))
	for stored, id := range k.byHash {
		if subtle.ConstantTimeCompare(stored[:], h[:]) == 1 {
			return id, nil
		}
	}
	return Identity{}, ErrUnauthorized
}

// Dev trusts X-Tenant (default "dev") and grants admin. Only for local
// development; the server refuses it in production mode.
type Dev struct{}

// Authenticate returns an admin identity for the X-Tenant header.
func (Dev) Authenticate(r *http.Request) (Identity, error) {
	t := r.Header.Get("X-Tenant")
	if t == "" {
		t = "dev"
	}
	return Identity{Tenant: t, Principal: memory.Principal{Admin: true}}, nil
}

// Server serves the HTTP API.
type Server struct {
	Engine  *niromem.Engine
	Auth    Authenticator
	Metrics *metrics.Registry // optional: /metrics and request timings
	Logger  *slog.Logger
	// MaxBody limits request bodies (default 32 MiB, for inline media).
	MaxBody int64
}

type ctxKey struct{}

func identity(r *http.Request) Identity { return r.Context().Value(ctxKey{}).(Identity) }

// Handler returns the API handler.
func (s *Server) Handler() http.Handler {
	if s.MaxBody == 0 {
		s.MaxBody = 32 << 20
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	if s.Metrics != nil {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			s.Metrics.WritePrometheus(w)
		})
	}
	s.native(mux)
	s.mem0(mux)
	return mux
}

// route wraps a handler with authentication, body limits, metrics and
// panic recovery.
func (s *Server) route(mux *http.ServeMux, pattern string, h func(w http.ResponseWriter, r *http.Request) error) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var err error
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
				s.Logger.Error("handler panic", "pattern", pattern, "panic", p)
				writeError(w, err, s.Logger)
			}
			if s.Metrics != nil {
				s.Metrics.Observe("http "+pattern, time.Since(start), err)
			}
		}()
		id, aerr := s.Auth.Authenticate(r)
		if aerr != nil {
			err = aerr
			writeError(w, ErrUnauthorized, s.Logger)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxBody)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
		if err = h(w, r); err != nil {
			writeError(w, err, s.Logger)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, err error, log *slog.Logger) {
	status, msg := http.StatusInternalServerError, "internal error"
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "missing or invalid API key"
	case errors.Is(err, errForbidden):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, memory.ErrInvalid), errors.Is(err, vector.ErrDims), errors.As(err, new(*json.SyntaxError)), errors.As(err, new(*json.UnmarshalTypeError)):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.As(err, &mbe):
		status, msg = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, memory.ErrNotFound):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, memory.ErrConflict):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status, msg = http.StatusGatewayTimeout, "deadline exceeded"
	default:
		log.Error("request failed", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{msg})
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return fmt.Errorf("%w: body: %v", memory.ErrInvalid, err)
	}
	return nil
}

// scope applies the identity's binding to requested user/agent ids: a
// bound identity may omit them or repeat its own, never use another.
func scope(id Identity, user, agent string) (string, string, error) {
	p := id.Principal
	if p.Admin {
		return user, agent, nil
	}
	if p.User != "" {
		if user != "" && user != p.User {
			return "", "", fmt.Errorf("%w: key is bound to user %q", errForbidden, p.User)
		}
		user = p.User
	}
	if p.Agent != "" {
		if agent != "" && agent != p.Agent {
			return "", "", fmt.Errorf("%w: key is bound to agent %q", errForbidden, p.Agent)
		}
		agent = p.Agent
	}
	return user, agent, nil
}

// principal is the identity's principal, narrowed to the requested user
// and agent for admins acting on behalf of someone.
func principal(id Identity, user, agent string) memory.Principal {
	if id.Principal.Admin && (user != "" || agent != "") {
		return memory.Principal{User: user, Agent: agent, Roles: id.Principal.Roles}
	}
	if id.Principal.Admin {
		return id.Principal
	}
	p := id.Principal
	if p.User == "" {
		p.User = user
	}
	if p.Agent == "" {
		p.Agent = agent
	}
	return p
}

func filterParam(raw json.RawMessage) (memory.Filter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return memory.Filter{}, nil
	}
	return memory.ParseFilter(raw)
}

func qInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func qTime(r *http.Request, name string) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := memory.ParseTime(v)
	if err != nil {
		return t, fmt.Errorf("%w: %s", memory.ErrInvalid, name)
	}
	return t, nil
}

func qBool(r *http.Request, name string) bool {
	b, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return b
}
