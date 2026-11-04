package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func (s *Server) knowledge(mux *http.ServeMux) {
	e := s.Engine
	s.route(mux, "GET /v1/knowledge/enrichment/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		result, err := e.EnrichmentStatus(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	})
	evaluate := func(w http.ResponseWriter, r *http.Request) error {
		var req niromem.KnowledgeRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		if err := s.bindKnowledgeRequest(r, &req); err != nil {
			return err
		}
		result, err := e.EvaluateKnowledge(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}
	s.route(mux, "POST /v1/knowledge/evaluate", evaluate)
	s.route(mux, "POST /v1/knowledge/plan", evaluate)
	s.route(mux, "POST /v1/knowledge/commit", func(w http.ResponseWriter, r *http.Request) error {
		var req niromem.KnowledgeRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		if strings.TrimSpace(req.Input.OperationKey) == "" {
			return fmt.Errorf("%w: operation_key is required", memory.ErrInvalid)
		}
		if err := s.bindKnowledgeRequest(r, &req); err != nil {
			return err
		}
		// An explicit low-trust origin stays low; trust and authority are
		// otherwise assigned by the authenticated write endpoint.
		if req.Input.Trust != memory.TrustLow {
			req.Input.Trust = memory.TrustHigh
		}
		result, err := e.CommitKnowledge(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	})
	s.route(mux, "GET /v1/knowledge/state", func(w http.ResponseWriter, r *http.Request) error {
		req, err := s.knowledgeQueryRequest(r)
		if err != nil {
			return err
		}
		state, err := e.KnowledgeState(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, state)
	})
	s.route(mux, "GET /v1/knowledge/relations/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		u, a, err := scope(id, r.URL.Query().Get("user_id"), r.URL.Query().Get("agent_id"))
		if err != nil {
			return err
		}
		validAt, err := qTime(r, "valid_at")
		if err != nil {
			return err
		}
		knownAt, err := qTime(r, "known_at")
		if err != nil {
			return err
		}
		filter, err := filterParam(json.RawMessage(r.URL.Query().Get("filter")))
		if err != nil {
			return err
		}
		req := niromem.RelationRequest{
			ListRequest: niromem.ListRequest{
				Tenant: id.Tenant, As: principal(id, u, a), User: u, Agent: a,
				Run: r.URL.Query().Get("run_id"), Filter: filter, ValidAt: validAt, KnownAt: knownAt,
				AllVersions: qBool(r, "all_versions"), ShowExpired: qBool(r, "show_expired"),
				Order: store.Order(r.URL.Query().Get("order")),
				Limit: qInt(r, "limit", 100), Offset: qInt(r, "offset", 0),
			},
			MemoryID: r.PathValue("id"), Direction: r.URL.Query().Get("direction"),
			Types:    queryValues(r.URL.Query()["types"]),
			Depth:    boundedQueryInt(r, "depth", 1, 1, 4),
			MaxEdges: boundedQueryInt(r, "max_edges", 100, 1, 400),
		}
		links, err := e.KnowledgeRelations(r.Context(), req)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{"relations": links})
	})
	s.route(mux, "GET /v1/knowledge/traces/{key}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		u, a, err := scope(id, r.URL.Query().Get("user_id"), r.URL.Query().Get("agent_id"))
		if err != nil {
			return err
		}
		trace, err := e.KnowledgeTrace(r.Context(), id.Tenant, principal(id, u, a), r.PathValue("key"))
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, trace)
	})
}

func queryValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, splitQuery(value)...)
	}
	return out
}

func (s *Server) bindKnowledgeRequest(r *http.Request, req *niromem.KnowledgeRequest) error {
	id := identity(r)
	u, a, err := scope(id, req.Input.Scope.User, req.Input.Scope.Agent)
	if err != nil {
		return err
	}
	req.Input.Scope = memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: req.Input.Scope.Run}
	req.As = principal(id, u, a)
	req.Input.Authority = id.Principal.Admin
	return nil
}

func (s *Server) knowledgeQueryRequest(r *http.Request) (niromem.KnowledgeRequest, error) {
	id := identity(r)
	q := r.URL.Query()
	u, a, err := scope(id, q.Get("user_id"), q.Get("agent_id"))
	if err != nil {
		return niromem.KnowledgeRequest{}, err
	}
	validAt, err := qTime(r, "valid_at")
	if err != nil {
		return niromem.KnowledgeRequest{}, err
	}
	knownAt, err := qTime(r, "known_at")
	if err != nil {
		return niromem.KnowledgeRequest{}, err
	}
	req := niromem.KnowledgeRequest{}
	req.Input.Scope = memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: q.Get("run_id")}
	req.Input.OperationKey = q.Get("operation_key")
	req.Input.TargetID = q.Get("target_id")
	req.Input.Text = q.Get("text")
	req.Input.Slot = q.Get("slot")
	req.ValidAt, req.KnownAt, req.AllVersions = validAt, knownAt, qBool(r, "all_versions")
	if q.Get("max_candidates") != "" {
		req.MaxCandidates = boundedQueryInt(r, "max_candidates", 16, 1, 1000)
	}
	if q.Get("subject") != "" || q.Get("predicate") != "" || q.Get("object") != "" {
		req.Input.Assertion = &memory.Assertion{
			Subject: q.Get("subject"), Predicate: q.Get("predicate"), Object: q.Get("object"),
		}
	}
	if intent := q.Get("intent"); intent != "" {
		req.Input.Intent = knowledge.OperationType(intent)
	}
	req.As = principal(id, u, a)
	req.Input.Authority = id.Principal.Admin
	return req, nil
}

func splitQuery(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	values := strings.Split(value, ",")
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func boundedQueryInt(r *http.Request, name string, fallback, min, max int) int {
	value := qInt(r, name, fallback)
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
