package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Fact is a memory written explicitly by an agent or application — the
// primary, most precise way to remember something.
type Fact struct {
	memory.Scope
	Kind      memory.Kind              `json:"kind,omitempty"`  // default semantic
	Layer     memory.Layer             `json:"layer,omitempty"` // default atom
	Title     string                   `json:"title,omitempty"`
	Text      string                   `json:"text"`
	Subject   string                   `json:"subject,omitempty"`
	Predicate string                   `json:"predicate,omitempty"`
	Object    string                   `json:"object,omitempty"`
	Slot      string                   `json:"slot,omitempty"`
	Tags      []string                 `json:"tags,omitempty"`
	Metadata  map[string]any           `json:"metadata,omitempty"`
	Assertion *memory.Assertion        `json:"assertion,omitempty"`
	Decision  *memory.DecisionMetadata `json:"decision,omitempty"`
	// OperationKey allows a caller to retry a write across process restarts.
	OperationKey string             `json:"operation_key,omitempty"`
	Entities     []memory.EntityRef `json:"entities,omitempty"`
	// Confidence and Importance default to 1.0 and 0.5 when zero.
	Confidence float64           `json:"confidence,omitempty"`
	Importance float64           `json:"importance,omitempty"`
	Visibility memory.Visibility `json:"visibility,omitempty"`
	Grants     []string          `json:"grants,omitempty"`
	ValidFrom  time.Time         `json:"valid_from,omitempty"`
	ValidTo    *time.Time        `json:"valid_to,omitempty"`
	// TTL sets ExpiresAt relative to now (working memory).
	TTL       time.Duration     `json:"ttl,omitempty"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	Actor     string            `json:"actor,omitempty"`
	Evidence  []memory.Evidence `json:"evidence,omitempty"`
	// Vectors are caller-computed embeddings of this memory (any model).
	Vectors []Vector `json:"vectors,omitempty"`
	// Invalidate ends the open value of Slot instead of adding a memory
	// (optionally only when it contains Match).
	Invalidate bool   `json:"invalidate,omitempty"`
	Match      string `json:"match,omitempty"`
}

// Vector is a caller-supplied embedding.
type Vector struct {
	Model    string    `json:"model"`
	Modality string    `json:"modality,omitempty"`
	Values   []float32 `json:"values"`
}

func (f Fact) memory(now time.Time) memory.Memory {
	m := memory.Memory{
		Scope: f.Scope, Kind: f.Kind, Layer: f.Layer, Title: f.Title, Text: f.Text,
		Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, Slot: f.Slot,
		Tags: f.Tags, Metadata: f.Metadata, Entities: f.Entities,
		Confidence: f.Confidence, Importance: f.Importance, Visibility: f.Visibility, Grants: f.Grants,
		ValidFrom: f.ValidFrom, ValidTo: f.ValidTo, ExpiresAt: f.ExpiresAt,
		Source: memory.Source{Kind: "explicit", Actor: f.Actor, Trust: memory.TrustHigh, Evidence: f.Evidence},
	}
	if m.Confidence == 0 {
		m.Confidence = 1
	}
	if m.Importance == 0 {
		m.Importance = 0.5
	}
	if f.TTL > 0 && m.ExpiresAt == nil {
		exp := now.Add(f.TTL)
		m.ExpiresAt = &exp
	}
	return m
}

// Remember writes facts of one tenant atomically and returns what happened
// to each: added, superseded an older value, recognized as a duplicate, or
// rejected by validation.
func (e *Engine) Remember(ctx context.Context, facts ...Fact) (changes []pipeline.Change, err error) {
	defer e.observe("remember", time.Now(), err)
	if len(facts) == 0 {
		return nil, nil
	}
	tenant := facts[0].Tenant
	if tenant == "" {
		return nil, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	models, _ := e.models(s)
	for _, f := range facts {
		if f.Tenant != tenant {
			return nil, fmt.Errorf("%w: all facts of one call must share a tenant", memory.ErrInvalid)
		}
		if err := checkVectors(models, f.Vectors); err != nil {
			return nil, err
		}
		if len(f.OperationKey) > 200 || f.OperationKey != "" && f.OperationKey != facts[0].OperationKey {
			return nil, fmt.Errorf("%w: facts must share one operation key of at most 200 bytes", memory.ErrInvalid)
		}
	}
	key := facts[0].OperationKey
	if key == "" {
		key = memory.NewID()
	}
	hash, err := operationHash(facts)
	if err != nil {
		return nil, err
	}
	if replayed, err := e.replayOperation(ctx, tenant, "remember:"+key, hash, &changes); replayed || err != nil {
		return changes, err
	}
	receipt, err := e.write(ctx, func() (*store.Batch, error) {
		now := e.o.Now()
		props := make([]pipeline.Proposal, len(facts))
		for i, f := range facts {
			m := f.memory(now)
			m.Metadata, err = memory.WithKnowledgeMetadata(m.Metadata, f.Assertion, f.Decision)
			if err != nil {
				return nil, err
			}
			assertion, err := memory.ReadAssertion(m)
			if err != nil {
				return nil, err
			}
			if assertion != nil {
				m.Subject, m.Predicate, m.Object = assertion.Subject, assertion.Predicate, assertion.Object
			}
			decision, err := memory.ReadDecision(m)
			if err != nil {
				return nil, err
			}
			if assertion != nil || decision != nil {
				if err := e.validateKnowledgeEvidence(ctx, KnowledgeRequest{Input: knowledge.Input{
					Scope: f.Scope, Assertion: assertion, Decision: decision, Evidence: f.Evidence,
					Visibility: f.Visibility, Grants: f.Grants}}); err != nil {
					return nil, err
				}
			}
			props[i] = pipeline.Proposal{Memory: m, Invalidate: f.Invalidate, Match: f.Match}
		}
		b := &store.Batch{Tenant: tenant, OperationKey: "remember:" + key, OperationHash: hash}
		changes, err = e.rec.Plan(ctx, tenant, props, b)
		if err != nil {
			return nil, err
		}
		for i := range changes { // one change per fact, in order
			if a := changes[i].Action; a == memory.ActionAdd || a == memory.ActionSupersede {
				e.attachVectors(b, changes[i].Memory, facts[i].Vectors)
			}
		}
		e.followUp(b, changes)
		b.Result, err = json.Marshal(changes)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
	if err == nil && receipt.Replayed {
		err = json.Unmarshal(receipt.Result, &changes)
	}
	return changes, err
}

func checkVectors(models map[string]vector.Model, vs []Vector) error {
	for _, v := range vs {
		m, ok := models[v.Model]
		if !ok {
			return fmt.Errorf("%w: vector model %q is not declared in tenant settings", memory.ErrInvalid, v.Model)
		}
		if len(v.Values) != m.Dims {
			return fmt.Errorf("%w: model %s has %d dims, got %d", vector.ErrDims, m.ID, m.Dims, len(v.Values))
		}
		if err := vector.Normalize(append([]float32(nil), v.Values...)); err != nil {
			return fmt.Errorf("%w: %v", memory.ErrInvalid, err)
		}
	}
	return nil
}

// attachVectors adds caller vectors of a memory to the batch, and an index
// job when an external index must receive them.
func (e *Engine) attachVectors(b *store.Batch, m memory.Memory, vs []Vector) {
	var pts []vector.Point
	for _, v := range vs {
		pts = append(pts, vector.Point{Tenant: m.Tenant, Model: v.Model, Modality: v.Modality, User: m.User, Agent: m.Agent, Run: m.Run,
			Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, Values: v.Values})
	}
	e.addPoints(b, pts)
}

func (e *Engine) addPoints(b *store.Batch, pts []vector.Point) {
	if len(pts) == 0 {
		return
	}
	b.Vectors = append(b.Vectors, pts...)
	if e.external {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobIndex, Payload: payload(indexPayload{Points: pts})})
	}
}

// Get returns one memory if the principal may read it.
func (e *Engine) Get(ctx context.Context, tenant string, as memory.Principal, id string) (memory.Memory, error) {
	ms, err := e.o.Store.Get(ctx, tenant, []string{id})
	if err != nil {
		return memory.Memory{}, err
	}
	if len(ms) == 0 {
		return memory.Memory{}, fmt.Errorf("memory %s: %w", id, memory.ErrNotFound)
	}
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return memory.Memory{}, err
	}
	f := memory.And(memory.Readable(as), bindingFilter(s, as))
	if err := f.Check(); err != nil {
		return memory.Memory{}, err
	}
	if !f.Match(&ms[0]) {
		return memory.Memory{}, fmt.Errorf("memory %s: %w", id, memory.ErrNotFound)
	}
	return ms[0], nil
}

// ListRequest lists memories by scope and filter, without ranking.
type ListRequest struct {
	Tenant           string           `json:"-"`
	As               memory.Principal `json:"-"`
	User             string           `json:"user_id,omitempty"`
	Agent            string           `json:"agent_id,omitempty"`
	Run              string           `json:"run_id,omitempty"`
	Filter           memory.Filter    `json:"-"`
	ValidAt, KnownAt time.Time        `json:"-"`
	AllVersions      bool             `json:"all_versions,omitempty"`
	ShowExpired      bool             `json:"show_expired,omitempty"`
	Order            store.Order      `json:"order,omitempty"`
	Limit            int              `json:"limit,omitempty"`
	Offset           int              `json:"offset,omitempty"`
}

// List returns memories in store order (newest first by default).
func (e *Engine) List(ctx context.Context, r ListRequest) (out []memory.Memory, err error) {
	defer e.observe("list", time.Now(), err)
	q, err := e.query(ctx, r.Tenant, r.As, r.User, r.Agent, r.Run, r.Filter)
	if err != nil {
		return nil, err
	}
	q.ValidAt, q.KnownAt, q.AllVersions, q.ShowExpired, q.Order, q.Offset = r.ValidAt, r.KnownAt, r.AllVersions, r.ShowExpired, r.Order, r.Offset
	q.Limit = r.Limit
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	return e.o.Store.Find(ctx, q)
}

// query builds a store query with visibility, binding and scope filters.
func (e *Engine) query(ctx context.Context, tenant string, as memory.Principal, user, agent, run string, f memory.Filter) (store.Query, error) {
	if tenant == "" {
		return store.Query{}, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return store.Query{}, err
	}
	if as.User == "" && as.Agent == "" && len(as.Roles) == 0 && !as.Admin {
		as = memory.Principal{User: user, Agent: agent}
	}
	all := memory.And(memory.Readable(as), bindingFilter(s, as), f)
	for field, v := range map[string]string{"user_id": user, "agent_id": agent, "run_id": run} {
		if v != "" {
			all.And = append(all.And, memory.Eq(field, v))
		}
	}
	if err := all.Check(); err != nil {
		return store.Query{}, err
	}
	return store.Query{Tenant: tenant, Filter: all}, nil
}

// History returns the lifecycle events of a memory.
func (e *Engine) History(ctx context.Context, tenant, id string) ([]memory.Event, error) {
	return e.o.Store.History(ctx, tenant, id)
}

// Versions returns every version of a memory, oldest first, by following
// Supersedes links backwards and forwards.
func (e *Engine) Versions(ctx context.Context, tenant, id string) ([]memory.Memory, error) {
	ms, err := e.o.Store.Get(ctx, tenant, []string{id})
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	chain := []memory.Memory{ms[0]}
	for i := 0; i < 1000 && chain[0].Supersedes != ""; i++ {
		prev, err := e.o.Store.Get(ctx, tenant, []string{chain[0].Supersedes})
		if err != nil || len(prev) == 0 {
			break
		}
		chain = append([]memory.Memory{prev[0]}, chain...)
	}
	for i := 0; i < 1000; i++ {
		next, err := e.o.Store.Find(ctx, store.Query{Tenant: tenant, AllVersions: true, ShowExpired: true, Limit: 1,
			Filter: memory.Eq("supersedes", chain[len(chain)-1].ID)})
		if err != nil {
			return nil, err
		}
		if len(next) == 0 {
			break
		}
		chain = append(chain, next[0])
	}
	return chain, nil
}

// Patch corrects a memory. Nil fields are kept.
type Patch struct {
	OperationKey string             `json:"operation_key,omitempty"`
	Text         *string            `json:"text,omitempty"`
	Title        *string            `json:"title,omitempty"`
	Tags         *[]string          `json:"tags,omitempty"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
	Confidence   *float64           `json:"confidence,omitempty"`
	Importance   *float64           `json:"importance,omitempty"`
	Visibility   *memory.Visibility `json:"visibility,omitempty"`
	Grants       *[]string          `json:"grants,omitempty"`
	ExpiresAt    *time.Time         `json:"expires_at,omitempty"`
	Vectors      []Vector           `json:"vectors,omitempty"`
	Actor        string             `json:"actor,omitempty"`
	Reason       string             `json:"reason,omitempty"`
}

// Update corrects an open memory: the old version is invalidated (it was
// never right) and a new version supersedes it. Use Remember with the same
// slot when the world changed instead.
func (e *Engine) Update(ctx context.Context, tenant, id string, p Patch) (m memory.Memory, err error) {
	defer e.observe("update", time.Now(), err)
	if len(p.OperationKey) > 200 {
		return m, fmt.Errorf("%w: operation key exceeds 200 bytes", memory.ErrInvalid)
	}
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return m, err
	}
	models, _ := e.models(s)
	if err := checkVectors(models, p.Vectors); err != nil {
		return m, err
	}
	hash, err := operationHash(struct {
		ID    string
		Patch Patch
	}{id, p})
	if err != nil {
		return m, err
	}
	key := p.OperationKey
	if key == "" {
		key = hash
	}
	key = "update:" + key
	if replayed, err := e.replayOperation(ctx, tenant, key, hash, &m); replayed || err != nil {
		return m, err
	}
	receipt, err := e.write(ctx, func() (*store.Batch, error) {
		ms, err := e.o.Store.Get(ctx, tenant, []string{id})
		if err != nil {
			return nil, err
		}
		if len(ms) == 0 {
			return nil, fmt.Errorf("memory %s: %w", id, memory.ErrNotFound)
		}
		old := ms[0]
		if !store.Open(&old) {
			return nil, fmt.Errorf("%w: memory %s is a closed version; update the current one", memory.ErrConflict, id)
		}
		now := e.o.Now()
		m = old
		m.ID, m.Version, m.Supersedes, m.Heat, m.ExpiresAt = "", old.Version+1, old.ID, 0, old.ExpiresAt
		m.Source.Kind, m.Source.Actor, m.Source.Trust = "explicit", p.Actor, memory.TrustHigh
		if p.Text != nil {
			m.Text = *p.Text
		}
		if p.Title != nil {
			m.Title = *p.Title
		}
		if p.Tags != nil {
			m.Tags = *p.Tags
		}
		if p.Metadata != nil {
			m.Metadata = p.Metadata
		}
		if p.Confidence != nil {
			m.Confidence = *p.Confidence
		}
		if p.Importance != nil {
			m.Importance = *p.Importance
		}
		if p.Visibility != nil {
			m.Visibility = *p.Visibility
		}
		if p.Grants != nil {
			m.Grants = *p.Grants
		}
		if p.ExpiresAt != nil {
			m.ExpiresAt = p.ExpiresAt
		}
		pipelinePrepare(&m, now)
		if err := m.Validate(e.o.Limits); err != nil {
			return nil, err
		}
		b := &store.Batch{Tenant: tenant, OperationKey: key, OperationHash: hash,
			Closes:   []store.Close{{ID: old.ID, InvalidatedAt: &now}},
			Memories: []memory.Memory{m},
			Events: []memory.Event{
				{MemoryID: old.ID, Action: memory.ActionUpdate, Old: old.Text, New: m.Text, Related: m.ID, Actor: p.Actor, Reason: p.Reason, At: now},
				{MemoryID: m.ID, Action: memory.ActionAdd, New: m.Text, Related: old.ID, Actor: p.Actor, Reason: p.Reason, At: now},
			}}
		e.attachVectors(b, m, p.Vectors)
		e.followUp(b, []pipeline.Change{{Action: memory.ActionAdd, Memory: m}})
		b.Result, err = json.Marshal(m)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
	if err == nil && receipt.Replayed {
		err = json.Unmarshal(receipt.Result, &m)
	}
	return m, err
}

func pipelinePrepare(m *memory.Memory, now time.Time) { pipeline.Prepare(m, now) }

// Invalidate ends a memory's validity at time at (zero = now): the fact
// stopped being true. If at is not after its start, it is invalidated instead.
func (e *Engine) Invalidate(ctx context.Context, tenant, id string, at time.Time, actor, reason string) (err error) {
	return e.InvalidateWithKey(ctx, tenant, id, "", at, actor, reason)
}

func (e *Engine) InvalidateWithKey(ctx context.Context, tenant, id, key string, at time.Time, actor, reason string) (err error) {
	defer e.observe("invalidate", time.Now(), err)
	if len(key) > 200 {
		return fmt.Errorf("%w: operation key exceeds 200 bytes", memory.ErrInvalid)
	}
	hash, err := operationHash(struct {
		ID, Actor, Reason string
		At                time.Time
	}{id, actor, reason, at})
	if err != nil {
		return err
	}
	if key == "" {
		key = hash
	}
	key = "invalidate:" + key
	var completed map[string]string
	if replayed, err := e.replayOperation(ctx, tenant, key, hash, &completed); replayed || err != nil {
		return err
	}
	_, err = e.write(ctx, func() (*store.Batch, error) {
		ms, err := e.o.Store.Get(ctx, tenant, []string{id})
		if err != nil {
			return nil, err
		}
		if len(ms) == 0 {
			return nil, fmt.Errorf("memory %s: %w", id, memory.ErrNotFound)
		}
		now := e.o.Now()
		if at.IsZero() {
			at = now
		}
		c := store.Close{ID: id}
		if at.After(ms[0].ValidFrom) {
			c.ValidTo = &at
			c.ClosedAt = &now
		} else {
			c.InvalidatedAt = &now
		}
		return &store.Batch{Tenant: tenant, OperationKey: key, OperationHash: hash,
			Result: payload(map[string]string{"status": "invalidated", "id": id}), Closes: []store.Close{c},
			Events: []memory.Event{{MemoryID: id, Action: memory.ActionInvalidate, Old: ms[0].Text, Actor: actor, Reason: reason, At: now}}}, nil
	})
	return err
}

// Delete permanently removes memories with their vectors and entity links.
// Their history keeps the audit trail (actions and times) without content.
func (e *Engine) Delete(ctx context.Context, tenant, actor string, ids ...string) (err error) {
	return e.DeleteWithKey(ctx, tenant, actor, "", ids...)
}

func (e *Engine) DeleteWithKey(ctx context.Context, tenant, actor, key string, ids ...string) (err error) {
	defer e.observe("delete", time.Now(), err)
	if len(key) > 200 {
		return fmt.Errorf("%w: operation key exceeds 200 bytes", memory.ErrInvalid)
	}
	if len(ids) == 0 {
		return nil
	}
	hash, err := operationHash(struct {
		IDs   []string
		Actor string
	}{ids, actor})
	if err != nil {
		return err
	}
	if key == "" {
		key = hash
	}
	key = "delete:" + key
	var completed map[string]string
	if replayed, err := e.replayOperation(ctx, tenant, key, hash, &completed); replayed || err != nil {
		return err
	}
	now := e.o.Now()
	b := &store.Batch{Tenant: tenant, OperationKey: key, OperationHash: hash,
		Result: payload(map[string]string{"status": "deleted"}), Deletes: ids}
	var targets []vector.Target
	for _, id := range ids {
		b.Events = append(b.Events, memory.Event{MemoryID: id, Action: memory.ActionDelete, Actor: actor, At: now})
		targets = append(targets, vector.Target{Kind: vector.TargetMemory, ID: id})
	}
	if e.external {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobUnindex, Payload: payload(indexPayload{Targets: targets})})
	}
	_, err = e.o.Store.Write(ctx, b)
	return err
}

// ForgetResult reports what Forget removed.
type ForgetResult struct {
	Memories int `json:"memories"`
	Runs     int `json:"runs"`
	Blobs    int `json:"blobs"`
}

// Forget deletes everything owned by a scope: all memory versions and all
// runs (episodes) of the given user/agent/run, plus blobs nothing else
// references. At least one of User, Agent or Run must be set.
func (e *Engine) Forget(ctx context.Context, scope memory.Scope, actor string) (res ForgetResult, err error) {
	defer e.observe("forget", time.Now(), err)
	if scope.Tenant == "" || (scope.User == "" && scope.Agent == "" && scope.Run == "") {
		return res, fmt.Errorf("%w: forget needs a tenant and a user, agent or run", memory.ErrInvalid)
	}
	f := memory.And()
	for field, v := range map[string]string{"user_id": scope.User, "agent_id": scope.Agent, "run_id": scope.Run} {
		if v != "" {
			f.And = append(f.And, memory.Eq(field, v))
		}
	}
	if err := f.Check(); err != nil {
		return res, err
	}
	for {
		ms, err := e.o.Store.Find(ctx, store.Query{Tenant: scope.Tenant, Filter: f, AllVersions: true, ShowExpired: true, Limit: 500})
		if err != nil {
			return res, err
		}
		if len(ms) == 0 {
			break
		}
		ids := make([]string, len(ms))
		for i, m := range ms {
			ids[i] = m.ID
		}
		if err := e.Delete(ctx, scope.Tenant, actor, ids...); err != nil {
			return res, err
		}
		res.Memories += len(ids)
	}
	eps, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: scope.Tenant, User: scope.User, Agent: scope.Agent, Run: scope.Run, IncludeDeleted: true})
	if err != nil {
		return res, err
	}
	runs, blobs := map[string]bool{}, map[string]bool{}
	for _, ep := range eps {
		runs[ep.Run] = true
		for _, p := range ep.Parts {
			if p.Blob != nil {
				blobs[p.Blob.SHA256] = true
			}
		}
	}
	if len(runs) > 0 {
		b := &store.Batch{Tenant: scope.Tenant}
		for r := range runs {
			b.DeleteRuns = append(b.DeleteRuns, r)
		}
		if _, err := e.o.Store.Write(ctx, b); err != nil {
			return res, err
		}
		res.Runs = len(runs)
	}
	for sha := range blobs {
		if e.o.Blob == nil {
			break
		}
		still, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: scope.Tenant, BlobSHA: sha, IncludeDeleted: true, Limit: 1})
		if err != nil {
			return res, err
		}
		if len(still) == 0 {
			if err := e.o.Blob.Delete(ctx, scope.Tenant, sha); err != nil && !errors.Is(err, memory.ErrNotFound) {
				return res, err
			}
			res.Blobs++
		}
	}
	return res, nil
}
