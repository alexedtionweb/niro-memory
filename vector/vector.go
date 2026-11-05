// Package vector defines optional vector search: points, queries, the Index
// connector interface, and a brute-force in-process index.
//
// Vectors are always bring-your-own: callers attach them to memories or
// episode parts (any modality), or an embed.Embedder computes them in the
// background. The primary store keeps every vector as the source of truth;
// an Index is a rebuildable projection used for search.
package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Model describes one embedding space of a tenant. Vectors are only compared
// with vectors of the same model, which guarantees dimension compatibility.
type Model struct {
	ID         string   `json:"id"`   // e.g. "openai/text-embedding-3-small@1"
	Dims       int      `json:"dims"` // e.g. 1536
	Metric     string   `json:"metric,omitempty"`
	Modalities []string `json:"modalities,omitempty"` // text, image, audio, video
	Active     bool     `json:"active"`               // used for text queries by default
}

// TargetKind says what a vector belongs to.
type TargetKind string

// Target kinds.
const (
	TargetMemory TargetKind = "m" // a memory
	TargetPart   TargetKind = "p" // one part of an episode (image, audio, …)
)

// Target identifies the owner of a vector.
type Target struct {
	Kind TargetKind `json:"kind"`
	ID   string     `json:"id"`             // memory id or episode id
	Part int        `json:"part,omitempty"` // part index for TargetPart
}

// Key is a stable string form of the target, unique within a tenant and model.
func (t Target) Key() string {
	if t.Kind == TargetPart {
		return fmt.Sprintf("p:%s:%d", t.ID, t.Part)
	}
	return "m:" + t.ID
}

// Point is one stored vector with the scope fields indexes may prefilter on.
// Hits are always re-checked against the primary store, so these fields are
// a performance hint, not a security boundary.
type Point struct {
	Tenant   string    `json:"tenant"`
	Model    string    `json:"model"`
	Target   Target    `json:"target"`
	Modality string    `json:"modality,omitempty"`
	User     string    `json:"user_id,omitempty"`
	Agent    string    `json:"agent_id,omitempty"`
	Run      string    `json:"run_id,omitempty"`
	Values   []float32 `json:"values"`
}

// Query is a nearest-neighbour search inside one tenant and model.
// Empty User/Agent/Run match any value.
type Query struct {
	Tenant string
	Model  string
	Values []float32
	K      int
	User   string
	Agent  string
	Run    string
	Kind   TargetKind // "" = memories and parts
}

// Hit is a search result. Score is cosine similarity in [-1, 1].
type Hit struct {
	Target Target  `json:"target"`
	Score  float64 `json:"score"`
}

// Index is the connector interface for vector search backends
// (flat scan, pgvector, Qdrant, OpenSearch, S3 Vectors, …).
type Index interface {
	Upsert(ctx context.Context, points []Point) error
	Delete(ctx context.Context, tenant string, targets []Target) error
	Search(ctx context.Context, q Query) ([]Hit, error)
}

// TenantPurger removes all points of one tenant without enumerating targets.
type TenantPurger interface {
	PurgeTenant(context.Context, string) error
}

// ErrDims is returned when a vector does not match its model's dimensions.
var ErrDims = errors.New("vector: dimension mismatch")

// Normalize scales v to unit length in place so that cosine similarity is a
// plain dot product. Zero and non-finite vectors are rejected.
func Normalize(v []float32) error {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return errors.New("vector: zero or non-finite vector")
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return nil
}

// Dot returns the dot product of two vectors over their common length.
func Dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for ; i+4 <= n; i += 4 { // four accumulators let the CPU pipeline the adds
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}
