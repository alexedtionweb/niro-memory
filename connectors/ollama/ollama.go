// Package ollama implements the optional embedding connector for Ollama's
// native /api/embed endpoint, using only the standard library.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Options configures an embedding space. The caller installs the model;
// this connector never downloads one or starts an Ollama process.
type Options struct {
	BaseURL                     string // default http://127.0.0.1:11434
	APIKey                      string
	Model                       string
	Dims                        int
	ID                          string // default ollama/<Model>@1
	QueryPrefix, DocumentPrefix string
	BatchSize                   int // default 32
	KeepAlive                   string
	Truncate                    bool // default false: reject oversized input rather than silently losing text
	SendDimensions              bool
	HTTP                        *http.Client
}

// Embedder embeds queries and documents in the same declared model space.
type Embedder struct{ o Options }

var _ embed.Embedder = (*Embedder)(nil)

// New validates configuration without making network calls.
func New(o Options) (*Embedder, error) {
	if strings.TrimSpace(o.Model) == "" || o.Dims <= 0 {
		return nil, fmt.Errorf("ollama: model and positive dims are required")
	}
	if o.BaseURL == "" {
		o.BaseURL = "http://127.0.0.1:11434"
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ollama: base_url must be an HTTP(S) base URL without query or fragment")
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.ID == "" {
		o.ID = "ollama/" + o.Model + "@1"
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 32
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return &Embedder{o: o}, nil
}

// Model describes the embedding space.
func (e *Embedder) Model() vector.Model {
	return vector.Model{ID: e.o.ID, Dims: e.o.Dims, Metric: "cosine", Modalities: []string{"text"}, Active: true}
}

// Embed uses the native batch API. Cancellation and server errors propagate;
// the engine's durable embedding jobs supply retries for document writes.
func (e *Embedder) Embed(ctx context.Context, purpose embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	usage := embed.Usage{Inputs: len(in)}
	prefix := e.o.DocumentPrefix
	if purpose == embed.Query {
		prefix = e.o.QueryPrefix
	}
	for _, input := range in {
		if (input.Modality != "" && input.Modality != "text") || len(input.Data) != 0 {
			return nil, usage, fmt.Errorf("ollama: only text embedding inputs are supported")
		}
	}
	out := make([][]float32, 0, len(in))
	for start := 0; start < len(in); start += e.o.BatchSize {
		batch := in[start:min(start+e.o.BatchSize, len(in))]
		texts := make([]string, len(batch))
		for i, input := range batch {
			texts[i] = prefix + input.Text
		}
		body := map[string]any{"model": e.o.Model, "input": texts, "truncate": e.o.Truncate}
		if e.o.KeepAlive != "" {
			body["keep_alive"] = e.o.KeepAlive
		}
		if e.o.SendDimensions {
			body["dimensions"] = e.o.Dims
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, usage, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.o.BaseURL+"/api/embed", bytes.NewReader(payload))
		if err != nil {
			return nil, usage, err
		}
		req.Header.Set("Content-Type", "application/json")
		if e.o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+e.o.APIKey)
		}
		resp, err := e.o.HTTP.Do(req)
		if err != nil {
			return nil, usage, fmt.Errorf("ollama: embed request: %w", err)
		}
		var result struct {
			Embeddings [][]float32 `json:"embeddings"`
			Tokens     int         `json:"prompt_eval_count"`
			Error      string      `json:"error"`
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if readErr != nil {
				return nil, usage, fmt.Errorf("ollama: read error response: %w", readErr)
			}
			return nil, usage, fmt.Errorf("ollama: %s: %s", resp.Status, raw)
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, usage, fmt.Errorf("ollama: decode embeddings: %w", err)
		}
		if result.Error != "" {
			return nil, usage, fmt.Errorf("ollama: %s", result.Error)
		}
		if len(result.Embeddings) != len(batch) {
			return nil, usage, fmt.Errorf("ollama: %d embeddings for %d inputs", len(result.Embeddings), len(batch))
		}
		for _, v := range result.Embeddings {
			if len(v) != e.o.Dims {
				return nil, usage, fmt.Errorf("%w: ollama returned %d dims, want %d", vector.ErrDims, len(v), e.o.Dims)
			}
			var norm float64
			for _, value := range v {
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
					return nil, usage, fmt.Errorf("ollama: non-finite embedding value")
				}
				norm += float64(value) * float64(value)
			}
			if norm == 0 {
				return nil, usage, fmt.Errorf("ollama: returned a zero embedding")
			}
		}
		usage.Tokens += result.Tokens
		out = append(out, result.Embeddings...)
	}
	return out, usage, nil
}
