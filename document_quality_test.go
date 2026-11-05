package niromem_test

import (
	"context"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type sectionParser struct{}

func (sectionParser) Validate(string, []byte) error { return nil }
func (sectionParser) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	return []niromem.DocumentPage{{Number: 1, Text: "5.2.1.1 Validacion de instalacion\nAntes de activar, confirme que el cliente termino la instalacion."},
		{Number: 2, Text: "Compruebe que el equipo esta encendido antes de continuar."}}, nil
}

type documentExtractiveFixture struct{ extractiveFixture }

func (*documentExtractiveFixture) KnowledgeExtractionMode() string { return "extractive" }

func TestDocumentSectionsHaveEntitiesAndNoRedundantExtractiveInference(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	p := &documentExtractiveFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Documents: sectionParser{}, Embedder: embed.Hash{Dims: 8},
		Knowledge: niromem.KnowledgeOptions{Mode: "enforced", BackgroundEnrichment: true, Planner: p}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d, err := e.ImportDocument(ctx, memory.Scope{Tenant: "t", User: "u"}, "manual.txt", []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("import jobs: %v %v", js, err)
	}
	if err := e.Worker().Handlers[niromem.JobDocumentImport](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	status, err := e.DocumentStatus(ctx, "t", memory.Principal{User: "u"}, d.ID)
	if err != nil || len(status.Memories) != 2 {
		t.Fatalf("status: %+v %v", status, err)
	}
	for _, id := range status.Memories {
		m, err := e.Get(ctx, "t", memory.Principal{User: "u"}, id)
		if err != nil || m.Metadata["section"] != "5.2.1.1" || m.Title == "" || len(m.Entities) != 3 {
			t.Fatalf("section context lost: %+v %v", m, err)
		}
		jobs, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("enrichment jobs: %v %v", jobs, err)
		}
		for i := 0; i < 2; i++ {
			if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, jobs[0]); err != nil {
				t.Fatal(err)
			}
		}
		result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, id)
		if err != nil || result.Status != "source_only" || result.Reason == "" || len(result.Changes) != 0 {
			t.Fatalf("result: %+v %v", result, err)
		}
	}
	if p.calls != 0 {
		t.Fatal("redundant document inference still called the model")
	}
	if err := e.Worker().Handlers[niromem.JobEmbed](ctx, store.Job{Tenant: "t"}); err != nil {
		t.Fatal(err)
	}
	have, total, err := e.Coverage(ctx, "t", embed.Hash{Dims: 8}.Model().ID)
	if err != nil || have != 2 || total != 2 {
		t.Fatalf("coverage: %d/%d %v", have, total, err)
	}
	result, err := e.Search(ctx, niromem.SearchRequest{Tenant: "t", As: memory.Principal{User: "u"}, Query: "Validacion de instalacion", Explain: true})
	if err != nil || len(result.Hits) != 2 {
		t.Fatalf("search: %+v %v", result, err)
	}
	entityCandidates := 0
	for _, step := range result.Trace {
		if step.Strategy == "entity" && step.Ran {
			entityCandidates = step.Candidates
		}
	}
	if entityCandidates != 2 {
		t.Fatalf("deterministic section entities did not contribute: %+v", result.Trace)
	}
	result, err = e.Search(ctx, niromem.SearchRequest{Tenant: "t", As: memory.Principal{User: "other"}, Query: "Validacion de instalacion"})
	if err != nil || len(result.Hits) != 0 {
		t.Fatal("section entities bypassed source ACL")
	}
}
