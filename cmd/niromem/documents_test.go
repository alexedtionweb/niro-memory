package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/connectors/sqlite"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestDocumentConnectorConfiguration(t *testing.T) {
	cfg := &Config{}
	var opts niromem.Options
	if err := configureDocuments(cfg, &opts); err != nil || opts.Documents != nil {
		t.Fatalf("disabled: %v", err)
	}

	t.Run("sqlite-restart-and-formats", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		path := filepath.Join(dir, "memory.db")
		openEngine := func() *niromem.Engine {
			t.Helper()
			s, err := sqlite.Open(ctx, sqlite.Options{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			o := niromem.Options{Store: s, Blob: blob.Dir{Root: filepath.Join(dir, "blobs")}}
			cfg := &Config{}
			cfg.Documents.Enabled = true
			if err := configureDocuments(cfg, &o); err != nil {
				t.Fatal(err)
			}
			e, err := niromem.Open(o)
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		var docx bytes.Buffer
		z := zip.NewWriter(&docx)
		w, _ := z.Create("word/document.xml")
		w.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Atlas uses Redis.</w:t></w:r></w:p></w:body></w:document>`))
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		fixtures := map[string][]byte{"note.txt": []byte("Atlas uses Redis."), "note.md": []byte("# Atlas\n\nAtlas uses Redis."), "note.docx": docx.Bytes()}
		if _, err := exec.LookPath("pdftotext"); err == nil {
			var pdf bytes.Buffer
			pdf.WriteString("%PDF-1.4\n")
			offsets := []int{0}
			content := "BT /F1 14 Tf 50 700 Td (Atlas uses Redis.) Tj ET"
			for i, body := range []string{
				"<< /Type /Catalog /Pages 2 0 R >>",
				"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
				"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
				fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
			} {
				offsets = append(offsets, pdf.Len())
				fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, body)
			}
			xref := pdf.Len()
			fmt.Fprintf(&pdf, "xref\n0 6\n0000000000 65535 f \n")
			for _, offset := range offsets[1:] {
				fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
			}
			fmt.Fprintf(&pdf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
			fixtures["note.pdf"] = pdf.Bytes()
		}
		e := openEngine()
		scope := memory.Scope{Tenant: "docs", User: "u", Agent: "a"}
		ids := map[string]string{}
		for name, data := range fixtures {
			result, err := e.ImportDocument(ctx, scope, name, data)
			if err != nil {
				t.Fatal(err)
			}
			ids[name] = result.ID
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		e = openEngine()
		defer e.Close()
		jobs, err := e.Store().Jobs(ctx, store.JobQuery{Tenant: "docs", Kind: niromem.JobDocumentImport, Limit: 20})
		if err != nil || len(jobs) != len(fixtures) {
			t.Fatalf("jobs after restart: %d %v", len(jobs), err)
		}
		for _, job := range jobs {
			if err := e.Worker().Handlers[niromem.JobDocumentImport](ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := e.Worker().Handlers[niromem.JobDocumentImport](ctx, job); err != nil {
				t.Fatal(err)
			}
		}
		for name, id := range ids {
			status, err := e.DocumentStatus(ctx, "docs", memory.Principal{User: "u", Agent: "a"}, id)
			if err != nil || status.Status != "ingested" || len(status.Memories) == 0 {
				t.Fatalf("%s: %+v %v", name, status, err)
			}
			data, filename, err := e.DocumentFile(ctx, "docs", memory.Principal{User: "u", Agent: "a"}, id)
			if err != nil || filename != name || !bytes.Equal(data, fixtures[name]) {
				t.Fatalf("original %s: %v", name, err)
			}
		}
	})
	cfg.Documents.Enabled = true
	if err := configureDocuments(cfg, &opts); err == nil || !strings.Contains(err.Error(), "blob") {
		t.Fatalf("missing blob: %v", err)
	}
	opts.Blob = blob.NewMem()
	if err := configureDocuments(cfg, &opts); err != nil {
		t.Fatal(err)
	}
	pages, err := opts.Documents.Extract(context.Background(), "note.txt", []byte("Atlas uses Redis."))
	if err != nil || len(pages) != 1 || pages[0].Text != "Atlas uses Redis." {
		t.Fatalf("parse: %+v %v", pages, err)
	}
	for _, concurrency := range []int{-1, 5} {
		cfg.Documents.Concurrency = concurrency
		if err := configureDocuments(cfg, &opts); err == nil {
			t.Fatalf("accepted concurrency %d", concurrency)
		}
	}
}
