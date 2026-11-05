package niromem_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestBackgroundEnrichmentKeepsValidRecordsFromMalformedProviderOutput(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	p := &extractionFixture{output: &knowledge.Enrichment{
		Facts: []knowledge.ExtractedFact{
			{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact},
			{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "invented database", Type: memory.EpistemicFact},
		},
		Entities:  []knowledge.ExtractedEntity{{Name: "", Type: "person", Quote: "Atlas uses Redis."}},
		Relations: []knowledge.ExtractedRelation{},
	}}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas uses Redis."})
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich})
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %+v %v", js, err)
	}
	for range 2 {
		if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, changes[0].Memory.ID)
	if err != nil || result.Status != "partial" || len(result.Changes) != 1 || result.Extraction == nil || len(result.Extraction.Facts) != 1 || len(result.Extraction.Issues) != 2 || p.calls != 1 {
		t.Fatalf("background enrichment discarded healthy facts or repeated model work: %+v calls=%d %v", result, p.calls, err)
	}
}

type extractionFixture struct {
	calls  int
	input  knowledge.EnrichmentInput
	fail   error
	after  func(context.Context, knowledge.EnrichmentInput) error
	output *knowledge.Enrichment
}

func TestNonGroundedProviderOutputRequiresReview(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	defer s.Close()
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: &extractionFixture{}, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Completely unrelated source."})
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich})
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %v %v", js, err)
	}
	if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, changes[0].Memory.ID)
	if err != nil || result.Status != "needs_review" || result.Error == "" || len(result.Changes) != 0 {
		t.Fatalf("non-grounded output not isolated: %+v %v", result, err)
	}
}

func TestIncompleteExtractionStaysRetryableAndPreservesOriginal(t *testing.T) {
	for _, invalidEvidence := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete-generation", true: "invalid-evidence"}[invalidEvidence], func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			clock := func() time.Time { return now }
			s := memstore.New(clock)
			p := &extractionFixture{fail: io.ErrUnexpectedEOF}
			wantState := store.JobPending
			if invalidEvidence {
				p.fail = memory.ErrInvalid
			}
			e, err := niromem.Open(niromem.Options{Store: s, Now: clock, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis."})
			if err != nil {
				t.Fatal(err)
			}
			worker := e.Worker()
			worker.BaseBackoff = time.Hour
			now = now.Add(2 * time.Second)
			if _, err := worker.RunOnce(ctx, 100); err != nil {
				t.Fatal(err)
			}
			rows, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich})
			if invalidEvidence {
				if err != nil || len(rows) != 0 {
					t.Fatalf("invalid extraction should complete with review receipt: %v %v", rows, err)
				}
				result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, changes[0].Memory.ID)
				if err != nil || result.Status != "needs_review" || result.Error == "" {
					t.Fatalf("missing explicit review: %+v %v", result, err)
				}
			} else if err != nil || len(rows) != 1 || rows[0].State != wantState || rows[0].Attempts != 1 || rows[0].LastError == "" || p.calls != 1 {
				t.Fatalf("incorrect first-attempt classification %+v calls=%d err=%v", rows, p.calls, err)
			}
			original, err := s.Get(ctx, "t", []string{changes[0].Memory.ID})
			if err != nil || len(original) != 1 || original[0].Text != "Atlas decided to use Redis." {
				t.Fatalf("original lost: %+v %v", original, err)
			}
		})
	}
}
func (*extractionFixture) Plan(ctx context.Context, in knowledge.Input, s knowledge.State) (knowledge.Plan, error) {
	return (knowledge.Deterministic{}).Plan(ctx, in, s)
}
func (p *extractionFixture) Enrich(ctx context.Context, in knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	p.calls++
	p.input = in
	if p.after != nil {
		if err := p.after(ctx, in); err != nil {
			return knowledge.Enrichment{}, err
		}
	}
	if p.fail != nil {
		return knowledge.Enrichment{}, p.fail
	}
	if p.output != nil {
		return *p.output, nil
	}
	return knowledge.Enrichment{Facts: []knowledge.ExtractedFact{{Quote: in.Text, Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicDecision}},
		Relations: []knowledge.ExtractedRelation{}, Provider: "fixture", Model: "cpu-test"}, nil
}

func TestLegacyEvaluationUsesDurableEnrichmentAfterConfigurationUpgrade(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	defer s.Close()
	p := &extractionFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{
		Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis."})
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].Memory.ID
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
	if err != nil || len(js) != 1 {
		t.Fatalf("source job: %+v %v", js, err)
	}
	old := js[0]
	old.ID, old.Kind = "legacy-evaluation", niromem.JobKnowledgeEvaluate
	for _, job := range []store.Job{old, old, js[0]} {
		if err := e.Worker().Handlers[job.Kind](ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, id)
	if err != nil || result.Status != "completed" || p.calls != 1 {
		t.Fatalf("legacy/new jobs must share one durable extraction: %+v calls=%d err=%v", result, p.calls, err)
	}
}

func TestDeletedSourceCannotReappearInEnrichmentReceipts(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	defer s.Close()
	p := &extractionFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis."})
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].Memory.ID
	p.after = func(ctx context.Context, _ knowledge.EnrichmentInput) error { return e.Delete(ctx, "t", "fixture", id) }
	js, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
	handler := e.Worker().Handlers[niromem.JobKnowledgeEnrich]
	if err := handler(ctx, js[0]); err == nil {
		t.Fatal("deleted source was written back in a durable model receipt")
	}
	if _, err := s.Operation(ctx, "t", "enrichment:"+id+":plan"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("deleted plaintext survived in plan: %v", err)
	}
	if err := handler(ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatal("deleted source called model again")
	}
	ms, _ := s.Find(ctx, store.Query{Tenant: "t", AllVersions: true})
	if len(ms) != 0 {
		t.Fatal("derived memories resurrected deleted data")
	}
}

type enrichmentCrashStore struct {
	*memstore.Store
	failSave  bool
	failApply bool
}

func TestExtractionProfileSnapshotSurvivesSettingsChange(t *testing.T) {
	ctx := context.Background()
	s := &enrichmentCrashStore{Store: memstore.New(nil), failApply: true}
	defer s.Close()
	p := &extractionFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis."})
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].Memory.ID
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
	if err != nil || len(js) != 1 {
		t.Fatal(err)
	}
	handle := e.Worker().Handlers[niromem.JobKnowledgeEnrich]
	if err := handle(ctx, js[0]); err == nil {
		t.Fatal("expected interrupted application")
	}
	changed := knowledge.DefaultExtractionProfile()
	changed.EntityTypes = []string{"person"}
	changed.FactTypes = []memory.EpistemicType{memory.EpistemicFact}
	if err := e.PutSettings(ctx, "t", niromem.Settings{Knowledge: &niromem.KnowledgePolicy{Extraction: &changed}}); err != nil {
		t.Fatal(err)
	}
	if err := handle(ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, id)
	if err != nil || result.Status != "completed" || p.calls != 1 || result.Profile == nil || len(result.Schema) == 0 || !reflect.DeepEqual(*result.Profile, knowledge.DefaultExtractionProfile()) {
		t.Fatalf("profile changed on retry: %+v calls=%d err=%v", result, p.calls, err)
	}
}

func (s *enrichmentCrashStore) Write(ctx context.Context, b *store.Batch) (*store.Receipt, error) {
	if s.failSave && strings.HasPrefix(b.OperationKey, "remember:") {
		s.failSave = false
		return nil, errors.New("source transaction failure")
	}
	if s.failApply && strings.HasPrefix(b.OperationKey, "enrichment:") && !strings.HasSuffix(b.OperationKey, ":plan") {
		s.failApply = false
		return nil, errors.New("crash after durable extraction")
	}
	return s.Store.Write(ctx, b)
}

func TestEnrichmentFastAtomicSaveRecoveryAndHistory(t *testing.T) {
	ctx := context.Background()
	base := memstore.New(nil)
	defer base.Close()
	s := &enrichmentCrashStore{Store: base, failSave: true, failApply: true}
	p := &extractionFixture{}
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := niromem.Options{Store: s, Now: func() time.Time { return now }, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}}
	e, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	fact := niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis.", OperationKey: "stable-source", Visibility: memory.Private}
	if _, err := e.Remember(ctx, fact); err == nil {
		t.Fatal("failed source transaction was hidden")
	}
	pending, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich})
	if len(pending) != 0 {
		t.Fatal("job escaped failed source transaction")
	}
	changes, err := e.Remember(ctx, fact)
	if err != nil || len(changes) != 1 {
		t.Fatalf("save: %+v %v", changes, err)
	}
	if p.calls != 0 {
		t.Fatal("save called model")
	}
	source := changes[0].Memory
	pending, err = s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: source.ID})
	if err != nil || len(pending) != 1 {
		t.Fatalf("atomic enqueue: %+v %v", pending, err)
	}
	if _, err := e.Remember(ctx, fact); err != nil {
		t.Fatal(err)
	}
	status, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, source.ID)
	if err != nil || status.Status != "queued" {
		t.Fatalf("status: %+v %v", status, err)
	}
	if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, pending[0]); err == nil {
		t.Fatal("crash injection did not fire")
	}
	ms, _ := s.Find(ctx, store.Query{Tenant: "t", AllVersions: true})
	if len(ms) != 1 {
		t.Fatal("partial enriched memories after failed batch")
	}
	e.Close()
	now = now.Add(time.Hour)
	e, err = niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	handler := e.Worker().Handlers[niromem.JobKnowledgeEnrich]
	if err := handler(ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	if err := handler(ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider called %d times across crash/retry", p.calls)
	}
	status, err = e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, source.ID)
	if err != nil || status.Status != "completed" || len(status.Changes) != 1 || len(status.Relations) != 1 {
		t.Fatalf("completed: %+v %v", status, err)
	}
	derived := status.Changes[0].Memory
	if len(derived.Entities) != 2 || derived.Source.Trust != source.Source.Trust || derived.Visibility != source.Visibility {
		t.Fatalf("lost entity/audience/trust: %+v", derived)
	}
	d, err := memory.ReadDecision(derived)
	if err != nil || d.Status != memory.DecisionProposed {
		t.Fatalf("decision granted authority: %+v %v", d, err)
	}
	after, _ := s.Get(ctx, "t", []string{source.ID})
	if !reflect.DeepEqual(after[0], source) {
		t.Fatal("source interpretation/evidence was modified in place")
	}
	ms, _ = s.Find(ctx, store.Query{Tenant: "t", AllVersions: true})
	if len(ms) != 2 {
		t.Fatalf("retry duplicated transitions: %d", len(ms))
	}
	// The extracted claim applies to the source's world interval, but was not
	// believed before the extraction was recorded.
	earlier, _ := s.Find(ctx, store.Query{Tenant: "t", ValidAt: source.ValidFrom, KnownAt: source.RecordedAt})
	currentPast, _ := s.Find(ctx, store.Query{Tenant: "t", ValidAt: source.ValidFrom, KnownAt: now})
	if len(earlier) != 1 || len(currentPast) != 2 {
		t.Fatalf("bitemporal distinction lost: %d %d", len(earlier), len(currentPast))
	}
	if _, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "other"}, source.ID); err == nil {
		t.Fatal("private extraction leaked")
	}
	if _, err := e.EnrichmentStatus(ctx, "other", memory.Principal{Admin: true}, source.ID); err == nil {
		t.Fatal("cross-tenant extraction leaked")
	}
	if err := e.Invalidate(ctx, "t", source.ID, time.Time{}, "test", "source withdrawn"); err != nil {
		t.Fatal(err)
	}
	deps, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeReevaluate})
	if len(deps) == 0 {
		t.Fatal("source withdrawal did not atomically notify dependents")
	}
}

func TestEnrichmentModesAndCandidateIsolation(t *testing.T) {
	for _, mode := range []string{"disabled", "shadow", "enforced"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := memstore.New(nil)
			defer s.Close()
			p := &extractionFixture{}
			e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: mode, Planner: p, BackgroundEnrichment: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			_, err = e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "other"}, Text: "other owner secret", Visibility: memory.Private},
				niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "private secret", Visibility: memory.Private})
			if err != nil {
				t.Fatal(err)
			}
			changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas decided to use Redis.", Visibility: memory.Team})
			if err != nil {
				t.Fatal(err)
			}
			id := changes[0].Memory.ID
			jobs, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
			if mode == "disabled" {
				if len(jobs) != 0 || p.calls != 0 {
					t.Fatal("disabled extraction ran")
				}
				return
			}
			if len(jobs) != 1 {
				t.Fatal("missing enrichment job")
			}
			if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, jobs[0]); err != nil {
				t.Fatal(err)
			}
			if len(p.input.Candidates) != 0 {
				t.Fatalf("candidate audience leaked: %+v", p.input.Candidates)
			}
			status, err := e.EnrichmentStatus(ctx, "t", memory.Principal{Admin: true}, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "shadow" && (status.Status != "shadow" || len(status.Changes) != 0) {
				t.Fatal("shadow applied interpretation")
			}
			if mode == "enforced" && len(status.Changes) != 1 {
				t.Fatal("live extraction not applied")
			}
		})
	}
}

func TestConversationEnrichmentDoesNotNeedLexicalRules(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	defer s.Close()
	p := &extractionFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	res, err := e.Append(ctx, memory.Scope{Tenant: "t", User: "u", Run: "r"}, []niromem.Turn{{ClientMsgID: "one", Role: "user", Text: "Atlas decided to use Redis."}}, niromem.AppendOptions{Wait: true})
	if err != nil || len(res.Changes) != 1 || p.calls != 0 {
		t.Fatalf("conversation source path: %+v %v", res, err)
	}
	js, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: res.Changes[0].Memory.ID})
	if len(js) != 1 {
		t.Fatal("chat source was not transactionally queued")
	}
	if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
}
