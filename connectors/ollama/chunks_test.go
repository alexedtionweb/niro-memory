package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/embed"
)

func TestChunkingPreservesTextOrderAndWeightedMean(t *testing.T) {
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Truncate {
			t.Error("truncation enabled")
		}
		vectors := make([][]float32, len(body.Input))
		for i, text := range body.Input {
			if !strings.HasPrefix(text, "doc:") {
				t.Error("prefix missing")
			}
			text = strings.TrimPrefix(text, "doc:")
			received = append(received, text)
			if !utf8.ValidString(text) || len(text) > 4 {
				t.Error("invalid UTF8 chunk or bound")
			}
			vectors[i] = []float32{1, 0}
			if text == "x" {
				vectors[i] = []float32{0, 1}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors, "prompt_eval_count": len(vectors)})
	}))
	defer server.Close()
	e, err := New(Options{BaseURL: server.URL, Model: "test", Dims: 2, ChunkSize: 4, BatchSize: 2, DocumentPrefix: "doc:"})
	if err != nil {
		t.Fatal(err)
	}
	vectors, usage, err := e.Embed(context.Background(), embed.Document, []embed.Input{{Text: "abcdx"}, {Text: "界界🙂"}})
	if err != nil || len(vectors) != 2 {
		t.Fatalf("chunk embed: %v %v", vectors, err)
	}
	if strings.Join(received, "") != "abcdx界界🙂" || usage.Inputs != 2 || usage.Tokens != 5 {
		t.Fatalf("text or accounting lost: %q %+v", received, usage)
	}
	want := []float64{4 / math.Sqrt(17), 1 / math.Sqrt(17)}
	for i, v := range vectors[0] {
		if math.Abs(float64(v)-want[i]) > 1e-6 {
			t.Fatalf("weighted mean=%v", vectors[0])
		}
	}
	if vectors[1][0] != 1 || vectors[1][1] != 0 {
		t.Fatal("UTF8 chunks combined incorrectly")
	}
	if e.Model().ID != "ollama/test-chunk4-mean@1" {
		t.Fatal("default ID reused unchunked vector space")
	}
}

func TestChunkingAdaptsOnlyContextErrors(t *testing.T) {
	for _, status := range []int{400, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var accepted []string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Input []string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for _, text := range body.Input {
					if len(text) > 4 || status == 503 {
						w.WriteHeader(status)
						w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
						return
					}
				}
				accepted = append(accepted, body.Input...)
				vectors := make([][]float32, len(body.Input))
				for i := range vectors {
					vectors[i] = []float32{1, 2}
				}
				json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
			}))
			defer server.Close()
			e, err := New(Options{BaseURL: server.URL, Model: "test", Dims: 2, ChunkSize: 16})
			if err != nil {
				t.Fatal(err)
			}
			vectors, _, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "abcd界🙂ef"}})
			if status == 503 {
				if err == nil || calls != 1 {
					t.Fatalf("unrelated failure retried: %d %v", calls, err)
				}
				return
			}
			if err != nil || len(vectors) != 1 || strings.Join(accepted, "") != "abcd界🙂ef" {
				t.Fatalf("adaptive splitting lost input: %q %v", accepted, err)
			}
		})
	}
}

func TestChunkingMinimalFailureAndValidation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
	}))
	defer server.Close()
	e, err := New(Options{BaseURL: server.URL, Model: "test", Dims: 2, ChunkSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "🙂"}}); err == nil || calls != 1 {
		t.Fatalf("minimal chunk retry loop: %d %v", calls, err)
	}
	for _, options := range []Options{{Model: "test", Dims: 2, ChunkSize: 1}, {Model: "test", Dims: 2, ChunkSize: -1}, {Model: "test", Dims: 2, ChunkSize: 4, Truncate: true}} {
		if _, err := New(options); err == nil {
			t.Fatal("invalid chunk configuration accepted")
		}
	}
	if _, _, err := e.Embed(context.Background(), embed.Document, []embed.Input{{Text: string([]byte{255})}}); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.Embed(ctx, embed.Query, []embed.Input{{Text: "hello"}}); err == nil {
		t.Fatal("cancellation lost")
	}
}

func TestLocalBGEChunkedEmbedding(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EMBEDDING") != "1" {
		t.Skip("opt-in local embedding fixture")
	}
	e, err := New(Options{Model: "qllama/bge-small-en-v1.5", Dims: 384, ChunkSize: 1024, QueryPrefix: "Represent this sentence for searching relevant passages: ", KeepAlive: "10m"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, purpose := range []embed.Purpose{embed.Query, embed.Document} {
		text := strings.Repeat("Atlas caches data in Redis. ", 240) + strings.Repeat("界🙂 café ", 80)
		raw := *e
		raw.o.ChunkSize = 0
		if _, _, err := raw.Embed(ctx, purpose, []embed.Input{{Text: text}}); !errors.Is(err, errContextLength) {
			t.Fatalf("oversized fixture did not reproduce context rejection: %v", err)
		}
		start := time.Now()
		vectors, usage, err := e.Embed(ctx, purpose, []embed.Input{{Text: text}, {Text: "short text"}})
		if err != nil || len(vectors) != 2 || len(vectors[0]) != 384 {
			t.Fatalf("long BGE input failed: %v", err)
		}
		var norm float64
		for _, v := range vectors[0] {
			norm += float64(v) * float64(v)
		}
		if math.Abs(norm-1) > 1e-5 {
			t.Fatalf("combined vector norm=%f", norm)
		}
		t.Logf("%v: %d source bytes, 2 vectors of 384 dimensions, tokens=%d, %s", purpose, len(text), usage.Tokens, time.Since(start).Round(time.Millisecond))
	}
}
