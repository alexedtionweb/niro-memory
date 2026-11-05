package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/sqlgen"
)

func (s *Store) Operation(ctx context.Context, tenant, key string) (*store.Receipt, error) {
	var raw []byte
	err := s.r.QueryRowContext(ctx, `SELECT receipt FROM operation_receipts WHERE tenant = ? AND key = ?`, tenant, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, memory.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return store.DecodeReceipt(raw)
}

func (w *writer) replay(b *store.Batch) (*store.Receipt, error) {
	if b.OperationKey == "" {
		return nil, nil
	}
	var hash string
	var raw []byte
	err := w.tx.QueryRowContext(w.ctx, `SELECT hash, receipt FROM operation_receipts WHERE tenant = ? AND key = ?`, w.t, b.OperationKey).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hash != b.OperationHash {
		return nil, conflict("operation hash changed")
	}
	r, err := store.DecodeReceipt(raw)
	if err != nil {
		return nil, err
	}
	r.Replayed = true
	return r, nil
}

func (w *writer) checkBatch(b *store.Batch) error {
	if len(b.Result) != 0 && !json.Valid(b.Result) {
		return fmt.Errorf("%w: invalid result JSON", memory.ErrInvalid)
	}
	if _, err := json.Marshal(b); err != nil {
		return fmt.Errorf("%w: batch cannot be encoded: %v", memory.ErrInvalid, err)
	}
	for _, c := range b.Checks {
		if c.NoMatch != nil {
			f := *c.NoMatch
			if err := f.Check(); err != nil {
				return err
			}
			builder := sqlgen.Builder{D: dialect{}}
			tenant := builder.Arg(w.t)
			where, err := builder.Where(f)
			if err != nil {
				return err
			}
			var exists bool
			if err := w.tx.QueryRowContext(w.ctx, `SELECT EXISTS(SELECT 1 FROM memories m WHERE m.tenant = `+tenant+` AND `+where+`)`, builder.Args...).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return conflict("memory set changed")
			}
			continue
		}
		var version int
		var open bool
		err := w.tx.QueryRowContext(w.ctx, `SELECT version, valid_to IS NULL AND invalidated_at IS NULL FROM memories WHERE tenant = ? AND id = ?`, w.t, c.ID).Scan(&version, &open)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (version != c.Version || (c.Open && !open))) {
			return conflict("memory %s changed", c.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) redactOperations(deleted []string) error {
	if len(deleted) == 0 {
		return nil
	}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT key, receipt FROM operation_receipts WHERE tenant = ?`, w.t)
	if err != nil {
		return err
	}
	type update struct {
		key string
		raw []byte
	}
	var updates []update
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			break
		}
		var r *store.Receipt
		r, err = store.DecodeReceipt(raw)
		if err != nil {
			break
		}
		if store.RedactReceipt(r, deleted) {
			raw, err = store.EncodeReceipt(r)
			if err != nil {
				break
			}
			updates = append(updates, update{key, raw})
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, u := range updates {
		if _, err := w.exec(`UPDATE operation_receipts SET receipt = ? WHERE tenant = ? AND key = ?`, string(u.raw), w.t, u.key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RelationshipSets(ctx context.Context, tenant string, anchors []string) ([]relations.Link, error) {
	if len(anchors) == 0 {
		return []relations.Link{}, nil
	}
	args := []any{tenant}
	params := make([]string, len(anchors))
	for i, id := range anchors {
		args = append(args, id)
		params[i] = "?" + strconv.Itoa(i+2)
	}
	in := strings.Join(params, ",")
	rows, err := s.r.QueryContext(ctx, `SELECT data FROM knowledge_relations WHERE tenant = ?1 AND (source IN (`+in+`) OR target IN (`+in+`))
		ORDER BY julianday(json_extract(data,'$.at')) DESC, id LIMIT `+strconv.Itoa(relations.MaxReadLinks), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relations.Link{}
	for rows.Next() {
		var raw []byte
		var r relations.Link
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (w *writer) relations(b *store.Batch) error {
	if len(b.Relations)+len(b.RelationCloses)+len(b.Closes)+len(b.Deletes) == 0 {
		return nil
	}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT data FROM knowledge_relations WHERE tenant = ? ORDER BY id`, w.t)
	if err != nil {
		return err
	}
	var existing []relations.Link
	for rows.Next() {
		var raw []byte
		var r relations.Link
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &r); err != nil {
			break
		}
		existing = append(existing, r)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	present := map[string]bool{}
	for _, r := range b.Relations {
		for _, id := range []string{r.Source, r.Target} {
			if _, checked := present[id]; checked {
				continue
			}
			var n int
			if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM memories WHERE tenant = ? AND id = ?`, w.t, id).Scan(&n); err != nil {
				return err
			}
			present[id] = n != 0
		}
	}
	links, jobs, err := store.PlanRelations(b, existing, func(id string) bool { return present[id] }, w.now)
	if err != nil {
		return err
	}
	for _, r := range links {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := w.exec(`INSERT INTO knowledge_relations(tenant,id,source,target,data) VALUES(?,?,?,?,?)
			ON CONFLICT(tenant,id) DO UPDATE SET data=excluded.data
			WHERE knowledge_relations.data <> excluded.data`, w.t, r.ID, r.Source, r.Target, string(raw)); err != nil {
			return err
		}
	}
	w.relationJobs = append(w.relationJobs, jobs...)
	return nil
}
