// Package postgres is the PostgreSQL connector: a primary store for
// stateless, horizontally scaled deployments.
//
//   - Full-text search with tsvector/GIN ('simple' configuration, language
//     neutral; ranking is ts_rank, reported as approximate BM25).
//   - Vectors stored as float4[] (source of truth). With the pgvector
//     extension, EnsureVectorIndex creates one HNSW index per model and
//     Search uses it with iterative scans; without it, Search scans exactly.
//   - Jobs claimed with FOR UPDATE SKIP LOCKED and per-key advisory locks.
//   - Optional row-level security (Options.RLS) as defense in depth.
//
// Schema migrations run on Open under an advisory lock, so any number of
// nodes may start concurrently.
package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/sqlgen"
	"github.com/alexedtionweb/niro-memory/textutil"
	"github.com/alexedtionweb/niro-memory/vector"
)

//go:embed schema.sql
var schema string

//go:embed rls.sql
var rlsSchema string

// Options configures the store.
type Options struct {
	// DSN is a PostgreSQL connection string.
	DSN string
	// Schema, when set, is created and used as search_path (isolated test
	// schemas, or several deployments in one database).
	Schema string
	// MaxConns sizes the pool (default: pgx default, max(4, NumCPU)).
	MaxConns int32
	// RLS enables row-level security policies on tenant data and sets the
	// tenant on every transaction. The connecting role must not own the
	// tables or must not have BYPASSRLS for the policies to apply.
	RLS bool
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Store is a PostgreSQL-backed store.Store.
type Store struct {
	pool     *pgxpool.Pool
	now      func() time.Time
	rls      bool
	pgvector bool
	d        dialect
	indexMu  sync.Mutex
	indexed  map[string]bool // models with an HNSW index
}

var (
	_ store.Store         = (*Store)(nil)
	_ store.Lexical       = (*Store)(nil)
	_ store.Entities      = (*Store)(nil)
	_ store.VectorCatalog = (*Store)(nil)
	_ store.Admin         = (*Store)(nil)
	_ store.SQLConsole    = (*Store)(nil)
	_ vector.Index        = (*Store)(nil)
)

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Open connects, migrates and detects pgvector.
func Open(ctx context.Context, o Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(o.DSN)
	if err != nil {
		return nil, err
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if o.Schema != "" {
		if !identRe.MatchString(o.Schema) {
			return nil, fmt.Errorf("%w: schema name %q", memory.ErrInvalid, o.Schema)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = o.Schema
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, now: o.Now, rls: o.RLS, indexed: map[string]bool{}}
	if err := s.migrate(ctx, o.Schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context, schemaName string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('niromem_migrate'))`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('niromem_migrate'))`)
	if schemaName != "" {
		if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schemaName); err != nil {
			return err
		}
	}
	// pgvector is optional: use it when installed or installable.
	conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&n); err == nil && n > 0 {
		s.pgvector = true
	}
	if _, err := conn.Exec(ctx, schema); err != nil {
		return err
	}
	if s.rls {
		if _, err := conn.Exec(ctx, rlsSchema); err != nil {
			return fmt.Errorf("rls: %w", err)
		}
	}
	return nil
}

// Close closes the pool.
func (s *Store) Close() error { s.pool.Close(); return nil }

// Capabilities reports approximate BM25 (ts_rank).
func (s *Store) Capabilities() store.Capabilities { return store.Capabilities{BM25: "approx"} }

// HasPGVector reports whether the pgvector extension is available.
func (s *Store) HasPGVector() bool { return s.pgvector }

// q is what both a pool and a transaction can do.
type q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// read runs fn on the pool, or in a read-only transaction scoped to the
// tenant when RLS is enabled.
func (s *Store) read(ctx context.Context, tenant string, fn func(q) error) error {
	if !s.rls {
		return fn(s.pool)
	}
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('niromem.tenant', $1, true)`, tenant); err != nil {
			return err
		}
		return fn(tx)
	})
}

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{memory.ErrConflict}, a...)...)
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func js(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func jsList(v []string) []byte {
	if v == nil {
		return []byte("[]")
	}
	return js(v)
}

func metaJSON(m map[string]any) []byte {
	if m == nil {
		return []byte("{}")
	}
	return js(m)
}

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := utc(*t)
	return &v
}

// Write applies the batch in one transaction.
func (s *Store) Write(ctx context.Context, b *store.Batch) (*store.Receipt, error) {
	if b.Tenant == "" {
		return nil, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	var rec *store.Receipt
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if s.rls {
			if _, err := tx.Exec(ctx, `SELECT set_config('niromem.tenant', $1, true)`, b.Tenant); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "niromem.write:"+b.Tenant); err != nil {
			return err
		}
		var err error
		rec, err = (&writer{tx: tx, ctx: ctx, t: b.Tenant, now: utc(s.now())}).apply(b)
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("%v", err)
		}
		return nil, err
	}
	return rec, nil
}

type writer struct {
	tx           pgx.Tx
	ctx          context.Context
	t            string
	now          time.Time
	relationJobs []store.Job
}

func (w *writer) exec(sql string, args ...any) (int64, error) {
	tag, err := w.tx.Exec(w.ctx, sql, args...)
	return tag.RowsAffected(), err
}

func (w *writer) apply(b *store.Batch) (*store.Receipt, error) {
	if rec, err := w.replay(b); rec != nil || err != nil {
		return rec, err
	}
	if err := w.checkBatch(b); err != nil {
		return nil, err
	}
	for _, p := range b.Vectors {
		if vector.Normalize(append([]float32(nil), p.Values...)) != nil || p.Model == "" || p.Target.ID == "" {
			return nil, fmt.Errorf("%w: bad vector for %s", memory.ErrInvalid, p.Target.Key())
		}
	}
	for _, c := range b.Closes {
		at := w.now
		if c.ClosedAt != nil {
			at = utc(*c.ClosedAt)
		}
		n, err := w.exec(`UPDATE memories SET valid_to = COALESCE($1, valid_to), invalidated_at = COALESCE($2, invalidated_at),
			closed_at = CASE WHEN $1::timestamptz IS NOT NULL THEN $3 ELSE closed_at END
			WHERE tenant = $4 AND id = $5 AND valid_to IS NULL AND invalidated_at IS NULL`, utcPtr(c.ValidTo), utcPtr(c.InvalidatedAt), at, w.t, c.ID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, conflict("memory %s is not open", c.ID)
		}
	}
	for i := range b.Memories {
		if err := w.insertMemory(&b.Memories[i]); err != nil {
			return nil, err
		}
	}
	if err := w.relations(b); err != nil {
		return nil, err
	}
	for _, tc := range b.Touches {
		if _, err := w.exec(`UPDATE memories SET heat = heat + $1,
			source = jsonb_set(source, '{evidence}', COALESCE(source->'evidence', '[]'::jsonb) || $2::jsonb)
			WHERE tenant = $3 AND id = $4`, tc.Heat, evidence(tc.Evidence), w.t, tc.ID); err != nil {
			return nil, err
		}
	}
	for _, id := range b.Deletes {
		for _, sql := range []string{
			`UPDATE entities SET mentions = mentions - 1 WHERE tenant = $1 AND id IN (SELECT entity_id FROM memory_entities WHERE tenant = $1 AND memory_id = $2)`,
			`DELETE FROM memory_entities WHERE tenant = $1 AND memory_id = $2`,
			`DELETE FROM embeddings WHERE tenant = $1 AND target_key = 'm:' || $2`,
			`UPDATE history SET old = '', new = '' WHERE tenant = $1 AND memory_id = $2`,
			`DELETE FROM relationship_sets WHERE tenant = $1 AND (anchor = $2 OR data @> jsonb_build_array(jsonb_build_object('target_id', $2::text)))`,
			`DELETE FROM knowledge_relations WHERE tenant = $1 AND (source = $2 OR target = $2)`,
			`DELETE FROM memories WHERE tenant = $1 AND id = $2`,
		} {
			if _, err := w.exec(sql, w.t, id); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range b.Events {
		if _, err := w.exec(`INSERT INTO history (tenant, memory_id, action, old, new, related, actor, reason, at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			w.t, e.MemoryID, string(e.Action), e.Old, e.New, e.Related, e.Actor, e.Reason, utc(e.At)); err != nil {
			return nil, err
		}
	}
	rec := &store.Receipt{Result: append(json.RawMessage(nil), b.Result...), OperationHash: b.OperationHash}
	for i := range b.Episodes {
		r, err := w.appendEpisode(&b.Episodes[i])
		if err != nil {
			return nil, err
		}
		rec.Episodes = append(rec.Episodes, r)
	}
	for _, ed := range b.EpisodeEdits {
		var err error
		if ed.Parts != nil {
			e := memory.Episode{Parts: ed.Parts}
			_, err = w.exec(`UPDATE episodes SET status = $1, parts = $2, text = $3, blobs = $4 WHERE tenant = $5 AND id = $6`,
				string(ed.Status), js(ed.Parts), e.Text(), blobList(ed.Parts), w.t, ed.ID)
		} else {
			_, err = w.exec(`UPDATE episodes SET status = $1 WHERE tenant = $2 AND id = $3`, string(ed.Status), w.t, ed.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, run := range b.DeleteRuns {
		for _, sql := range []string{
			`DELETE FROM embeddings WHERE tenant = $1 AND target_kind = 'p' AND target_id IN (SELECT id FROM episodes WHERE tenant = $1 AND run_id = $2)`,
			`DELETE FROM episodes WHERE tenant = $1 AND run_id = $2`,
			`DELETE FROM runs WHERE tenant = $1 AND run_id = $2`,
		} {
			if _, err := w.exec(sql, w.t, run); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range b.Cursors {
		n, err := w.exec(`UPDATE runs SET processed_seq = $1 WHERE tenant = $2 AND run_id = $3 AND processed_seq = $4`, c.To, w.t, c.Run, c.From)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, conflict("run %s cursor moved", c.Run)
		}
	}
	for _, sm := range b.Summaries {
		if _, err := w.exec(`UPDATE runs SET summary = $1, summary_upto = $2, updated_at = $3 WHERE tenant = $4 AND run_id = $5`, sm.Text, sm.Upto, w.now, w.t, sm.Run); err != nil {
			return nil, err
		}
	}
	for _, p := range b.Vectors {
		v := append([]float32(nil), p.Values...)
		_ = vector.Normalize(v)
		if _, err := w.exec(`INSERT INTO embeddings (tenant, model, target_key, target_kind, target_id, part, modality, user_id, agent_id, run_id, dims, vec)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant, model, target_key) DO UPDATE SET modality = excluded.modality, user_id = excluded.user_id,
			agent_id = excluded.agent_id, run_id = excluded.run_id, dims = excluded.dims, vec = excluded.vec`,
			w.t, p.Model, p.Target.Key(), string(p.Target.Kind), p.Target.ID, p.Target.Part, p.Modality, p.User, p.Agent, p.Run, len(v), v); err != nil {
			return nil, err
		}
	}
	for _, tg := range b.VectorDeletes {
		if _, err := w.exec(`DELETE FROM embeddings WHERE tenant = $1 AND target_key = $2`, w.t, tg.Key()); err != nil {
			return nil, err
		}
	}
	for _, a := range b.Aliases {
		if _, err := w.exec(`INSERT INTO entity_aliases (tenant, alias, entity_key, name)
			SELECT $1, $2, key, $3 FROM entities WHERE tenant = $1 AND key = $4
			ON CONFLICT (tenant, alias) DO UPDATE SET entity_key = excluded.entity_key, name = excluded.name`,
			w.t, memory.EntityKey(a.Name), a.Name, a.EntityKey); err != nil {
			return nil, err
		}
	}
	for _, j := range b.Jobs {
		if err := w.enqueue(j); err != nil {
			return nil, err
		}
	}
	for _, j := range w.relationJobs {
		if err := w.enqueue(j); err != nil {
			return nil, err
		}
	}
	if b.Settings != nil {
		if _, err := w.exec(`INSERT INTO settings (tenant, doc) VALUES ($1, $2) ON CONFLICT (tenant) DO UPDATE SET doc = excluded.doc`, w.t, b.Settings); err != nil {
			return nil, err
		}
	}
	if err := w.redactOperations(b.Deletes); err != nil {
		return nil, err
	}
	store.RedactReceipt(rec, b.Deletes)
	if b.OperationKey != "" {
		raw, err := store.EncodeReceipt(rec)
		if err != nil {
			return nil, err
		}
		if _, err := w.exec(`INSERT INTO operation_receipts(tenant,key,hash,receipt) VALUES($1,$2,$3,$4)`, w.t, b.OperationKey, b.OperationHash, raw); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func evidence(ev []memory.Evidence) []byte {
	if ev == nil {
		return []byte("[]")
	}
	return js(ev)
}

func (w *writer) insertMemory(m *memory.Memory) error {
	if m.ID == "" {
		return fmt.Errorf("%w: memory id is required", memory.ErrInvalid)
	}
	_, err := w.exec(`INSERT INTO memories (tenant, id, user_id, agent_id, run_id, kind, layer, title, text, subject, predicate, object,
		slot, hash, tags, metadata, confidence, importance, heat, visibility, grants, valid_from, valid_to, recorded_at,
		invalidated_at, expires_at, version, supersedes, src_kind, actor, role, trust, derived_by, source, fts_scope, closed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36)`,
		w.t, m.ID, m.User, m.Agent, m.Run, string(m.Kind), int(m.Layer), m.Title, m.Text, m.Subject, m.Predicate, m.Object,
		m.Slot, m.Hash, jsList(m.Tags), metaJSON(m.Metadata), m.Confidence, m.Importance, m.Heat, string(m.Visibility), jsList(m.Grants),
		utc(m.ValidFrom), utcPtr(m.ValidTo), utc(m.RecordedAt), utcPtr(m.InvalidatedAt), utcPtr(m.ExpiresAt),
		m.Version, m.Supersedes, m.Source.Kind, m.Source.Actor, m.Source.Role, string(m.Source.Trust), m.Source.DerivedBy, js(m.Source),
		sqlgen.TenantToken(w.t)+" "+sqlgen.UserToken(w.t, m.User), utcPtr(m.ClosedAt))
	if err != nil {
		return err
	}
	for _, ref := range m.Entities {
		key := memory.EntityKey(ref.Name)
		if key == "" {
			continue
		}
		var eid string
		if err := w.tx.QueryRow(w.ctx, `WITH k AS (SELECT COALESCE((SELECT entity_key FROM entity_aliases WHERE tenant = $1 AND alias = $3), $3) AS key)
			INSERT INTO entities (tenant, id, key, name, type) SELECT $1, $2, k.key, $4, $5 FROM k
			ON CONFLICT (tenant, key) DO UPDATE SET type = CASE WHEN entities.type = '' THEN excluded.type ELSE entities.type END
			RETURNING id`, w.t, memory.NewID(), key, ref.Name, ref.Type).Scan(&eid); err != nil {
			return err
		}
		n, err := w.exec(`INSERT INTO memory_entities (tenant, entity_id, memory_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, w.t, eid, m.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := w.exec(`UPDATE entities SET mentions = mentions + 1 WHERE tenant = $1 AND id = $2`, w.t, eid); err != nil {
				return err
			}
		}
	}
	return nil
}

func blobList(parts []memory.Part) []string {
	shas := []string{}
	for _, p := range parts {
		if p.Blob != nil {
			shas = append(shas, p.Blob.SHA256)
		}
	}
	return shas
}

func (w *writer) appendEpisode(e *memory.Episode) (store.EpisodeResult, error) {
	if e.ClientMsgID != "" {
		got, err := scanEpisode(w.tx.QueryRow(w.ctx, `SELECT `+epCols+` FROM episodes e WHERE tenant = $1 AND run_id = $2 AND client_msg_id = $3`, w.t, e.Run, e.ClientMsgID), w.t)
		if err == nil {
			return store.EpisodeResult{Episode: got, Duplicate: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.EpisodeResult{}, err
		}
	}
	var seq int64
	if err := w.tx.QueryRow(w.ctx, `INSERT INTO runs (tenant, run_id, user_id, agent_id, last_seq, updated_at) VALUES ($1,$2,$3,$4,1,$5)
		ON CONFLICT (tenant, run_id) DO UPDATE SET last_seq = runs.last_seq + 1, updated_at = excluded.updated_at
		RETURNING last_seq`, w.t, e.Run, e.User, e.Agent, w.now).Scan(&seq); err != nil {
		return store.EpisodeResult{}, err
	}
	c := *e
	c.Tenant, c.Seq, c.RecordedAt, c.OccurredAt = w.t, seq, w.now, utc(c.OccurredAt)
	if c.ID == "" {
		c.ID = memory.NewID()
	}
	if c.Status == "" {
		c.Status = memory.Final
	}
	if c.Parts == nil {
		c.Parts = []memory.Part{}
	}
	_, err := w.exec(`INSERT INTO episodes (tenant, id, run_id, seq, client_msg_id, user_id, agent_id, role, actor, occurred_at, recorded_at, status, parts, metadata, text, blobs, fts_scope)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		w.t, c.ID, c.Run, c.Seq, c.ClientMsgID, c.User, c.Agent, c.Role, c.Actor, c.OccurredAt, c.RecordedAt,
		string(c.Status), js(c.Parts), metaJSON(c.Metadata), c.Text(), blobList(c.Parts), sqlgen.EpisodeTokens(w.t, &c))
	return store.EpisodeResult{Episode: c}, err
}

func (w *writer) enqueue(j store.Job) error {
	if j.NotBefore.IsZero() {
		j.NotBefore = w.now
	}
	if j.ID == "" {
		j.ID = memory.NewID()
	}
	if j.Key != "" {
		n, err := w.exec(`UPDATE jobs SET not_before = GREATEST(not_before, $1), updated_at = $2 WHERE tenant = $3 AND kind = $4 AND key = $5 AND state = 'pending'`,
			utc(j.NotBefore), w.now, w.t, j.Kind, j.Key)
		if err != nil || n > 0 {
			return err
		}
	}
	_, err := w.exec(`INSERT INTO jobs (id, tenant, kind, key, payload, state, not_before, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$7)`,
		j.ID, w.t, j.Kind, j.Key, []byte(j.Payload), utc(j.NotBefore), w.now)
	return err
}

const memCols = `m.id, m.user_id, m.agent_id, m.run_id, m.kind, m.layer, m.title, m.text, m.subject, m.predicate, m.object,
	m.slot, m.hash, m.tags, m.metadata, m.confidence, m.importance, m.heat, m.visibility, m.grants, m.valid_from, m.valid_to,
	m.recorded_at, m.invalidated_at, m.expires_at, m.version, m.supersedes, m.source, m.closed_at`

func scanMemory(r pgx.Row, tenant string) (memory.Memory, error) {
	var m memory.Memory
	var kind, vis string
	var layer int
	var tags, meta, grants, src []byte
	err := r.Scan(&m.ID, &m.User, &m.Agent, &m.Run, &kind, &layer, &m.Title, &m.Text, &m.Subject, &m.Predicate, &m.Object,
		&m.Slot, &m.Hash, &tags, &meta, &m.Confidence, &m.Importance, &m.Heat, &vis, &grants, &m.ValidFrom, &m.ValidTo,
		&m.RecordedAt, &m.InvalidatedAt, &m.ExpiresAt, &m.Version, &m.Supersedes, &src, &m.ClosedAt)
	if err != nil {
		return m, err
	}
	m.Tenant, m.Kind, m.Layer, m.Visibility = tenant, memory.Kind(kind), memory.Layer(layer), memory.Visibility(vis)
	m.ValidFrom, m.RecordedAt = m.ValidFrom.UTC(), m.RecordedAt.UTC()
	for _, p := range []**time.Time{&m.ValidTo, &m.InvalidatedAt, &m.ExpiresAt, &m.ClosedAt} {
		if *p != nil {
			v := (*p).UTC()
			*p = &v
		}
	}
	err = errors.Join(json.Unmarshal(tags, &m.Tags), json.Unmarshal(grants, &m.Grants), json.Unmarshal(meta, &m.Metadata), json.Unmarshal(src, &m.Source))
	if len(m.Metadata) == 0 {
		m.Metadata = nil
	}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	if len(m.Grants) == 0 {
		m.Grants = nil
	}
	return m, err
}

func (s *Store) memories(ctx context.Context, tenant, sql string, args ...any) ([]memory.Memory, error) {
	var out []memory.Memory
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMemory(rows, tenant)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return s.loadEntities(ctx, c, tenant, out)
	})
	return out, err
}

func (s *Store) loadEntities(ctx context.Context, c q, tenant string, ms []memory.Memory) error {
	if len(ms) == 0 {
		return nil
	}
	idx := make(map[string]int, len(ms))
	ids := make([]string, len(ms))
	for i, m := range ms {
		idx[m.ID], ids[i] = i, m.ID
	}
	rows, err := c.Query(ctx, `SELECT me.memory_id, e.type, e.name FROM memory_entities me
		JOIN entities e ON e.tenant = me.tenant AND e.id = me.entity_id
		WHERE me.tenant = $1 AND me.memory_id = ANY($2) ORDER BY e.key`, tenant, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		var ref memory.EntityRef
		if err := rows.Scan(&mid, &ref.Type, &ref.Name); err != nil {
			return err
		}
		ms[idx[mid]].Entities = append(ms[idx[mid]].Entities, ref)
	}
	return rows.Err()
}

// Get returns memories by id.
func (s *Store) Get(ctx context.Context, tenant string, ids []string) ([]memory.Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.memories(ctx, tenant, `SELECT `+memCols+` FROM memories m WHERE m.tenant = $1 AND m.id = ANY($2)`, tenant, ids)
}

func (s *Store) where(qr store.Query, lead ...any) (string, []any, error) {
	b := &sqlgen.Builder{D: s.d, Args: lead}
	ten := b.Arg(qr.Tenant)
	temporal := b.Temporal(qr, utc(s.now()))
	f, err := b.Where(qr.Filter)
	if err != nil {
		return "", nil, err
	}
	return "m.tenant = " + ten + " AND " + temporal + " AND " + f, b.Args, nil
}

func limitOffset(limit, offset int) string {
	out := ""
	if limit > 0 {
		out = " LIMIT " + strconv.Itoa(limit)
	}
	if offset > 0 {
		out += " OFFSET " + strconv.Itoa(offset)
	}
	return out
}

// Find returns memories matching q.
func (s *Store) Find(ctx context.Context, qr store.Query) ([]memory.Memory, error) {
	where, args, err := s.where(qr)
	if err != nil {
		return nil, err
	}
	return s.memories(ctx, qr.Tenant, `SELECT `+memCols+` FROM memories m WHERE `+where+` ORDER BY `+sqlgen.OrderBy(qr.Order, s.d)+limitOffset(qr.Limit, qr.Offset), args...)
}

// History returns the events of one memory.
func (s *Store) History(ctx context.Context, tenant, id string) ([]memory.Event, error) {
	var out []memory.Event
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT memory_id, action, old, new, related, actor, reason, at FROM history WHERE tenant = $1 AND memory_id = $2 ORDER BY seq`, tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e memory.Event
			var action string
			if err := rows.Scan(&e.MemoryID, &action, &e.Old, &e.New, &e.Related, &e.Actor, &e.Reason, &e.At); err != nil {
				return err
			}
			e.Action, e.At = memory.Action(action), e.At.UTC()
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

const epCols = `e.id, e.run_id, e.seq, e.client_msg_id, e.user_id, e.agent_id, e.role, e.actor, e.occurred_at, e.recorded_at, e.status, e.parts, e.metadata`

func scanEpisode(r pgx.Row, tenant string, extra ...any) (memory.Episode, error) {
	var e memory.Episode
	var status string
	var parts, meta []byte
	dst := append([]any{&e.ID, &e.Run, &e.Seq, &e.ClientMsgID, &e.User, &e.Agent, &e.Role, &e.Actor, &e.OccurredAt, &e.RecordedAt, &status, &parts, &meta}, extra...)
	if err := r.Scan(dst...); err != nil {
		return e, err
	}
	e.Tenant, e.Status, e.OccurredAt, e.RecordedAt = tenant, memory.EpisodeStatus(status), e.OccurredAt.UTC(), e.RecordedAt.UTC()
	err := errors.Join(json.Unmarshal(parts, &e.Parts), json.Unmarshal(meta, &e.Metadata))
	if len(e.Metadata) == 0 {
		e.Metadata = nil
	}
	return e, err
}

func epWhere(eq store.EpisodeQuery, lead ...any) (string, []any) {
	args := append(lead, eq.Tenant)
	conds := []string{"e.tenant = $" + strconv.Itoa(len(args))}
	add := func(c string, a any) {
		args = append(args, a)
		conds = append(conds, strings.ReplaceAll(c, "$?", "$"+strconv.Itoa(len(args))))
	}
	if len(eq.IDs) > 0 {
		add("e.id = ANY($?)", eq.IDs)
	}
	if eq.Run != "" {
		add("e.run_id = $?", eq.Run)
	}
	if eq.User != "" {
		add("e.user_id = $?", eq.User)
	}
	if eq.Agent != "" {
		add("e.agent_id = $?", eq.Agent)
	}
	if len(eq.Roles) > 0 {
		add("e.role = ANY($?)", eq.Roles)
	}
	if eq.AfterSeq > 0 {
		add("e.seq > $?", eq.AfterSeq)
	}
	if eq.BeforeSeq > 0 {
		add("e.seq < $?", eq.BeforeSeq)
	}
	if !eq.Since.IsZero() {
		add("e.occurred_at >= $?", utc(eq.Since))
	}
	if !eq.Until.IsZero() {
		add("e.occurred_at < $?", utc(eq.Until))
	}
	if eq.BlobSHA != "" {
		add("$? = ANY(e.blobs)", eq.BlobSHA)
	}
	if !eq.IncludeDeleted {
		conds = append(conds, "e.status <> 'deleted'")
	}
	return strings.Join(conds, " AND "), args
}

func epOrder(eq store.EpisodeQuery) string {
	dir := " ASC"
	if eq.Desc {
		dir = " DESC"
	}
	if eq.Run != "" {
		return " ORDER BY e.seq" + dir
	}
	return " ORDER BY e.occurred_at" + dir + ", e.id" + dir
}

// Episodes returns episodes matching q.
func (s *Store) Episodes(ctx context.Context, eq store.EpisodeQuery) ([]memory.Episode, error) {
	where, args := epWhere(eq)
	var out []memory.Episode
	err := s.read(ctx, eq.Tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT `+epCols+` FROM episodes e WHERE `+where+epOrder(eq)+limitOffset(eq.Limit, 0), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEpisode(rows, eq.Tenant)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Run returns run state.
func (s *Store) Run(ctx context.Context, tenant, run string) (memory.Run, error) {
	r := memory.Run{Scope: memory.Scope{Tenant: tenant, Run: run}}
	err := s.read(ctx, tenant, func(c q) error {
		return c.QueryRow(ctx, `SELECT user_id, agent_id, last_seq, processed_seq, summary, summary_upto, updated_at FROM runs WHERE tenant = $1 AND run_id = $2`,
			tenant, run).Scan(&r.User, &r.Agent, &r.LastSeq, &r.ProcessedSeq, &r.Summary, &r.SummaryUpto, &r.UpdatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, memory.ErrNotFound
	}
	r.UpdatedAt = r.UpdatedAt.UTC()
	return r, err
}

// Settings returns the tenant settings document.
func (s *Store) Settings(ctx context.Context, tenant string) ([]byte, error) {
	var doc []byte
	err := s.read(ctx, tenant, func(c q) error {
		return c.QueryRow(ctx, `SELECT doc FROM settings WHERE tenant = $1`, tenant).Scan(&doc)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return doc, err
}

// scopedTS restricts a tsquery to documents carrying a scope token, so the
// GIN index intersects posting lists before any row is read.
func scopedTS(token, expr string) string {
	if expr == "" {
		return ""
	}
	return "'" + token + "' & (" + expr + ")"
}

// tsQuery builds a to_tsquery expression matching any term. Terms are
// reduced to letter/digit tokens, so input can never inject query syntax;
// long terms become prefix matches (see store.PrefixMin).
func tsQuery(terms []string) string {
	var toks []string
	for _, t := range terms {
		for _, tok := range textutil.Tokens(t) {
			q := "'" + tok + "'"
			if utf8.RuneCountInString(tok) >= store.PrefixMin {
				q += ":*"
			}
			toks = append(toks, q)
		}
	}
	return strings.Join(toks, " | ")
}

// SearchMemories ranks with ts_rank over the 'simple' tsvector.
func (s *Store) SearchMemories(ctx context.Context, tq store.TextQuery) ([]store.Hit, error) {
	token := sqlgen.TenantToken(tq.Tenant)
	if u, ok := sqlgen.OwnerUser(tq.Filter); ok {
		token = sqlgen.UserToken(tq.Tenant, u)
	}
	expr := scopedTS(token, tsQuery(tq.Terms))
	if expr == "" {
		return nil, nil
	}
	where, args, err := s.where(tq.Query, expr)
	if err != nil {
		return nil, err
	}
	var out []store.Hit
	err = s.read(ctx, tq.Tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT m.id, ts_rank(m.tsv, to_tsquery('simple', $1)) AS r FROM memories m
			WHERE m.tsv @@ to_tsquery('simple', $1) AND `+where+` ORDER BY r DESC, m.id DESC`+limitOffset(tq.Limit, tq.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h store.Hit
			var r float32
			if err := rows.Scan(&h.ID, &r); err != nil {
				return err
			}
			h.Score = float64(r)
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// SearchEpisodes ranks episodes with ts_rank.
func (s *Store) SearchEpisodes(ctx context.Context, tq store.EpisodeTextQuery) ([]store.EpisodeHit, error) {
	token := sqlgen.TenantToken(tq.Tenant)
	switch {
	case tq.Run != "":
		token = sqlgen.RunToken(tq.Tenant, tq.Run)
	case tq.User != "":
		token = sqlgen.UserToken(tq.Tenant, tq.User)
	}
	expr := scopedTS(token, tsQuery(tq.Terms))
	if expr == "" {
		return nil, nil
	}
	where, args := epWhere(tq.EpisodeQuery, expr)
	var out []store.EpisodeHit
	err := s.read(ctx, tq.Tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT `+epCols+`, ts_rank(e.tsv, to_tsquery('simple', $1)) AS r FROM episodes e
			WHERE e.tsv @@ to_tsquery('simple', $1) AND `+where+` ORDER BY r DESC`+limitOffset(tq.Limit, 0), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r float32
			e, err := scanEpisode(rows, tq.Tenant, &r)
			if err != nil {
				return err
			}
			out = append(out, store.EpisodeHit{Episode: e, Score: float64(r)})
		}
		return rows.Err()
	})
	return out, err
}

const entCols = `e.id, e.key, e.name, e.type, e.mentions,
	COALESCE((SELECT array_agg(a.name ORDER BY a.alias) FROM entity_aliases a WHERE a.tenant = e.tenant AND a.entity_key = e.key), '{}')`

func (s *Store) entities(ctx context.Context, tenant, sql string, args ...any) ([]memory.Entity, error) {
	var out []memory.Entity
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e memory.Entity
			if err := rows.Scan(&e.ID, &e.Key, &e.Name, &e.Type, &e.Mentions, &e.Aliases); err != nil {
				return err
			}
			if len(e.Aliases) == 0 {
				e.Aliases = nil
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// FindEntities matches keys and alias keys.
func (s *Store) FindEntities(ctx context.Context, tenant string, keys []string) ([]memory.Entity, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return s.entities(ctx, tenant, `SELECT `+entCols+` FROM entities e WHERE e.tenant = $1 AND (e.key = ANY($2)
		OR e.key IN (SELECT entity_key FROM entity_aliases WHERE tenant = $1 AND alias = ANY($2)))`, tenant, keys)
}

// EntityMemories returns up to limit links per entity, newest memory first.
func (s *Store) EntityMemories(ctx context.Context, tenant string, ids []string, limit int) ([]store.Link, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	var out []store.Link
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT l.entity_id, l.memory_id FROM unnest($2::text[]) AS e(id)
			CROSS JOIN LATERAL (SELECT entity_id, memory_id FROM memory_entities WHERE tenant = $1 AND entity_id = e.id ORDER BY memory_id DESC LIMIT $3) l`,
			tenant, ids, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l store.Link
			if err := rows.Scan(&l.EntityID, &l.MemoryID); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// ListEntities lists entities by key prefix, most mentioned first.
func (s *Store) ListEntities(ctx context.Context, tenant, prefix string, limit int) ([]memory.Entity, error) {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return s.entities(ctx, tenant, `SELECT `+entCols+` FROM entities e WHERE e.tenant = $1 AND e.key LIKE $2
		ORDER BY e.mentions DESC, e.key`+limitOffset(limit, 0), tenant, esc+"%")
}

// Tenants lists tenants with record counts.
func (s *Store) Tenants(ctx context.Context) ([]store.TenantStats, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant, SUM(m)::int, SUM(e)::int, SUM(n)::int, SUM(j)::int FROM (
		SELECT tenant, COUNT(*) m, 0 e, 0 n, 0 j FROM memories GROUP BY tenant
		UNION ALL SELECT tenant, 0, COUNT(*), 0, 0 FROM episodes GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, COUNT(*), 0 FROM entities GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, 0, COUNT(*) FROM jobs GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, 0, 0 FROM settings) x GROUP BY tenant ORDER BY tenant`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TenantStats
	for rows.Next() {
		var t store.TenantStats
		if err := rows.Scan(&t.Tenant, &t.Memories, &t.Episodes, &t.Entities, &t.Jobs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryReadOnly runs one query in a READ ONLY transaction with a statement
// timeout, rolled back afterwards, so writes are impossible.
func (s *Store) QueryReadOnly(ctx context.Context, sql string, maxRows int) (cols []string, out [][]any, err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for _, f := range rows.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	for rows.Next() && len(out) < maxRows {
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	return cols, out, rows.Err()
}
