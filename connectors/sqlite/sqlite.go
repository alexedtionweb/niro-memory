// Package sqlite is the SQLite connector: a primary store with full-text
// search (FTS5 BM25), an entity index, stored vectors with an exact
// brute-force vector index, and a leased job queue.
//
// It suits laptops, embedded deployments and single-node services. All
// writes go through one connection (SQLite allows one writer); reads use a
// pool of read-only connections that WAL mode never blocks. For horizontal
// scaling use the postgres connector.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/sqlgen"
	"github.com/alexedtionweb/niro-memory/vector"
)

//go:embed schema.sql
var schema string

// Options configures the store.
type Options struct {
	// Path of the database file. It is created if missing.
	Path string
	// Readers is the size of the read-only connection pool (default: GOMAXPROCS).
	Readers int
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Store is a SQLite-backed store.Store.
type Store struct {
	w, r *sql.DB
	now  func() time.Time
	d    dialect
}

// Compile-time checks of the capabilities this connector provides.
var (
	_ store.Store         = (*Store)(nil)
	_ store.Lexical       = (*Store)(nil)
	_ store.Entities      = (*Store)(nil)
	_ store.VectorCatalog = (*Store)(nil)
	_ store.Admin         = (*Store)(nil)
	_ store.SQLConsole    = (*Store)(nil)
	_ vector.Index        = (*Store)(nil)
)

// Open opens (and migrates) the database at o.Path.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.Path == "" || strings.ContainsAny(o.Path, "?#") {
		return nil, fmt.Errorf("%w: sqlite path must be a plain file path", memory.ErrInvalid)
	}
	if err := register(); err != nil {
		return nil, err
	}
	if o.Readers <= 0 {
		o.Readers = runtime.GOMAXPROCS(0)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	w, err := sql.Open(driverName, dsn(o.Path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if _, err := w.ExecContext(ctx, schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	if err := migrateJobPriority(ctx, w); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: migrate job priority: %w", err)
	}
	if _, err := w.ExecContext(ctx, `SELECT closed_at FROM memories LIMIT 0`); err != nil {
		if _, err := w.ExecContext(ctx, `ALTER TABLE memories ADD COLUMN closed_at INTEGER`); err != nil {
			// A second opener may have added it while this opener waited.
			if _, check := w.ExecContext(ctx, `SELECT closed_at FROM memories LIMIT 0`); check != nil {
				w.Close()
				return nil, fmt.Errorf("sqlite: migrate closed_at: %w", err)
			}
		}
	}
	r, err := sql.Open(driverName, dsn(o.Path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(o.Readers)
	r.SetMaxIdleConns(o.Readers)
	return &Store{w: w, r: r, now: o.Now}, nil
}

// Close closes both pools.
func (s *Store) Close() error { return errors.Join(s.r.Close(), s.w.Close()) }

// Capabilities reports exact BM25 (FTS5).
func (s *Store) Capabilities() store.Capabilities { return store.Capabilities{BM25: "exact"} }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullMs(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func ptrMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsList(v []string) string {
	if v == nil {
		return "[]"
	}
	return js(v)
}

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{memory.ErrConflict}, a...)...)
}

// Write applies the batch in one IMMEDIATE transaction.
func (s *Store) Write(ctx context.Context, b *store.Batch) (rec *store.Receipt, err error) {
	if b.Tenant == "" {
		return nil, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	w := &writer{tx: tx, ctx: ctx, t: b.Tenant, now: s.now()}
	var fenced bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tenant_purges WHERE tenant=? AND state<>'complete')`, b.Tenant).Scan(&fenced); err != nil {
		return nil, err
	}
	if fenced {
		return nil, conflict("tenant cleanup is in progress; writes are locked")
	}
	rec, err = w.apply(b)
	if err != nil {
		if isUnique(err) {
			err = conflict("%v", err)
		}
		return nil, err
	}
	return rec, tx.Commit()
}

type writer struct {
	tx           *sql.Tx
	ctx          context.Context
	t            string
	now          time.Time
	relationJobs []store.Job
}

func (w *writer) exec(q string, args ...any) (int64, error) {
	r, err := w.tx.ExecContext(w.ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
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
			at = *c.ClosedAt
		}
		n, err := w.exec(`UPDATE memories SET valid_to = COALESCE(?1, valid_to), invalidated_at = COALESCE(?2, invalidated_at),
			closed_at = CASE WHEN ?1 IS NOT NULL THEN ?3 ELSE closed_at END
			WHERE tenant = ?4 AND id = ?5 AND valid_to IS NULL AND invalidated_at IS NULL`, nullMs(c.ValidTo), nullMs(c.InvalidatedAt), ms(at), w.t, c.ID)
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
		if err := w.touch(tc); err != nil {
			return nil, err
		}
	}
	for _, id := range b.Deletes {
		if err := w.deleteMemory(id); err != nil {
			return nil, err
		}
	}
	for _, e := range b.Events {
		if _, err := w.exec(`INSERT INTO history (tenant, memory_id, action, old, new, related, actor, reason, at) VALUES (?,?,?,?,?,?,?,?,?)`,
			w.t, e.MemoryID, e.Action, e.Old, e.New, e.Related, e.Actor, e.Reason, ms(e.At)); err != nil {
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
		q, args := `UPDATE episodes SET status = ? WHERE tenant = ? AND id = ?`, []any{ed.Status, w.t, ed.ID}
		if ed.Parts != nil {
			e := memory.Episode{Parts: ed.Parts}
			q, args = `UPDATE episodes SET status = ?, parts = ?, text = ?, blobs = ? WHERE tenant = ? AND id = ?`,
				[]any{ed.Status, js(ed.Parts), e.Text(), blobList(ed.Parts), w.t, ed.ID}
		}
		if _, err := w.exec(q, args...); err != nil {
			return nil, err
		}
	}
	for _, run := range b.DeleteRuns {
		for _, q := range []string{
			`DELETE FROM embeddings WHERE tenant = ?1 AND target_kind = 'p' AND target_id IN (SELECT id FROM episodes WHERE tenant = ?1 AND run_id = ?2)`,
			`DELETE FROM episodes WHERE tenant = ?1 AND run_id = ?2`,
			`DELETE FROM runs WHERE tenant = ?1 AND run_id = ?2`,
		} {
			if _, err := w.exec(q, w.t, run); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range b.Cursors {
		n, err := w.exec(`UPDATE runs SET processed_seq = ? WHERE tenant = ? AND run_id = ? AND processed_seq = ?`, c.To, w.t, c.Run, c.From)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, conflict("run %s cursor moved", c.Run)
		}
	}
	for _, sm := range b.Summaries {
		if _, err := w.exec(`UPDATE runs SET summary = ?, summary_upto = ?, updated_at = ? WHERE tenant = ? AND run_id = ?`, sm.Text, sm.Upto, ms(w.now), w.t, sm.Run); err != nil {
			return nil, err
		}
	}
	for _, p := range b.Vectors {
		v := append([]float32(nil), p.Values...)
		_ = vector.Normalize(v) // validated before the transaction
		if _, err := w.exec(`INSERT INTO embeddings (tenant, model, target_key, target_kind, target_id, part, modality, user_id, agent_id, run_id, vec)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (tenant, model, target_key) DO UPDATE SET modality = excluded.modality, user_id = excluded.user_id,
			agent_id = excluded.agent_id, run_id = excluded.run_id, vec = excluded.vec`,
			w.t, p.Model, p.Target.Key(), string(p.Target.Kind), p.Target.ID, p.Target.Part, p.Modality, p.User, p.Agent, p.Run, encodeVec(v)); err != nil {
			return nil, err
		}
	}
	for _, tg := range b.VectorDeletes {
		if _, err := w.exec(`DELETE FROM embeddings WHERE tenant = ? AND target_key = ?`, w.t, tg.Key()); err != nil {
			return nil, err
		}
	}
	for _, a := range b.Aliases {
		if _, err := w.exec(`INSERT INTO entity_aliases (tenant, alias, entity_key, name)
			SELECT ?1, ?2, key, ?3 FROM entities WHERE tenant = ?1 AND key = ?4
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
		if _, err := w.exec(`INSERT INTO settings (tenant, doc) VALUES (?, ?) ON CONFLICT (tenant) DO UPDATE SET doc = excluded.doc`, w.t, b.Settings); err != nil {
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
		if _, err := w.exec(`INSERT INTO operation_receipts(tenant,key,hash,receipt) VALUES(?,?,?,?)`, w.t, b.OperationKey, b.OperationHash, string(raw)); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func (w *writer) insertMemory(m *memory.Memory) error {
	if m.ID == "" {
		return fmt.Errorf("%w: memory id is required", memory.ErrInvalid)
	}
	_, err := w.exec(`INSERT INTO memories (tenant, id, user_id, agent_id, run_id, kind, layer, title, text, subject, predicate, object,
		slot, hash, tags, metadata, confidence, importance, heat, visibility, grants, valid_from, valid_to, recorded_at,
		invalidated_at, expires_at, version, supersedes, src_kind, actor, role, trust, derived_by, source, fts_scope, closed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.t, m.ID, m.User, m.Agent, m.Run, m.Kind, m.Layer, m.Title, m.Text, m.Subject, m.Predicate, m.Object,
		m.Slot, m.Hash, jsList(m.Tags), metaJSON(m.Metadata), m.Confidence, m.Importance, m.Heat, m.Visibility, jsList(m.Grants),
		ms(m.ValidFrom), nullMs(m.ValidTo), ms(m.RecordedAt), nullMs(m.InvalidatedAt), nullMs(m.ExpiresAt),
		m.Version, m.Supersedes, m.Source.Kind, m.Source.Actor, m.Source.Role, m.Source.Trust, m.Source.DerivedBy, js(m.Source),
		sqlgen.TenantToken(w.t)+" "+sqlgen.UserToken(w.t, m.User), nullMs(m.ClosedAt))
	if err != nil {
		return err
	}
	for _, ref := range m.Entities {
		key := memory.EntityKey(ref.Name)
		if key == "" {
			continue
		}
		var aliased string
		err := w.tx.QueryRowContext(w.ctx, `SELECT entity_key FROM entity_aliases WHERE tenant = ? AND alias = ?`, w.t, key).Scan(&aliased)
		if err == nil {
			key = aliased
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var eid string
		if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO entities (tenant, id, key, name, type) VALUES (?,?,?,?,?)
			ON CONFLICT (tenant, key) DO UPDATE SET type = CASE WHEN entities.type = '' THEN excluded.type ELSE entities.type END
			RETURNING id`, w.t, memory.NewID(), key, ref.Name, ref.Type).Scan(&eid); err != nil {
			return err
		}
		n, err := w.exec(`INSERT OR IGNORE INTO memory_entities (tenant, entity_id, memory_id) VALUES (?,?,?)`, w.t, eid, m.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := w.exec(`UPDATE entities SET mentions = mentions + 1 WHERE tenant = ? AND id = ?`, w.t, eid); err != nil {
				return err
			}
		}
	}
	return nil
}

func metaJSON(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	return js(m)
}

func (w *writer) touch(tc store.Touch) error {
	var raw string
	err := w.tx.QueryRowContext(w.ctx, `SELECT source FROM memories WHERE tenant = ? AND id = ?`, w.t, tc.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var src memory.Source
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		return err
	}
	src.Evidence = append(src.Evidence, tc.Evidence...)
	_, err = w.exec(`UPDATE memories SET heat = heat + ?, source = ? WHERE tenant = ? AND id = ?`, tc.Heat, js(src), w.t, tc.ID)
	return err
}

func (w *writer) deleteMemory(id string) error {
	for _, q := range []string{
		`UPDATE entities SET mentions = mentions - 1 WHERE tenant = ?1 AND id IN (SELECT entity_id FROM memory_entities WHERE tenant = ?1 AND memory_id = ?2)`,
		`DELETE FROM memory_entities WHERE tenant = ?1 AND memory_id = ?2`,
		`DELETE FROM embeddings WHERE tenant = ?1 AND target_key = 'm:' || ?2`,
		`UPDATE history SET old = '', new = '' WHERE tenant = ?1 AND memory_id = ?2`,
		`DELETE FROM relationship_sets WHERE tenant = ?1 AND (anchor = ?2 OR EXISTS
			(SELECT 1 FROM json_each(relationship_sets.data) AS link WHERE json_extract(link.value, '$.target_id') = ?2))`,
		`DELETE FROM knowledge_relations WHERE tenant = ?1 AND (source = ?2 OR target = ?2)`,
		`DELETE FROM memories WHERE tenant = ?1 AND id = ?2`,
	} {
		if _, err := w.exec(q, w.t, id); err != nil {
			return err
		}
	}
	return nil
}

func blobList(parts []memory.Part) string {
	var shas []string
	for _, p := range parts {
		if p.Blob != nil {
			shas = append(shas, p.Blob.SHA256)
		}
	}
	return strings.Join(shas, " ")
}

func (w *writer) appendEpisode(e *memory.Episode) (store.EpisodeResult, error) {
	if e.ClientMsgID != "" {
		row := w.tx.QueryRowContext(w.ctx, `SELECT `+epCols+` FROM episodes e WHERE tenant = ? AND run_id = ? AND client_msg_id = ?`, w.t, e.Run, e.ClientMsgID)
		got, err := scanEpisode(row, w.t)
		if err == nil {
			return store.EpisodeResult{Episode: got, Duplicate: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.EpisodeResult{}, err
		}
	}
	var seq int64
	if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO runs (tenant, run_id, user_id, agent_id, last_seq, updated_at) VALUES (?,?,?,?,1,?)
		ON CONFLICT (tenant, run_id) DO UPDATE SET last_seq = last_seq + 1, updated_at = excluded.updated_at
		RETURNING last_seq`, w.t, e.Run, e.User, e.Agent, ms(w.now)).Scan(&seq); err != nil {
		return store.EpisodeResult{}, err
	}
	c := *e
	c.Tenant, c.Seq, c.RecordedAt = w.t, seq, w.now
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.t, c.ID, c.Run, c.Seq, c.ClientMsgID, c.User, c.Agent, c.Role, c.Actor, ms(c.OccurredAt), ms(c.RecordedAt),
		c.Status, js(c.Parts), metaJSON(c.Metadata), c.Text(), blobList(c.Parts), sqlgen.EpisodeTokens(w.t, &c))
	c.RecordedAt = fromMs(ms(c.RecordedAt))
	c.OccurredAt = fromMs(ms(c.OccurredAt))
	return store.EpisodeResult{Episode: c}, err
}

func (w *writer) enqueue(j store.Job) error {
	if err := store.ValidateJobPriority(j.Priority); err != nil {
		return err
	}
	if j.NotBefore.IsZero() {
		j.NotBefore = w.now
	}
	if j.ID == "" {
		j.ID = memory.NewID()
	}
	if j.Key != "" {
		n, err := w.exec(`UPDATE jobs SET not_before = MAX(not_before, ?), updated_at = ? WHERE tenant = ? AND kind = ? AND key = ? AND state = 'pending'`,
			ms(j.NotBefore), ms(w.now), w.t, j.Kind, j.Key)
		if err != nil || n > 0 {
			return err
		}
	}
	_, err := w.exec(`INSERT INTO jobs (id, tenant, kind, key, payload, state, not_before, created_at, updated_at, priority) VALUES (?,?,?,?,?,'pending',?,?,?,?)`,
		j.ID, w.t, j.Kind, j.Key, []byte(j.Payload), ms(j.NotBefore), ms(w.now), ms(w.now), j.Priority)
	return err
}

const memCols = `m.id, m.user_id, m.agent_id, m.run_id, m.kind, m.layer, m.title, m.text, m.subject, m.predicate, m.object,
	m.slot, m.hash, m.tags, m.metadata, m.confidence, m.importance, m.heat, m.visibility, m.grants, m.valid_from, m.valid_to,
	m.recorded_at, m.invalidated_at, m.expires_at, m.version, m.supersedes, m.source, m.closed_at`

type scanner interface{ Scan(...any) error }

func scanMemory(r scanner, tenant string) (memory.Memory, error) {
	var m memory.Memory
	var tags, meta, grants, src string
	var vf, ra int64
	var vt, ia, ea, ca sql.NullInt64
	err := r.Scan(&m.ID, &m.User, &m.Agent, &m.Run, &m.Kind, &m.Layer, &m.Title, &m.Text, &m.Subject, &m.Predicate, &m.Object,
		&m.Slot, &m.Hash, &tags, &meta, &m.Confidence, &m.Importance, &m.Heat, &m.Visibility, &grants, &vf, &vt,
		&ra, &ia, &ea, &m.Version, &m.Supersedes, &src, &ca)
	if err != nil {
		return m, err
	}
	m.Tenant = tenant
	m.ValidFrom, m.RecordedAt = fromMs(vf), fromMs(ra)
	m.ValidTo, m.InvalidatedAt, m.ExpiresAt = ptrMs(vt), ptrMs(ia), ptrMs(ea)
	m.ClosedAt = ptrMs(ca)
	err = errors.Join(json.Unmarshal([]byte(tags), &m.Tags), json.Unmarshal([]byte(grants), &m.Grants),
		json.Unmarshal([]byte(meta), &m.Metadata), json.Unmarshal([]byte(src), &m.Source))
	if len(m.Metadata) == 0 {
		m.Metadata = nil
	}
	return m, err
}

func (s *Store) queryMemories(ctx context.Context, tenant, q string, args ...any) ([]memory.Memory, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows, tenant)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.loadEntities(ctx, tenant, out)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func (s *Store) loadEntities(ctx context.Context, tenant string, ms []memory.Memory) error {
	if len(ms) == 0 {
		return nil
	}
	idx := make(map[string]int, len(ms))
	ids := make([]any, 0, len(ms)+1)
	ids = append(ids, tenant)
	for i, m := range ms {
		idx[m.ID] = i
		ids = append(ids, m.ID)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT me.memory_id, e.type, e.name FROM memory_entities me
		JOIN entities e ON e.tenant = me.tenant AND e.id = me.entity_id
		WHERE me.tenant = ? AND me.memory_id IN (`+placeholders(len(ms))+`) ORDER BY e.key`, ids...)
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
	return s.queryMemories(ctx, tenant, `SELECT `+memCols+` FROM memories m WHERE m.tenant = ? AND m.id IN (`+placeholders(len(ids))+`)`,
		append([]any{tenant}, anys(ids)...)...)
}

// where compiles tenant, temporal and filter conditions of q. Placeholders
// are numbered (?N) and continue after the given leading arguments.
func (s *Store) where(q store.Query, lead ...any) (string, []any, error) {
	b := &sqlgen.Builder{D: s.d, Args: lead}
	ten := b.Arg(q.Tenant)
	temporal := b.Temporal(q, s.now())
	f, err := b.Where(q.Filter)
	if err != nil {
		return "", nil, err
	}
	return "m.tenant = " + ten + " AND " + temporal + " AND " + f, b.Args, nil
}

func limitOffset(limit, offset int) string {
	if limit <= 0 {
		limit = -1
	}
	return fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
}

// Find returns memories matching q.
func (s *Store) Find(ctx context.Context, q store.Query) ([]memory.Memory, error) {
	where, args, err := s.where(q)
	if err != nil {
		return nil, err
	}
	return s.queryMemories(ctx, q.Tenant, `SELECT `+memCols+` FROM memories m WHERE `+where+
		` ORDER BY `+sqlgen.OrderBy(q.Order, s.d)+limitOffset(q.Limit, q.Offset), args...)
}

// History returns the events of one memory.
func (s *Store) History(ctx context.Context, tenant, id string) ([]memory.Event, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT memory_id, action, old, new, related, actor, reason, at FROM history
		WHERE tenant = ? AND memory_id = ? ORDER BY rowid`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Event
	for rows.Next() {
		var e memory.Event
		var at int64
		if err := rows.Scan(&e.MemoryID, &e.Action, &e.Old, &e.New, &e.Related, &e.Actor, &e.Reason, &at); err != nil {
			return nil, err
		}
		e.At = fromMs(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

const epCols = `e.id, e.run_id, e.seq, e.client_msg_id, e.user_id, e.agent_id, e.role, e.actor, e.occurred_at, e.recorded_at, e.status, e.parts, e.metadata`

func scanEpisode(r scanner, tenant string) (memory.Episode, error) {
	var e memory.Episode
	var oa, ra int64
	var parts, meta string
	if err := r.Scan(&e.ID, &e.Run, &e.Seq, &e.ClientMsgID, &e.User, &e.Agent, &e.Role, &e.Actor, &oa, &ra, &e.Status, &parts, &meta); err != nil {
		return e, err
	}
	e.Tenant, e.OccurredAt, e.RecordedAt = tenant, fromMs(oa), fromMs(ra)
	err := errors.Join(json.Unmarshal([]byte(parts), &e.Parts), json.Unmarshal([]byte(meta), &e.Metadata))
	if len(e.Metadata) == 0 {
		e.Metadata = nil
	}
	return e, err
}

func epWhere(q store.EpisodeQuery) (string, []any) {
	conds, args := []string{"e.tenant = ?"}, []any{q.Tenant}
	add := func(c string, a ...any) { conds, args = append(conds, c), append(args, a...) }
	if len(q.IDs) > 0 {
		add("e.id IN ("+placeholders(len(q.IDs))+")", anys(q.IDs)...)
	}
	if q.Run != "" {
		add("e.run_id = ?", q.Run)
	}
	if q.User != "" {
		add("e.user_id = ?", q.User)
	}
	if q.Agent != "" {
		add("e.agent_id = ?", q.Agent)
	}
	if len(q.Roles) > 0 {
		add("e.role IN ("+placeholders(len(q.Roles))+")", anys(q.Roles)...)
	}
	if q.AfterSeq > 0 {
		add("e.seq > ?", q.AfterSeq)
	}
	if q.BeforeSeq > 0 {
		add("e.seq < ?", q.BeforeSeq)
	}
	if !q.Since.IsZero() {
		add("e.occurred_at >= ?", ms(q.Since))
	}
	if !q.Until.IsZero() {
		add("e.occurred_at < ?", ms(q.Until))
	}
	if q.BlobSHA != "" {
		add("instr(e.blobs, ?) > 0", q.BlobSHA)
	}
	if !q.IncludeDeleted {
		add("e.status <> 'deleted'")
	}
	return strings.Join(conds, " AND "), args
}

func epOrder(q store.EpisodeQuery) string {
	dir := " ASC"
	if q.Desc {
		dir = " DESC"
	}
	if q.Run != "" {
		return " ORDER BY e.seq" + dir
	}
	return " ORDER BY e.occurred_at" + dir + ", e.id" + dir
}

func (s *Store) queryEpisodes(ctx context.Context, tenant, q string, args ...any) ([]memory.Episode, []float64, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []memory.Episode
	var scores []float64
	cols, _ := rows.Columns()
	for rows.Next() {
		var sc float64
		var e memory.Episode
		var err error
		if len(cols) == 14 {
			e, err = scanEpisode(scoredRow{rows, &sc}, tenant)
		} else {
			e, err = scanEpisode(rows, tenant)
		}
		if err != nil {
			return nil, nil, err
		}
		out, scores = append(out, e), append(scores, sc)
	}
	return out, scores, rows.Err()
}

// scoredRow scans a row whose last column is a score.
type scoredRow struct {
	r     *sql.Rows
	score *float64
}

func (s scoredRow) Scan(dst ...any) error { return s.r.Scan(append(dst, s.score)...) }

// Episodes returns episodes matching q.
func (s *Store) Episodes(ctx context.Context, q store.EpisodeQuery) ([]memory.Episode, error) {
	where, args := epWhere(q)
	out, _, err := s.queryEpisodes(ctx, q.Tenant, `SELECT `+epCols+` FROM episodes e WHERE `+where+epOrder(q)+limitOffset(q.Limit, 0), args...)
	return out, err
}

// Run returns run state.
func (s *Store) Run(ctx context.Context, tenant, run string) (memory.Run, error) {
	r := memory.Run{Scope: memory.Scope{Tenant: tenant, Run: run}}
	var up int64
	err := s.r.QueryRowContext(ctx, `SELECT user_id, agent_id, last_seq, processed_seq, summary, summary_upto, updated_at FROM runs WHERE tenant = ? AND run_id = ?`,
		tenant, run).Scan(&r.User, &r.Agent, &r.LastSeq, &r.ProcessedSeq, &r.Summary, &r.SummaryUpto, &up)
	if errors.Is(err, sql.ErrNoRows) {
		return r, memory.ErrNotFound
	}
	r.UpdatedAt = fromMs(up)
	return r, err
}

// Settings returns the tenant settings document.
func (s *Store) Settings(ctx context.Context, tenant string) ([]byte, error) {
	var doc []byte
	err := s.r.QueryRowContext(ctx, `SELECT doc FROM settings WHERE tenant = ?`, tenant).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return doc, err
}

// scoped restricts an FTS5 query to documents carrying a scope token, so the
// index intersects posting lists before any row is read.
func scoped(token, expr string) string {
	if expr == "" {
		return ""
	}
	return `fts_scope : "` + token + `" AND (` + expr + `)`
}

// SearchMemories selects candidates with FTS5 inside the requester's scope
// and ranks them with BM25 in Go (see fts.go).
func (s *Store) SearchMemories(ctx context.Context, q store.TextQuery) ([]store.Hit, error) {
	x, err := s.expand(ctx, "memories_vocab", q.Terms)
	if err != nil {
		return nil, err
	}
	token := sqlgen.TenantToken(q.Tenant)
	if u, ok := sqlgen.OwnerUser(q.Filter); ok {
		token = sqlgen.UserToken(q.Tenant, u)
	}
	expr := x.match(token)
	if expr == "" {
		return nil, nil
	}
	where, args, err := s.where(q.Query, expr)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT m.id, m.title, m.text FROM memories_fts JOIN memories m ON m.rowid = memories_fts.rowid
		WHERE memories_fts MATCH ?1 AND `+where+` ORDER BY m.rowid DESC LIMIT `+strconv.Itoa(maxCandidates), args...)
	if err != nil {
		return nil, err
	}
	var docs []doc
	for rows.Next() {
		var d doc
		var title string
		if err := rows.Scan(&d.key, &title, &d.text); err != nil {
			rows.Close()
			return nil, err
		}
		d.text = title + " " + d.text
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scores := x.bm25(docs, s.corpusSize(ctx, "memories"))
	hits := make([]store.Hit, 0, len(docs))
	for _, d := range docs {
		hits = append(hits, store.Hit{ID: d.key, Score: scores[d.key]})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID > hits[j].ID
	})
	return page(hits, q.Offset, q.Limit), nil
}

func page[T any](xs []T, offset, limit int) []T {
	if offset >= len(xs) {
		return nil
	}
	xs = xs[offset:]
	if limit > 0 && len(xs) > limit {
		xs = xs[:limit]
	}
	return xs
}

// SearchEpisodes selects episode candidates with FTS5 inside the narrowest
// scope (run, user or tenant) and ranks them with BM25 in Go.
func (s *Store) SearchEpisodes(ctx context.Context, q store.EpisodeTextQuery) ([]store.EpisodeHit, error) {
	x, err := s.expand(ctx, "episodes_vocab", q.Terms)
	if err != nil {
		return nil, err
	}
	token := sqlgen.TenantToken(q.Tenant)
	switch {
	case q.Run != "":
		token = sqlgen.RunToken(q.Tenant, q.Run)
	case q.User != "":
		token = sqlgen.UserToken(q.Tenant, q.User)
	}
	expr := x.match(token)
	if expr == "" {
		return nil, nil
	}
	where, args := epWhere(q.EpisodeQuery)
	eps, _, err := s.queryEpisodes(ctx, q.Tenant, `SELECT `+epCols+` FROM episodes_fts JOIN episodes e ON e.rowid = episodes_fts.rowid
		WHERE episodes_fts MATCH ? AND `+where+` ORDER BY e.rowid DESC LIMIT `+strconv.Itoa(maxCandidates), append([]any{expr}, args...)...)
	if err != nil {
		return nil, err
	}
	docs := make([]doc, len(eps))
	for i, e := range eps {
		docs[i] = doc{key: e.ID, text: e.Text()}
	}
	scores := x.bm25(docs, s.corpusSize(ctx, "episodes"))
	out := make([]store.EpisodeHit, len(eps))
	for i, e := range eps {
		out[i] = store.EpisodeHit{Episode: e, Score: scores[e.ID]}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return page(out, 0, q.Limit), nil
}

const entCols = `e.id, e.key, e.name, e.type, e.mentions,
	COALESCE((SELECT json_group_array(a.name) FROM entity_aliases a WHERE a.tenant = e.tenant AND a.entity_key = e.key), '[]')`

func (s *Store) queryEntities(ctx context.Context, q string, args ...any) ([]memory.Entity, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Entity
	for rows.Next() {
		var e memory.Entity
		var aliases string
		if err := rows.Scan(&e.ID, &e.Key, &e.Name, &e.Type, &e.Mentions, &aliases); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(aliases), &e.Aliases); err != nil {
			return nil, err
		}
		if len(e.Aliases) == 0 {
			e.Aliases = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FindEntities matches keys and alias keys.
func (s *Store) FindEntities(ctx context.Context, tenant string, keys []string) ([]memory.Entity, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ph := placeholders(len(keys))
	args := append([]any{tenant}, anys(keys)...)
	args = append(args, tenant)
	args = append(args, anys(keys)...)
	return s.queryEntities(ctx, `SELECT `+entCols+` FROM entities e WHERE e.tenant = ? AND (e.key IN (`+ph+`)
		OR e.key IN (SELECT entity_key FROM entity_aliases WHERE tenant = ? AND alias IN (`+ph+`)))`, args...)
}

// EntityMemories returns up to limit links per entity, newest memory first.
func (s *Store) EntityMemories(ctx context.Context, tenant string, ids []string, limit int) ([]store.Link, error) {
	var out []store.Link
	for _, eid := range ids {
		rows, err := s.r.QueryContext(ctx, `SELECT memory_id FROM memory_entities WHERE tenant = ? AND entity_id = ? ORDER BY memory_id DESC`+limitOffset(limit, 0), tenant, eid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			l := store.Link{EntityID: eid}
			if err := rows.Scan(&l.MemoryID); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListEntities lists entities by key prefix, most mentioned first.
func (s *Store) ListEntities(ctx context.Context, tenant, prefix string, limit int) ([]memory.Entity, error) {
	return s.queryEntities(ctx, `SELECT `+entCols+` FROM entities e WHERE e.tenant = ? AND substr(e.key, 1, ?) = ?
		ORDER BY e.mentions DESC, e.key`+limitOffset(limit, 0), tenant, len(prefix), prefix)
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte, dst []float32) []float32 {
	n := len(b) / 4
	if cap(dst) < n {
		dst = make([]float32, n)
	}
	dst = dst[:n]
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return dst
}

// Upsert writes points outside a batch (vector.Index).
func (s *Store) Upsert(ctx context.Context, points []vector.Point) error {
	byTenant := map[string][]vector.Point{}
	for _, p := range points {
		byTenant[p.Tenant] = append(byTenant[p.Tenant], p)
	}
	for t, pts := range byTenant {
		if _, err := s.Write(ctx, &store.Batch{Tenant: t, Vectors: pts}); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes vectors (vector.Index).
func (s *Store) Delete(ctx context.Context, tenant string, targets []vector.Target) error {
	_, err := s.Write(ctx, &store.Batch{Tenant: tenant, VectorDeletes: targets})
	return err
}

// Search scans the stored vectors of the tenant and model exactly.
func (s *Store) Search(ctx context.Context, q vector.Query) ([]vector.Hit, error) {
	qv := append([]float32(nil), q.Values...)
	if err := vector.Normalize(qv); err != nil {
		return nil, fmt.Errorf("%w: %v", memory.ErrInvalid, err)
	}
	conds, args := []string{"tenant = ?", "model = ?"}, []any{q.Tenant, q.Model}
	for col, v := range map[string]string{"user_id": q.User, "agent_id": q.Agent, "run_id": q.Run, "target_kind": string(q.Kind)} {
		if v != "" {
			conds, args = append(conds, col+" = ?"), append(args, v)
		}
	}
	rows, err := s.r.QueryContext(ctx, `SELECT target_kind, target_id, part, vec FROM embeddings WHERE `+strings.Join(conds, " AND "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	top := vector.NewTopK(q.K)
	var buf []float32
	for rows.Next() {
		var h vector.Hit
		var raw []byte
		if err := rows.Scan(&h.Target.Kind, &h.Target.ID, &h.Target.Part, &raw); err != nil {
			return nil, err
		}
		buf = decodeVec(raw, buf)
		if len(buf) != len(qv) {
			return nil, vector.ErrDims
		}
		h.Score = float64(vector.Dot(qv, buf))
		top.Push(h)
	}
	return top.Sorted(), rows.Err()
}

// Vectors pages through stored points of a model.
func (s *Store) Vectors(ctx context.Context, tenant, model, after string, limit int) ([]vector.Point, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT target_kind, target_id, part, modality, user_id, agent_id, run_id, vec FROM embeddings
		WHERE tenant = ? AND model = ? AND target_key > ? ORDER BY target_key`+limitOffset(limit, 0), tenant, model, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vector.Point
	for rows.Next() {
		p := vector.Point{Tenant: tenant, Model: model}
		var raw []byte
		if err := rows.Scan(&p.Target.Kind, &p.Target.ID, &p.Target.Part, &p.Modality, &p.User, &p.Agent, &p.Run, &raw); err != nil {
			return nil, err
		}
		p.Values = decodeVec(raw, nil)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Missing lists open memories without a vector for model, oldest first.
func (s *Store) Missing(ctx context.Context, tenant, model string, limit int) ([]vector.Target, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT m.id FROM memories m WHERE m.tenant = ? AND m.valid_to IS NULL AND m.invalidated_at IS NULL
		AND m.src_kind <> 'document_manifest'
		AND NOT EXISTS (SELECT 1 FROM embeddings v WHERE v.tenant = m.tenant AND v.model = ? AND v.target_key = 'm:' || m.id)
		ORDER BY m.id`+limitOffset(limit, 0), tenant, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vector.Target
	for rows.Next() {
		t := vector.Target{Kind: vector.TargetMemory}
		if err := rows.Scan(&t.ID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Coverage counts open memories with and without a vector for model.
func (s *Store) Coverage(ctx context.Context, tenant, model string) (int, int, error) {
	var have, total int
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(EXISTS (SELECT 1 FROM embeddings v WHERE v.tenant = m.tenant AND v.model = ? AND v.target_key = 'm:' || m.id)), 0)
		FROM memories m WHERE m.tenant = ? AND m.valid_to IS NULL AND m.invalidated_at IS NULL AND m.src_kind <> 'document_manifest'`, model, tenant).Scan(&total, &have)
	return have, total, err
}

// Tenants lists tenants with record counts.
func (s *Store) Tenants(ctx context.Context) ([]store.TenantStats, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT tenant, SUM(m), SUM(e), SUM(n), SUM(j) FROM (
		SELECT tenant, COUNT(*) m, 0 e, 0 n, 0 j FROM memories GROUP BY tenant
		UNION ALL SELECT tenant, 0, COUNT(*), 0, 0 FROM episodes GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, COUNT(*), 0 FROM entities GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, 0, COUNT(*) FROM jobs GROUP BY tenant
		UNION ALL SELECT tenant, 0, 0, 0, 0 FROM settings) GROUP BY tenant ORDER BY tenant`)
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

// QueryReadOnly runs one statement on a read-only connection
// (mode=ro, query_only), so writes fail inside SQLite itself.
func (s *Store) QueryReadOnly(ctx context.Context, query string, maxRows int) ([]string, [][]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.r.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]any
	for rows.Next() && len(out) < maxRows {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
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
