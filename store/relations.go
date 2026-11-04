package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
)

// Dependency notification budget for one batch. Dependents are reached by
// following active derived_from/depends_on links backwards from every closed
// memory, at most MaxDependencyDepth hops and MaxDependencyJobs jobs in total.
// When either bound stops the walk, every job of the batch carries
// "truncated":true so handlers never assume the dependent set is complete.
const (
	MaxDependencyDepth = 4
	MaxDependencyJobs  = 128
)

// dependencyJobs plans one knowledge.reevaluate job per (closed root,
// transitive dependent) pair. Payload is {"id":dependent,"source":root}; the
// stable pending key is the JSON array [dependent, root]. Cycles are cut by a
// per-root visited set; memories closed in the same batch are not notified.
func dependencyJobs(b *Batch, rows []relations.Link, active map[string]bool) []Job {
	if len(b.Closes) == 0 {
		return nil
	}
	dependents := map[string][]string{}
	for _, r := range rows {
		if active[r.ID] && (r.Relation == "derived_from" || r.Relation == "depends_on") {
			dependents[r.Target] = append(dependents[r.Target], r.Source)
		}
	}
	closed := map[string]bool{}
	for _, c := range b.Closes {
		closed[c.ID] = true
	}
	type pair struct{ id, root string }
	var found []pair
	seenRoot := map[string]bool{}
	truncated, capped := false, false
	for _, c := range b.Closes {
		if capped {
			break
		}
		root := c.ID
		if seenRoot[root] {
			continue
		}
		seenRoot[root] = true
		visited := map[string]bool{root: true}
		frontier := []string{root}
		for depth := 1; len(frontier) > 0 && !capped; depth++ {
			var next []string
			for _, n := range frontier {
				for _, d := range dependents[n] {
					if visited[d] {
						continue
					}
					if depth > MaxDependencyDepth {
						truncated = true
						continue
					}
					if len(found) >= MaxDependencyJobs {
						truncated, capped = true, true
						break
					}
					visited[d] = true
					if closed[d] {
						continue
					}
					found = append(found, pair{d, root})
					next = append(next, d)
				}
				if capped {
					break
				}
			}
			frontier = next
		}
	}
	jobs := make([]Job, 0, len(found))
	for _, f := range found {
		p := map[string]any{"id": f.id, "source": f.root}
		if truncated {
			p["truncated"] = true
		}
		payload, _ := json.Marshal(p)
		key, _ := json.Marshal([]string{f.id, f.root})
		jobs = append(jobs, Job{Kind: "knowledge.reevaluate", Key: string(key), Payload: payload})
	}
	return jobs
}

// PlanRelations validates the complete relation change before returning its
// replacement rows and durable dependency notifications. Existing rows are
// copied; callers must commit the result with the rest of the batch.
func PlanRelations(b *Batch, existing []relations.Link, exists func(string) bool, now time.Time) ([]relations.Link, []Job, error) {
	rows := append([]relations.Link(nil), existing...)
	byID := map[string]int{}
	for i, r := range rows {
		byID[r.ID] = i
	}
	deleted := map[string]bool{}
	for _, id := range b.Deletes {
		deleted[id] = true
	}
	for _, r := range b.Relations {
		if r.ID == "" || r.Source == "" || r.Target == "" || r.Relation == "" ||
			(r.Tenant != "" && r.Tenant != b.Tenant) || !exists(r.Source) || !exists(r.Target) ||
			deleted[r.Source] || deleted[r.Target] {
			return nil, nil, fmt.Errorf("%w: invalid relation endpoints or identity", memory.ErrInvalid)
		}
		r.Tenant = b.Tenant
		if r.OperationKey == "" {
			r.OperationKey = b.OperationKey
		}
		if i, ok := byID[r.ID]; ok {
			old := rows[i]
			if r.At.IsZero() {
				r.At = old.At
			}
			if r.ValidFrom.IsZero() {
				r.ValidFrom = old.ValidFrom
			}
			// A repeated append may not reopen a system-closed historical row.
			if r.InvalidatedAt == nil {
				old.InvalidatedAt = nil
			}
			a, ea := json.Marshal(old)
			c, ec := json.Marshal(r)
			if ea != nil || ec != nil || !bytes.Equal(a, c) {
				return nil, nil, fmt.Errorf("%w: relation %s changed meaning", memory.ErrConflict, r.ID)
			}
			continue
		}
		if r.At.IsZero() {
			r.At = now
		}
		if r.ValidFrom.IsZero() {
			r.ValidFrom = r.At
		}
		if _, err := json.Marshal(r); err != nil {
			return nil, nil, fmt.Errorf("%w: invalid relation: %v", memory.ErrInvalid, err)
		}
		byID[r.ID] = len(rows)
		rows = append(rows, r)
	}
	active := map[string]bool{}
	for _, r := range rows {
		active[r.ID] = r.InvalidatedAt == nil
	}
	for _, c := range b.RelationCloses {
		i, ok := byID[c.ID]
		if !ok || rows[i].InvalidatedAt != nil {
			return nil, nil, fmt.Errorf("%w: relation %s is not open", memory.ErrConflict, c.ID)
		}
		at := c.At
		if at.IsZero() {
			at = now
		}
		rows[i].InvalidatedAt = &at
	}
	jobs := dependencyJobs(b, rows, active)
	for _, c := range b.Closes {
		for i := range rows {
			r := &rows[i]
			if !active[r.ID] || (r.Source != c.ID && r.Target != c.ID) {
				continue
			}
			at := now
			if c.ClosedAt != nil {
				at = *c.ClosedAt
			} else if c.InvalidatedAt != nil {
				at = *c.InvalidatedAt
			}
			if r.InvalidatedAt == nil {
				r.InvalidatedAt = &at
			}
		}
	}
	out := rows[:0]
	for _, r := range rows {
		if !deleted[r.Source] && !deleted[r.Target] {
			out = append(out, r)
		}
	}
	return out, jobs, nil
}
