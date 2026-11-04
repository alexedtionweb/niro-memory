package niromem_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type recoveringStore struct {
	store.Store
	ops  store.Operations
	rel  relations.Store
	fail atomic.Bool
}

func (s *recoveringStore) Operation(ctx context.Context, tenant, key string) (*store.Receipt, error) {
	return s.ops.Operation(ctx, tenant, key)
}
func (s *recoveringStore) RelationshipSets(ctx context.Context, tenant string, ids []string) ([]relations.Link, error) {
	return s.rel.RelationshipSets(ctx, tenant, ids)
}
func (s *recoveringStore) Write(ctx context.Context, b *store.Batch) (*store.Receipt, error) {
	if b.OperationKey == "knowledge.commit:recover" && s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("injected crash before atomic commit")
	}
	return s.Store.Write(ctx, b)
}

type countingPlanner struct{ calls atomic.Int64 }

func (p *countingPlanner) Plan(ctx context.Context, in knowledge.Input, state knowledge.State) (knowledge.Plan, error) {
	p.calls.Add(1)
	return (knowledge.Deterministic{}).Plan(ctx, in, state)
}

func TestKnowledgeRecoversDurablePlanWithoutAnotherProviderCall(t *testing.T) {
	base := memstore.New(nil)
	defer base.Close()
	s := &recoveringStore{Store: base, ops: base, rel: base}
	s.fail.Store(true)
	provider := &countingPlanner{}
	options := niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: provider}}
	e, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := niromem.KnowledgeRequest{Input: knowledge.Input{Scope: memory.Scope{Tenant: "recovery", User: "project"},
		OperationKey: "recover", Text: "Use focused tests", Intent: "ADD", Trust: memory.TrustHigh, Authority: true}}
	if _, err := e.CommitKnowledge(ctx, req); err == nil {
		t.Fatal("injected failure was not surfaced")
	}
	e.Close()
	restarted, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result, err := restarted.CommitKnowledge(ctx, req)
	if err != nil || len(result.Changes) != 1 {
		t.Fatalf("recovered commit: %+v %v", result, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("called provider %d times, want one durable plan", got)
	}
	replayed, err := restarted.CommitKnowledge(ctx, req)
	if err != nil || !replayed.Replayed {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	ms, err := restarted.List(ctx, niromem.ListRequest{Tenant: "recovery", AllVersions: true})
	if err != nil || len(ms) != 1 {
		t.Fatalf("duplicate transition after crash: %+v %v", ms, err)
	}
}
