package niromem_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type documentFixture struct {
	calls atomic.Int32
	fail  error
}

type mediaFixture struct{ documentFixture }

func (*mediaFixture) Name() string { return "ollama/lfm3-omni:latest" }
func (p *mediaFixture) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	p.calls.Add(1)
	return []niromem.DocumentPage{{Number: 1, Text: "The image appears to show a red square.", Model: "ollama/lfm3-omni:latest"}}, p.fail
}

func TestMediaImportIsAsyncPrivateUnreviewedAndReplayable(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	parser := &mediaFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Media: parser})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	scope := memory.Scope{Tenant: "t", User: "u", Agent: "a"}
	result, err := e.ImportMedia(ctx, scope, "photo.png", []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if parser.calls.Load() != 0 || !result.Unreviewed || result.Model != parser.Name() {
		t.Fatalf("upload: %+v", result)
	}
	eps, err := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", IDs: []string{result.ID}})
	if err != nil || len(eps) != 1 || eps[0].Parts[0].Kind != memory.PartImage || eps[0].Parts[0].Mime != "image/png" {
		t.Fatalf("original media typing lost: %+v %v", eps, err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobMediaInterpret, Key: result.ID})
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %v %v", js, err)
	}
	handler := e.Worker().Handlers[niromem.JobMediaInterpret]
	for i := 0; i < 2; i++ {
		if err := handler(ctx, js[0]); err != nil {
			t.Fatal(err)
		}
	}
	status, err := e.DocumentStatus(ctx, "t", memory.Principal{User: "u", Agent: "a"}, result.ID)
	if err != nil || status.Status != "ingested" || status.Kind != "media" || !status.Unreviewed || parser.calls.Load() != 1 {
		t.Fatalf("status: %+v %v", status, err)
	}
	m, err := e.Get(ctx, "t", memory.Principal{User: "u", Agent: "a"}, status.Memories[0])
	if err != nil {
		t.Fatal(err)
	}
	if m.Source.Kind != "media_interpreted" || m.Source.Trust != memory.TrustLow || m.Confidence != 0 || m.Source.DerivedBy != parser.Name() ||
		m.Visibility != memory.Private || m.Metadata["original_episode"] != result.ID || m.Metadata["interpretation"] != "unreviewed" {
		t.Fatalf("missing provenance: %+v", m)
	}
	if _, _, err := e.DocumentFile(ctx, "t", memory.Principal{User: "other"}, result.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("original leaked: %v", err)
	}
}

func (*documentFixture) Validate(string, []byte) error { return nil }
func (p *documentFixture) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	p.calls.Add(1)
	return []niromem.DocumentPage{{Number: 1, Text: strings.Repeat("Atlas uses Redis. ", 400)}, {Number: 2, Text: "Second page: Atlas is a service."}}, p.fail
}

func TestDocumentImportDurableEvidenceAndReplay(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	parser := &documentFixture{}
	e, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Documents: parser,
		Knowledge: niromem.KnowledgeOptions{Mode: "enforced", BackgroundEnrichment: true, Planner: knowledge.Deterministic{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	scope := memory.Scope{Tenant: "t", User: "u", Agent: "a"}
	result, err := e.ImportDocument(ctx, scope, "manual.txt", []byte("original bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if parser.calls.Load() != 0 || result.Status != "queued" {
		t.Fatal("parsing ran on upload")
	}
	again, err := e.ImportDocument(ctx, scope, "manual.txt", []byte("original bytes"))
	if err != nil || again.ID != result.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %v %v", js, err)
	}
	other, err := niromem.Open(niromem.Options{Store: s, Blob: nil, Documents: parser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.DocumentStatus(ctx, "t", memory.Principal{User: "else"}, result.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("ACL: %v", err)
	}
	if _, err := other.DocumentStatus(ctx, "else", memory.Principal{Admin: true}, result.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("tenant: %v", err)
	}
	handle := e.Worker().Handlers[niromem.JobDocumentImport]
	if err := handle(ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	if err := handle(ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	status, err := e.DocumentStatus(ctx, "t", memory.Principal{User: "u", Agent: "a"}, result.ID)
	if err != nil || status.Status != "ingested" || status.Pages != 2 || len(status.Memories) < 3 || parser.calls.Load() != 1 {
		t.Fatalf("status: %+v calls=%d err=%v", status, parser.calls.Load(), err)
	}
	for _, id := range status.Memories {
		m, err := e.Get(ctx, "t", memory.Principal{User: "u", Agent: "a"}, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Text) > 3500 || m.Visibility != memory.Private || len(m.Source.Evidence) == 0 {
			t.Fatalf("invalid chunk: %+v", m)
		}
		ev := m.Source.Evidence[0]
		eps, err := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", IDs: []string{ev.Episode}})
		if err != nil || len(eps) != 1 || eps[0].Parts[ev.Part].Text[ev.Start:ev.End] != m.Text {
			// Normalization may remove leading/trailing whitespace only.
			if len(eps) != 1 || memory.Normalize(eps[0].Parts[ev.Part].Text[ev.Start:ev.End]) != m.Text {
				t.Fatalf("ungrounded chunk: %v", err)
			}
		}
	}
	enrichment, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich})
	if err != nil || len(enrichment) == 0 {
		t.Fatalf("missing background extraction: %v %v", enrichment, err)
	}
}

func TestDocumentImportParserFailureDoesNotSavePartialChunks(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	p := &documentFixture{fail: errors.New("OCR unavailable: install tesseract")}
	e, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Documents: p})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	result, err := e.ImportDocument(ctx, memory.Scope{Tenant: "t"}, "scan.pdf", []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	js, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobDocumentImport, Key: result.ID})
	err = e.Worker().Handlers[niromem.JobDocumentImport](ctx, js[0])
	if err == nil || !strings.Contains(err.Error(), "install tesseract") {
		t.Fatalf("error: %v", err)
	}
	ms, err := e.List(ctx, niromem.ListRequest{Tenant: "t", As: memory.Principal{Admin: true}})
	if err != nil || len(ms) != 0 {
		t.Fatal("partial chunks were saved")
	}
}
