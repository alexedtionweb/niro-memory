package niromem

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/store"
)

func entityAnnotations(f knowledge.ExtractedFact, entities []knowledge.ExtractedEntity) ([]memory.EntityRef, []knowledge.ExtractedEntity) {
	refs := []memory.EntityRef{}
	annotations := []knowledge.ExtractedEntity{}
	seen := map[string]bool{}
	for _, e := range entities {
		if !strings.Contains(strings.ToLower(f.Quote), strings.ToLower(e.Name)) {
			continue
		}
		refs = append(refs, memory.EntityRef{Name: e.Name, Type: e.Type})
		annotations = append(annotations, e)
		seen[memory.EntityKey(e.Name)] = true
	}
	// Legacy enrichers may supply only triples. Keep those untyped rather than
	// inferring an entity class that the provider never returned.
	if entities != nil {
		return refs, annotations
	}
	for _, name := range []string{f.Subject, f.Object} {
		if !seen[memory.EntityKey(name)] {
			refs = append(refs, memory.EntityRef{Name: name})
			seen[memory.EntityKey(name)] = true
		}
	}
	return refs, annotations
}

func enrichmentIdentity(m memory.Memory, f knowledge.ExtractedFact, entities []knowledge.ExtractedEntity, source memory.Memory) (string, error) {
	canonical := func(s string) string { return strings.ToLower(memory.Normalize(s)) }
	// A triple alone cannot represent qualifiers omitted by a model. Retain the
	// whole quote whenever it contains additional substantive/conditional words.
	words := func(s string) []string {
		return strings.FieldsFunc(canonical(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
	}
	core := map[string]bool{}
	for _, w := range words(f.Subject + " " + f.Predicate + " " + f.Object) {
		core[w] = true
	}
	for _, w := range []string{"a", "an", "the", "is", "are", "does", "do"} {
		core[w] = true
	}
	qualifier := ""
	for _, w := range words(f.Quote) {
		if !core[w] {
			qualifier = canonical(f.Quote)
			break
		}
	}
	type entityIdentity struct {
		Name       string
		Type       string
		Attributes map[string]any
	}
	es := []entityIdentity{}
	for _, e := range entities {
		es = append(es, entityIdentity{canonical(e.Name), e.Type, e.Attributes})
	}
	slices.SortFunc(es, func(a, b entityIdentity) int { return strings.Compare(a.Name, b.Name) })
	grants := append([]string(nil), m.Grants...)
	slices.Sort(grants)
	grants = slices.Compact(grants)
	a, err := memory.ReadAssertion(m)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", fmt.Errorf("%w: enrichment identity requires a structured assertion", memory.ErrInvalid)
	}
	validity := "current_observation"
	if !source.ValidFrom.Equal(source.RecordedAt) {
		validity = source.ValidFrom.String()
	}
	raw, err := json.Marshal(struct {
		User, Agent, Run, Visibility, Trust, Subject, Predicate, Object, Type, Qualifier, Validity string
		Grants                                                                                     []string
		Conditions                                                                                 map[string]string
		ValidTo, ExpiresAt                                                                         *time.Time
		Entities                                                                                   []entityIdentity
	}{m.User, m.Agent, m.Run, string(m.Visibility), string(m.Source.Trust), canonical(f.Subject), canonical(f.Predicate), canonical(f.Object),
		string(f.Type), qualifier, validity, grants, a.Conditions, m.ValidTo, m.ExpiresAt, es})
	if err != nil {
		return "", err
	}
	return memory.Hash(string(raw)), nil
}

// reconcileEnriched adds append-only evidence instead of modifying assertion or
// provenance. NoMatch is checked under the same tenant transaction as insertion.
func (e *Engine) reconcileEnriched(ctx context.Context, b *store.Batch, m memory.Memory, f knowledge.ExtractedFact, entities []knowledge.ExtractedEntity, source memory.Memory) (pipeline.Change, error) {
	identity, err := enrichmentIdentity(m, f, entities, source)
	if err != nil {
		return pipeline.Change{}, err
	}
	m.Metadata["enrichment_identity"] = identity
	filter := memory.And(memory.Eq("metadata.enrichment_identity", identity),
		memory.Eq("user_id", source.User), memory.Eq("agent_id", source.Agent), memory.Eq("run_id", source.Run),
		memory.Eq("visibility", string(source.Visibility)), memory.Eq("trust", string(source.Source.Trust)), memory.Eq("source", "knowledge_enriched"),
		memory.Filter{Field: "valid_to", Op: memory.OpExists, Value: false},
		memory.Filter{Field: "invalidated_at", Op: memory.OpExists, Value: false})
	if err := filter.Check(); err != nil {
		return pipeline.Change{}, err
	}
	existing, err := e.o.Store.Find(ctx, store.Query{Tenant: b.Tenant, Filter: filter, AllVersions: true, Limit: 1, Order: store.NewestFirst})
	if err != nil {
		return pipeline.Change{}, err
	}
	if len(existing) > 0 {
		target := existing[0]
		a, err := memory.ReadAssertion(target)
		if err != nil {
			return pipeline.Change{}, err
		}
		if a == nil {
			return pipeline.Change{}, fmt.Errorf("%w: stored enrichment identity has no assertion", memory.ErrInvalid)
		}
		raw, err := json.Marshal(target.Metadata["extracted_entities"])
		if err != nil {
			return pipeline.Change{}, err
		}
		var annotations []knowledge.ExtractedEntity
		if err := json.Unmarshal(raw, &annotations); err != nil {
			return pipeline.Change{}, err
		}
		actual, err := enrichmentIdentity(target, knowledge.ExtractedFact{Subject: a.Subject, Predicate: a.Predicate, Object: a.Object, Type: a.EpistemicType, Quote: target.Text}, annotations, source)
		if err != nil {
			return pipeline.Change{}, err
		}
		if actual != identity || !source.ValidFrom.Equal(source.RecordedAt) && !target.ValidFrom.Equal(source.ValidFrom) {
			return pipeline.Change{}, fmt.Errorf("%w: stored enrichment identity does not match its assertion, audience or validity", memory.ErrInvalid)
		}
		b.Checks = append(b.Checks, store.Check{ID: target.ID, Version: target.Version, Open: true})
		b.Events = append(b.Events, memory.Event{MemoryID: target.ID, Action: memory.ActionEvidence, New: f.Quote, Related: source.ID, Reason: "grounded_extraction:duplicate_support", At: e.o.Now()})
		return pipeline.Change{Action: memory.ActionEvidence, Memory: target, Reason: "grounded_extraction:duplicate_support"}, nil
	}
	// Also reconcile repeated interpretations inside this single extraction.
	for _, target := range b.Memories {
		if target.Metadata["enrichment_identity"] == identity {
			return pipeline.Change{Action: memory.ActionEvidence, Memory: target, Reason: "grounded_extraction:duplicate_support"}, nil
		}
	}
	b.Checks = append(b.Checks, store.Check{NoMatch: &filter})
	b.Memories = append(b.Memories, m)
	b.Events = append(b.Events, memory.Event{MemoryID: m.ID, Action: memory.ActionAdd, New: m.Text, Related: source.ID, Reason: "grounded_extraction:unreviewed", At: e.o.Now()})
	return pipeline.Change{Action: memory.ActionAdd, Memory: m, Reason: "grounded_extraction:unreviewed"}, nil
}
