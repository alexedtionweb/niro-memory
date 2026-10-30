package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
)

// Mem0-compatible API, mounted under /mem0. It accepts the shapes of the
// Mem0 OSS REST server (/memories, /search) and of the Mem0 platform
// client (/v1/memories/{id}/, /v3/memories/add/, /v3/memories/search/,
// /v3/memories/), so existing Mem0 clients can point their host at
// http://server/mem0. Differences are listed in docs/mem0-compat.md.

type mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

type mem0Add struct {
	Messages       json.RawMessage `json:"messages"`
	User           string          `json:"user_id"`
	Agent          string          `json:"agent_id"`
	Run            string          `json:"run_id"`
	Metadata       map[string]any  `json:"metadata"`
	Infer          *bool           `json:"infer"`
	AsyncMode      bool            `json:"async_mode"`
	ExpirationDate string          `json:"expiration_date"`
}

type mem0Item struct {
	ID        string         `json:"id"`
	Memory    string         `json:"memory"`
	Hash      string         `json:"hash"`
	Metadata  map[string]any `json:"metadata"`
	Score     *float64       `json:"score,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt *string        `json:"updated_at"`
	User      string         `json:"user_id,omitempty"`
	Agent     string         `json:"agent_id,omitempty"`
	Run       string         `json:"run_id,omitempty"`
	Expires   string         `json:"expiration_date,omitempty"`
	Explain   any            `json:"score_details,omitempty"`
}

func toMem0(m memory.Memory) mem0Item {
	it := mem0Item{ID: m.ID, Memory: m.Text, Hash: m.Hash, Metadata: m.Metadata, CreatedAt: m.RecordedAt.Format(time.RFC3339Nano),
		User: m.User, Agent: m.Agent, Run: m.Run}
	if it.Metadata == nil {
		it.Metadata = map[string]any{}
	}
	if m.ExpiresAt != nil {
		it.Expires = m.ExpiresAt.Format("2006-01-02")
	}
	return it
}

func (s *Server) mem0(mux *http.ServeMux) {
	for _, p := range []string{"POST /mem0/memories", "POST /mem0/v1/memories/{$}", "POST /mem0/v3/memories/add/{$}"} {
		s.route(mux, p, s.mem0Add)
	}
	for _, p := range []string{"GET /mem0/memories", "GET /mem0/v1/memories/{$}"} {
		s.route(mux, p, s.mem0List)
	}
	s.route(mux, "POST /mem0/v3/memories/{$}", s.mem0ListFiltered)
	for _, p := range []string{"GET /mem0/memories/{id}", "GET /mem0/v1/memories/{id}/{$}"} {
		s.route(mux, p, func(w http.ResponseWriter, r *http.Request) error {
			m, err := s.readable(r, r.PathValue("id"))
			if err != nil {
				return err
			}
			return writeJSON(w, 200, toMem0(m))
		})
	}
	for _, p := range []string{"POST /mem0/search", "POST /mem0/v1/memories/search/{$}", "POST /mem0/v2/memories/search/{$}", "POST /mem0/v3/memories/search/{$}"} {
		s.route(mux, p, s.mem0Search)
	}
	for _, p := range []string{"PUT /mem0/memories/{id}", "PUT /mem0/v1/memories/{id}/{$}"} {
		s.route(mux, p, s.mem0Update)
	}
	for _, p := range []string{"DELETE /mem0/memories/{id}", "DELETE /mem0/v1/memories/{id}/{$}"} {
		s.route(mux, p, func(w http.ResponseWriter, r *http.Request) error {
			if _, err := s.readable(r, r.PathValue("id")); err != nil {
				return err
			}
			id := identity(r)
			if err := s.Engine.Delete(r.Context(), id.Tenant, firstNonEmpty(id.Principal.Agent, id.Principal.User, "mem0"), r.PathValue("id")); err != nil {
				return err
			}
			return writeJSON(w, 200, map[string]string{"message": "Memory deleted successfully!"})
		})
	}
	for _, p := range []string{"DELETE /mem0/memories", "DELETE /mem0/v1/memories/{$}"} {
		s.route(mux, p, func(w http.ResponseWriter, r *http.Request) error {
			id := identity(r)
			q := r.URL.Query()
			u, a, err := scope(id, q.Get("user_id"), q.Get("agent_id"))
			if err != nil {
				return err
			}
			if _, err := s.Engine.Forget(r.Context(), memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: q.Get("run_id")}, "mem0"); err != nil {
				return err
			}
			return writeJSON(w, 200, map[string]string{"message": "Memories deleted successfully!"})
		})
	}
	for _, p := range []string{"GET /mem0/memories/{id}/history", "GET /mem0/v1/memories/{id}/history/{$}"} {
		s.route(mux, p, s.mem0History)
	}
}

func parseMessages(raw json.RawMessage) ([]mem0Message, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []mem0Message{{Role: "user", Content: s}}, nil
	}
	var one mem0Message
	if json.Unmarshal(raw, &one) == nil && one.Content != "" {
		return []mem0Message{one}, nil
	}
	var many []mem0Message
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("%w: messages must be a string, a message or a list of messages", memory.ErrInvalid)
	}
	return many, nil
}

func (s *Server) mem0Add(w http.ResponseWriter, r *http.Request) error {
	var body mem0Add
	if err := decode(r, &body); err != nil {
		return err
	}
	msgs, err := parseMessages(body.Messages)
	if err != nil {
		return err
	}
	id := identity(r)
	u, a, err := scope(id, body.User, body.Agent)
	if err != nil {
		return err
	}
	if u == "" && a == "" && body.Run == "" {
		return fmt.Errorf("%w: one of user_id, agent_id or run_id is required", memory.ErrInvalid)
	}
	var exp *time.Time
	if body.ExpirationDate != "" {
		t, err := memory.ParseTime(body.ExpirationDate)
		if err != nil {
			return err
		}
		exp = &t
	}
	sc := memory.Scope{Tenant: id.Tenant, User: u, Agent: a, Run: body.Run}
	var results []map[string]any
	if body.Infer != nil && !*body.Infer {
		// infer=false: store messages verbatim, like Mem0.
		var facts []niromem.Fact
		for _, m := range msgs {
			if m.Role == "system" || strings.TrimSpace(m.Content) == "" {
				continue
			}
			meta := map[string]any{"role": m.Role}
			for k, v := range body.Metadata {
				meta[k] = v
			}
			facts = append(facts, niromem.Fact{Scope: sc, Kind: memory.Episodic, Text: m.Content, Metadata: meta, ExpiresAt: exp, Actor: firstNonEmpty(m.Name, m.Role)})
		}
		changes, err := s.Engine.Remember(r.Context(), facts...)
		if err != nil {
			return err
		}
		results = mem0Events(changes)
		return writeJSON(w, 200, map[string]any{"results": nonNil(results)})
	}
	run := body.Run
	if run == "" {
		run = "mem0:" + u + ":" + a
	}
	sc.Run = run
	var turns []niromem.Turn
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		turns = append(turns, niromem.Turn{Role: m.Role, Actor: m.Name, Text: m.Content, Metadata: body.Metadata})
	}
	res, err := s.Engine.Append(r.Context(), sc, turns, niromem.AppendOptions{Wait: !body.AsyncMode})
	if err != nil {
		return err
	}
	if body.AsyncMode {
		return writeJSON(w, 200, map[string]any{"message": "Memory processing has been queued for background execution", "status": "PENDING"})
	}
	return writeJSON(w, 200, map[string]any{"results": nonNil(mem0Events(res.Changes))})
}

func mem0Events(changes []pipeline.Change) []map[string]any {
	var out []map[string]any
	for _, c := range changes {
		ev := map[string]any{"id": c.Memory.ID, "memory": c.Memory.Text}
		switch c.Action {
		case memory.ActionAdd:
			ev["event"] = "ADD"
		case memory.ActionSupersede, memory.ActionUpdate:
			ev["event"], ev["previous_memory_id"] = "UPDATE", c.Previous
		case memory.ActionInvalidate:
			ev["event"] = "DELETE"
		default:
			continue
		}
		out = append(out, ev)
	}
	return out
}

func (s *Server) mem0List(w http.ResponseWriter, r *http.Request) error {
	id := identity(r)
	q := r.URL.Query()
	u, a, err := scope(id, q.Get("user_id"), q.Get("agent_id"))
	if err != nil {
		return err
	}
	ms, err := s.Engine.List(r.Context(), niromem.ListRequest{Tenant: id.Tenant, As: principal(id, u, a), User: u, Agent: a, Run: q.Get("run_id"),
		Limit: qInt(r, "limit", 100), ShowExpired: qBool(r, "show_expired")})
	if err != nil {
		return err
	}
	items := make([]mem0Item, len(ms))
	for i, m := range ms {
		items[i] = toMem0(m)
	}
	return writeJSON(w, 200, map[string]any{"results": items})
}

func (s *Server) mem0ListFiltered(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Filters json.RawMessage `json:"filters"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	f, user, agent, err := mem0Filters(body.Filters)
	if err != nil {
		return err
	}
	id := identity(r)
	u, a, err := scope(id, user, agent)
	if err != nil {
		return err
	}
	page, size := max(qInt(r, "page", 1), 1), qInt(r, "page_size", 100)
	if size <= 0 || size > 1000 {
		size = 100
	}
	ms, err := s.Engine.List(r.Context(), niromem.ListRequest{Tenant: id.Tenant, As: principal(id, u, a), User: u, Agent: a, Filter: f,
		Limit: size, Offset: (page - 1) * size})
	if err != nil {
		return err
	}
	items := make([]mem0Item, len(ms))
	for i, m := range ms {
		items[i] = toMem0(m)
	}
	return writeJSON(w, 200, map[string]any{"count": len(items), "next": nil, "previous": nil, "results": items})
}

// mem0Filters parses Mem0 filters and lifts top-level user_id/agent_id
// equalities so they also drive the visibility principal.
func mem0Filters(raw json.RawMessage) (memory.Filter, string, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return memory.Filter{}, "", "", nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return memory.Filter{}, "", "", fmt.Errorf("%w: filters: %v", memory.ErrInvalid, err)
	}
	user, _ := m["user_id"].(string)
	agent, _ := m["agent_id"].(string)
	f, err := memory.FilterFromMap(m)
	if err != nil {
		return f, "", "", err
	}
	return f, user, agent, f.Check()
}

func (s *Server) mem0Search(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Query       string          `json:"query"`
		User        string          `json:"user_id"`
		Agent       string          `json:"agent_id"`
		Run         string          `json:"run_id"`
		Filters     json.RawMessage `json:"filters"`
		TopK        int             `json:"top_k"`
		Limit       int             `json:"limit"`
		Threshold   float64         `json:"threshold"`
		Explain     bool            `json:"explain"`
		ShowExpired bool            `json:"show_expired"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	f, fu, fa, err := mem0Filters(body.Filters)
	if err != nil {
		return err
	}
	req, err := s.searchRequest(r, niromem.SearchRequest{Query: body.Query, User: firstNonEmpty(body.User, fu), Agent: firstNonEmpty(body.Agent, fa),
		Run: body.Run, Limit: firstPositive(body.TopK, body.Limit), Threshold: body.Threshold, Explain: body.Explain, ShowExpired: body.ShowExpired}, nil)
	if err != nil {
		return err
	}
	req.Filter = f
	res, err := s.Engine.Search(r.Context(), req)
	if err != nil {
		return err
	}
	items := make([]mem0Item, len(res.Hits))
	for i, h := range res.Hits {
		items[i] = toMem0(h.Memory)
		score := h.Score
		items[i].Score = &score
		if h.Explain != nil {
			items[i].Explain = h.Explain
		}
	}
	return writeJSON(w, 200, map[string]any{"results": items})
}

func firstPositive(xs ...int) int {
	for _, x := range xs {
		if x > 0 {
			return x
		}
	}
	return 0
}

func (s *Server) mem0Update(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.readable(r, r.PathValue("id")); err != nil {
		return err
	}
	var body struct {
		Text     *string        `json:"text"`
		Data     *string        `json:"data"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.Text == nil {
		body.Text = body.Data
	}
	id := identity(r)
	m, err := s.Engine.Update(r.Context(), id.Tenant, r.PathValue("id"), niromem.Patch{Text: body.Text, Metadata: body.Metadata, Actor: firstNonEmpty(id.Principal.Agent, id.Principal.User, "mem0")})
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"message": "Memory updated successfully!", "id": m.ID})
}

func (s *Server) mem0History(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.readable(r, r.PathValue("id")); err != nil && !isAdmin(r) {
		return err
	}
	evs, err := s.Engine.History(r.Context(), identity(r).Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for i, e := range evs {
		ev := map[memory.Action]string{memory.ActionAdd: "ADD", memory.ActionUpdate: "UPDATE", memory.ActionSupersede: "UPDATE",
			memory.ActionInvalidate: "DELETE", memory.ActionDelete: "DELETE"}[e.Action]
		if ev == "" {
			ev = "NONE"
		}
		out = append(out, map[string]any{"id": fmt.Sprintf("%s-%d", e.MemoryID, i), "memory_id": e.MemoryID, "old_memory": e.Old,
			"new_memory": e.New, "event": ev, "created_at": e.At.Format(time.RFC3339Nano), "is_deleted": e.Action == memory.ActionDelete, "actor_id": e.Actor})
	}
	return writeJSON(w, 200, out)
}
