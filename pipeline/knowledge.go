package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

// PlanKnowledge maps already validated semantic operations onto the same
// reconciliation and transactional write boundary as explicit memories.
func (r *Reconciler) PlanKnowledge(ctx context.Context, tenant string, plan knowledge.Plan, b *store.Batch) ([]Change, error) {
	var out []Change
	targeted := map[string]bool{}
	for index, op := range plan.Operations {
		typ := string(op.Type)
		if typ == "IGNORE" || typ == "DEFER" {
			continue
		}
		var old memory.Memory
		if op.TargetID != "" {
			if targeted[op.TargetID] {
				return nil, fmt.Errorf("%w: multiple operations target one version", memory.ErrInvalid)
			}
			targeted[op.TargetID] = true
			ms, err := r.Store.Get(ctx, tenant, []string{op.TargetID})
			if err != nil {
				return nil, err
			}
			if len(ms) != 1 || ms[0].Version != op.ExpectedVersion || !store.Open(&ms[0]) {
				return nil, fmt.Errorf("%w: knowledge target changed", memory.ErrConflict)
			}
			old = ms[0]
			b.Checks = append(b.Checks, store.Check{ID: old.ID, Version: old.Version, Open: true})
		}
		now := r.Now()
		if typ == "RELATE" {
			for i, link := range op.Relations {
				endpoints, err := r.Store.Get(ctx, tenant, []string{link.Source, link.Target})
				if err != nil {
					return nil, err
				}
				if len(endpoints) != 2 || !store.Open(&endpoints[0]) || !store.Open(&endpoints[1]) {
					return nil, fmt.Errorf("%w: relationship endpoint changed", memory.ErrConflict)
				}
				for _, endpoint := range endpoints {
					b.Checks = append(b.Checks, store.Check{ID: endpoint.ID, Version: endpoint.Version, Open: true})
				}
				link.ID = memory.Hash(plan.OperationKey + ":" + strconv.Itoa(index) + ":" + strconv.Itoa(i))
				link.Tenant, link.OperationKey, link.At = tenant, plan.OperationKey, now
				link.Origin = "validated"
				b.Relations = append(b.Relations, link)
			}
			continue
		}
		m := op.Memory
		m.ID, m.Supersedes, m.Version, m.Heat = "", "", 0, 0
		m.InvalidatedAt, m.ClosedAt = nil, nil
		m.Tenant, m.Source.Kind = tenant, "knowledge"
		if typ == "REINFORCE" {
			if old.ID == "" {
				return nil, fmt.Errorf("%w: reinforcement needs a target", memory.ErrInvalid)
			}
			b.Touches = append(b.Touches, store.Touch{ID: old.ID, Heat: 1})
			// New evidence is associated through an append-only event; the
			// original version's provenance is not rewritten.
			evidence, err := json.Marshal(m.Source.Evidence)
			if err != nil {
				return nil, err
			}
			b.Events = append(b.Events, memory.Event{MemoryID: old.ID, Action: memory.ActionEvidence,
				New: string(evidence), Reason: op.ReasonCode, At: now})
			out = append(out, Change{Action: memory.ActionEvidence, Memory: old, Reason: op.ReasonCode})
			continue
		}
		if typ == "RETRACT" {
			if old.ID == "" {
				return nil, fmt.Errorf("%w: retraction needs a target", memory.ErrInvalid)
			}
			b.Closes = append(b.Closes, store.Close{ID: old.ID, InvalidatedAt: &now})
			b.Events = append(b.Events, memory.Event{MemoryID: old.ID, Action: memory.ActionInvalidate,
				Old: old.Text, Reason: op.ReasonCode, At: now})
			out = append(out, Change{Action: memory.ActionInvalidate, Memory: old, Reason: op.ReasonCode})
			continue
		}
		Prepare(&m, now)
		if typ == "CORRECT" || typ == "SUPERSEDE" {
			if old.ID == "" {
				return nil, fmt.Errorf("%w: replacement needs a target", memory.ErrInvalid)
			}
			c := store.Close{ID: old.ID}
			action := memory.ActionUpdate
			if typ == "SUPERSEDE" {
				if !m.ValidFrom.After(old.ValidFrom) {
					return nil, fmt.Errorf("%w: supersession must start after the previous assertion", memory.ErrInvalid)
				}
				c.ValidTo, c.ClosedAt, action = &m.ValidFrom, &now, memory.ActionSupersede
			} else {
				c.InvalidatedAt = &now
			}
			b.Closes = append(b.Closes, c)
			m.Supersedes, m.Version, m.Slot = old.ID, old.Version+1, old.Slot
			m.User, m.Agent, m.Visibility, m.Grants = old.User, old.Agent, old.Visibility, append([]string(nil), old.Grants...)
			b.Events = append(b.Events, memory.Event{MemoryID: old.ID, Action: action,
				Old: old.Text, New: m.Text, Related: m.ID, Reason: op.ReasonCode, At: now})
		} else if typ == "BRANCH" || typ == "REFINE" {
			if old.ID == "" {
				return nil, fmt.Errorf("%w: refinement or branch needs a target", memory.ErrInvalid)
			}
			m.Slot = ""
		} else if typ != "ADD" {
			return nil, fmt.Errorf("%w: unsupported knowledge operation %q", memory.ErrInvalid, typ)
		}
		if typ == "ADD" && m.Slot != "" {
			q := open(tenant, memory.And(owner(&m), memory.Eq("slot", m.Slot)))
			q.Limit = 1
			ms, err := r.Store.Find(ctx, q)
			if err != nil {
				return nil, err
			}
			if len(ms) > 0 {
				return nil, fmt.Errorf("%w: ADD cannot implicitly replace an occupied slot", memory.ErrInvalid)
			}
		}
		changes, err := r.Plan(ctx, tenant, []Proposal{{Memory: m, ForceVersion: typ == "CORRECT" || typ == "SUPERSEDE"}}, b)
		if err != nil {
			return nil, err
		}
		if changes[0].Action == ActionReject {
			return nil, fmt.Errorf("%w: %s", memory.ErrInvalid, changes[0].Reason)
		}
		out = append(out, changes...)
		if old.ID != "" && changes[0].Memory.ID != old.ID {
			relation := "supersedes"
			if typ == "REFINE" {
				relation = "refines"
			}
			if typ == "BRANCH" {
				relation = "exception_to"
			}
			b.Relations = append(b.Relations, relations.Link{
				ID: memory.Hash(plan.OperationKey + ":" + strconv.Itoa(index)), Tenant: tenant,
				Source: changes[0].Memory.ID, Target: old.ID, Relation: relation,
				Confidence: m.Confidence, Evidence: m.Source.Evidence, Reason: op.ReasonCode,
				Provider: plan.Provider, Origin: "validated", At: now, ValidFrom: m.ValidFrom, OperationKey: plan.OperationKey})
		}
	}
	return out, nil
}
