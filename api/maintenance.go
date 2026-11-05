package api

import (
	"fmt"
	"net/http"

	"github.com/alexedtionweb/niro-memory/memory"
)

func (s *Server) maintenance(mux *http.ServeMux) {
	s.route(mux, "POST /v1/maintenance/purge", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: tenant cleanup requires admin", errForbidden)
		}
		var body struct {
			Confirmation string `json:"confirmation"`
			RequestID    string `json:"request_id"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		tenant := identity(r).Tenant
		if body.Confirmation != tenant {
			return fmt.Errorf("%w: type the exact tenant name to confirm irreversible deletion", memory.ErrInvalid)
		}
		if body.RequestID == "" || len(body.RequestID) > 128 {
			return fmt.Errorf("%w: request_id required (at most 128 bytes)", memory.ErrInvalid)
		}
		status, err := s.Engine.PurgeTenant(r.Context(), tenant, body.RequestID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusAccepted, status)
	})
	s.route(mux, "GET /v1/maintenance/purge", func(w http.ResponseWriter, r *http.Request) error {
		if !isAdmin(r) {
			return fmt.Errorf("%w: tenant cleanup requires admin", errForbidden)
		}
		status, err := s.Engine.TenantPurgeStatus(r.Context(), identity(r).Tenant)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, status)
	})
}
