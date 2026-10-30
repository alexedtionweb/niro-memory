// Package embed defines the optional Embedder connector interface and a
// deterministic hashing embedder for tests and offline development.
//
// The engine never requires an embedder: callers may supply vectors
// themselves. When one is configured, it fills missing vectors in background
// jobs and embeds query text within a time budget.
package embed

import (
	"context"
	"hash/fnv"
	"strings"
	"unicode"

	"github.com/alexedtionweb/niro-memory/vector"
)

// Purpose tells the embedder how an input will be used; some models embed
// documents and queries differently.
type Purpose string

// Purposes.
const (
	Document Purpose = "document"
	Query    Purpose = "query"
)

// Input is one thing to embed: text, or binary media for multimodal models.
type Input struct {
	Modality string // "text", "image", "audio", "video"
	Text     string
	Data     []byte
	Mime     string
}

// Usage reports what an embedding call cost.
type Usage struct {
	Inputs int
	Tokens int
}

// Embedder computes vectors for one model.
type Embedder interface {
	Model() vector.Model
	Embed(ctx context.Context, p Purpose, in []Input) ([][]float32, Usage, error)
}

// Hash is a deterministic bag-of-words embedder (feature hashing of words and
// character trigrams). It has no semantic understanding; it exists so tests
// and offline demos exercise the vector path without any model.
type Hash struct{ Dims int }

// Model describes the hashing space.
func (h Hash) Model() vector.Model {
	return vector.Model{ID: "hash@1", Dims: h.Dims, Metric: "cosine", Modalities: []string{"text"}, Active: true}
}

// Embed hashes each text input; non-text inputs get the hash of their bytes.
func (h Hash) Embed(_ context.Context, _ Purpose, in []Input) ([][]float32, Usage, error) {
	out := make([][]float32, len(in))
	for i, x := range in {
		v := make([]float32, h.Dims)
		add := func(s string, w float32) {
			f := fnv.New32a()
			f.Write([]byte(s))
			n := f.Sum32()
			sign := float32(1)
			if n&1 == 1 {
				sign = -1
			}
			v[int(n>>1)%h.Dims] += sign * w
		}
		text := strings.ToLower(x.Text)
		if x.Text == "" {
			text = string(x.Data)
		}
		for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			add(w, 1)
			for j := 0; j+3 <= len(w); j++ {
				add("#"+w[j:j+3], 0.3)
			}
		}
		if err := vector.Normalize(v); err != nil {
			v[0] = 1 // empty input: arbitrary unit vector
		}
		out[i] = v
	}
	return out, Usage{Inputs: len(in)}, nil
}
