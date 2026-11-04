package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
)

type KnowledgeOptions struct {
	Mode                 string
	Planner              knowledge.Planner
	Admission            knowledge.AdmissionDecider
	MaxCandidates        int
	MaxGraphHops         int
	Timeout              time.Duration
	Fallback             bool
	Consolidation        bool
	BackgroundEnrichment bool
}

type KnowledgeRequest struct {
	Input         knowledge.Input  `json:"input"`
	As            memory.Principal `json:"-"`
	MaxCandidates int              `json:"max_candidates,omitempty"`
	ValidAt       time.Time        `json:"valid_at,omitempty"`
	KnownAt       time.Time        `json:"known_at,omitempty"`
	AllVersions   bool             `json:"all_versions,omitempty"`
}

type KnowledgeResult struct {
	Plan            knowledge.Plan             `json:"plan"`
	Changes         []pipeline.Change          `json:"changes,omitempty"`
	Scope           memory.Scope               `json:"scope"`
	Mode            string                     `json:"mode"`
	Status          string                     `json:"status"`
	Error           string                     `json:"error,omitempty"`
	Duration        time.Duration              `json:"duration_ns"`
	Replayed        bool                       `json:"replayed,omitempty"`
	ProposedPlan    *knowledge.Plan            `json:"proposed_plan,omitempty"`
	Validation      string                     `json:"validation,omitempty"`
	ValidationError *knowledge.ValidationError `json:"validation_error,omitempty"`
}

func (e *Engine) knowledgePrincipal(r KnowledgeRequest) (memory.Principal, error) {
	as, sc := r.As, r.Input.Scope
	if sc.Tenant == "" {
		return as, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	if !as.Admin && (as.User != "" && as.User != sc.User || as.Agent != "" && as.Agent != sc.Agent) {
		return as, fmt.Errorf("%w: knowledge scope does not match principal", memory.ErrInvalid)
	}
	if !as.Admin && as.User == "" && as.Agent == "" && len(as.Roles) == 0 {
		as.User, as.Agent = sc.User, sc.Agent
	}
	return as, nil
}

// KnowledgeState gives conflicts and exact structured matches priority over
// user-facing ranking, while reusing the existing retrieval implementations.
func (e *Engine) KnowledgeState(ctx context.Context, r KnowledgeRequest) (knowledge.State, error) {
	var out knowledge.State
	as, err := e.knowledgePrincipal(r)
	if err != nil {
		return out, err
	}
	options, err := e.knowledgeOptions(ctx, r.Input.Scope.Tenant)
	if err != nil {
		return out, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	max := options.MaxCandidates
	if max <= 0 {
		max = 16
	}
	if r.MaxCandidates > 0 && r.MaxCandidates < max {
		max = r.MaxCandidates
	}
	sc := r.Input.Scope
	q, err := e.query(ctx, sc.Tenant, as, sc.User, sc.Agent, sc.Run, memory.Filter{})
	if err != nil {
		return out, err
	}
	q.Limit, q.AllVersions, q.ShowExpired = max, true, true
	seen := map[string]bool{}
	var candidates []memory.Memory
	appendCandidates := func(ms []memory.Memory) {
		for _, m := range ms {
			if len(candidates) < max && !seen[m.ID] {
				candidates, seen[m.ID] = append(candidates, m), true
			}
		}
	}
	if r.Input.TargetID != "" {
		m, err := e.Get(ctx, sc.Tenant, as, r.Input.TargetID)
		if err != nil {
			return out, err
		}
		if m.User != sc.User || m.Agent != sc.Agent {
			return out, fmt.Errorf("%w: target belongs to another scope", memory.ErrInvalid)
		}
		appendCandidates([]memory.Memory{m})
	}
	var exact []memory.Filter
	if r.Input.Slot != "" {
		exact = append(exact, memory.Eq("slot", r.Input.Slot))
	}
	if a := r.Input.Assertion; a != nil {
		exact = append(exact, memory.And(memory.Eq("subject", a.Subject), memory.Eq("predicate", a.Predicate)))
	}
	if len(exact) != 0 {
		eq := q
		eq.Filter = memory.And(q.Filter, memory.Or(exact...))
		ms, err := e.o.Store.Find(ctx, eq)
		if err != nil {
			return out, err
		}
		appendCandidates(ms)
	}
	if strings.TrimSpace(r.Input.Text) != "" && len(candidates) < max {
		s, err := e.Settings(ctx, sc.Tenant)
		if err != nil {
			return out, err
		}
		p, err := e.policy(s, "deep")
		if err != nil {
			return out, err
		}
		_, active := e.models(s)
		// Direct searcher use avoids even heat-counter writes during dry-run.
		result, err := e.search.Search(ctx, search.Request{Tenant: sc.Tenant, As: as, User: sc.User, Agent: sc.Agent,
			Run: sc.Run, Text: r.Input.Text, Filter: q.Filter, Policy: p, Model: active,
			ValidAt: r.ValidAt, KnownAt: r.KnownAt, AllVersions: r.AllVersions, Limit: max})
		if err != nil {
			return out, err
		}
		for _, h := range result.Hits {
			appendCandidates([]memory.Memory{h.Memory})
		}
	}
	if len(candidates) == 0 && r.Input.Text == "" {
		ms, err := e.o.Store.Find(ctx, q)
		if err != nil {
			return out, err
		}
		appendCandidates(ms)
	}
	if _, ok := e.o.Store.(relations.Store); ok && len(candidates) > 0 && len(candidates) < max {
		seeds := append([]memory.Memory(nil), candidates...)
		for _, seed := range seeds {
			links, err := e.KnowledgeRelations(ctx, RelationRequest{
				ListRequest: ListRequest{Tenant: sc.Tenant, As: as, User: sc.User, Agent: sc.Agent,
					Run: sc.Run, ValidAt: r.ValidAt, KnownAt: r.KnownAt, AllVersions: r.AllVersions},
				MemoryID: seed.ID, Depth: options.MaxGraphHops, MaxEdges: max})
			if err != nil {
				return out, err
			}
			for _, link := range links {
				ms, err := e.o.Store.Get(ctx, sc.Tenant, []string{link.Source, link.Target})
				if err != nil {
					return out, err
				}
				appendCandidates(ms)
			}
			if len(candidates) == max {
				break
			}
		}
	}
	temporal := store.Query{ValidAt: r.ValidAt, KnownAt: r.KnownAt}
	for _, m := range candidates {
		if temporal.Admits(&m, e.o.Now()) {
			out.Current = append(out.Current, m)
		} else {
			out.Historical = append(out.Historical, m)
		}
		a, err := memory.ReadAssertion(m)
		if err != nil {
			return out, err
		}
		if a != nil && r.Input.Assertion != nil && a.Subject == r.Input.Assertion.Subject &&
			a.Predicate == r.Input.Assertion.Predicate && a.Object != r.Input.Assertion.Object {
			out.Conflicts = append(out.Conflicts, m)
		}
		d, err := memory.ReadDecision(m)
		if err != nil {
			return out, err
		}
		if d != nil {
			out.Decisions = append(out.Decisions, m)
		}
	}
	if rs, ok := e.o.Store.(relations.Store); ok && len(candidates) > 0 {
		ids := make([]string, len(candidates))
		for i, m := range candidates {
			ids[i] = m.ID
		}
		links, err := rs.RelationshipSets(ctx, sc.Tenant, ids)
		if err != nil {
			return out, err
		}
		for _, l := range links {
			if seen[l.Source] && seen[l.Target] && relationAdmits(l, r.ValidAt, r.KnownAt, e.o.Now(), r.AllVersions) {
				out.Relations = append(out.Relations, l)
				if l.Relation == "contradicts" || l.Confidence < 0.5 {
					out.Unresolved = append(out.Unresolved, l)
				}
				if l.Relation == "supports" {
					for _, m := range candidates {
						if m.ID == l.Source {
							out.SupportingEvidence = append(out.SupportingEvidence, m)
							break
						}
					}
				}
				if len(out.Relations) == 100 {
					break
				}
			}
		}
	}
	return out, nil
}

func (e *Engine) validateKnowledgeEvidence(ctx context.Context, r KnowledgeRequest) error {
	as, err := e.knowledgePrincipal(r)
	if err != nil {
		return err
	}
	var references []string
	if r.Input.Assertion != nil {
		references = append(references, r.Input.Assertion.EvidenceIDs...)
	}
	if r.Input.Decision != nil {
		references = append(references, r.Input.Decision.EvidenceIDs...)
	}
	for _, id := range references {
		m, err := e.Get(ctx, r.Input.Scope.Tenant, as, id)
		if err != nil {
			return fmt.Errorf("%w: inaccessible evidence memory %q", memory.ErrInvalid, id)
		}
		if !knowledgeAudienceMatches(m, r.Input) {
			return fmt.Errorf("%w: evidence visibility differs from the proposed assertion", memory.ErrInvalid)
		}
	}
	for _, ev := range r.Input.Evidence {
		eps, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: r.Input.Scope.Tenant, IDs: []string{ev.Episode}, Limit: 1})
		if err != nil {
			return err
		}
		if len(eps) != 1 || eps[0].Status == memory.Deleted || ev.Part < 0 || ev.Part >= len(eps[0].Parts) ||
			ev.Start < 0 || ev.End < ev.Start || ev.End > len(eps[0].Parts[ev.Part].Text) {
			return fmt.Errorf("%w: invalid evidence reference", memory.ErrInvalid)
		}
		ep := eps[0]
		if !as.Admin && (ep.User != r.Input.Scope.User || ep.Agent != r.Input.Scope.Agent) {
			return fmt.Errorf("%w: evidence belongs to another scope", memory.ErrInvalid)
		}
	}
	return nil
}

func (e *Engine) evaluateKnowledge(ctx context.Context, r KnowledgeRequest, state knowledge.State) (*KnowledgeResult, error) {
	if err := e.validateKnowledgeEvidence(ctx, r); err != nil {
		return nil, err
	}
	state = knowledgePlanningAudience(state, r.Input)
	options, err := e.knowledgeOptions(ctx, r.Input.Scope.Tenant)
	if err != nil {
		return nil, err
	}
	planner := options.Planner
	if planner == nil {
		planner = knowledge.Deterministic{}
	}
	start := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	p, err := planner.Plan(callCtx, r.Input, state)
	fallbackError := ""
	if err != nil {
		if !options.Fallback || ctx.Err() != nil {
			return nil, fmt.Errorf("knowledge planner: %w", err)
		}
		fallbackError = "planner_failed:deterministic_fallback"
		e.o.Logger.Warn("knowledge planner failed; using deterministic fallback", "provider", fmt.Sprintf("%T", planner))
		p, err = (knowledge.Deterministic{}).Plan(ctx, r.Input, state)
		if err != nil {
			return nil, err
		}
	}
	as, err := e.knowledgePrincipal(r)
	if err != nil {
		return nil, err
	}
	proposed := p
	p, err = knowledge.Validate(r.Input, p, state, as)
	if err != nil {
		var detail *knowledge.ValidationError
		errors.As(err, &detail)
		return &KnowledgeResult{ProposedPlan: &proposed, Scope: r.Input.Scope, Mode: options.Mode,
			Status: "failed", Validation: "failed", ValidationError: detail, Error: err.Error(), Duration: time.Since(start)}, err
	}
	hash, err := operationHash(r.Input)
	if err != nil {
		return nil, err
	}
	p.OperationKey, p.InputHash = r.Input.OperationKey, hash
	return &KnowledgeResult{Plan: p, ProposedPlan: &proposed, Validation: "passed", Scope: r.Input.Scope, Mode: options.Mode,
		Status: "proposed", Duration: time.Since(start), Error: fallbackError}, nil
}

// EvaluateKnowledge is side-effect-free, including in shadow mode.
func (e *Engine) EvaluateKnowledge(ctx context.Context, r KnowledgeRequest) (*KnowledgeResult, error) {
	options, err := e.knowledgeOptions(ctx, r.Input.Scope.Tenant)
	if err != nil {
		return nil, err
	}
	if options.Mode == "disabled" {
		return nil, fmt.Errorf("%w: knowledge intelligence is disabled", memory.ErrInvalid)
	}
	state, err := e.KnowledgeState(ctx, r)
	if err != nil {
		return nil, err
	}
	result, err := e.evaluateKnowledge(ctx, r, state)
	if result != nil {
		result.Mode = options.Mode
	}
	return result, err
}

// CommitKnowledge reloads state and revalidates the plan on every conflict.
// The durable plan receipt prevents another model call after a worker crash.
func (e *Engine) CommitKnowledge(ctx context.Context, r KnowledgeRequest) (*KnowledgeResult, error) {
	if _, err := e.knowledgePrincipal(r); err != nil {
		return nil, err
	}
	options, err := e.knowledgeOptions(ctx, r.Input.Scope.Tenant)
	if err != nil {
		return nil, err
	}
	if options.Mode == "disabled" || r.Input.OperationKey == "" || len(r.Input.OperationKey) > 200 {
		return nil, fmt.Errorf("%w: enabled knowledge intelligence and an operation key (1..200 bytes) are required", memory.ErrInvalid)
	}
	hash, err := operationHash(r.Input)
	if err != nil {
		return nil, err
	}
	var result KnowledgeResult
	key := "knowledge.commit:" + r.Input.OperationKey
	if replayed, err := e.replayOperation(ctx, r.Input.Scope.Tenant, key, hash, &result); replayed || err != nil {
		result.Replayed = replayed
		return &result, err
	}
	receipt, err := e.write(ctx, func() (*store.Batch, error) {
		state, err := e.KnowledgeState(ctx, r)
		if err != nil {
			return nil, err
		}
		stateHash, err := knowledgeStateHash(state)
		if err != nil {
			return nil, err
		}
		planKey := "knowledge.plan:" + r.Input.OperationKey + ":" + stateHash
		replayed, err := e.replayOperation(ctx, r.Input.Scope.Tenant, planKey, hash, &result)
		if err != nil {
			return nil, err
		}
		if !replayed {
			res, err := e.evaluateKnowledge(ctx, r, state)
			if err != nil {
				class := "planner_failed"
				var validationErr *knowledge.ValidationError
				if errors.Is(err, memory.ErrInvalid) || errors.As(err, &validationErr) {
					class = "validation_failed"
				} else if errors.Is(err, context.DeadlineExceeded) {
					class = "planner_timeout"
				} else if errors.Is(err, context.Canceled) {
					class = "canceled"
				}
				failure := KnowledgeResult{Scope: r.Input.Scope, Mode: options.Mode,
					Status: "failed", Error: class,
					Plan: knowledge.Plan{OperationKey: r.Input.OperationKey, InputHash: hash}}
				if res != nil {
					failure.ProposedPlan, failure.Validation = res.ProposedPlan, res.Validation
					failure.Duration = res.Duration
					failure.ValidationError = res.ValidationError
				}
				traceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, traceErr := e.o.Store.Write(traceCtx, &store.Batch{Tenant: r.Input.Scope.Tenant,
					OperationKey: "knowledge.failure:" + r.Input.OperationKey, OperationHash: hash, Result: payload(failure)})
				return nil, errors.Join(err, traceErr)
			}
			result = *res
			raw, err := json.Marshal(result)
			if err != nil {
				return nil, err
			}
			saved, err := e.o.Store.Write(ctx, &store.Batch{Tenant: r.Input.Scope.Tenant,
				OperationKey: planKey, OperationHash: hash, Result: raw})
			if err != nil {
				return nil, err
			}
			if saved.Replayed {
				if err := json.Unmarshal(saved.Result, &result); err != nil {
					return nil, err
				}
			}
		}
		as, err := e.knowledgePrincipal(r)
		if err != nil {
			return nil, err
		}
		validated, err := knowledge.Validate(r.Input, result.Plan, state, as)
		if err != nil {
			return nil, err
		}
		result.Plan, result.Validation = validated, "passed"
		b := &store.Batch{Tenant: r.Input.Scope.Tenant, OperationKey: key, OperationHash: hash}
		result.Mode = options.Mode
		if options.Mode == "enforced" {
			result.Changes, err = e.rec.PlanKnowledge(ctx, b.Tenant, validated, b)
			if err != nil {
				return nil, err
			}
			e.followUp(b, result.Changes)
			result.Status = "committed"
		} else {
			result.Status = "shadow"
		}
		b.Result, err = json.Marshal(result)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if receipt.Replayed {
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return nil, err
		}
		result.Replayed = true
	}
	return &result, nil
}

func (e *Engine) KnowledgeTrace(ctx context.Context, tenant string, as memory.Principal, key string) (*KnowledgeResult, error) {
	ops, ok := e.o.Store.(store.Operations)
	if !ok {
		return nil, fmt.Errorf("%w: operation traces unsupported", memory.ErrInvalid)
	}
	r, err := ops.Operation(ctx, tenant, "knowledge.commit:"+key)
	if errors.Is(err, memory.ErrNotFound) {
		r, err = ops.Operation(ctx, tenant, "knowledge.failure:"+key)
	}
	if err != nil {
		return nil, err
	}
	var result KnowledgeResult
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return nil, err
	}
	if !as.Admin && (as.User != result.Scope.User || as.Agent != result.Scope.Agent) {
		return nil, memory.ErrNotFound
	}
	return &result, nil
}

const JobKnowledgeEvaluate = "knowledge.evaluate"
const JobKnowledgeReevaluate = "knowledge.reevaluate"

func (e *Engine) handleKnowledgeEvaluate(ctx context.Context, j store.Job) error {
	options, err := e.knowledgeOptions(ctx, j.Tenant)
	if err != nil {
		return err
	}
	if options.Mode == "disabled" {
		e.o.Logger.Info("knowledge job skipped by tenant policy", "job", j.ID)
		return nil
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(j.Payload, &body); err != nil || body.ID == "" {
		return jobs.Permanent(fmt.Errorf("%w: invalid knowledge job", memory.ErrInvalid))
	}
	ms, err := e.o.Store.Get(ctx, j.Tenant, []string{body.ID})
	if err != nil {
		return err
	}
	if len(ms) == 0 || !current(ms[0], e.o.Now()) {
		return nil
	}
	m := ms[0]
	req := KnowledgeRequest{As: memory.Principal{User: m.User, Agent: m.Agent},
		Input: knowledge.Input{OperationKey: "job:" + j.ID, Scope: m.Scope, Text: m.Text,
			Evidence: m.Source.Evidence, Trust: m.Source.Trust, Visibility: m.Visibility, Grants: append([]string(nil), m.Grants...)}}
	inputHash, err := operationHash(req.Input)
	if err != nil {
		return err
	}
	var prior KnowledgeResult
	if replayed, err := e.replayOperation(ctx, j.Tenant, "knowledge.commit:"+req.Input.OperationKey, inputHash, &prior); replayed || err != nil {
		return err
	}
	if options.Admission != nil {
		callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
		defer cancel()
		result, err := options.Admission.DecideAdmission(callCtx, knowledge.AdmissionInput{
			Scope: m.Scope, Text: m.Text, Trust: m.Source.Trust, Explicit: m.Source.Kind == "explicit"})
		if err != nil {
			return fmt.Errorf("knowledge admission: %w", err)
		}
		switch result {
		case knowledge.AdmissionProcess:
		case knowledge.AdmissionSkip, knowledge.AdmissionDefer, knowledge.AdmissionRequireConfirmation:
			hash, err := operationHash(req.Input)
			if err != nil {
				return err
			}
			e.o.Logger.Info("knowledge admission deferred interpretation", "job", j.ID, "decision", result)
			_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant,
				OperationKey: "knowledge.commit:" + req.Input.OperationKey, OperationHash: hash,
				Result: payload(KnowledgeResult{Scope: m.Scope, Mode: options.Mode, Status: "admission:" + string(result)})})
			return err
		default:
			return jobs.Permanent(fmt.Errorf("%w: unsupported admission result %q", memory.ErrInvalid, result))
		}
	}
	_, err = e.CommitKnowledge(ctx, req)
	return err
}

func knowledgeAudienceMatches(m memory.Memory, input knowledge.Input) bool {
	visibility := input.Visibility
	if visibility == "" {
		visibility = memory.Team
	}
	a, b := append([]string{}, m.Grants...), append([]string{}, input.Grants...)
	sort.Strings(a)
	sort.Strings(b)
	return m.Visibility == visibility && reflect.DeepEqual(a, b)
}

func knowledgePlanningAudience(state knowledge.State, input knowledge.Input) knowledge.State {
	allowed := map[string]bool{}
	filter := func(ms []memory.Memory) []memory.Memory {
		out := []memory.Memory{}
		for _, m := range ms {
			if knowledgeAudienceMatches(m, input) {
				out, allowed[m.ID] = append(out, m), true
			}
		}
		return out
	}
	state.Current, state.Historical = filter(state.Current), filter(state.Historical)
	state.Conflicts, state.Decisions = filter(state.Conflicts), filter(state.Decisions)
	state.SupportingEvidence = filter(state.SupportingEvidence)
	edges := func(links []relations.Link) []relations.Link {
		out := []relations.Link{}
		for _, l := range links {
			if allowed[l.Source] && allowed[l.Target] {
				out = append(out, l)
			}
		}
		return out
	}
	state.Relations, state.Unresolved = edges(state.Relations), edges(state.Unresolved)
	return state
}

func knowledgeStateHash(state knowledge.State) (string, error) {
	canonical := func(ms []memory.Memory) []memory.Memory {
		out := append([]memory.Memory(nil), ms...)
		for i := range out {
			out[i].Heat = 0
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	state.Current, state.Historical = canonical(state.Current), canonical(state.Historical)
	state.Conflicts, state.Decisions = canonical(state.Conflicts), canonical(state.Decisions)
	state.SupportingEvidence = canonical(state.SupportingEvidence)
	state.Relations = append([]relations.Link(nil), state.Relations...)
	sort.Slice(state.Relations, func(i, j int) bool { return state.Relations[i].ID < state.Relations[j].ID })
	state.Unresolved = append([]relations.Link(nil), state.Unresolved...)
	sort.Slice(state.Unresolved, func(i, j int) bool { return state.Unresolved[i].ID < state.Unresolved[j].ID })
	return operationHash(state)
}

// A source change records an auditable review notification, never silently
// retracts the dependent assertion.
func (e *Engine) handleKnowledgeReevaluate(ctx context.Context, j store.Job) error {
	var body struct {
		ID        string `json:"id"`
		Source    string `json:"source"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	if err := json.Unmarshal(j.Payload, &body); err != nil || body.ID == "" || body.Source == "" {
		return jobs.Permanent(fmt.Errorf("%w: invalid dependency job", memory.ErrInvalid))
	}
	ms, err := e.o.Store.Get(ctx, j.Tenant, []string{body.ID})
	if err != nil || len(ms) == 0 {
		return err
	}
	if !current(ms[0], e.o.Now()) {
		return nil
	}
	// Recording the durable notification is useful even with intelligence off.
	key := "knowledge.review:" + j.ID
	hash, err := operationHash(body)
	if err != nil {
		return err
	}
	var saved map[string]string
	if replayed, err := e.replayOperation(ctx, j.Tenant, key, hash, &saved); replayed || err != nil {
		return err
	}
	status, reason := "needs_review", "dependency_changed:needs_review"
	if body.Truncated {
		status, reason = "needs_review_truncated", "dependency_changed:needs_review:truncated"
		e.o.Logger.Warn("dependency traversal truncated; additional review required", "tenant", j.Tenant, "source", body.Source)
	}
	_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash,
		Result: payload(map[string]string{"status": status, "id": body.ID, "source": body.Source}),
		Events: []memory.Event{{MemoryID: body.ID, Action: memory.ActionEvidence, Related: body.Source,
			Reason: reason, At: e.o.Now()}}})
	return err
}

func relationAdmits(l relations.Link, valid, known, now time.Time, all bool) bool {
	if all {
		return true
	}
	if valid.IsZero() {
		valid = now
	}
	if known.IsZero() {
		known = now
	}
	return !l.At.After(known) && !l.ValidFrom.After(valid) &&
		(l.ValidTo == nil || valid.Before(*l.ValidTo)) &&
		(l.InvalidatedAt == nil || known.Before(*l.InvalidatedAt))
}

type RelationRequest struct {
	ListRequest
	MemoryID  string
	Direction string
	Types     []string
	Depth     int
	MaxEdges  int
}

// KnowledgeRelations enforces ACL and temporal conditions on every endpoint,
// bounds both visited nodes and edges, and terminates on cycles.
func (e *Engine) KnowledgeRelations(ctx context.Context, r RelationRequest) ([]relations.Link, error) {
	rs, ok := e.o.Store.(relations.Store)
	if !ok {
		return nil, fmt.Errorf("%w: relationships unsupported", memory.ErrInvalid)
	}
	if r.Depth == 0 {
		r.Depth = 1
	}
	options, err := e.knowledgeOptions(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	if r.MaxEdges == 0 {
		r.MaxEdges = 100
	}
	if r.Depth < 1 || r.Depth > options.MaxGraphHops || r.MaxEdges < 1 || r.MaxEdges > 400 ||
		(r.Direction != "" && r.Direction != "both" && r.Direction != "in" && r.Direction != "out") {
		return nil, fmt.Errorf("%w: invalid relationship bounds or direction", memory.ErrInvalid)
	}
	allowed := map[string]bool{}
	for _, typ := range r.Types {
		if !relations.ValidRelation(typ) {
			return nil, fmt.Errorf("%w: invalid relation type", memory.ErrInvalid)
		}
		allowed[typ] = true
	}
	root, err := e.Get(ctx, r.Tenant, r.As, r.MemoryID)
	if err != nil {
		return nil, err
	}
	q, err := e.query(ctx, r.Tenant, r.As, r.User, r.Agent, r.Run, r.Filter)
	if err != nil {
		return nil, err
	}
	q.ValidAt, q.KnownAt, q.AllVersions = r.ValidAt, r.KnownAt, r.AllVersions
	admitted := func(m memory.Memory) bool { return q.Filter.Match(&m) && q.Admits(&m, e.o.Now()) }
	if !admitted(root) {
		return []relations.Link{}, nil
	}
	out := []relations.Link{}
	frontier, visited, edges := []string{root.ID}, map[string]bool{root.ID: true}, map[string]bool{}
	for depth := 0; depth < r.Depth && len(frontier) > 0; depth++ {
		links, err := rs.RelationshipSets(ctx, r.Tenant, frontier)
		if err != nil {
			return nil, err
		}
		this := map[string]bool{}
		for _, id := range frontier {
			this[id] = true
		}
		frontier = nil
		for _, l := range links {
			if edges[l.ID] || len(allowed) > 0 && !allowed[l.Relation] ||
				!relationAdmits(l, r.ValidAt, r.KnownAt, e.o.Now(), r.AllVersions) ||
				r.Direction == "out" && !this[l.Source] || r.Direction == "in" && !this[l.Target] {
				continue
			}
			ms, err := e.o.Store.Get(ctx, r.Tenant, []string{l.Source, l.Target})
			if err != nil {
				return nil, err
			}
			if len(ms) != 2 || !admitted(ms[0]) || !admitted(ms[1]) {
				continue
			}
			edges[l.ID], out = true, append(out, l)
			for _, m := range ms {
				if !visited[m.ID] && len(visited) < 400 {
					visited[m.ID], frontier = true, append(frontier, m.ID)
				}
			}
			if len(out) >= r.MaxEdges {
				return out, nil
			}
		}
	}
	return out, nil
}
