//go:build devui

package devui

import (
	"context"
	"encoding/json"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type sourceParser struct{}

func (sourceParser) Validate(string, []byte) error { return nil }
func (sourceParser) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	return []niromem.DocumentPage{{Number: 1, Text: "Document source."}}, nil
}

func TestJobSourcesGroupsImportAndChunkWorkWithTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Documents: sourceParser{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	imp, err := e.ImportDocument(ctx, memory.Scope{Tenant: "t"}, "report.txt", []byte("Document source."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 1); err != nil {
		t.Fatal(err)
	}
	status, err := e.DocumentStatus(ctx, "t", memory.Principal{Admin: true}, imp.ID)
	if err != nil || len(status.Memories) != 1 {
		t.Fatalf("import %+v %v", status, err)
	}
	derived, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t"}, Text: "Derived assertion.", Metadata: map[string]any{
		"enrichment": map[string]any{"source_id": status.Memories[0]},
	}})
	if err != nil || len(derived) != 1 {
		t.Fatalf("derived source %+v %v", derived, err)
	}
	payload := func(id string) json.RawMessage { b, _ := json.Marshal(map[string]string{"id": id}); return b }
	rows := []store.Job{
		{Tenant: "t", Kind: niromem.JobDocumentImport, Payload: payload(imp.ID)},
		{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Payload: payload(status.Memories[0])},
		{Tenant: "t", Kind: niromem.JobEmbed, Key: "model"},
		{Tenant: "other", Kind: niromem.JobDocumentImport, Payload: payload(imp.ID)},
		{Tenant: "t", Kind: niromem.JobRelationships, Payload: payload(derived[0].Memory.ID)},
	}
	d := &dev{o: Options{Engine: e}}
	out, err := d.jobSources(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].File == nil || out[1].File == nil || out[0].File.ID != out[1].File.ID || out[0].File.Name != "report.txt" {
		t.Fatalf("file stages not grouped %+v", out)
	}
	if out[2].File != nil || out[3].File != nil {
		t.Fatal("shared/foreign work assigned to file")
	}
	if out[4].File == nil || out[4].File.ID != imp.ID {
		t.Fatal("derived assertion job lost original-file grouping")
	}
}
