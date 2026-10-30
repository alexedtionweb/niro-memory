package niromem

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/textutil"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Turn is one conversation turn, event or tool output to append to a run.
type Turn struct {
	// ClientMsgID makes Append idempotent: re-sending a turn with the same
	// id returns the stored turn instead of a duplicate.
	ClientMsgID string         `json:"client_msg_id,omitempty"`
	Role        string         `json:"role"` // user, assistant, system, tool, event
	Actor       string         `json:"actor,omitempty"`
	Text        string         `json:"text,omitempty"` // shorthand for one text part
	Parts       []PartInput    `json:"parts,omitempty"`
	OccurredAt  time.Time      `json:"occurred_at,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// PartInput is a part to store. Bytes (base64 in JSON) are saved in the blob
// store and replaced by a content-addressed reference.
type PartInput struct {
	memory.Part
	Bytes   []byte   `json:"bytes,omitempty"`
	Vectors []Vector `json:"vectors,omitempty"` // embeddings of this part (e.g. CLIP image vectors)
}

// AppendOptions controls Append.
type AppendOptions struct {
	// Wait processes the run synchronously (rules, extensions) and returns
	// the resulting changes. By default processing happens in the background.
	Wait bool
}

// AppendResult is what Append stored.
type AppendResult struct {
	Episodes   []memory.Episode  `json:"episodes"`
	Duplicates []bool            `json:"duplicates"`
	Changes    []pipeline.Change `json:"changes,omitempty"`
}

var roles = map[string]bool{"user": true, "assistant": true, "system": true, "tool": true, "event": true}

// Append stores turns of one run in one transaction and schedules their
// processing. The hot path is a single write; nothing slow runs inline
// unless opts.Wait is set.
func (e *Engine) Append(ctx context.Context, scope memory.Scope, turns []Turn, opts AppendOptions) (res *AppendResult, err error) {
	defer e.observe("append", time.Now(), err)
	if scope.Tenant == "" || scope.Run == "" {
		return nil, fmt.Errorf("%w: append needs a tenant and a run", memory.ErrInvalid)
	}
	if len(turns) == 0 || len(turns) > e.o.Limits.MaxTurnsBatch {
		return nil, fmt.Errorf("%w: 1..%d turns per call", memory.ErrInvalid, e.o.Limits.MaxTurnsBatch)
	}
	s, err := e.Settings(ctx, scope.Tenant)
	if err != nil {
		return nil, err
	}
	models, _ := e.models(s)
	now := e.o.Now()
	b := &store.Batch{Tenant: scope.Tenant}
	var partVecs [][][]Vector // per turn, per part
	for _, t := range turns {
		ep, vecs, err := e.episode(ctx, scope, t, now, models)
		if err != nil {
			return nil, err
		}
		b.Episodes = append(b.Episodes, ep)
		partVecs = append(partVecs, vecs)
	}
	b.Jobs = []store.Job{{Kind: JobProcess, Key: scope.Run, Payload: payload(runPayload{Run: scope.Run}), NotBefore: now.Add(e.o.Debounce)}}
	rec, err := e.o.Store.Write(ctx, b)
	if err != nil {
		return nil, err
	}
	res = &AppendResult{}
	vb := &store.Batch{Tenant: scope.Tenant}
	for i, r := range rec.Episodes {
		res.Episodes = append(res.Episodes, r.Episode)
		res.Duplicates = append(res.Duplicates, r.Duplicate)
		if r.Duplicate {
			continue
		}
		var pts []vector.Point
		for pi, vs := range partVecs[i] {
			for _, v := range vs {
				pts = append(pts, vector.Point{Tenant: scope.Tenant, Model: v.Model, Modality: v.Modality,
					User: scope.User, Agent: scope.Agent, Run: scope.Run,
					Target: vector.Target{Kind: vector.TargetPart, ID: r.Episode.ID, Part: pi}, Values: v.Values})
			}
		}
		e.addPoints(vb, pts)
	}
	if !vb.Empty() {
		// Vectors are derived data: a failure here leaves the turns stored.
		if _, err := e.o.Store.Write(ctx, vb); err != nil {
			return res, fmt.Errorf("turns stored, part vectors failed: %w", err)
		}
	}
	if opts.Wait {
		st, err := e.proc.ProcessRun(ctx, scope.Tenant, scope.Run)
		res.Changes = st.Changes
		return res, err
	}
	return res, nil
}

// episode validates a turn and stores inline media in the blob store.
func (e *Engine) episode(ctx context.Context, scope memory.Scope, t Turn, now time.Time, models map[string]vector.Model) (memory.Episode, [][]Vector, error) {
	if !roles[t.Role] {
		return memory.Episode{}, nil, fmt.Errorf("%w: role %q must be user, assistant, system, tool or event", memory.ErrInvalid, t.Role)
	}
	parts := t.Parts
	if t.Text != "" {
		parts = append([]PartInput{{Part: memory.Part{Kind: memory.PartText, Text: t.Text}}}, parts...)
	}
	if len(parts) == 0 || len(parts) > e.o.Limits.MaxParts {
		return memory.Episode{}, nil, fmt.Errorf("%w: 1..%d parts per turn", memory.ErrInvalid, e.o.Limits.MaxParts)
	}
	ep := memory.Episode{Scope: scope, ClientMsgID: t.ClientMsgID, Role: t.Role, Actor: t.Actor,
		OccurredAt: t.OccurredAt, Metadata: t.Metadata, Status: memory.Final}
	if ep.OccurredAt.IsZero() {
		ep.OccurredAt = now
	}
	vecs := make([][]Vector, len(parts))
	for i, p := range parts {
		part := p.Part
		for _, s := range []string{part.Text, part.Caption, part.DerivedText} {
			if len(s) > e.o.Limits.MaxTextBytes {
				return ep, nil, fmt.Errorf("%w: part %d text is %d bytes, limit %d", memory.ErrInvalid, i, len(s), e.o.Limits.MaxTextBytes)
			}
		}
		if len(p.Bytes) > 0 {
			if e.o.Blob == nil {
				return ep, nil, fmt.Errorf("%w: part %d has bytes but no blob store is configured", memory.ErrInvalid, i)
			}
			if int64(len(p.Bytes)) > e.o.Limits.MaxPartBytes {
				return ep, nil, fmt.Errorf("%w: part %d is %d bytes, limit %d", memory.ErrInvalid, i, len(p.Bytes), e.o.Limits.MaxPartBytes)
			}
			if part.Mime == "" {
				return ep, nil, fmt.Errorf("%w: part %d needs a mime type", memory.ErrInvalid, i)
			}
			ref, err := e.o.Blob.Put(ctx, scope.Tenant, p.Bytes, part.Mime)
			if err != nil {
				return ep, nil, err
			}
			part.Blob = &ref
		}
		if part.Kind == "" {
			part.Kind = kindOf(part.Mime)
		}
		if err := checkVectors(models, p.Vectors); err != nil {
			return ep, nil, err
		}
		ep.Parts = append(ep.Parts, part)
		vecs[i] = p.Vectors
	}
	return ep, vecs, nil
}

func kindOf(mime string) memory.PartKind {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return memory.PartImage
	case strings.HasPrefix(mime, "audio/"):
		return memory.PartAudio
	case strings.HasPrefix(mime, "video/"):
		return memory.PartVideo
	case mime == "":
		return memory.PartText
	}
	return memory.PartFile
}

// TurnsRequest pages through a run's history.
type TurnsRequest struct {
	Tenant         string `json:"-"`
	Run            string `json:"run_id"`
	Last           int    `json:"last,omitempty"`       // the most recent N turns
	BeforeSeq      int64  `json:"before_seq,omitempty"` // exclusive
	AfterSeq       int64  `json:"after_seq,omitempty"`  // exclusive
	IncludeDeleted bool   `json:"include_deleted,omitempty"`
}

// Turns returns turns of a run in order.
func (e *Engine) Turns(ctx context.Context, r TurnsRequest) ([]memory.Episode, error) {
	q := store.EpisodeQuery{Tenant: r.Tenant, Run: r.Run, BeforeSeq: r.BeforeSeq, AfterSeq: r.AfterSeq, IncludeDeleted: r.IncludeDeleted}
	if r.Last > 0 {
		q.Desc, q.Limit = true, r.Last
	}
	eps, err := e.o.Store.Episodes(ctx, q)
	if q.Desc {
		for i, j := 0, len(eps)-1; i < j; i, j = i+1, j-1 {
			eps[i], eps[j] = eps[j], eps[i]
		}
	}
	return eps, err
}

// Window is a prompt-ready view of a conversation: the rolling summary of
// older turns plus the most recent turns that fit a token budget.
type Window struct {
	Summary     string           `json:"summary,omitempty"`
	SummaryUpto int64            `json:"summary_upto,omitempty"`
	Omitted     int64            `json:"omitted"` // turns neither verbatim nor (by seq) covered by the summary
	Turns       []memory.Episode `json:"turns"`
	Tokens      int              `json:"tokens"`
}

// EstimateTokens is a cheap, model-independent token estimate (~4 bytes per
// token). Pass a real counter to Window when exact budgets matter.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// Window returns the summary and the latest turns within tokens. count may
// be nil (EstimateTokens).
func (e *Engine) Window(ctx context.Context, tenant, run string, tokens int, count func(string) int) (*Window, error) {
	if count == nil {
		count = EstimateTokens
	}
	r, err := e.o.Store.Run(ctx, tenant, run)
	if err != nil {
		return nil, err
	}
	w := &Window{Summary: r.Summary, SummaryUpto: r.SummaryUpto}
	budget := tokens - count(r.Summary)
	eps, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, Run: run, Desc: true, Limit: 500, AfterSeq: r.SummaryUpto})
	if err != nil {
		return nil, err
	}
	w.Tokens = tokens - budget
	for _, ep := range eps {
		n := count(ep.Text()) + 4 // role and separators
		if n > budget {
			break
		}
		budget -= n
		w.Tokens += n
		w.Turns = append([]memory.Episode{ep}, w.Turns...)
	}
	first := r.LastSeq + 1
	if len(w.Turns) > 0 {
		first = w.Turns[0].Seq
	}
	w.Omitted = max(first-1-r.SummaryUpto, 0)
	return w, nil
}

// SetSummary stores the rolling summary of a run, covering turns up to and
// including seq upto. Summaries are written by the application or by a
// summarizer extension; the engine never invents them.
func (e *Engine) SetSummary(ctx context.Context, tenant, run, text string, upto int64) error {
	_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant, Summaries: []store.Summary{{Run: run, Text: text, Upto: upto}}})
	return err
}

// HistoryQuery searches raw turns.
type HistoryQuery struct {
	Tenant       string           `json:"-"`
	As           memory.Principal `json:"-"`
	User         string           `json:"user_id,omitempty"`
	Agent        string           `json:"agent_id,omitempty"`
	Run          string           `json:"run_id,omitempty"`
	Query        string           `json:"query,omitempty"`
	Roles        []string         `json:"roles,omitempty"`
	Since, Until time.Time        `json:"-"`
	Limit        int              `json:"limit,omitempty"`
}

// SearchHistory finds turns by text (BM25) or lists them by time. A
// non-admin principal with a user only sees that user's turns.
func (e *Engine) SearchHistory(ctx context.Context, q HistoryQuery) (hits []store.EpisodeHit, err error) {
	defer e.observe("search_history", time.Now(), err)
	if !q.As.Admin && q.As.User != "" {
		q.User = q.As.User
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 20
	}
	eq := store.EpisodeQuery{Tenant: q.Tenant, User: q.User, Agent: q.Agent, Run: q.Run, Roles: q.Roles, Since: q.Since, Until: q.Until, Limit: q.Limit, Desc: true}
	terms := textutil.Terms(q.Query, 32)
	if lx, ok := e.o.Store.(store.Lexical); ok && len(terms) > 0 {
		return lx.SearchEpisodes(ctx, store.EpisodeTextQuery{EpisodeQuery: eq, Terms: terms})
	}
	eps, err := e.o.Store.Episodes(ctx, eq)
	for _, ep := range eps {
		if len(terms) == 0 || containsAny(strings.ToLower(ep.Text()), terms) {
			hits = append(hits, store.EpisodeHit{Episode: ep})
		}
	}
	return hits, err
}

func containsAny(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// EditTurn replaces the parts of a turn. Memories derived only from it are
// invalidated and it is extracted again in the background.
func (e *Engine) EditTurn(ctx context.Context, tenant, episodeID string, parts []PartInput) error {
	eps, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, IDs: []string{episodeID}})
	if err != nil {
		return err
	}
	if len(eps) == 0 {
		return fmt.Errorf("turn %s: %w", episodeID, memory.ErrNotFound)
	}
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return err
	}
	models, _ := e.models(s)
	ep, _, err := e.episode(ctx, eps[0].Scope, Turn{Role: eps[0].Role, Parts: parts}, e.o.Now(), models)
	if err != nil {
		return err
	}
	return e.changeTurn(ctx, tenant, store.EpisodeEdit{ID: episodeID, Status: memory.Edited, Parts: ep.Parts})
}

// DeleteTurn hides a turn (it is kept as a tombstone for history) and
// invalidates memories derived only from it.
func (e *Engine) DeleteTurn(ctx context.Context, tenant, episodeID string) error {
	return e.changeTurn(ctx, tenant, store.EpisodeEdit{ID: episodeID, Status: memory.Deleted})
}

func (e *Engine) changeTurn(ctx context.Context, tenant string, ed store.EpisodeEdit) error {
	_, err := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant, EpisodeEdits: []store.EpisodeEdit{ed},
		Jobs: []store.Job{{Kind: JobRederive, Key: ed.ID, Payload: payload(runPayload{Episode: ed.ID})}}})
	return err
}

// Blob returns the bytes of a stored part.
func (e *Engine) Blob(ctx context.Context, tenant, sha256 string) ([]byte, error) {
	if e.o.Blob == nil {
		return nil, fmt.Errorf("no blob store: %w", memory.ErrNotFound)
	}
	return e.o.Blob.Get(ctx, tenant, sha256)
}

// Process runs extraction for a run now (what the background job does).
func (e *Engine) Process(ctx context.Context, tenant, run string) (pipeline.Stats, error) {
	return e.proc.ProcessRun(ctx, tenant, run)
}
