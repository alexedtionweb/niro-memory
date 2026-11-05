package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func current(m memory.Memory, now time.Time) bool {
	q := store.Query{}
	return q.Admits(&m, now)
}

func (e *Engine) handleRelationships(ctx context.Context, job store.Job) error {
	if e.o.Relationships == nil {
		return jobs.Permanent(errors.New("relationship inference is not configured"))
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.ID == "" {
		return jobs.Permanent(errors.New("relationship job needs a memory id"))
	}
	if _, ok := e.o.Store.(relations.Store); !ok {
		return jobs.Permanent(errors.New("store does not support derived relationships"))
	}
	hash, err := operationHash(body)
	if err != nil {
		return err
	}
	key := "relationships:" + job.ID
	var completed []relations.Link
	if replayed, err := e.replayOperation(ctx, job.Tenant, key, hash, &completed); replayed || err != nil {
		return err
	}
	ms, err := e.o.Store.Get(ctx, job.Tenant, []string{body.ID})
	if err != nil {
		return err
	}
	if len(ms) == 0 || !current(ms[0], e.o.Now()) {
		return nil
	}
	source := ms[0]
	// Inference never combines owners or visibility/grant groups. Explicit
	// empty owner values are significant and must not broaden the query.
	candidates, err := e.o.Store.Find(ctx, store.Query{Tenant: job.Tenant, Limit: 64,
		Filter: memory.And(memory.Eq("user_id", source.User), memory.Eq("agent_id", source.Agent),
			memory.Cmp("id", memory.OpNe, source.ID)), Order: store.NewestFirst})
	if err != nil {
		return err
	}
	ranked, err := e.Search(ctx, SearchRequest{Tenant: job.Tenant, As: memory.Principal{Admin: true},
		Query: source.Text, Limit: 20, Policy: "deep",
		Filter: memory.And(memory.Eq("user_id", source.User), memory.Eq("agent_id", source.Agent), memory.Cmp("id", memory.OpNe, source.ID))})
	if err != nil {
		return err
	}
	ordered := make([]memory.Memory, 0, len(ranked.Hits)+len(candidates))
	for _, hit := range ranked.Hits {
		ordered = append(ordered, hit.Memory)
	}
	candidates = append(ordered, candidates...)
	input := relations.Input{Source: source, Candidates: []memory.Memory{}}
	allowed := map[string]bool{}
	for _, m := range candidates {
		if !allowed[m.ID] && m.Visibility == source.Visibility && reflect.DeepEqual(m.Grants, source.Grants) {
			input.Candidates = append(input.Candidates, m)
			allowed[m.ID] = true
			if len(input.Candidates) == 20 {
				break
			}
		}
	}
	if len(input.Candidates) == 0 {
		_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: job.Tenant, OperationKey: key, OperationHash: hash, Result: payload([]relations.Link{})})
		return err
	}
	var proposals []relations.Proposal
	planKey := "relationships.plan:" + job.ID
	replayed, err := e.replayOperation(ctx, job.Tenant, planKey, hash, &proposals)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !replayed {
		proposals, err = e.o.Relationships.Infer(callCtx, input)
		if err != nil {
			return fmt.Errorf("infer relationships: %w", err)
		}
	}
	if len(proposals) > 20 {
		return jobs.Permanent(errors.New("relationship provider returned more than 20 proposals"))
	}
	links := []relations.Link{}
	seen := map[string]bool{}
	for _, p := range proposals {
		if !allowed[p.Target] || !relations.ValidRelation(p.Relation) || strings.TrimSpace(p.Reason) == "" ||
			len(p.Reason) > 2000 || math.IsNaN(p.Confidence) || p.Confidence <= 0 || p.Confidence > 1 {
			return jobs.Permanent(errors.New("relationship provider returned an unsupported target, relation, reason, or confidence"))
		}
		key := p.Target + ":" + p.Relation
		if seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, relations.Link{ID: memory.Hash(key + ":" + source.ID + ":" + p.Target + ":" + p.Relation),
			Tenant: job.Tenant, OperationKey: key, Source: source.ID, Target: p.Target, Relation: p.Relation,
			Origin:   "inferred",
			Evidence: append(append([]memory.Evidence(nil), source.Source.Evidence...), candidateEvidence(input.Candidates, p.Target)...),
			Reason:   p.Reason, Confidence: p.Confidence, Provider: e.o.Relationships.Name(), At: e.o.Now()})
	}
	if !replayed {
		receipt, err := e.o.Store.Write(ctx, &store.Batch{Tenant: job.Tenant, OperationKey: planKey,
			OperationHash: hash, Result: payload(proposals)})
		if err != nil {
			return err
		}
		if receipt.Replayed {
			// A concurrent planner won; retry with its durable proposals.
			return memory.ErrConflict
		}
	}
	b := &store.Batch{Tenant: job.Tenant, OperationKey: key, OperationHash: hash, Result: payload(links),
		Checks: []store.Check{{ID: source.ID, Version: source.Version, Open: true}}, Relations: links}
	for _, m := range input.Candidates {
		if seenTarget(links, m.ID) {
			b.Checks = append(b.Checks, store.Check{ID: m.ID, Version: m.Version, Open: true})
		}
	}
	existing, err := e.o.Store.(relations.Store).RelationshipSets(ctx, job.Tenant, []string{source.ID})
	if err != nil {
		return err
	}
	for _, l := range existing {
		if l.Source == source.ID && l.Provider == e.o.Relationships.Name() && l.InvalidatedAt == nil {
			b.RelationCloses = append(b.RelationCloses, relations.Close{ID: l.ID, At: e.o.Now()})
		}
	}
	_, err = e.o.Store.Write(ctx, b)
	return err
}

func candidateEvidence(ms []memory.Memory, id string) []memory.Evidence {
	for _, m := range ms {
		if m.ID == id {
			return m.Source.Evidence
		}
	}
	return nil
}

func seenTarget(links []relations.Link, id string) bool {
	for _, l := range links {
		if l.Target == id {
			return true
		}
	}
	return false
}

// BackfillRelationships queues inference over existing memories in bounded
// pages. It never waits for a model in the API or retrieval path.
func (e *Engine) BackfillRelationships(ctx context.Context, tenant string) error {
	if tenant == "" || e.o.Relationships == nil {
		return fmt.Errorf("%w: tenant and configured relationship provider are required", memory.ErrInvalid)
	}
	_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant,
		Jobs: []store.Job{{Kind: JobRelationshipBackfill, Key: "all", Payload: payload(map[string]int{"offset": 0})}}})
	return err
}

func (e *Engine) handleRelationshipBackfill(ctx context.Context, job store.Job) error {
	var body struct {
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.Offset < 0 {
		return jobs.Permanent(errors.New("invalid relationship backfill page"))
	}
	ms, err := e.o.Store.Find(ctx, store.Query{Tenant: job.Tenant, Limit: 100, Offset: body.Offset, Order: store.OldestFirst})
	if err != nil {
		return err
	}
	b := &store.Batch{Tenant: job.Tenant, OperationKey: "relationships.backfill:" + job.ID,
		OperationHash: memory.Hash(string(job.Payload)), Result: payload(map[string]string{"status": "queued"})}
	for _, m := range ms {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobRelationships, Key: m.ID, Payload: payload(map[string]string{"id": m.ID})})
	}
	if len(ms) == 100 {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobRelationshipBackfill, Key: fmt.Sprint(body.Offset + 100), Payload: payload(map[string]int{"offset": body.Offset + 100})})
	}
	_, err = e.o.Store.Write(ctx, b)
	return err
}

// Exploration is a bounded graph/timeline slice, not an exhaustive tenant dump.
type Exploration struct {
	Memories   []memory.Memory  `json:"memories"`
	Links      []relations.Link `json:"inferred_links"`
	Relations  []relations.Link `json:"relationships"`
	More       bool             `json:"more"`
	Offset     int              `json:"offset"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// Explore revalidates both endpoints of inferred links against the requested
// scope, ACL and current validity. Expired/deleted evidence never produces a link.
func (e *Engine) Explore(ctx context.Context, req ListRequest) (*Exploration, error) {
	if req.Limit <= 0 || req.Limit > 200 {
		req.Limit = 100
	}
	limit := req.Limit
	req.Limit++
	ms, err := e.List(ctx, req)
	if err != nil {
		return nil, err
	}
	out := &Exploration{Memories: ms, Links: []relations.Link{}, Relations: []relations.Link{}, More: len(ms) > limit, Offset: req.Offset}
	if out.More {
		out.Memories = ms[:limit]
		out.NextCursor = out.Memories[len(out.Memories)-1].ID
	}
	if out.Memories == nil {
		out.Memories = []memory.Memory{}
	}
	rs, ok := e.o.Store.(relations.Store)
	if !ok {
		return out, nil
	}
	anchors := make([]string, len(out.Memories))
	visible := map[string]memory.Memory{}
	for i, m := range out.Memories {
		anchors[i] = m.ID
		visible[m.ID] = m
	}
	links, err := rs.RelationshipSets(ctx, req.Tenant, anchors)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		a, aok := visible[l.Source]
		b, bok := visible[l.Target]
		if aok && bok && (req.AllVersions || current(a, e.o.Now()) && current(b, e.o.Now())) &&
			relationAdmits(l, req.ValidAt, req.KnownAt, e.o.Now(), req.AllVersions) {
			out.Relations = append(out.Relations, l)
			if l.Origin == "" || l.Origin == "inferred" {
				out.Links = append(out.Links, l)
			}
		}
	}
	return out, nil
}
