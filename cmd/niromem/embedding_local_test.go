package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
)

func TestLocalConfiguredEmbeddingGemma(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EMBEDDING") != "1" {
		t.Skip("opt-in local embedding API check with generated fixtures")
	}
	cfg, err := loadConfig("../../configs/lfm2.5-350m.example.json")
	if err != nil {
		t.Fatal(err)
	}
	storeOptions, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	isolated := &Config{Store: Choice{Use: "sqlite", Options: map[string]json.RawMessage{"sqlite": storeOptions}}, Embedder: cfg.Embedder}
	var options niromem.Options
	closers, err := connectors(context.Background(), isolated, &options)
	for _, closer := range closers {
		defer closer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, purpose := range []embed.Purpose{embed.Query, embed.Document} {
		start := time.Now()
		vectors, _, err := options.Embedder.Embed(ctx, purpose, []embed.Input{
			{Text: "Where does Atlas store cached data?"},
			{Text: strings.Repeat("Atlas caches data in Redis. ", 100) + " Bruna trabalha na Contoso."},
		})
		if err != nil || len(vectors) != 2 {
			t.Fatalf("%v: vectors=%d error=%v", purpose, len(vectors), err)
		}
		for i, v := range vectors {
			if len(v) != 768 {
				t.Fatalf("input %d dimensions=%d want 768", i, len(v))
			}
			var norm float64
			for _, value := range v {
				norm += float64(value) * float64(value)
			}
			if math.IsNaN(norm) || math.IsInf(norm, 0) || norm == 0 {
				t.Fatalf("input %d invalid vector", i)
			}
			if i == 1 && math.Abs(norm-1) > 1e-5 {
				t.Fatalf("combined vector norm=%f", norm)
			}
		}
		t.Logf("%v: short + long inputs, 2 x 768-dimensional vectors in %s", purpose, time.Since(start).Round(time.Millisecond))
	}
}
