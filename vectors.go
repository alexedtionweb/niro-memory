package niromem

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

type indexPayload struct {
	Points  []vector.Point  `json:"points,omitempty"`
	Targets []vector.Target `json:"targets,omitempty"`
}

func (e *Engine) handleIndex(ctx context.Context, j store.Job) error {
	var p indexPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return jobs.Permanent(err)
	}
	if !e.external || len(p.Points) == 0 {
		return nil
	}
	for i := range p.Points {
		p.Points[i].Tenant = j.Tenant
	}
	return e.index.Upsert(ctx, p.Points)
}

func (e *Engine) handleUnindex(ctx context.Context, j store.Job) error {
	var p indexPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return jobs.Permanent(err)
	}
	if !e.external || len(p.Targets) == 0 {
		return nil
	}
	return e.index.Delete(ctx, j.Tenant, p.Targets)
}

// handleEmbed fills missing memory vectors for the embedder's model in
// batches, until none are missing or the job has run for a while (then it
// re-enqueues itself, so one tenant cannot hog a worker).
func (e *Engine) handleEmbed(ctx context.Context, j store.Job) error {
	if e.o.Embedder == nil {
		return nil
	}
	vc, ok := e.o.Store.(store.VectorCatalog)
	if !ok {
		return jobs.Permanent(fmt.Errorf("store does not persist vectors"))
	}
	model := e.o.Embedder.Model()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		targets, err := vc.Missing(ctx, j.Tenant, model.ID, 64)
		if err != nil || len(targets) == 0 {
			return err
		}
		n, err := e.embedTargets(ctx, j.Tenant, model, targets)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, Jobs: []store.Job{{Kind: JobEmbed, Key: model.ID}}})
	return err
}

func (e *Engine) embedTargets(ctx context.Context, tenant string, model vector.Model, targets []vector.Target) (int, error) {
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.ID
	}
	ms, err := e.o.Store.Get(ctx, tenant, ids)
	if err != nil || len(ms) == 0 {
		return 0, err
	}
	in := make([]embed.Input, len(ms))
	for i, m := range ms {
		in[i] = embed.Input{Modality: "text", Text: m.Title + "\n" + m.Text}
	}
	start := time.Now()
	vs, _, err := e.o.Embedder.Embed(ctx, embed.Document, in)
	e.observe("embed", start, err)
	if err != nil {
		return 0, err
	}
	if len(vs) != len(ms) {
		return 0, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vs), len(ms))
	}
	b := &store.Batch{Tenant: tenant}
	var pts []vector.Point
	for i, m := range ms {
		if len(vs[i]) != model.Dims {
			return 0, jobs.Permanent(fmt.Errorf("%w: embedder returned %d dims, model %s declares %d", vector.ErrDims, len(vs[i]), model.ID, model.Dims))
		}
		pts = append(pts, vector.Point{Tenant: tenant, Model: model.ID, Modality: "text", User: m.User, Agent: m.Agent, Run: m.Run,
			Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, Values: vs[i]})
	}
	e.addPoints(b, pts)
	_, err = e.o.Store.Write(ctx, b)
	return len(pts), err
}

// Coverage reports how many current memories of a tenant have a vector for
// model — the readiness signal for switching the active model.
func (e *Engine) Coverage(ctx context.Context, tenant, model string) (have, total int, err error) {
	vc, ok := e.o.Store.(store.VectorCatalog)
	if !ok {
		return 0, 0, fmt.Errorf("store does not persist vectors: %w", memory.ErrNotFound)
	}
	return vc.Coverage(ctx, tenant, model)
}

// MissingVectors lists memories without a vector for model, so callers that
// bring their own embeddings can backfill a new model with PutVectors.
func (e *Engine) MissingVectors(ctx context.Context, tenant, model string, limit int) ([]vector.Target, error) {
	vc, ok := e.o.Store.(store.VectorCatalog)
	if !ok {
		return nil, fmt.Errorf("store does not persist vectors: %w", memory.ErrNotFound)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return vc.Missing(ctx, tenant, model, limit)
}

// PutVectors stores caller-computed vectors for memories or episode parts.
func (e *Engine) PutVectors(ctx context.Context, tenant string, pts []vector.Point) error {
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return err
	}
	models, _ := e.models(s)
	for i := range pts {
		pts[i].Tenant = tenant
		if err := checkVectors(models, []Vector{{Model: pts[i].Model, Values: pts[i].Values}}); err != nil {
			return err
		}
	}
	b := &store.Batch{Tenant: tenant}
	e.addPoints(b, pts)
	_, err = e.o.Store.Write(ctx, b)
	return err
}

// Backfill enqueues embedding of every memory missing a vector for the
// embedder's model (e.g. after switching models).
func (e *Engine) Backfill(ctx context.Context, tenant string) error {
	if e.o.Embedder == nil {
		return fmt.Errorf("%w: no embedder configured", memory.ErrInvalid)
	}
	_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant, Jobs: []store.Job{{Kind: JobEmbed, Key: e.o.Embedder.Model().ID}}})
	return err
}
