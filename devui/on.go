//go:build devui

package devui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"sort"
	"time"

	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store"
)

// Enabled reports whether the console is compiled in (build tag devui).
const Enabled = true

//go:embed static
var static embed.FS

// Handler serves the console under /_dev/ and its helper API under /_dev/api/.
func Handler(o Options) http.Handler {
	sub, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /_dev/", http.StripPrefix("/_dev/", http.FileServerFS(sub)))
	d := &dev{o}
	mux.HandleFunc("GET /_dev/views/{screen}", d.workspace)
	d.route(mux, "GET /_dev/api/info", d.info)
	d.route(mux, "GET /_dev/api/knowledge/config", d.knowledgeConfig)
	d.route(mux, "GET /_dev/api/tenants", d.tenants)
	d.route(mux, "GET /_dev/api/runs", d.runs)
	d.route(mux, "POST /_dev/api/sql", d.sql)
	d.route(mux, "GET /_dev/api/jobs", d.jobs)
	d.route(mux, "POST /_dev/api/jobs/retry", d.retry)
	d.route(mux, "POST /_dev/api/jobs/control", d.controlJob)
	d.route(mux, "POST /_dev/api/jobs/run", d.runJobs)
	d.route(mux, "GET /_dev/api/metrics", d.metrics)
	d.route(mux, "GET /_dev/api/rules", d.rulesDoc)
	d.route(mux, "POST /_dev/api/rules/test", d.rulesTest)
	return securityHeaders(mux)
}

func (d *dev) knowledgeConfig(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	return d.o.Engine.KnowledgeConfiguration(r.Context(), id.Tenant)
}

// securityHeaders keeps the console from being framed or loading remote code.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; frame-src blob:; object-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

type dev struct{ o Options }

type handler func(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error)

var errForbidden = errors.New("the development console requires an admin identity")

func (d *dev) route(mux *http.ServeMux, pattern string, h handler) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		id, err := d.o.Auth.Authenticate(r)
		status := http.StatusOK
		var out any
		switch {
		case err != nil:
			status, out = http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"}
		case !id.Principal.Admin:
			status, out = http.StatusForbidden, map[string]string{"error": errForbidden.Error()}
		default:
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			if out, err = h(w, r, id); err != nil {
				status = http.StatusBadRequest
				if errors.Is(err, memory.ErrNotFound) {
					status = http.StatusNotFound
				}
				out = map[string]string{"error": err.Error()}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(out)
	})
}

func (d *dev) info(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	pol, err := d.o.Engine.Policies(r.Context(), id.Tenant)
	if err != nil {
		return nil, err
	}
	_, sqlOK := d.o.Engine.Store().(store.SQLConsole)
	knowledge, err := d.o.Engine.KnowledgeConfiguration(r.Context(), id.Tenant)
	if err != nil {
		return nil, err
	}
	capabilities := d.o.Engine.Capabilities()
	capabilities.KnowledgeMode = knowledge.Effective.Mode
	vectors, err := d.o.Engine.VectorConfiguration(r.Context(), id.Tenant)
	if err != nil {
		return nil, err
	}
	return map[string]any{"tenant": id.Tenant, "capabilities": capabilities, "knowledge": knowledge, "vectors": vectors, "policies": pol,
		"sql": sqlOK, "metrics": d.o.Metrics != nil, "inline_worker": d.o.InlineWorker}, nil
}

func (d *dev) tenants(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	a, ok := d.o.Engine.Store().(store.Admin)
	if !ok {
		return []store.TenantStats{{Tenant: id.Tenant}}, nil
	}
	return a.Tenants(r.Context())
}

// runs summarizes recent runs from recent episodes (works on any store).
func (d *dev) runs(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	eps, err := d.o.Engine.Store().Episodes(r.Context(), store.EpisodeQuery{Tenant: id.Tenant, Desc: true, Limit: 2000, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	type run struct {
		Run   string    `json:"run_id"`
		User  string    `json:"user_id"`
		Agent string    `json:"agent_id"`
		Turns int       `json:"turns"`
		Last  time.Time `json:"last"`
	}
	by := map[string]*run{}
	var order []string
	for _, e := range eps {
		x := by[e.Run]
		if x == nil {
			x = &run{Run: e.Run, User: e.User, Agent: e.Agent, Last: e.OccurredAt}
			by[e.Run] = x
			order = append(order, e.Run)
		}
		x.Turns++
	}
	out := make([]*run, len(order))
	for i, k := range order {
		out[i] = by[k]
	}
	return out, nil
}

func (d *dev) sql(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	c, ok := d.o.Engine.Store().(store.SQLConsole)
	if !ok {
		return nil, errors.New("this store has no SQL console")
	}
	var body struct {
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.MaxRows <= 0 || body.MaxRows > 1000 {
		body.MaxRows = 200
	}
	start := time.Now()
	cols, rows, err := c.QueryReadOnly(r.Context(), body.SQL, body.MaxRows)
	if err != nil {
		return nil, err
	}
	return map[string]any{"columns": cols, "rows": rows, "took_ms": time.Since(start).Seconds() * 1000}, nil
}

func (d *dev) jobs(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	q := r.URL.Query()
	tenant := q.Get("tenant")
	if q.Get("all") == "" {
		tenant = id.Tenant
	}
	limit := 101
	jobs, err := d.o.Engine.Store().Jobs(r.Context(), store.JobQuery{Tenant: tenant, State: store.JobState(q.Get("state")), Kind: q.Get("kind"), ID: q.Get("id"), AfterID: q.Get("after_id"), Limit: limit})
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	return d.jobSources(r.Context(), jobs)
}

// retry keeps the original job identity; repeated legacy requests are safe.
func (d *dev) retry(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	js, err := d.o.Engine.Store().Jobs(r.Context(), store.JobQuery{Tenant: id.Tenant, ID: body.ID, Limit: 1})
	if err != nil {
		return nil, err
	}
	for _, j := range js {
		if j.ID == body.ID {
			if j.State == store.JobPending {
				return map[string]any{"status": "requeued", "replayed": true}, nil
			}
			controller, ok := d.o.Engine.Store().(store.JobController)
			if !ok {
				return nil, errors.New("store does not support job controls")
			}
			if _, err := controller.JobControl(r.Context(), store.JobControl{Tenant: id.Tenant, ID: j.ID, Action: store.JobRetry}); err != nil {
				return nil, err
			}
			return map[string]any{"status": "requeued", "replayed": false}, nil
		}

	}
	return nil, memory.ErrNotFound
}

func (d *dev) controlJob(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	var body store.JobControl
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Tenant != "" && body.Tenant != id.Tenant {
		return nil, memory.ErrNotFound
	}
	body.Tenant = id.Tenant
	controller, ok := d.o.Engine.Store().(store.JobController)
	if !ok {
		return nil, errors.New("store does not support job controls")
	}
	return controller.JobControl(r.Context(), body)
}

func (d *dev) runJobs(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	if d.o.InlineWorker {
		return nil, errors.New("inline worker is active; jobs are processed automatically, not by an additional manual worker")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	worker := d.o.Engine.Worker()
	worker.Concurrency = 1
	n, err := worker.RunOnce(ctx, 1)
	return map[string]int{"processed": n}, err
}

func (d *dev) metrics(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	if d.o.Metrics == nil {
		return []any{}, nil
	}
	s := d.o.Metrics.Snapshot()
	sort.Slice(s, func(i, j int) bool { return s[i].Count > s[j].Count })
	return s, nil
}

func (d *dev) rulesDoc(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	rs, err := d.o.Engine.Rules(r.Context(), id.Tenant)
	if err != nil || rs == nil {
		return map[string]any{"version": "", "rules": nil}, err
	}
	return map[string]any{"version": rs.Version, "rules": rs.Rules()}, nil
}

// rulesTest compiles a rules document (or uses the tenant's rules) and
// shows what it extracts from a text, with byte spans for highlighting.
func (d *dev) rulesTest(w http.ResponseWriter, r *http.Request, id api.Identity) (any, error) {
	var body struct {
		Rules json.RawMessage `json:"rules"`
		Text  string          `json:"text"`
		Role  string          `json:"role"`
		User  string          `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	var rs *rules.Set
	var err error
	if len(body.Rules) > 0 && string(body.Rules) != "null" {
		if rs, err = rules.Parse(body.Rules); err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
	} else if rs, err = d.o.Engine.Rules(r.Context(), id.Tenant); err != nil || rs == nil {
		return map[string]any{"error": "no rules configured"}, err
	}
	if body.Role == "" {
		body.Role = "user"
	}
	c := rules.Context{User: body.User, Role: body.Role, Actor: body.Role, Observed: time.Now()}
	return map[string]any{"version": rs.Version, "candidates": rs.Text(body.Text, c),
		"entities": rs.Entities(body.Text), "flags": rs.Flags(body.Text)}, nil
}
