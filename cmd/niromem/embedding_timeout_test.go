package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
)

func TestOllamaEmbeddingTimeoutConfiguration(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	for _, timeoutMS := range []int64{20, -1, 1<<63 - 1} {
		t.Run(fmt.Sprint(timeoutMS), func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"base_url": server.URL, "model": "test", "dims": 2, "timeout_ms": timeoutMS})
			if err != nil {
				t.Fatal(err)
			}
			storeOptions, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &Config{Store: Choice{Use: "sqlite", Options: map[string]json.RawMessage{"sqlite": storeOptions}},
				Embedder: Choice{Use: "ollama", Options: map[string]json.RawMessage{"ollama": raw}}}
			var o niromem.Options
			closers, err := connectors(context.Background(), cfg, &o)
			for _, closer := range closers {
				defer closer.Close()
			}
			if timeoutMS != 20 {
				if err == nil {
					t.Fatal("invalid timeout accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, err = o.Embedder.Embed(ctx, embed.Query, []embed.Input{{Text: "generated fixture"}})
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "HTTP timeout 20ms") {
				t.Fatalf("configured timeout not applied: %v", err)
			}
		})
	}
}
