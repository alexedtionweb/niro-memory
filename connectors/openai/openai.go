// Package openai is an embed.Embedder connector for any OpenAI-compatible
// embeddings endpoint (OpenAI, Azure OpenAI, Ollama, vLLM, LM Studio,
// LiteLLM, Hugging Face TEI, …), using only the standard library.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Options configures the embedder.
type Options struct {
	BaseURL string // default https://api.openai.com/v1
	APIKey  string
	Model   string // provider model name, e.g. text-embedding-3-small
	Dims    int    // vector dimensions the model returns
	// ID names the embedding space in niromem (default "openai/<Model>@1").
	// Change the revision suffix when the provider changes the model.
	ID string
	// SendDimensions passes Dims to the API (models that support truncation).
	SendDimensions bool
	// QueryPrefix and DocumentPrefix are prepended to inputs, for models
	// trained with instructions such as "query: " / "passage: ".
	QueryPrefix, DocumentPrefix string
	BatchSize                   int // inputs per request (default 128)
	MaxRetries                  int // on 429 and 5xx (default 3)
	HTTP                        *http.Client
	// Header adds headers to every request (e.g. Azure "api-key").
	Header http.Header
}

// Embedder calls an OpenAI-compatible /embeddings endpoint.
type Embedder struct{ o Options }

var _ embed.Embedder = (*Embedder)(nil)

// New validates options.
func New(o Options) (*Embedder, error) {
	if o.Model == "" || o.Dims <= 0 {
		return nil, fmt.Errorf("openai: model and dims are required")
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.openai.com/v1"
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.ID == "" {
		o.ID = "openai/" + o.Model + "@1"
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 128
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = 3
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return &Embedder{o}, nil
}

// Model describes the embedding space.
func (e *Embedder) Model() vector.Model {
	return vector.Model{ID: e.o.ID, Dims: e.o.Dims, Metric: "cosine", Modalities: []string{"text"}, Active: true}
}

// Embed embeds text inputs in batches, retrying rate limits and server errors.
func (e *Embedder) Embed(ctx context.Context, p embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	prefix := e.o.DocumentPrefix
	if p == embed.Query {
		prefix = e.o.QueryPrefix
	}
	out := make([][]float32, 0, len(in))
	usage := embed.Usage{Inputs: len(in)}
	for start := 0; start < len(in); start += e.o.BatchSize {
		batch := in[start:min(start+e.o.BatchSize, len(in))]
		texts := make([]string, len(batch))
		for i, x := range batch {
			if x.Modality != "" && x.Modality != "text" {
				return nil, usage, fmt.Errorf("openai: modality %q not supported", x.Modality)
			}
			texts[i] = prefix + x.Text
		}
		vs, tokens, err := e.call(ctx, texts)
		if err != nil {
			return nil, usage, err
		}
		usage.Tokens += tokens
		out = append(out, vs...)
	}
	return out, usage, nil
}

func (e *Embedder) call(ctx context.Context, texts []string) ([][]float32, int, error) {
	body := map[string]any{"model": e.o.Model, "input": texts, "encoding_format": "float"}
	if e.o.SendDimensions {
		body["dimensions"] = e.o.Dims
	}
	payload, _ := json.Marshal(body)
	var lastErr error
	for attempt := 0; attempt <= e.o.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", e.o.BaseURL+"/embeddings", bytes.NewReader(payload))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		if e.o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+e.o.APIKey)
		}
		for k, v := range e.o.Header {
			req.Header[k] = v
		}
		resp, err := e.o.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var res struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			lastErr = fmt.Errorf("openai: %s: %s", resp.Status, b)
			continue
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return nil, 0, fmt.Errorf("openai: %s: %s", resp.Status, b)
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		if len(res.Data) != len(texts) {
			return nil, 0, fmt.Errorf("openai: %d embeddings for %d inputs", len(res.Data), len(texts))
		}
		out := make([][]float32, len(texts))
		for _, d := range res.Data {
			if d.Index < 0 || d.Index >= len(out) || len(d.Embedding) != e.o.Dims {
				return nil, 0, fmt.Errorf("%w: openai returned index %d with %d dims, want %d", vector.ErrDims, d.Index, len(d.Embedding), e.o.Dims)
			}
			if out[d.Index] != nil {
				return nil, 0, fmt.Errorf("openai: duplicate embedding index %d", d.Index)
			}
			out[d.Index] = d.Embedding
		}
		return out, res.Usage.PromptTokens, nil
	}
	return nil, 0, lastErr
}
