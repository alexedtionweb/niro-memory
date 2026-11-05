package api

import (
	"fmt"
	"net/http"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func (s *Server) jobs(mux *http.ServeMux) {
	s.route(mux, "GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: job inspection requires admin", errForbidden)
		}
		q := r.URL.Query()
		limit := boundedQueryInt(r, "limit", 50, 1, 100)
		rows, err := s.Engine.Store().Jobs(r.Context(), store.JobQuery{
			Tenant: identity(r).Tenant, ID: q.Get("id"), Kind: q.Get("kind"),
			State: store.JobState(q.Get("state")), AfterID: q.Get("after_id"), Limit: limit + 1,
		})
		if err != nil {
			return err
		}
		next := ""
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		return writeJSON(w, 200, map[string]any{"jobs": nonNil(rows), "next_after_id": next})
	})
	s.route(mux, "POST /v1/jobs/control", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: job control requires admin", errForbidden)
		}
		var body store.JobControl
		if err := decode(r, &body); err != nil {
			return err
		}
		if body.Tenant != "" && body.Tenant != identity(r).Tenant {
			return memory.ErrNotFound
		}
		body.Tenant = identity(r).Tenant
		controller, ok := s.Engine.Store().(store.JobController)
		if !ok {
			return fmt.Errorf("%w: store does not support job control", memory.ErrInvalid)
		}
		result, err := controller.JobControl(r.Context(), body)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
	s.route(mux, "GET /v1/vectors/config", func(w http.ResponseWriter, r *http.Request) error {
		result, err := s.Engine.VectorConfiguration(r.Context(), identity(r).Tenant)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
	s.route(mux, "GET /v1/knowledge/config", func(w http.ResponseWriter, r *http.Request) error {
		result, err := s.Engine.KnowledgeConfiguration(r.Context(), identity(r).Tenant)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
}
