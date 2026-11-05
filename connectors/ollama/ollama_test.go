package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/connectors/ollama"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/vector"
)

func TestLocalEmbeddingAPI(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EMBEDDING") != "1" {
		t.Skip("set NIROMEM_TEST_LOCAL_EMBEDDING=1 to call local Ollama with generated fixtures")
	}
	e, err := ollama.New(ollama.Options{Model: "nomic-embed-text", Dims: 768, ID: "ollama/nomic-embed-text@1",
		QueryPrefix: "search_query: ", DocumentPrefix: "search_document: ", KeepAlive: "10m", Timeout: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sample := range []struct {
		name    string
		purpose embed.Purpose
		inputs  []embed.Input
	}{
		{"query", embed.Query, []embed.Input{{Text: "generated embedding latency check"}}},
		{"documents", embed.Document, []embed.Input{{Text: "Atlas uses Redis."}, {Text: "Bruna trabalha na Contoso."}}},
	} {
		start := time.Now()
		vectors, _, err := e.Embed(ctx, sample.purpose, sample.inputs)
		if err != nil || len(vectors) != len(sample.inputs) {
			t.Fatalf("%s embeddings: count=%d error=%v", sample.name, len(vectors), err)
		}
		for _, v := range vectors {
			if len(v) != 768 {
				t.Fatalf("embedding dimensions=%d want 768", len(v))
			}
		}
		t.Logf("%s: %d vectors, 768 dimensions, %s", sample.name, len(vectors), time.Since(start).Round(time.Millisecond))
	}
}

func TestBatchAndPrefixes(t *testing.T) {
	var inputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/embed" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			Truncate  bool     `json:"truncate"`
			KeepAlive string   `json:"keep_alive"`
			Dims      int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "test" || body.Truncate || body.KeepAlive != "10m" || body.Dims != 2 || len(body.Input) > 2 {
			t.Errorf("unexpected body: %+v", body)
		}
		inputs = append(inputs, body.Input...)
		vs := make([][]float32, len(body.Input))
		for i, text := range body.Input {
			vs[i] = []float32{float32(len(text)), 1}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vs, "prompt_eval_count": len(body.Input)})
	}))
	defer srv.Close()
	e, err := ollama.New(ollama.Options{
		BaseURL: srv.URL, APIKey: "test-key", Model: "test", Dims: 2, BatchSize: 2,
		QueryPrefix: "query: ", DocumentPrefix: "passage: ", KeepAlive: "10m", SendDimensions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Model().ID != "ollama/test@1" || e.Model().Dims != 2 {
		t.Fatalf("unexpected model: %+v", e.Model())
	}
	vs, usage, err := e.Embed(context.Background(), embed.Document, []embed.Input{{Text: "a"}, {Text: "bb"}, {Text: "ccc"}})
	if err != nil || len(vs) != 3 || usage.Inputs != 3 || usage.Tokens != 3 {
		t.Fatalf("batch failed: %v %+v %v", vs, usage, err)
	}
	if !reflect.DeepEqual(inputs, []string{"passage: a", "passage: bb", "passage: ccc"}) {
		t.Fatalf("wrong document prefix/order: %v", inputs)
	}
	if _, _, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "question"}}); err != nil {
		t.Fatal(err)
	}
	if inputs[len(inputs)-1] != "query: question" {
		t.Fatalf("wrong query prefix: %v", inputs)
	}
}

func TestInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		dims       bool
	}{
		{"dimensions", `{"embeddings":[[1,2,3]]}`, 200, true},
		{"missing", `{"embeddings":[]}`, 200, false},
		{"zero", `{"embeddings":[[0,0]]}`, 200, false},
		{"malformed", `{`, 200, false},
		{"server-error", `{"error":"unavailable"}`, 503, false},
		{"model-error", `{"error":"model not found"}`, 404, false},
		{"inline-error", `{"error":"failure"}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			e, err := ollama.New(ollama.Options{BaseURL: srv.URL, Model: "test", Dims: 2})
			if err != nil {
				t.Fatal(err)
			}
			vs, _, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "x"}})
			if err == nil || vs != nil {
				t.Fatalf("invalid response returned success: %v %v", vs, err)
			}
			if tc.dims && !errors.Is(err, vector.ErrDims) {
				t.Fatalf("wrong dimension error: %v", err)
			}
		})
	}
}

func TestInputAndCancellation(t *testing.T) {
	e, err := ollama.New(ollama.Options{Model: "test", Dims: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Embed(context.Background(), embed.Document, []embed.Input{{Modality: "image"}}); err == nil {
		t.Fatal("image accepted")
	}
	if _, _, err := e.Embed(context.Background(), embed.Document, []embed.Input{{Data: []byte("binary")}}); err == nil {
		t.Fatal("binary input accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.Embed(ctx, embed.Query, []embed.Input{{Text: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	vs, usage, err := e.Embed(context.Background(), embed.Query, nil)
	if err != nil || len(vs) != 0 || usage.Inputs != 0 {
		t.Fatalf("empty batch: %v %+v %v", vs, usage, err)
	}
	for _, opts := range []ollama.Options{{}, {Model: "x"}, {Model: "x", Dims: 2, BaseURL: "ftp://localhost"}} {
		if _, err := ollama.New(opts); err == nil {
			t.Fatalf("invalid config accepted: %+v", opts)
		}
	}
}
