package pipeline

import (
	"context"
	"errors"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store"
)

// Extractor is the extension point for AI (or any other) extraction. It
// receives new episodes of a run and proposes memories. Every proposal must
// carry Evidence; the pipeline validates and reconciles it like rule output.
type Extractor interface {
	Name() string
	Extract(ctx context.Context, run memory.Run, episodes []memory.Episode) ([]Proposal, error)
}

// Processor extracts memories from newly appended episodes of a run.
type Processor struct {
	Store      store.Store
	Reconciler *Reconciler
	// RulesFor returns the rules of a tenant, or nil for none.
	RulesFor   func(ctx context.Context, tenant string) (*rules.Set, error)
	Extractors []Extractor
	// OnWrite may add follow-up work (embedding, indexing jobs) to a batch
	// before it is written.
	OnWrite   func(b *store.Batch, changes []Change)
	BatchSize int // episodes per transaction (default 64)
}

// Stats summarizes a processing pass.
type Stats struct {
	Episodes int      `json:"episodes"`
	Changes  []Change `json:"changes,omitempty"`
}

const maxConflictRetries = 5

// ProcessRun consumes every episode after the run's cursor. Each slice of
// episodes is reconciled and committed together with the cursor advance, so
// a crash or a concurrent worker can never apply the same episode twice.
func (p *Processor) ProcessRun(ctx context.Context, tenant, run string) (Stats, error) {
	var st Stats
	size := p.BatchSize
	if size <= 0 {
		size = 64
	}
	for {
		n, changes, err := p.step(ctx, tenant, run, size)
		if err != nil {
			return st, err
		}
		st.Episodes += n
		st.Changes = append(st.Changes, changes...)
		if n < size {
			return st, nil
		}
	}
}

func (p *Processor) step(ctx context.Context, tenant, run string, size int) (int, []Change, error) {
	seen := int64(-1)
	for attempt := 0; ; attempt++ {
		state, err := p.Store.Run(ctx, tenant, run)
		if errors.Is(err, memory.ErrNotFound) {
			return 0, nil, nil
		}
		if err != nil {
			return 0, nil, err
		}
		if state.ProcessedSeq != seen { // another worker made progress: not a failure
			seen, attempt = state.ProcessedSeq, 0
		}
		eps, err := p.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, Run: run, AfterSeq: state.ProcessedSeq, Limit: size, IncludeDeleted: true})
		if err != nil || len(eps) == 0 {
			return 0, nil, err
		}
		props, err := p.propose(ctx, state, eps)
		if err != nil {
			return 0, nil, err
		}
		b := &store.Batch{Tenant: tenant, Cursors: []store.Cursor{{Run: run, From: state.ProcessedSeq, To: eps[len(eps)-1].Seq}}}
		changes, err := p.Reconciler.Plan(ctx, tenant, props, b)
		if err != nil {
			return 0, nil, err
		}
		if p.OnWrite != nil {
			p.OnWrite(b, changes)
		}
		_, err = p.Store.Write(ctx, b)
		if errors.Is(err, memory.ErrConflict) && attempt < maxConflictRetries {
			continue // another worker advanced the cursor or a slot changed; re-plan
		}
		return len(eps), changes, err
	}
}

// propose runs rules and extractors over episodes.
func (p *Processor) propose(ctx context.Context, run memory.Run, eps []memory.Episode) ([]Proposal, error) {
	var rs *rules.Set
	if p.RulesFor != nil {
		var err error
		if rs, err = p.RulesFor(ctx, run.Tenant); err != nil {
			return nil, err
		}
	}
	var props []Proposal
	live := eps[:0:0]
	for _, ep := range eps {
		if ep.Status == memory.Deleted {
			continue
		}
		live = append(live, ep)
		if rs != nil {
			props = append(props, FromRules(rs, ep)...)
		}
	}
	for _, x := range p.Extractors {
		if len(live) == 0 {
			break
		}
		got, err := x.Extract(ctx, run, live)
		if err != nil {
			return nil, err
		}
		for i := range got {
			got[i].Memory.Source.Kind = "ext:" + x.Name()
			if len(got[i].Memory.Source.Evidence) == 0 && !got[i].Invalidate {
				continue // ungrounded proposals are dropped
			}
			props = append(props, got[i])
		}
	}
	return props, nil
}

// FromRules converts the rule matches of one episode into proposals with
// provenance pointing at the exact part and byte span.
func FromRules(rs *rules.Set, ep memory.Episode) []Proposal {
	c := rules.Context{User: ep.User, Agent: ep.Agent, Actor: ep.Actor, Role: ep.Role, Observed: ep.OccurredAt}
	if c.Actor == "" {
		c.Actor = ep.Role
	}
	var out []Proposal
	for i, part := range ep.Parts {
		var cands []rules.Candidate
		switch part.Kind {
		case memory.PartToolCall, memory.PartToolResult, memory.PartData:
			if len(part.Data) > 0 {
				cands = rs.Structured(rules.Part{Name: part.Name, Data: part.Data}, c)
			}
			if part.Text != "" {
				cands = append(cands, rs.Text(part.Text, c)...)
			}
		default:
			for _, text := range []string{part.Text, part.Caption, part.DerivedText} {
				if text != "" {
					cands = append(cands, rs.Text(text, c)...)
				}
			}
		}
		for _, cd := range cands {
			out = append(out, proposal(rs.Version, ep, i, cd))
		}
	}
	return out
}

func proposal(version string, ep memory.Episode, part int, c rules.Candidate) Proposal {
	trust := memory.TrustNormal
	if ep.Role == "tool" || ep.Role == "event" || len(c.Flags) > 0 {
		trust = memory.TrustLow
	}
	m := memory.Memory{
		Scope: memory.Scope{Tenant: ep.Tenant, User: ep.User, Agent: ep.Agent, Run: ep.Run},
		Kind:  c.Kind, Layer: c.Layer, Title: c.Title, Text: c.Text,
		Subject: c.Subject, Predicate: c.Predicate, Object: c.Object, Slot: c.Slot,
		Tags: c.Tags, Entities: c.Entities, Confidence: c.Confidence, Importance: c.Importance,
		Visibility: c.Visibility, ValidFrom: c.ValidFrom, ExpiresAt: c.ExpiresAt,
		Source: memory.Source{Kind: "rules", Actor: actor(ep), Role: ep.Role, Trust: trust, DerivedBy: version + "/" + c.Rule,
			Flags:    c.Flags,
			Evidence: []memory.Evidence{{Episode: ep.ID, Part: part, Start: c.Start, End: c.End}}},
	}
	if m.ValidFrom.IsZero() {
		m.ValidFrom = ep.OccurredAt
	}
	return Proposal{Memory: m, Invalidate: c.Op == "invalidate", Match: c.Match}
}

func actor(ep memory.Episode) string {
	if ep.Actor != "" {
		return ep.Actor
	}
	return ep.Role
}

// Rederive handles an edited or deleted episode: memories derived only from
// it are invalidated, and (unless deleted) it is extracted again.
func (p *Processor) Rederive(ctx context.Context, tenant, episodeID string) ([]Change, error) {
	for attempt := 0; ; attempt++ {
		eps, err := p.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, IDs: []string{episodeID}, IncludeDeleted: true})
		if err != nil || len(eps) == 0 {
			return nil, err
		}
		ep := eps[0]
		derived, err := p.Store.Find(ctx, open(tenant, memory.And(memory.Eq("run_id", ep.Run), memory.Cmp("source", memory.OpNin, []any{"explicit", "import"}))))
		if err != nil {
			return nil, err
		}
		b := &store.Batch{Tenant: tenant}
		now := p.Reconciler.Now()
		var changes []Change
		for _, m := range derived {
			if !onlyFrom(m, episodeID) {
				continue
			}
			b.Closes = append(b.Closes, store.Close{ID: m.ID, InvalidatedAt: &now})
			b.Events = append(b.Events, memory.Event{MemoryID: m.ID, Action: memory.ActionInvalidate, Old: m.Text, Reason: "source turn " + string(ep.Status), At: now})
			changes = append(changes, Change{Action: memory.ActionInvalidate, Memory: m, Reason: "source turn " + string(ep.Status)})
		}
		if ep.Status != memory.Deleted {
			run, err := p.Store.Run(ctx, tenant, ep.Run)
			if err != nil {
				return nil, err
			}
			if ep.Seq <= run.ProcessedSeq { // not yet processed episodes are handled by ProcessRun
				props, err := p.propose(ctx, run, []memory.Episode{ep})
				if err != nil {
					return nil, err
				}
				// Plan skips memories this batch closes, so re-derived facts become new versions.
				more, err := p.Reconciler.Plan(ctx, tenant, props, b)
				if err != nil {
					return nil, err
				}
				changes = append(changes, more...)
			}
		}
		if p.OnWrite != nil {
			p.OnWrite(b, changes)
		}
		if b.Empty() {
			return changes, nil
		}
		_, err = p.Store.Write(ctx, b)
		if errors.Is(err, memory.ErrConflict) && attempt < maxConflictRetries {
			continue
		}
		return changes, err
	}
}

func onlyFrom(m memory.Memory, episodeID string) bool {
	if len(m.Source.Evidence) == 0 {
		return false
	}
	for _, e := range m.Source.Evidence {
		if e.Episode != episodeID {
			return false
		}
	}
	return true
}
