package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alexedtionweb/niro-memory/connectors/openai"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/vector"
)

func TestEmbedder(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", 429) // first call is rate limited
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/v1/embeddings" {
			http.Error(w, "bad request", 400)
			return
		}
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
			Dims  int      `json:"dimensions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(in.Input) - 1; i >= 0; i-- { // out of order on purpose
			data = append(data, item{i, []float32{float32(len(in.Input[i])), 1, float32(in.Dims)}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"prompt_tokens": 7}})
	}))
	defer srv.Close()
	e, err := openai.New(openai.Options{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m", Dims: 3, SendDimensions: true, BatchSize: 2, QueryPrefix: "q: "})
	if err != nil {
		t.Fatal(err)
	}
	if e.Model().ID != "openai/m@1" {
		t.Fatal(e.Model().ID)
	}
	vs, u, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "a"}, {Text: "bb"}, {Text: "ccc"}})
	if err != nil || len(vs) != 3 || u.Tokens != 14 {
		t.Fatalf("%v %+v %v", vs, u, err)
	}
	if vs[0][0] != 4 || vs[1][0] != 5 || vs[2][0] != 6 || vs[0][2] != 3 { // "q: " prefix, order restored, dims sent
		t.Fatalf("%v", vs)
	}
	bad, _ := openai.New(openai.Options{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m", Dims: 4})
	if _, _, err := bad.Embed(context.Background(), embed.Document, []embed.Input{{Text: "x"}}); !errors.Is(err, vector.ErrDims) {
		t.Fatalf("dimension mismatch must fail: %v", err)
	}
}
