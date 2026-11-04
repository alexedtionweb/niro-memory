package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
)

func TestEmbeddingProviderConfiguration(t *testing.T) {
	for _, provider := range []string{"ollama", "openai"} {
		t.Run(provider, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/api/embed"
				if provider == "openai" {
					wantPath = "/v1/embeddings"
					if r.Header.Get("Api-Key") != "fixture-key" {
						t.Errorf("custom provider header missing")
					}
				}
				if r.URL.Path != wantPath {
					t.Errorf("path %s, want %s", r.URL.Path, wantPath)
				}
				if provider == "ollama" {
					w.Write([]byte(`{"embeddings":[[1,2]],"prompt_eval_count":3}`))
				} else {
					w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
				}
			}))
			defer srv.Close()
			baseURL := srv.URL
			if provider == "openai" {
				baseURL += "/v1"
			}
			options := map[string]any{"base_url": baseURL, "model": "fixture", "dims": 2, "batch_size": 1}
			if provider == "openai" {
				options["headers"] = map[string][]string{"api-key": {"fixture-key"}}
				options["max_retries"] = 1
			} else {
				options["keep_alive"] = "10m"
			}
			raw, err := json.Marshal(options)
			if err != nil {
				t.Fatal(err)
			}
			storeOptions, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &Config{
				Store:    Choice{Use: "sqlite", Options: map[string]json.RawMessage{"sqlite": storeOptions}},
				Embedder: Choice{Use: provider, Options: map[string]json.RawMessage{provider: raw}},
			}
			var o niromem.Options
			closers, err := connectors(context.Background(), cfg, &o)
			for _, closer := range closers {
				defer closer.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.Embedder.Model().ID != provider+"/fixture@1" {
				t.Fatalf("wrong provider selected: %+v", o.Embedder.Model())
			}
			vectors, _, err := o.Embedder.Embed(context.Background(), embed.Query, []embed.Input{{Text: "question"}})
			if err != nil || len(vectors) != 1 || len(vectors[0]) != 2 {
				t.Fatalf("configured provider failed: %v %v", vectors, err)
			}
		})
	}
}
