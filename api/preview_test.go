package api_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestFilePreviewUsesSafeTypesAndOriginalBytes(t *testing.T) {
	e, err := niromem.Open(niromem.Options{Store: memstore.New(nil), Blob: blob.NewMem(), Documents: httpDocumentParser{}, Media: httpMediaParser{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer((&api.Server{Engine: e, Auth: api.Dev{}}).Handler())
	defer srv.Close()
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		data     []byte
		media    bool
		wantType string
		code     int
	}{
		{"photo.png", imageBytes.Bytes(), true, "image/png", 200},
		{"note.md", []byte("<script>alert('not executable')</script>"), false, "text/plain; charset=utf-8", 200},
		{"file.docx", []byte("not a supported inline format"), false, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importer := e.ImportDocument
			if tc.media {
				importer = e.ImportMedia
			}
			result, err := importer(context.Background(), memory.Scope{Tenant: "dev"}, tc.name, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Get(srv.URL + "/v1/documents/" + result.ID + "/preview")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.code {
				t.Fatalf("status %d: %s", resp.StatusCode, data)
			}
			if tc.code == 200 {
				if resp.Header.Get("Content-Type") != tc.wantType || !bytes.Equal(data, tc.data) {
					t.Fatalf("unsafe or changed preview: %s", resp.Header.Get("Content-Type"))
				}
				if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
					t.Fatal("missing preview safety headers")
				}
			}
		})
	}
}
