package memstore

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func cloneLink(r relations.Link) relations.Link {
	raw, _ := json.Marshal(r)
	var out relations.Link
	_ = json.Unmarshal(raw, &out)
	return out
}

func cloneReceipt(r *store.Receipt) *store.Receipt {
	raw, _ := store.EncodeReceipt(r)
	out, _ := store.DecodeReceipt(raw)
	return out
}

func (s *Store) Operation(_ context.Context, tenant, key string) (*store.Receipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t := s.tenants[tenant]; t != nil {
		if r := t.operations[key]; r != nil {
			return cloneReceipt(r), nil
		}
	}
	return nil, memory.ErrNotFound
}

func (s *Store) RelationshipSets(_ context.Context, tenant string, anchors []string) ([]relations.Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wanted := map[string]bool{}
	for _, id := range anchors {
		wanted[id] = true
	}
	out := []relations.Link{}
	if t := s.tenants[tenant]; t != nil {
		for _, r := range t.relationships {
			if wanted[r.Source] || wanted[r.Target] {
				out = append(out, cloneLink(r))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > relations.MaxReadLinks {
		out = out[:relations.MaxReadLinks]
	}
	return out, nil
}
