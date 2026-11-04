package niromem

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

const JobKnowledgeConsolidate = "knowledge.consolidate"

type consolidationPayload struct {
	ID    string       `json:"id"`
	Layer memory.Layer `json:"layer"`
}

// Consolidation creates transparent source collections, not invented model
// summaries. Applications can separately propose semantic refinements.
func (e *Engine) handleKnowledgeConsolidate(ctx context.Context, job store.Job) error {
	options, err := e.knowledgeOptions(ctx, job.Tenant)
	if err != nil {
		return err
	}
	if !options.Consolidation || options.Mode != "enforced" {
		e.o.Logger.Info("consolidation skipped by tenant policy", "job", job.ID)
		return nil
	}
	var body consolidationPayload
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.ID == "" ||
		(body.Layer != memory.Scene && body.Layer != memory.Profile) {
		return jobs.Permanent(fmt.Errorf("%w: invalid consolidation job", memory.ErrInvalid))
	}
	ms, err := e.o.Store.Get(ctx, job.Tenant, []string{body.ID})
	if err != nil || len(ms) == 0 {
		return err
	}
	anchor := ms[0]
	q := store.Query{Tenant: job.Tenant, Limit: 64, Filter: memory.And(
		memory.Eq("user_id", anchor.User), memory.Eq("agent_id", anchor.Agent),
		memory.Eq("visibility", string(anchor.Visibility)), memory.Eq("layer", int(body.Layer-1))),
		Order: store.OldestFirst}
	if err := q.Filter.Check(); err != nil {
		return err
	}
	candidates, err := e.o.Store.Find(ctx, q)
	if err != nil {
		return err
	}
	var sources []memory.Memory
	var ids []string
	var text strings.Builder
	text.WriteString("Source collection (individual assertions may disagree):\n")
	for _, m := range candidates {
		if !reflect.DeepEqual(m.Grants, anchor.Grants) || len(sources) == 16 {
			continue
		}
		line := "[" + m.ID + "] " + m.Text + "\n"
		if text.Len()+len(line) > e.o.Limits.MaxTextBytes {
			break
		}
		text.WriteString(line)
		sources, ids = append(sources, m), append(ids, m.ID)
	}
	if len(sources) == 0 {
		return nil
	}
	hash, err := operationHash(struct {
		IDs   []string
		Layer memory.Layer
	}{ids, body.Layer})
	if err != nil {
		return err
	}
	key := "knowledge.consolidate:" + hash
	var completed map[string]string
	if replayed, err := e.replayOperation(ctx, job.Tenant, key, hash, &completed); replayed || err != nil {
		return err
	}
	group, err := operationHash(struct {
		Scope      memory.Scope
		Visibility memory.Visibility
		Grants     []string
		Layer      memory.Layer
	}{memory.Scope{Tenant: job.Tenant, User: anchor.User, Agent: anchor.Agent}, anchor.Visibility, anchor.Grants, body.Layer})
	if err != nil {
		return err
	}
	m := memory.Memory{Scope: memory.Scope{Tenant: job.Tenant, User: anchor.User, Agent: anchor.Agent},
		Kind: memory.Episodic, Layer: body.Layer, Title: "Knowledge source collection", Text: text.String(),
		Slot: "knowledge.collection:" + group, Visibility: anchor.Visibility, Grants: append([]string(nil), anchor.Grants...),
		Confidence: 1, Importance: 0.5, Metadata: map[string]any{"source_ids": ids, "consolidation": "source_collection"},
		Source: memory.Source{Kind: "knowledge", DerivedBy: "collection@1", Trust: memory.TrustNormal}}
	b := &store.Batch{Tenant: job.Tenant, OperationKey: key, OperationHash: hash}
	for _, source := range sources {
		b.Checks = append(b.Checks, store.Check{ID: source.ID, Version: source.Version, Open: true})
		m.Source.Evidence = append(m.Source.Evidence, source.Source.Evidence...)
	}
	changes, err := e.rec.Plan(ctx, job.Tenant, []pipeline.Proposal{{Memory: m}}, b)
	if err != nil {
		return err
	}
	if changes[0].Action == pipeline.ActionReject {
		return fmt.Errorf("%w: %s", memory.ErrInvalid, changes[0].Reason)
	}
	m = changes[0].Memory
	for _, source := range sources {
		b.Relations = append(b.Relations, relations.Link{ID: memory.Hash(key + ":" + source.ID),
			Tenant: job.Tenant, Source: m.ID, Target: source.ID, Relation: "derived_from", Confidence: 1,
			Evidence: source.Source.Evidence, Provider: "collection@1", Origin: "derived", Reason: "source_collection",
			At: e.o.Now(), ValidFrom: m.ValidFrom, OperationKey: key})
	}
	e.followUp(b, changes)
	if body.Layer == memory.Scene {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobKnowledgeConsolidate, Key: group + ":profile",
			Payload: payload(consolidationPayload{ID: m.ID, Layer: memory.Profile})})
	}
	b.Result = payload(map[string]string{"status": "consolidated", "id": m.ID})
	_, err = e.o.Store.Write(ctx, b)
	return err
}
