package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

func (s *Server) native(mux *http.ServeMux) {
	e := s.Engine
	s.knowledge(mux)
	s.maintenance(mux)
	s.jobs(mux)
	s.route(mux, "GET /v1/explore", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		u, a, err := scope(id, r.URL.Query().Get("user_id"), r.URL.Query().Get("agent_id"))
		if err != nil {
			return err
		}
		result, err := e.Explore(r.Context(), niromem.ListRequest{Tenant: id.Tenant, As: principal(id, u, a), User: u, Agent: a,
			AllVersions: qBool(r, "all_versions"), ShowExpired: qBool(r, "show_expired"),
			Limit: qInt(r, "limit", 100), Offset: qInt(r, "offset", 0), BeforeID: r.URL.Query().Get("before_id")})
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
	s.route(mux, "POST /v1/relationships/backfill", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: relationship backfill requires an admin key", errForbidden)
		}
		if err := e.BackfillRelationships(r.Context(), identity(r).Tenant); err != nil {
			return err
		}
		return writeJSON(w, 202, map[string]string{"status": "queued"})
	})
	s.route(mux, "GET /v1/capabilities", func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, 200, e.Capabilities())
	})
	s.route(mux, "GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, 200, identity(r))
	})

	s.route(mux, "POST /v1/facts", func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Facts []niromem.Fact `json:"facts"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		id := identity(r)
		for i := range body.Facts {
			f := &body.Facts[i]
			if key := r.Header.Get("Idempotency-Key"); key != "" {
				if f.OperationKey != "" && f.OperationKey != key {
					return fmt.Errorf("%w: conflicting operation keys", memory.ErrInvalid)
				}
				f.OperationKey = key
			}
			u, a, err := scope(id, f.User, f.Agent)
			if err != nil {
				return err
			}
			f.Tenant, f.User, f.Agent = id.Tenant, u, a
			if f.Actor == "" {
				f.Actor = firstNonEmpty(id.Principal.Agent, id.Principal.User, "api")
			}
		}
		ch, err := e.Remember(r.Context(), body.Facts...)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"changes": ch})
	})

	s.route(mux, "GET /v1/memories", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		q := r.URL.Query()
		u, a, err := scope(id, q.Get("user_id"), q.Get("agent_id"))
		if err != nil {
			return err
		}
		f, err := filterParam(json.RawMessage(q.Get("filter")))
		if err != nil {
			return err
		}
		lr := niromem.ListRequest{Tenant: id.Tenant, As: principal(id, u, a), User: u, Agent: a, Run: q.Get("run_id"), Filter: f,
			AllVersions: qBool(r, "all_versions"), ShowExpired: qBool(r, "show_expired"), Order: store.Order(q.Get("order")),
			Limit: qInt(r, "limit", 50), Offset: qInt(r, "offset", 0), BeforeID: q.Get("before_id")}
		if lr.ValidAt, err = qTime(r, "valid_at"); err != nil {
			return err
		}
		if lr.KnownAt, err = qTime(r, "known_at"); err != nil {
			return err
		}
		ms, err := e.List(r.Context(), lr)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"memories": nonNil(ms)})
	})

	s.route(mux, "GET /v1/memories/{id}", func(w http.ResponseWriter, r *http.Request) error {
		m, err := s.readable(r, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, m)
	})

	s.route(mux, "PATCH /v1/memories/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.readable(r, r.PathValue("id")); err != nil {
			return err
		}
		var p niromem.Patch
		if err := decode(r, &p); err != nil {
			return err
		}
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			if p.OperationKey != "" && p.OperationKey != key {
				return fmt.Errorf("%w: conflicting operation keys", memory.ErrInvalid)
			}
			p.OperationKey = key
		}
		id := identity(r)
		p.Actor = firstNonEmpty(p.Actor, id.Principal.Agent, id.Principal.User, "api")
		m, err := e.Update(r.Context(), id.Tenant, r.PathValue("id"), p)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, m)
	})

	s.route(mux, "POST /v1/memories/{id}/invalidate", func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.readable(r, r.PathValue("id")); err != nil {
			return err
		}
		var body struct {
			At     time.Time `json:"at"`
			Reason string    `json:"reason"`
		}
		if r.ContentLength != 0 {
			if err := decode(r, &body); err != nil {
				return err
			}
		}
		id := identity(r)
		if err := e.InvalidateWithKey(r.Context(), id.Tenant, r.PathValue("id"), r.Header.Get("Idempotency-Key"), body.At, firstNonEmpty(id.Principal.Agent, id.Principal.User, "api"), body.Reason); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "invalidated"})
	})

	s.route(mux, "DELETE /v1/memories/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.readable(r, r.PathValue("id")); err != nil {
			key := r.Header.Get("Idempotency-Key")
			operations, ok := e.Store().(store.Operations)
			if !errors.Is(err, memory.ErrNotFound) || key == "" || !ok {
				return err
			}
			// A successful deletion removes its ACL record. Only an existing
			// receipt permits retry; DeleteWithKey still verifies the input hash.
			if _, receiptErr := operations.Operation(r.Context(), identity(r).Tenant, "delete:"+key); receiptErr != nil {
				if errors.Is(receiptErr, memory.ErrNotFound) {
					return err
				}
				return receiptErr
			}
		}
		id := identity(r)
		if err := e.DeleteWithKey(r.Context(), id.Tenant, firstNonEmpty(id.Principal.Agent, id.Principal.User, "api"), r.Header.Get("Idempotency-Key"), r.PathValue("id")); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "deleted"})
	})

	s.route(mux, "GET /v1/memories/{id}/history", func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.readable(r, r.PathValue("id")); err != nil && !isAdmin(r) {
			return err
		}
		h, err := e.History(r.Context(), identity(r).Tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"events": nonNil(h)})
	})

	s.route(mux, "GET /v1/memories/{id}/versions", func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.readable(r, r.PathValue("id")); err != nil {
			return err
		}
		vs, err := e.Versions(r.Context(), identity(r).Tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"versions": nonNil(vs)})
	})

	s.route(mux, "POST /v1/search", func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			niromem.SearchRequest
			Filter json.RawMessage `json:"filter"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		req, err := s.searchRequest(r, body.SearchRequest, body.Filter)
		if err != nil {
			return err
		}
		res, err := e.Search(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, res)
	})

	s.route(mux, "POST /v1/context", func(w http.ResponseWriter, r *http.Request) error {
		var req niromem.ContextRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		id := identity(r)
		u, a, err := scope(id, req.User, req.Agent)
		if err != nil {
			return err
		}
		req.Tenant, req.User, req.Agent, req.As = id.Tenant, u, a, principal(id, u, a)
		req.Timeout *= time.Millisecond // JSON carries milliseconds
		c, err := e.Context(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"context": c, "rendered": c.Render()})
	})

	s.route(mux, "POST /v1/runs/{run}/turns", func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			User  string         `json:"user_id"`
			Agent string         `json:"agent_id"`
			Turns []niromem.Turn `json:"turns"`
			Wait  bool           `json:"wait"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		id := identity(r)
		u, a, err := scope(id, body.User, body.Agent)
		if err != nil {
			return err
		}
		res, err := e.Append(r.Context(), memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: r.PathValue("run")}, body.Turns, niromem.AppendOptions{Wait: body.Wait})
		if err != nil {
			return err
		}
		return writeJSON(w, 200, res)
	})

	s.route(mux, "GET /v1/runs/{run}/turns", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsRun(r, r.PathValue("run")); err != nil {
			return err
		}
		eps, err := e.Turns(r.Context(), niromem.TurnsRequest{Tenant: identity(r).Tenant, Run: r.PathValue("run"), Last: qInt(r, "last", 0),
			BeforeSeq: int64(qInt(r, "before_seq", 0)), AfterSeq: int64(qInt(r, "after_seq", 0)), IncludeDeleted: qBool(r, "include_deleted")})
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"turns": nonNil(eps)})
	})

	s.route(mux, "GET /v1/runs/{run}/window", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsRun(r, r.PathValue("run")); err != nil {
			return err
		}
		win, err := e.Window(r.Context(), identity(r).Tenant, r.PathValue("run"), qInt(r, "tokens", 2000), nil)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, win)
	})

	s.route(mux, "PUT /v1/runs/{run}/summary", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsRun(r, r.PathValue("run")); err != nil {
			return err
		}
		var body struct {
			Text string `json:"text"`
			Upto int64  `json:"upto"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if err := e.SetSummary(r.Context(), identity(r).Tenant, r.PathValue("run"), body.Text, body.Upto); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	s.route(mux, "POST /v1/runs/{run}/process", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsRun(r, r.PathValue("run")); err != nil {
			return err
		}
		st, err := e.Process(r.Context(), identity(r).Tenant, r.PathValue("run"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, st)
	})

	s.route(mux, "POST /v1/history/search", func(w http.ResponseWriter, r *http.Request) error {
		var q niromem.HistoryQuery
		if err := decode(r, &q); err != nil {
			return err
		}
		id := identity(r)
		u, a, err := scope(id, q.User, q.Agent)
		if err != nil {
			return err
		}
		q.Tenant, q.User, q.Agent, q.As = id.Tenant, u, a, id.Principal
		hits, err := e.SearchHistory(r.Context(), q)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"hits": nonNil(hits)})
	})

	s.route(mux, "PATCH /v1/turns/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsTurn(r, r.PathValue("id")); err != nil {
			return err
		}
		var body struct {
			Parts []niromem.PartInput `json:"parts"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if err := e.EditTurn(r.Context(), identity(r).Tenant, r.PathValue("id"), body.Parts); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "edited"})
	})

	s.route(mux, "DELETE /v1/turns/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := s.ownsTurn(r, r.PathValue("id")); err != nil {
			return err
		}
		if err := e.DeleteTurn(r.Context(), identity(r).Tenant, r.PathValue("id")); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "deleted"})
	})

	s.route(mux, "GET /v1/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		if !id.Principal.Admin {
			eps, err := e.Store().Episodes(r.Context(), store.EpisodeQuery{Tenant: id.Tenant, BlobSHA: r.PathValue("sha"), User: id.Principal.User, Limit: 1})
			if err != nil {
				return err
			}
			if len(eps) == 0 {
				return fmt.Errorf("blob: %w", memory.ErrNotFound)
			}
		}
		b, err := e.Blob(r.Context(), id.Tenant, r.PathValue("sha"))
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		_, err = w.Write(b)
		return err
	})

	s.route(mux, "GET /v1/entities", func(w http.ResponseWriter, r *http.Request) error {
		es, err := e.Entities(r.Context(), identity(r).Tenant, r.URL.Query().Get("prefix"), qInt(r, "limit", 50))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"entities": nonNil(es)})
	})

	s.route(mux, "GET /v1/entities/{name}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		n, err := e.Neighbors(r.Context(), id.Tenant, principal(id, "", ""), r.PathValue("name"), qInt(r, "limit", 50))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, n)
	})

	s.route(mux, "POST /v1/entities/{key}/aliases", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: aliases change the whole tenant; admin key required", errForbidden)
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if err := e.AddAlias(r.Context(), identity(r).Tenant, r.PathValue("key"), body.Name); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	s.route(mux, "POST /v1/forget", func(w http.ResponseWriter, r *http.Request) error {
		var body memory.Scope
		if err := decode(r, &body); err != nil {
			return err
		}
		id := identity(r)
		u, a, err := scope(id, body.User, body.Agent)
		if err != nil {
			return err
		}
		res, err := e.Forget(r.Context(), memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: body.Run}, firstNonEmpty(id.Principal.Agent, id.Principal.User, "api"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, res)
	})

	s.route(mux, "GET /v1/settings", func(w http.ResponseWriter, r *http.Request) error {
		st, err := e.Settings(r.Context(), identity(r).Tenant)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, st)
	})

	s.route(mux, "PUT /v1/settings", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: admin key required", errForbidden)
		}
		var st niromem.Settings
		if err := decode(r, &st); err != nil {
			return err
		}
		if err := e.PutSettings(r.Context(), identity(r).Tenant, st); err != nil {
			return err
		}
		return writeJSON(w, 200, st)
	})

	s.route(mux, "GET /v1/vectors/coverage", func(w http.ResponseWriter, r *http.Request) error {
		have, total, err := e.Coverage(r.Context(), identity(r).Tenant, r.URL.Query().Get("model"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]int{"have": have, "total": total})
	})

	s.route(mux, "GET /v1/vectors/missing", func(w http.ResponseWriter, r *http.Request) error {
		ts, err := e.MissingVectors(r.Context(), identity(r).Tenant, r.URL.Query().Get("model"), qInt(r, "limit", 100))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"targets": nonNil(ts)})
	})

	s.route(mux, "PUT /v1/vectors", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: admin key required", errForbidden)
		}
		var body struct {
			Points []vector.Point `json:"points"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if err := e.PutVectors(r.Context(), identity(r).Tenant, body.Points); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]int{"stored": len(body.Points)})
	})

	s.route(mux, "POST /v1/vectors/backfill", func(w http.ResponseWriter, r *http.Request) error {
		if err := e.Backfill(r.Context(), identity(r).Tenant); err != nil {
			return err
		}
		return writeJSON(w, 202, map[string]string{"status": "queued"})
	})
}

func (s *Server) searchRequest(r *http.Request, req niromem.SearchRequest, rawFilter json.RawMessage) (niromem.SearchRequest, error) {
	id := identity(r)
	u, a, err := scope(id, req.User, req.Agent)
	if err != nil {
		return req, err
	}
	f, err := filterParam(rawFilter)
	if err != nil {
		return req, err
	}
	req.Tenant, req.User, req.Agent, req.As, req.Filter = id.Tenant, u, a, principal(id, u, a), f
	return req, nil
}

// readable returns a memory if the caller may read it.
func (s *Server) readable(r *http.Request, mid string) (memory.Memory, error) {
	id := identity(r)
	return s.Engine.Get(r.Context(), id.Tenant, id.Principal, mid)
}

// ownsRun checks a bound identity owns the run (by its user/agent).
func (s *Server) ownsRun(r *http.Request, run string) error {
	id := identity(r)
	if id.Principal.Admin {
		return nil
	}
	st, err := s.Engine.Store().Run(r.Context(), id.Tenant, run)
	if err != nil {
		return err
	}
	if (id.Principal.User != "" && st.User != id.Principal.User) || (id.Principal.Agent != "" && st.Agent != "" && st.Agent != id.Principal.Agent) {
		return fmt.Errorf("run %s: %w", run, memory.ErrNotFound)
	}
	return nil
}

func (s *Server) ownsTurn(r *http.Request, episodeID string) error {
	id := identity(r)
	eps, err := s.Engine.Store().Episodes(r.Context(), store.EpisodeQuery{Tenant: id.Tenant, IDs: []string{episodeID}, IncludeDeleted: true})
	if err != nil {
		return err
	}
	if len(eps) == 0 {
		return fmt.Errorf("turn %s: %w", episodeID, memory.ErrNotFound)
	}
	return s.ownsRun(r, eps[0].Run)
}

func isAdmin(r *http.Request) bool { return identity(r).Principal.Admin }

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
