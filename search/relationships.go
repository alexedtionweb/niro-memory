package search

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func (s *Searcher) relationshipCandidates(ctx context.Context, r Request, q store.Query, now time.Time, c *collector, limit int) (int, error) {
	if r.Policy.RelationshipHops < 0 || r.Policy.RelationshipHops > 4 {
		return 0, fmt.Errorf("%w: relationship_hops must be 0..4", memory.ErrInvalid)
	}
	rs, ok := s.Store.(relations.Store)
	if !ok {
		return 0, fmt.Errorf("%w: relationship expansion unsupported by store", memory.ErrInvalid)
	}
	var frontier []string
	for id := range c.cands {
		frontier = append(frontier, id)
	}
	sort.Strings(frontier)
	if len(frontier) > limit {
		frontier = frontier[:limit]
	}
	seen, count := map[string]bool{}, 0
	for hop := 0; hop < r.Policy.RelationshipHops && len(frontier) > 0; hop++ {
		ms, err := s.Store.Get(ctx, r.Tenant, frontier)
		if err != nil {
			return 0, err
		}
		frontier = nil
		for _, m := range ms {
			if q.Filter.Match(&m) && q.Admits(&m, now) && !seen[m.ID] {
				seen[m.ID], frontier = true, append(frontier, m.ID)
			}
			if len(frontier) == 0 {
				break
			}
		}
		links, err := rs.RelationshipSets(ctx, r.Tenant, frontier)
		if err != nil {
			return 0, err
		}
		next := []string{}
		for i, l := range links {
			if i >= 400 {
				break
			}
			tm := memory.Memory{RecordedAt: l.At, ValidFrom: l.ValidFrom, ValidTo: l.ValidTo, InvalidatedAt: l.InvalidatedAt}
			if !q.Admits(&tm, now) {
				continue
			}
			endpoints, err := s.Store.Get(ctx, r.Tenant, []string{l.Source, l.Target})
			if err != nil {
				return 0, err
			}
			if len(endpoints) != 2 || !q.Filter.Match(&endpoints[0]) || !q.Filter.Match(&endpoints[1]) ||
				!q.Admits(&endpoints[0], now) || !q.Admits(&endpoints[1], now) {
				continue
			}
			for _, m := range endpoints {
				if _, exists := c.cands[m.ID]; !exists {
					if count >= limit {
						continue
					}
					x := c.get(m.ID)
					x.ent, x.found, x.ranks["entity"] = 0.25*math.Max(0, l.Confidence), []string{"relationship"}, count+1
					count++
				}
				if !seen[m.ID] && len(next) < limit {
					next = append(next, m.ID)
				}
			}
		}
		frontier = next
	}
	return count, nil
}
