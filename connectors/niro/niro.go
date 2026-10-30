// Package niro adapts the engine to the Niro runtime
// (github.com/alexedtionweb/niro-stream): Store implements niro's
// memory.Store, so Niro pipelines, views and plans can keep their records
// in niromem — durable, searchable, multi-tenant and shared by every node.
//
// A Niro record becomes a conversation turn: Scope is the run, Source the
// role (or the actor of an "event" turn when it is not a role), Body the
// text, Ref a part URL, and Tags/Meta turn metadata. Turns then flow
// through the normal pipeline (rules, extensions) into memories.
package niro

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	nmem "github.com/alexedtionweb/niro-stream/memory"
)

// Store is a niro memory.Store backed by a niromem Engine.
type Store struct {
	Engine *niromem.Engine
	// Tenant, User and Agent own every record written through this store.
	Tenant, User, Agent string
	// MaxRecall bounds how many turns one Recall reads per scope (default 1000).
	MaxRecall int
}

var _ nmem.Store = (*Store)(nil)

var roles = map[string]bool{"user": true, "assistant": true, "system": true, "tool": true, "event": true}

// Put appends a record as a turn. A record whose ID already exists in the
// scope replaces that turn (edit), as niro's in-process store does.
func (s *Store) Put(ctx context.Context, rec nmem.Record) (nmem.Record, error) {
	if rec.Body == "" && rec.Ref == "" {
		return nmem.Record{}, fmt.Errorf("%w: record is empty", memory.ErrInvalid)
	}
	if rec.ID == "" {
		rec.ID = memory.NewID()
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	role, actor := rec.Source, ""
	if !roles[role] {
		role, actor = "event", rec.Source
	}
	parts := s.parts(rec)
	meta := map[string]any{"source": rec.Source}
	if len(rec.Tags) > 0 {
		meta["tags"] = rec.Tags
	}
	for k, v := range rec.Meta {
		meta["meta."+k] = v
	}
	res, err := s.Engine.Append(ctx, memory.Scope{Tenant: s.Tenant, User: s.User, Agent: s.Agent, Run: s.scope(rec.Scope)},
		[]niromem.Turn{{ClientMsgID: rec.ID, Role: role, Actor: actor, Parts: parts, OccurredAt: rec.At, Metadata: meta}}, niromem.AppendOptions{})
	if err != nil {
		return nmem.Record{}, err
	}
	if res.Duplicates[0] {
		if err := s.Engine.EditTurn(ctx, s.Tenant, res.Episodes[0].ID, parts); err != nil {
			return nmem.Record{}, err
		}
	}
	return rec, nil
}

func (s *Store) parts(rec nmem.Record) []niromem.PartInput {
	var ps []niromem.PartInput
	if rec.Body != "" {
		ps = append(ps, niromem.PartInput{Part: memory.Part{Kind: memory.PartText, Text: rec.Body}})
	}
	if rec.Ref != "" {
		ps = append(ps, niromem.PartInput{Part: memory.Part{Kind: memory.PartFile, URL: rec.Ref}})
	}
	return ps
}

// scope maps niro's scope to a run id ("" is a valid niro scope).
func (s *Store) scope(sc string) string { return runPrefix + sc }

// Recall returns matching records, oldest first; Limit keeps the newest.
func (s *Store) Recall(ctx context.Context, q nmem.Query) ([]nmem.Record, error) {
	ms, err := s.match(ctx, q)
	if err != nil {
		return nil, err
	}
	if q.Limit > 0 && len(ms) > q.Limit {
		ms = ms[len(ms)-q.Limit:]
	}
	out := make([]nmem.Record, len(ms))
	for i, m := range ms {
		out[i] = m.rec
	}
	return out, nil
}

// Forget deletes matching records (as turn tombstones) and returns how many.
func (s *Store) Forget(ctx context.Context, q nmem.Query) (int, error) {
	ms, err := s.match(ctx, q)
	if err != nil {
		return 0, err
	}
	for _, m := range ms {
		if err := s.Engine.DeleteTurn(ctx, s.Tenant, m.episode); err != nil {
			return 0, err
		}
	}
	return len(ms), nil
}

type matched struct {
	rec     nmem.Record
	episode string
}

const runPrefix = "niro:"

// match reads turns of the query's scope (or of every niro scope) and
// applies the niro predicate.
func (s *Store) match(ctx context.Context, q nmem.Query) ([]matched, error) {
	max := s.MaxRecall
	if max <= 0 {
		max = 1000
	}
	var eps []memory.Episode
	if q.AnyScope {
		hits, err := s.Engine.SearchHistory(ctx, niromem.HistoryQuery{Tenant: s.Tenant, User: s.User, Agent: s.Agent, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if strings.HasPrefix(h.Episode.Run, runPrefix) {
				eps = append(eps, h.Episode)
			}
		}
	} else {
		var err error
		if eps, err = s.Engine.Turns(ctx, niromem.TurnsRequest{Tenant: s.Tenant, Run: s.scope(q.Scope), Last: max}); err != nil {
			return nil, err
		}
	}
	var out []matched
	for _, ep := range eps {
		if r := record(ep); q.Allows(r) && (q.ID == "" || q.ID == r.ID) {
			out = append(out, matched{r, ep.ID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rec.At.Before(out[j].rec.At) })
	return out, nil
}

func record(ep memory.Episode) nmem.Record {
	r := nmem.Record{ID: ep.ClientMsgID, Scope: strings.TrimPrefix(ep.Run, runPrefix), Source: ep.Role, At: ep.OccurredAt}
	if src, ok := ep.Metadata["source"].(string); ok {
		r.Source = src
	}
	for _, p := range ep.Parts {
		if p.Text != "" {
			r.Body = p.Text
		}
		if p.URL != "" {
			r.Ref = p.URL
		}
	}
	switch tags := ep.Metadata["tags"].(type) {
	case []string:
		r.Tags = append(r.Tags, tags...)
	case []any:
		for _, t := range tags {
			if s, ok := t.(string); ok {
				r.Tags = append(r.Tags, s)
			}
		}
	}
	for k, v := range ep.Metadata {
		if name, ok := strings.CutPrefix(k, "meta."); ok {
			if r.Meta == nil {
				r.Meta = map[string]string{}
			}
			r.Meta[name] = fmt.Sprint(v)
		}
	}
	return r
}
