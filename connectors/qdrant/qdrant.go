// Package qdrant is a vector.Index connector for Qdrant over its REST API,
// using only the standard library.
//
// One collection per embedding model (Qdrant fixes dimensions per
// collection), cosine distance, and a keyword payload index on the tenant
// marked is_tenant so Qdrant co-locates each tenant's vectors. Point ids
// are UUIDs derived from tenant and target, so upserts are idempotent.
// The engine re-checks every hit against the primary store, so the index
// may lag or be rebuilt at any time.
package qdrant

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-memory/vector"
)

// Options configures the client.
type Options struct {
	URL    string // e.g. http://localhost:6333
	APIKey string // sent as the api-key header
	// Prefix names collections: Prefix + sanitized model id (default "niromem_").
	Prefix string
	HTTP   *http.Client
}

// Index is a Qdrant-backed vector.Index.
type Index struct {
	o       Options
	mu      sync.Mutex
	ensured map[string]bool
}

var _ vector.Index = (*Index)(nil)

// New returns an index. It performs no I/O; collections are created on
// first write.
func New(o Options) *Index {
	if o.Prefix == "" {
		o.Prefix = "niromem_"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	o.URL = strings.TrimRight(o.URL, "/")
	return &Index{o: o, ensured: map[string]bool{}}
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (x *Index) collection(model string) string {
	return x.o.Prefix + strings.Trim(unsafe.ReplaceAllString(model, "_"), "_")
}

// pointID is a name-based (SHA-1) UUID of tenant, model and target.
func pointID(tenant, model string, t vector.Target) string {
	sum := sha1.Sum([]byte(tenant + "\x00" + model + "\x00" + t.Key()))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// APIError is a non-2xx response from Qdrant.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("qdrant: %d %s", e.Status, e.Body) }

func status(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (x *Index) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.o.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if x.o.APIKey != "" {
		req.Header.Set("api-key", x.o.APIKey)
	}
	resp, err := x.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{resp.StatusCode, string(b)}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ensure creates the model's collection and payload indexes once per process.
func (x *Index) ensure(ctx context.Context, model string, dims int) error {
	c := x.collection(model)
	x.mu.Lock()
	done := x.ensured[c]
	x.mu.Unlock()
	if done {
		return nil
	}
	err := x.call(ctx, "PUT", "/collections/"+c, map[string]any{
		"vectors": map[string]any{"size": dims, "distance": "Cosine"},
	}, nil)
	if st := status(err); st == http.StatusConflict || (st == http.StatusBadRequest && strings.Contains(err.Error(), "already exists")) {
		err = nil
	}
	if err != nil {
		return err
	}
	for field, schema := range map[string]any{
		"tenant": map[string]any{"type": "keyword", "is_tenant": true},
		"user":   "keyword", "agent": "keyword", "run": "keyword", "kind": "keyword", "key": "keyword",
	} {
		if err := x.call(ctx, "PUT", "/collections/"+c+"/index?wait=true", map[string]any{"field_name": field, "field_schema": schema}, nil); err != nil {
			return err
		}
	}
	x.mu.Lock()
	x.ensured[c] = true
	x.mu.Unlock()
	return nil
}

type point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// Upsert writes points grouped by model and waits until they are searchable.
func (x *Index) Upsert(ctx context.Context, points []vector.Point) error {
	byModel := map[string][]point{}
	dims := map[string]int{}
	for _, p := range points {
		v := append([]float32(nil), p.Values...)
		if err := vector.Normalize(v); err != nil {
			return err
		}
		byModel[p.Model] = append(byModel[p.Model], point{ID: pointID(p.Tenant, p.Model, p.Target), Vector: v, Payload: map[string]any{
			"tenant": p.Tenant, "user": p.User, "agent": p.Agent, "run": p.Run, "kind": string(p.Target.Kind),
			"id": p.Target.ID, "part": p.Target.Part, "key": p.Target.Key(), "modality": p.Modality,
		}})
		dims[p.Model] = len(v)
	}
	for model, pts := range byModel {
		if err := x.ensure(ctx, model, dims[model]); err != nil {
			return err
		}
		for start := 0; start < len(pts); start += 256 {
			end := min(start+256, len(pts))
			if err := x.call(ctx, "PUT", "/collections/"+x.collection(model)+"/points?wait=true", map[string]any{"points": pts[start:end]}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func match(key, value string) map[string]any {
	return map[string]any{"key": key, "match": map[string]any{"value": value}}
}

// Delete removes the targets of a tenant from every collection of this prefix.
func (x *Index) Delete(ctx context.Context, tenant string, targets []vector.Target) error {
	if len(targets) == 0 {
		return nil
	}
	keys := make([]string, len(targets))
	for i, t := range targets {
		keys[i] = t.Key()
	}
	var list struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if err := x.call(ctx, "GET", "/collections", nil, &list); err != nil {
		return err
	}
	for _, c := range list.Result.Collections {
		if !strings.HasPrefix(c.Name, x.o.Prefix) {
			continue
		}
		filter := map[string]any{"must": []any{match("tenant", tenant), map[string]any{"key": "key", "match": map[string]any{"any": keys}}}}
		if err := x.call(ctx, "POST", "/collections/"+c.Name+"/points/delete?wait=true", map[string]any{"filter": filter}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Search finds the nearest points of a tenant and model.
func (x *Index) Search(ctx context.Context, q vector.Query) ([]vector.Hit, error) {
	v := append([]float32(nil), q.Values...)
	if err := vector.Normalize(v); err != nil {
		return nil, err
	}
	must := []any{match("tenant", q.Tenant)}
	for k, val := range map[string]string{"user": q.User, "agent": q.Agent, "run": q.Run, "kind": string(q.Kind)} {
		if val != "" {
			must = append(must, match(k, val))
		}
	}
	var out struct {
		Result []struct {
			Score   float64 `json:"score"`
			Payload struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
				Part int    `json:"part"`
			} `json:"payload"`
		} `json:"result"`
	}
	err := x.call(ctx, "POST", "/collections/"+x.collection(q.Model)+"/points/search", map[string]any{
		"vector": v, "limit": q.K, "with_payload": []string{"kind", "id", "part"}, "filter": map[string]any{"must": must},
	}, &out)
	if status(err) == http.StatusNotFound {
		return nil, nil // nothing indexed for this model yet
	}
	if err != nil {
		return nil, err
	}
	hits := make([]vector.Hit, len(out.Result))
	for i, r := range out.Result {
		hits[i] = vector.Hit{Target: vector.Target{Kind: vector.TargetKind(r.Payload.Kind), ID: r.Payload.ID, Part: r.Payload.Part}, Score: r.Score}
	}
	return hits, nil
}
