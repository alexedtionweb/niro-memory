package niromem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type revisionParser struct{ calls atomic.Int32 }

func (*revisionParser) Validate(string, []byte) error { return nil }
func (p *revisionParser) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	p.calls.Add(1)
	return []niromem.DocumentPage{{Number: 1, Text: "5.1 Cache\nAtlas uses Redis."}, {Number: 2, Text: "5.2 Storage\nAtlas uses Redis."}}, nil
}

type revisionPlanner struct {
	knowledge.Deterministic
	calls      int
	fail       error
	failAt     int
	entityType string
}

func (*revisionPlanner) KnowledgeExtractionMode() string     { return "structured" }
func (*revisionPlanner) KnowledgeProvider() (string, string) { return "fixture", "structured" }
func (p *revisionPlanner) Enrich(context.Context, knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	p.calls++
	if p.fail != nil && (p.failAt == 0 || p.failAt == p.calls) {
		return knowledge.Enrichment{}, p.fail
	}
	entityType := p.entityType
	if entityType == "" {
		entityType = "database"
	}
	return knowledge.Enrichment{Facts: []knowledge.ExtractedFact{{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, SubjectType: "project", ObjectType: entityType}},
		Entities: []knowledge.ExtractedEntity{{Name: "Redis", Type: entityType, Quote: "Atlas uses Redis."}}, Relations: []knowledge.ExtractedRelation{}, Provider: "fixture", Model: "structured"}, nil
}

type contextualRevisionPlanner struct{ knowledge.Deterministic }

func (*contextualRevisionPlanner) Enrich(_ context.Context, in knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	first := knowledge.ExtractedFact{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact}
	second := first
	second.Quote = in.Text
	return knowledge.Enrichment{Facts: []knowledge.ExtractedFact{first, second}, Entities: []knowledge.ExtractedEntity{}, Relations: []knowledge.ExtractedRelation{}}, nil
}

func TestDocumentRevisionDoesNotCollapseDistinctEvidenceWithTheSameTriple(t *testing.T) {
	f := newRevisionFixtureWithOptions(t, func(o *niromem.Options) { o.Knowledge.Planner = &contextualRevisionPlanner{} })
	r, err := f.e.ReprocessDocument(context.Background(), "t", memory.Principal{Admin: true}, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "distinct-evidence", Stages: []string{"knowledge"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	result, err := f.e.DocumentRevision(context.Background(), "t", memory.Principal{Admin: true}, f.doc.ID, r.ID)
	if err != nil || len(result.Enriched) != 4 || result.ExtractedFacts != 4 {
		t.Fatalf("same triple collapsed distinct evidence: %+v %v", result, err)
	}
}

type revisionFixture struct {
	e      *niromem.Engine
	s      *memstore.Store
	doc    *niromem.DocumentImport
	p      *revisionPlanner
	parser *revisionParser
}

func newRevisionFixture(t *testing.T) revisionFixture {
	return newRevisionFixtureWithOptions(t, nil)
}

func newRevisionFixtureWithOptions(t *testing.T, customize func(*niromem.Options)) revisionFixture {
	t.Helper()
	s := memstore.New(nil)
	parser, p := &revisionParser{}, &revisionPlanner{}
	options := niromem.Options{Store: s, Blob: blob.NewMem(), Documents: parser, Embedder: embed.Hash{Dims: 8},
		Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}}
	if customize != nil {
		customize(&options)
	}
	e, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	d, err := e.ImportDocument(context.Background(), memory.Scope{Tenant: "t", User: "u", Agent: "a"}, "manual.txt", []byte("original bytes"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(context.Background(), store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("import jobs: %v %v", js, err)
	}
	if err := e.Worker().Handlers[niromem.JobDocumentImport](context.Background(), js[0]); err != nil {
		t.Fatal(err)
	}
	return revisionFixture{e: e, s: s, doc: d, p: p, parser: parser}
}

type revisionAdmission struct{ calls int }

func (a *revisionAdmission) DecideAdmission(context.Context, knowledge.AdmissionInput) (knowledge.AdmissionResult, error) {
	a.calls++
	return knowledge.AdmissionRequireConfirmation, nil
}

func TestDocumentRevisionRespectsAdmissionAndPolicyChanges(t *testing.T) {
	for _, kind := range []string{"admission", "disabled-policy"} {
		t.Run(kind, func(t *testing.T) {
			admission := &revisionAdmission{}
			f := newRevisionFixtureWithOptions(t, func(o *niromem.Options) {
				if kind == "admission" {
					o.Knowledge.Admission = admission
				}
			})
			ctx := context.Background()
			as := memory.Principal{Admin: true}
			r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: kind, Stages: []string{"knowledge"}})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "disabled-policy" {
				if err := f.e.PutSettings(ctx, "t", niromem.Settings{Knowledge: &niromem.KnowledgePolicy{Mode: "disabled"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := runRevision(t, f, r); err != nil {
				t.Fatal(err)
			}
			result, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
			if err != nil || result.State != "needs_review" || result.Error == "" || f.p.calls != 0 {
				t.Fatalf("policy bypass: %+v calls=%d err=%v", result, f.p.calls, err)
			}
			if kind == "admission" && admission.calls != 1 {
				t.Fatal("admission policy was not consulted")
			}
			g, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
			if err != nil || g.Revision.ID != "original" {
				t.Fatalf("policy failure changed active revision: %+v %v", g, err)
			}
		})
	}
}

type invalidRevisionEmbedder struct {
	embed.Hash
	kind string
}

func (e invalidRevisionEmbedder) Embed(_ context.Context, _ embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	out := make([][]float32, len(in))
	if e.kind == "count" {
		return out[:0], embed.Usage{}, nil
	}
	for i := range out {
		out[i] = make([]float32, e.Dims)
		switch e.kind {
		case "dimensions":
			out[i] = out[i][:1]
		case "nonfinite":
			out[i][0] = float32(math.Inf(1))
		}
	}
	return out, embed.Usage{}, nil
}

func TestDocumentRevisionInvalidVectorsRequireReview(t *testing.T) {
	for _, kind := range []string{"count", "dimensions", "nonfinite", "zero"} {
		t.Run(kind, func(t *testing.T) {
			f := newRevisionFixtureWithOptions(t, func(o *niromem.Options) { o.Embedder = invalidRevisionEmbedder{Hash: embed.Hash{Dims: 8}, kind: kind} })
			ctx := context.Background()
			as := memory.Principal{Admin: true}
			r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: kind, Stages: []string{"embeddings"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := runRevision(t, f, r); err != nil {
				t.Fatal(err)
			}
			result, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
			if err != nil || result.State != "needs_review" {
				t.Fatalf("invalid vector accepted: %+v %v", result, err)
			}
			g, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
			if err != nil || g.Revision.ID != "original" {
				t.Fatal("invalid embedding replaced original")
			}
		})
	}
}

func TestDocumentRevisionPreservesExternalLinksAndActivationTime(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{Admin: true}
	g, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	source := g.Nodes[1].ID
	changes, err := f.e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u", Agent: "a"}, Text: "External project convention.", Visibility: memory.Private})
	if err != nil {
		t.Fatal(err)
	}
	external := changes[0].Memory.ID
	if _, err := f.s.Write(ctx, &store.Batch{Tenant: "t", Relations: []relations.Link{{ID: "external-link", Tenant: "t", Source: source, Target: external, Relation: "depends_on", Confidence: .8}}}); err != nil {
		t.Fatal(err)
	}
	r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "copy-external", Stages: []string{"embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	result, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
	if err != nil || result.CompletedAt == nil || result.State != "committed" {
		t.Fatalf("revision: %+v %v", result, err)
	}
	ms, err := f.s.Get(ctx, "t", result.Chunks)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if !m.RecordedAt.Equal(*result.CompletedAt) {
			t.Fatalf("knowledge backdated to queued work: %s vs %s", m.RecordedAt, result.CompletedAt)
		}
	}
	links, err := f.s.RelationshipSets(ctx, "t", []string{external})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range links {
		if l.Target == external && l.Source != source && l.Relation == "depends_on" && l.ValidTo == nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("unselected external relationship lost: %+v", links)
	}
}
func runRevision(t *testing.T, f revisionFixture, r *niromem.DocumentRevision) error {
	t.Helper()
	js, err := f.s.Jobs(context.Background(), store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentReprocess, Key: r.ID})
	if err != nil || len(js) != 1 {
		t.Fatalf("revision job: %v %v", js, err)
	}
	return f.e.Worker().Handlers[niromem.JobDocumentReprocess](context.Background(), js[0])
}

func TestDocumentRevisionAtomicActivationHistoryAndSelectiveReprocessing(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{User: "u", Agent: "a"}
	baseline, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 1)
	if err != nil || len(baseline.Nodes) != 2 || baseline.TotalChunks != 2 || baseline.Next == "" || baseline.Revision.ID != "original" {
		t.Fatalf("legacy graph: %+v %v", baseline, err)
	}
	r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "first", Stages: []string{"knowledge", "embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || current.Revision.ID != "original" {
		t.Fatal("queued work replaced active data")
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	current, err = f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || current.Revision.ID != r.ID || current.TotalChunks != 2 || current.TotalEnriched != 2 || !current.IsCurrent {
		t.Fatalf("active graph: %+v %v", current, err)
	}
	vectors, enriched := 0, 0
	for _, node := range current.Nodes {
		if node.Type == "embedding" {
			vectors++
			if node.Dims != 8 {
				t.Fatal("wrong vector dimension")
			}
		}
		if node.Type == "enriched" {
			enriched++
			link, err := f.e.MemoryDocument(ctx, "t", as, node.MemoryID)
			if err != nil || link.DocumentID != f.doc.ID || link.RevisionID != r.ID {
				t.Fatalf("lineage: %+v %v", link, err)
			}
		}
	}
	if vectors != 4 || enriched != 2 || f.p.calls != 2 || f.parser.calls.Load() != 1 {
		t.Fatalf("replayed model calls or missing layers: vectors=%d enriched=%d calls=%d parser=%d", vectors, enriched, f.p.calls, f.parser.calls.Load())
	}
	have, total, err := f.e.Coverage(ctx, "t", embed.Hash{Dims: 8}.Model().ID)
	if err != nil || have != 4 || total != 4 {
		t.Fatalf("manifest inflated embedding coverage: %d/%d %v", have, total, err)
	}
	found, err := f.e.Search(ctx, niromem.SearchRequest{Tenant: "t", As: as, Query: "Atlas Redis", Limit: 20})
	if err != nil || len(found.Hits) == 0 {
		t.Fatalf("activated revision is not retrievable: %+v %v", found, err)
	}
	for _, hit := range found.Hits {
		currentID := false
		for _, node := range current.Nodes {
			currentID = currentID || node.MemoryID == hit.Memory.ID
		}
		if !currentID {
			t.Fatalf("search exposed draft, old revision or manifest: %s", hit.Memory.ID)
		}
	}
	old, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "original", "", 20)
	if err != nil || old.IsCurrent || old.TotalChunks != 2 {
		t.Fatalf("original history lost: %+v %v", old, err)
	}
	for _, n := range old.Nodes {
		if n.Type == "chunk" && n.Metadata["valid_to"] == nil {
			t.Fatal("superseded source still open")
		}
	}
	r2, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "only-vectors", Stages: []string{"embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, r2); err != nil {
		t.Fatal(err)
	}
	latest, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || latest.TotalEnriched != 2 || latest.Revision.ID != r2.ID || f.p.calls != 2 {
		t.Fatalf("unselected knowledge was rerun or lost: %+v %v", latest, err)
	}
	previous, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, r.ID, "", 20)
	if err != nil || previous.IsCurrent || previous.TotalEnriched != 2 {
		t.Fatalf("previous revision lost: %+v %v", previous, err)
	}
	original, _, err := f.e.DocumentFile(ctx, "t", as, f.doc.ID)
	if err != nil || string(original) != "original bytes" {
		t.Fatal("raw source changed")
	}
	revisions, err := f.e.DocumentRevisions(ctx, "t", as, f.doc.ID)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revision history: %v %v", revisions, err)
	}
	for _, wrong := range []memory.Principal{{User: "other"}, {Agent: "other"}} {
		if _, err := f.e.DocumentGraph(ctx, "t", wrong, f.doc.ID, "", "", 20); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("graph leaked document: %v", err)
		}
	}
	if _, err := f.e.DocumentGraph(ctx, "other", memory.Principal{Admin: true}, f.doc.ID, "", "", 20); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("cross-tenant graph leaked")
	}
}

func TestDocumentRevisionFailuresKeepPreviousActive(t *testing.T) {
	for _, kind := range []string{"invalid", "transport", "concurrent"} {
		t.Run(kind, func(t *testing.T) {
			f := newRevisionFixture(t)
			ctx := context.Background()
			as := memory.Principal{Admin: true}
			r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: kind, Stages: []string{"knowledge"}})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid":
				f.p.fail = fmt.Errorf("%w: unsupported evidence", memory.ErrInvalid)
			case "transport":
				f.p.fail = errors.New("model unavailable")
			case "concurrent":
				other, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "winner", Stages: []string{"embeddings"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := runRevision(t, f, other); err != nil {
					t.Fatal(err)
				}
			}
			err = runRevision(t, f, r)
			if kind == "transport" && err == nil {
				t.Fatal("transport failure hidden")
			}
			if kind != "transport" && err != nil {
				t.Fatal(err)
			}
			state, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "invalid" && (state.State != "committed" || !state.Incomplete || state.RepairRevisionID == "") {
				t.Fatalf("missing explicit partial state: %+v", state)
			}
			if kind == "concurrent" && state.State != "needs_review" {
				t.Fatalf("missing review state: %+v", state)
			}
			graph, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
			if err != nil || kind != "invalid" && graph.Revision.ID == r.ID || graph.TotalChunks != 2 {
				t.Fatalf("failed revision replaced active data: %+v %v", graph, err)
			}
		})
	}
}

func TestDocumentGraphLegacyDerivedLinksAndPaginationAreAuthorized(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{User: "u", Agent: "a"}
	first, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", first.Next, 1)
	if err != nil || second.Next != "" || first.Nodes[1].ID == second.Nodes[1].ID {
		t.Fatalf("bad pagination: %+v %v", second, err)
	}
	if _, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "unknown", 1); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("foreign cursor accepted")
	}
	id := first.Nodes[1].ID
	derived, err := f.e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u", Agent: "a"}, Text: "A legacy extracted fact.",
		Metadata: map[string]any{"enrichment": map[string]any{"source_id": id}}})
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.e.MemoryDocument(ctx, "t", as, derived[0].Memory.ID)
	if err != nil || link.DocumentID != f.doc.ID {
		t.Fatalf("legacy provenance not reconstructed: %+v %v", link, err)
	}
	if _, err := f.e.MemoryDocument(ctx, "t", memory.Principal{User: "other"}, derived[0].Memory.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("legacy provenance bypassed memory ACL")
	}
}

func TestDocumentRevisionResumesCheckpointWithoutReextracting(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{Admin: true}
	r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "retry-partial", Stages: []string{"knowledge"}})
	if err != nil {
		t.Fatal(err)
	}
	f.p.fail, f.p.failAt = errors.New("temporary model outage"), 2
	if err := runRevision(t, f, r); err == nil {
		t.Fatal("transport failure swallowed")
	}
	status, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
	if err != nil || status.Progress == nil || status.Progress.Completed != 1 || status.Progress.Total != 2 || f.p.calls != 2 {
		t.Fatalf("missing checkpoint: %+v calls=%d %v", status, f.p.calls, err)
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	status, err = f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
	if err != nil || status.State != "committed" || f.p.calls != 3 {
		t.Fatalf("successful model call repeated: %+v calls=%d %v", status, f.p.calls, err)
	}
}

func TestDocumentRevisionReplacesGeneratedEntities(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{Admin: true}
	for _, kind := range []string{"database", "technology", "service"} {
		f.p.entityType = kind
		r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: kind, Stages: []string{"knowledge"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := runRevision(t, f, r); err != nil {
			t.Fatal(err)
		}
		graph, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
		if err != nil || graph.Revision.ID != r.ID {
			t.Fatalf("entity revision: %+v %v", graph, err)
		}
		status, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := f.s.Get(ctx, "t", status.Chunks)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			seen := map[memory.EntityRef]bool{}
			for _, ref := range m.Entities {
				if seen[ref] {
					t.Fatalf("duplicate entity across revisions: %+v", m.Entities)
				}
				seen[ref] = true
			}
			raw, err := json.Marshal(m.Metadata["extracted_entities"])
			if err != nil {
				t.Fatal(err)
			}
			var annotations []knowledge.ExtractedEntity
			if err := json.Unmarshal(raw, &annotations); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, annotation := range annotations {
				if annotation.Name == "Redis" {
					if annotation.Type != kind {
						t.Fatalf("stale extraction survives revision: %s", raw)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("new entity annotation missing: %s", raw)
			}
		}
	}
}

type revisionInferer struct {
	calls   int
	invalid bool
}

func (*revisionInferer) Name() string { return "fixture/relationships" }
func (p *revisionInferer) Infer(_ context.Context, in relations.Input) ([]relations.Proposal, error) {
	p.calls++
	target := in.Candidates[0].ID
	if p.invalid {
		target = "foreign-memory"
	}
	return []relations.Proposal{{Target: target, Relation: "related_to", Reason: "Source passages discuss Atlas.", Confidence: .7}}, nil
}

func TestDocumentRevisionRelationshipStageAndValidation(t *testing.T) {
	inferer := &revisionInferer{}
	f := newRevisionFixtureWithOptions(t, func(o *niromem.Options) { o.Relationships = inferer })
	ctx := context.Background()
	as := memory.Principal{Admin: true}
	r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "relations", Stages: []string{"relationships"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runRevision(t, f, r); err != nil {
			t.Fatal(err)
		}
	}
	g, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || g.Revision.ID != r.ID || inferer.calls != 2 {
		t.Fatalf("relationship stage: %+v calls=%d %v", g, inferer.calls, err)
	}
	found := 0
	for _, edge := range g.Edges {
		if edge.Relation == "related_to" && edge.Origin == "inferred" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("missing inferred links: %+v", g.Edges)
	}
	inferer.invalid = true
	failed, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "invalid-relations", Stages: []string{"relationships"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, failed); err != nil {
		t.Fatal(err)
	}
	status, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, failed.ID)
	if err != nil || status.State != "needs_review" {
		t.Fatalf("invalid target accepted: %+v %v", status, err)
	}
	g, err = f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || g.Revision.ID != r.ID {
		t.Fatal("invalid inferred links activated")
	}
}

func TestDocumentPartialRevisionActivatesValidResultsAndRepairsOnlyFailedChunk(t *testing.T) {
	f := newRevisionFixture(t)
	ctx := context.Background()
	as := memory.Principal{Admin: true}
	f.p.fail, f.p.failAt = memory.ErrInvalid, 2
	r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "partial", Stages: []string{"knowledge", "embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runRevision(t, f, r); err != nil {
		t.Fatal(err)
	}
	partial, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
	if err != nil || partial.State != "committed" || !partial.Incomplete || len(partial.Issues) != 1 || len(partial.Enriched) != 1 || partial.RepairRevisionID == "" {
		t.Fatalf("partial results lost: %+v %v", partial, err)
	}
	graph, err := f.e.DocumentGraph(ctx, "t", as, f.doc.ID, "", "", 20)
	if err != nil || !graph.Revision.Incomplete || graph.TotalEnriched != 1 {
		t.Fatalf("valid results not activated: %+v %v", graph, err)
	}
	search, err := f.e.Search(ctx, niromem.SearchRequest{Tenant: "t", As: as, Query: "Redis", Limit: 20})
	if err != nil || len(search.Hits) == 0 {
		t.Fatal("validated partial results not searchable")
	}
	repairJobs, err := f.s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentRepair})
	if err != nil || len(repairJobs) != 1 {
		t.Fatalf("repair not atomically queued: %+v %v", repairJobs, err)
	}
	f.p.fail = nil
	for range 2 {
		if err := f.e.Worker().Handlers[niromem.JobDocumentRepair](ctx, repairJobs[0]); err != nil {
			t.Fatal(err)
		}
	}
	repair, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, partial.RepairRevisionID)
	if err != nil || repair.RepairOf != partial.ID || len(repair.RepairTargets) != 1 {
		t.Fatalf("repair scope: %+v %v", repair, err)
	}
	if err := runRevision(t, f, &repair); err != nil {
		t.Fatal(err)
	}
	complete, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, repair.ID)
	if err != nil || complete.State != "committed" || complete.Incomplete || len(complete.Enriched) != 2 || f.p.calls != 3 {
		t.Fatalf("targeted repair reran healthy chunk or lost knowledge: %+v calls=%d %v", complete, f.p.calls, err)
	}
	have, total, err := f.e.Coverage(ctx, "t", embed.Hash{Dims: 8}.Model().ID)
	if err != nil || have != 4 || total != 4 {
		t.Fatalf("repair embedding coverage: %d/%d %v", have, total, err)
	}
}

func TestDocumentAutomaticRepairIsBoundedAndDoesNotReplaceNewerRevision(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			f := newRevisionFixture(t)
			ctx := context.Background()
			as := memory.Principal{Admin: true}
			f.p.fail = memory.ErrInvalid
			r, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "failed", Stages: []string{"knowledge"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := runRevision(t, f, r); err != nil {
				t.Fatal(err)
			}
			partial, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			js, err := f.s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentRepair})
			if err != nil || len(js) != 1 {
				t.Fatalf("repair jobs: %+v %v", js, err)
			}
			if newer {
				winner, err := f.e.ReprocessDocument(ctx, "t", as, f.doc.ID, niromem.DocumentReprocessRequest{RequestID: "newer", Stages: []string{"embeddings"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := runRevision(t, f, winner); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.e.Worker().Handlers[niromem.JobDocumentRepair](ctx, js[0]); err != nil {
				t.Fatal(err)
			}
			repair, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, partial.RepairRevisionID)
			if newer {
				if !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("stale repair scheduled: %+v %v", repair, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := runRevision(t, f, &repair); err != nil {
				t.Fatal(err)
			}
			result, err := f.e.DocumentRevision(ctx, "t", as, f.doc.ID, repair.ID)
			if err != nil || !result.Incomplete || result.RepairRevisionID != "" {
				t.Fatalf("unbounded automatic repair: %+v %v", result, err)
			}
			jobs, err := f.s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentRepair})
			if err != nil || len(jobs) != 1 {
				t.Fatalf("repair loop: %+v %v", jobs, err)
			}
		})
	}
}
