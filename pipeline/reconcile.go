// Package pipeline turns proposals into durable, versioned knowledge and
// runs the asynchronous extraction of conversation episodes.
//
// The Reconciler is the single place where knowledge evolves. For each
// proposed memory it decides, deterministically:
//
//   - duplicate (same content hash, same owner, still open) → add evidence
//   - open value for a slot that has an open value → supersede and link
//     versions: the old value ends where the new one starts (ValidTo), or,
//     if the new one starts no later than the old one, the old value was
//     wrong and is invalidated (a correction)
//   - a value that already ended (ValidTo set) → stored as history
//   - op "invalidate" → end the open value of the slot
//   - otherwise → add
//
// An open fact asserts that it is true now, so the newest knowledge about a
// slot always becomes its current value.
//
// It only plans: changes are appended to a store.Batch that the caller
// writes atomically, retrying the whole plan on memory.ErrConflict.
package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

// Proposal is a memory someone wants to store, or an invalidation.
type Proposal struct {
	Memory memory.Memory
	// ForceVersion preserves correction lineage even when identical content
	// exists elsewhere. The caller must conditionally close the predecessor.
	ForceVersion bool
	// Invalidate ends the open value of Memory.Slot instead of adding.
	Invalidate bool
	// Match restricts Invalidate to an open value whose text or object
	// contains it (case-insensitive).
	Match string
}

// Change is the outcome of one proposal.
type Change struct {
	Action   memory.Action `json:"action"`
	Memory   memory.Memory `json:"memory"`
	Previous string        `json:"previous,omitempty"` // superseded or invalidated version
	Reason   string        `json:"reason,omitempty"`
}

// Reconciler plans changes against current store state.
type Reconciler struct {
	Store  store.Reader
	Now    func() time.Time
	Limits memory.Limits
	// NoHeat makes duplicates add evidence without heat (reprocessing).
	NoHeat bool
}

// Prepare fills defaults of a new memory (ID, hash, times, version, kind,
// layer, visibility, trust) without overriding values that are set.
func Prepare(m *memory.Memory, now time.Time) {
	m.Text = memory.Normalize(m.Text)
	if m.ID == "" {
		m.ID = memory.NewID()
	}
	m.Hash = memory.KnowledgeHash(m)
	m.RecordedAt = now
	if m.ValidFrom.IsZero() {
		m.ValidFrom = now
	}
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Kind == "" {
		m.Kind = memory.Semantic
	}
	if m.Layer == 0 {
		m.Layer = memory.Atom
	}
	if m.Visibility == "" {
		m.Visibility = memory.Team
	}
	if m.Source.Kind == "" {
		m.Source.Kind = "explicit"
	}
	if m.Source.Trust == "" {
		m.Source.Trust = memory.TrustNormal
	}
	m.Slot = strings.TrimSpace(m.Slot)
}

// open selects memories that are neither closed nor invalidated, regardless
// of expiry or validity window.
func open(tenant string, f memory.Filter) store.Query {
	return store.Query{Tenant: tenant, AllVersions: true, ShowExpired: true, Filter: memory.And(f,
		memory.Cmp("valid_to", memory.OpExists, false), memory.Cmp("invalidated_at", memory.OpExists, false))}
}

func owner(m *memory.Memory) memory.Filter {
	return memory.And(memory.Eq("user_id", m.User), memory.Eq("agent_id", m.Agent))
}

func slotKey(m *memory.Memory) string { return m.User + "\x00" + m.Agent + "\x00" + m.Slot }

// ActionNoop reports a proposal that changed nothing (an invalidation
// without a matching open value).
const ActionNoop memory.Action = "noop"

// ActionReject reports a proposal that failed validation.
const ActionReject memory.Action = "reject"

// Plan appends the changes for props to b and returns exactly one Change
// per proposal, in order. Later proposals see earlier ones.
func (r *Reconciler) Plan(ctx context.Context, tenant string, props []Proposal, b *store.Batch) ([]Change, error) {
	now := r.Now()
	p := planner{r: r, ctx: ctx, tenant: tenant, b: b, now: now,
		slots: map[string]int{}, hashes: map[string]int{}}
	var out []Change
	for i := range props {
		pr := props[i]
		m := pr.Memory
		m.Tenant = tenant
		if pr.Invalidate {
			ch, err := p.invalidate(&m, pr.Match)
			if err != nil {
				return nil, err
			}
			if ch == nil {
				ch = &Change{Action: ActionNoop, Memory: m, Reason: "no matching open value"}
			}
			out = append(out, *ch)
			continue
		}
		Prepare(&m, now)
		if err := m.Validate(r.Limits); err != nil {
			out = append(out, Change{Action: ActionReject, Memory: m, Reason: err.Error()})
			continue
		}
		ch, err := p.add(&m, pr.ForceVersion)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

type planner struct {
	r      *Reconciler
	ctx    context.Context
	tenant string
	b      *store.Batch
	now    time.Time
	slots  map[string]int // slot key -> index in b.Memories of the open in-batch value
	hashes map[string]int // owner+hash -> index in b.Memories
}

func (p *planner) event(id string, a memory.Action, old, new, related, actor, reason string) {
	p.b.Events = append(p.b.Events, memory.Event{MemoryID: id, Action: a, Old: old, New: new, Related: related, Actor: actor, Reason: reason, At: p.now})
}

// stored returns the open stored memory of a slot or with a hash, if any.
func (p *planner) stored(f memory.Filter) (*memory.Memory, error) {
	ms, err := p.r.Store.Find(p.ctx, func() store.Query { q := open(p.tenant, f); q.Limit = 1; return q }())
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	for _, c := range p.b.Closes { // already closed earlier in this batch
		if c.ID == ms[0].ID {
			return nil, nil
		}
	}
	return &ms[0], nil
}

func (p *planner) add(m *memory.Memory, forceVersion bool) (Change, error) {
	hk := m.User + "\x00" + m.Agent + "\x00" + m.Hash
	if i, ok := p.hashes[hk]; ok && !forceVersion { // duplicate inside this batch
		prev := &p.b.Memories[i]
		prev.Source.Evidence = append(prev.Source.Evidence, m.Source.Evidence...)
		return Change{Action: memory.ActionEvidence, Memory: *prev, Reason: "duplicate in batch"}, nil
	}
	dup, err := p.stored(memory.And(owner(m), memory.Eq("hash", m.Hash)))
	if err != nil {
		return Change{}, err
	}
	if dup != nil && !forceVersion {
		heat := int64(1)
		if p.r.NoHeat {
			heat = 0
		}
		p.b.Touches = append(p.b.Touches, store.Touch{ID: dup.ID, Evidence: m.Source.Evidence, Heat: heat})
		p.event(dup.ID, memory.ActionEvidence, "", "", "", m.Source.Actor, "seen again")
		dup.Heat += heat
		return Change{Action: memory.ActionEvidence, Memory: *dup, Reason: "duplicate"}, nil
	}
	ch := Change{Action: memory.ActionAdd, Memory: *m}
	if m.Slot != "" && store.Open(m) {
		var err error
		if ch, err = p.supersede(m); err != nil {
			return Change{}, err
		}
	}
	p.b.Memories = append(p.b.Memories, *m)
	idx := len(p.b.Memories) - 1
	p.hashes[hk] = idx
	if m.Slot != "" && store.Open(m) {
		p.slots[slotKey(m)] = idx
	}
	p.event(m.ID, memory.ActionAdd, "", m.Text, m.Supersedes, m.Source.Actor, ch.Reason)
	ch.Memory = *m
	return ch, nil
}

// supersede resolves a new open slot value against the current one.
func (p *planner) supersede(m *memory.Memory) (Change, error) {
	ch := Change{Action: memory.ActionAdd}
	if i, ok := p.slots[slotKey(m)]; ok { // value proposed earlier in this batch
		prev := &p.b.Memories[i]
		p.closeInBatch(prev, m.ValidFrom)
		delete(p.slots, slotKey(m))
		m.Supersedes, m.Version = prev.ID, prev.Version+1
		ch.Action, ch.Previous = memory.ActionSupersede, prev.ID
		return ch, nil
	}
	old, err := p.stored(memory.And(owner(m), memory.Eq("slot", m.Slot)))
	if err != nil || old == nil {
		return ch, err
	}
	action := p.closeStored(old, m.ValidFrom)
	p.event(old.ID, action, old.Text, m.Text, m.ID, m.Source.Actor, "slot "+m.Slot)
	m.Supersedes, m.Version = old.ID, old.Version+1
	ch.Action, ch.Previous = memory.ActionSupersede, old.ID
	return ch, nil
}

// closeStored ends a stored version at time at: a world change when at is
// after its start (ValidTo), otherwise a correction (InvalidatedAt).
func (p *planner) closeStored(old *memory.Memory, at time.Time) memory.Action {
	c := store.Close{ID: old.ID}
	action := memory.ActionSupersede
	if at.After(old.ValidFrom) {
		c.ValidTo = &at
		now := p.now
		c.ClosedAt = &now
	} else {
		now := p.now
		c.InvalidatedAt = &now
		action = memory.ActionUpdate
	}
	p.b.Closes = append(p.b.Closes, c)
	return action
}

func (p *planner) closeInBatch(prev *memory.Memory, at time.Time) {
	if at.After(prev.ValidFrom) {
		prev.ValidTo = &at
		now := p.now
		prev.ClosedAt = &now
		return
	}
	now := p.now
	prev.InvalidatedAt = &now
}

func (p *planner) invalidate(m *memory.Memory, match string) (*Change, error) {
	if m.Slot == "" {
		return nil, fmt.Errorf("%w: invalidate needs a slot", memory.ErrInvalid)
	}
	at := m.ValidFrom
	if at.IsZero() {
		at = p.now
	}
	contains := func(t *memory.Memory) bool {
		hay := strings.ToLower(t.Text + " " + t.Object)
		return match == "" || strings.Contains(hay, strings.ToLower(match))
	}
	if i, ok := p.slots[slotKey(m)]; ok {
		prev := &p.b.Memories[i]
		if !contains(prev) {
			return nil, nil
		}
		p.closeInBatch(prev, at)
		delete(p.slots, slotKey(m))
		return &Change{Action: memory.ActionInvalidate, Memory: *prev, Reason: "slot " + m.Slot}, nil
	}
	old, err := p.stored(memory.And(owner(m), memory.Eq("slot", m.Slot)))
	if err != nil || old == nil || !contains(old) {
		return nil, err
	}
	p.closeStored(old, at)
	p.event(old.ID, memory.ActionInvalidate, old.Text, "", "", m.Source.Actor, "slot "+m.Slot)
	return &Change{Action: memory.ActionInvalidate, Memory: *old, Reason: "slot " + m.Slot}, nil
}
