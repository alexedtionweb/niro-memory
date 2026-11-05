package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Enricher extracts grounded annotations, not authority to replace assertions.
// Candidate indexes are request-local: models never invent persistent IDs.
type Enricher interface {
	Enrich(context.Context, EnrichmentInput) (Enrichment, error)
}

type EnrichmentInput struct {
	Text       string             `json:"text"`
	Candidates []memory.Memory    `json:"-"`
	Profile    *ExtractionProfile `json:"profile,omitempty"`
}

type ExtractedFact struct {
	Quote             string               `json:"quote"`
	Subject           string               `json:"subject"`
	Predicate         string               `json:"predicate"`
	Object            string               `json:"object"`
	Type              memory.EpistemicType `json:"type"`
	Negated           bool                 `json:"negated"`
	ModelType         memory.EpistemicType `json:"model_type,omitempty"`
	SubjectType       string               `json:"subject_type"`
	ObjectType        string               `json:"object_type"`
	SubjectAttributes map[string]any       `json:"subject_attributes,omitempty"`
	ObjectAttributes  map[string]any       `json:"object_attributes,omitempty"`
}

func sourceQuote(text, quote string) (string, bool) {
	if strings.Contains(text, quote) || len(quote) < 2 || !strings.ContainsAny(quote[len(quote)-1:], ".!") {
		return quote, false
	}
	stem := quote[:len(quote)-1]
	found := ""
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], stem)
		if index < 0 {
			break
		}
		index += offset
		end := index + len(stem)
		if end < len(text) && (text[end] == '.' || text[end] == '!') {
			if found != "" {
				return quote, false
			}
			found = text[index : end+1]
		} else if end == len(strings.TrimRight(text, " \t\r\n")) {
			if found != "" {
				return quote, false
			}
			found = text[index:end]
		}
		offset = index + len(stem)
	}
	if found == "" {
		return quote, false
	}
	return found, true
}

func sourceClaimsOnly(input EnrichmentInput, result Enrichment) (Enrichment, error) {
	if len(result.Facts) == 0 {
		return result, nil
	}
	indexes := make([]int, len(result.Facts))
	kept := []ExtractedFact{}
	for i, fact := range result.Facts {
		indexes[i] = -1
		if strings.Contains(input.Text, fact.Quote) {
			indexes[i] = len(kept)
			kept = append(kept, fact)
			continue
		}
		candidate := -1
		for j, m := range input.Candidates {
			if m.Text == fact.Quote {
				candidate = j
				break
			}
		}
		if candidate < 0 {
			return result, fmt.Errorf("%w: fact %d quote is not a verbatim source span or supplied candidate; select a source evidence_id instead of paraphrasing", memory.ErrInvalid, i)
		}
		if err := ValidateEnrichment(EnrichmentInput{Text: fact.Quote, Profile: input.Profile}, Enrichment{Facts: []ExtractedFact{fact}, Relations: []ExtractedRelation{}}); err != nil {
			return result, err
		}
		result.Notes = append(result.Notes, fmt.Sprintf("Ignored candidate-only fact %d from candidate %d: existing context is not new source evidence.", i, candidate))
	}
	if len(kept) == 0 {
		return result, fmt.Errorf("%w: extraction contains candidate claims but no grounded source claim", memory.ErrInvalid)
	}
	for i, relation := range result.Relations {
		if relation.Fact < 0 || relation.Fact >= len(indexes) || indexes[relation.Fact] < 0 {
			return result, fmt.Errorf("%w: relation must reference a grounded source claim", memory.ErrInvalid)
		}
		result.Relations[i].Fact = indexes[relation.Fact]
	}
	result.Facts = kept
	return result, nil
}

// NormalizeEnrichment classifies explicit decision predicates and adds only
// opposite-polarity matches against an already structured unconditional fact.
func NormalizeEnrichment(input EnrichmentInput, result Enrichment) (Enrichment, error) {
	if err := validateExtractedEntities(input, Enrichment{Facts: result.Facts}); err != nil {
		return result, err
	}
	// Only terminal statement/emphasis punctuation may be aligned, and only to
	// one exact source span. Never align a question, changed words or ambiguity.
	for i, fact := range result.Facts {
		if quote, changed := sourceQuote(input.Text, fact.Quote); changed {
			result.Notes = append(result.Notes, fmt.Sprintf("Aligned fact %d quote punctuation to a unique verbatim source span (original quote: %q).", i, fact.Quote))
			result.Facts[i].Quote = quote
		}
	}
	for i, entity := range result.Entities {
		if quote, changed := sourceQuote(input.Text, entity.Quote); changed {
			result.Notes = append(result.Notes, fmt.Sprintf("Aligned entity %d quote punctuation to a unique verbatim source span (original quote: %q).", i, entity.Quote))
			result.Entities[i].Quote = quote
		}
	}
	var err error
	result, err = sourceClaimsOnly(input, result)
	if err != nil {
		return result, err
	}
	for i, fact := range result.Facts {
		if fact.SubjectType != "" && entityPronoun(fact.Subject) {
			result.Notes = append(result.Notes, fmt.Sprintf("Ignored entity type %q for pronoun subject %q.", fact.SubjectType, fact.Subject))
			result.Facts[i].SubjectType = ""
		}
		if fact.ObjectType != "" && entityPronoun(fact.Object) {
			result.Notes = append(result.Notes, fmt.Sprintf("Ignored entity type %q for pronoun object %q.", fact.ObjectType, fact.Object))
			result.Facts[i].ObjectType = ""
		}
	}
	if result.Entities != nil {
		kept := []ExtractedEntity{}
		for _, entity := range result.Entities {
			if err := validateExtractedEntities(input, Enrichment{Entities: []ExtractedEntity{entity}}); err != nil {
				return result, err
			}
			if entityPronoun(entity.Name) {
				result.Notes = append(result.Notes, fmt.Sprintf("Ignored entity type %q for pronoun %q.", entity.Type, entity.Name))
				continue
			}
			kept = append(kept, entity)
		}
		result.Entities = kept
	}
	for _, fact := range result.Facts {
		for _, entity := range []ExtractedEntity{{Name: fact.Subject, Type: fact.SubjectType, Quote: fact.Quote, Attributes: fact.SubjectAttributes}, {Name: fact.Object, Type: fact.ObjectType, Quote: fact.Quote, Attributes: fact.ObjectAttributes}} {
			if entity.Type == "" {
				if len(entity.Attributes) > 0 {
					return result, fmt.Errorf("%w: entity attributes require a named entity type", memory.ErrInvalid)
				}
				continue
			}
			if err := validateExtractedEntities(input, Enrichment{Entities: []ExtractedEntity{entity}}); err != nil {
				return result, err
			}
			found := false
			for j, existing := range result.Entities {
				if memory.EntityKey(existing.Name) == memory.EntityKey(entity.Name) && existing.Type == entity.Type {
					for name, value := range entity.Attributes {
						if old, ok := existing.Attributes[name]; ok && !reflect.DeepEqual(old, value) {
							return result, fmt.Errorf("%w: inconsistent entity attributes", memory.ErrInvalid)
						}
						if result.Entities[j].Attributes == nil {
							result.Entities[j].Attributes = map[string]any{}
						}
						result.Entities[j].Attributes[name] = value
					}
					found = true
					break
				}
			}
			if !found {
				result.Entities = append(result.Entities, entity)
			}
		}
	}
	if result.Entities != nil {
		unique := []ExtractedEntity{}
		seen := map[string]bool{}
		for _, entity := range result.Entities {
			if err := validateExtractedEntities(input, Enrichment{Entities: []ExtractedEntity{entity}}); err != nil {
				return result, err
			}
			raw, err := json.Marshal(struct {
				Name, Type string
				Attributes map[string]any
			}{memory.EntityKey(entity.Name), entity.Type, entity.Attributes})
			if err != nil {
				return result, fmt.Errorf("%w: entity attributes: %v", memory.ErrInvalid, err)
			}
			key := string(raw)
			if seen[key] {
				continue
			}
			seen[key] = true
			unique = append(unique, entity)
		}
		result.Entities = unique
	}
	unique := []ExtractedFact{}
	indexes := make([]int, len(result.Facts))
	seen := map[string]int{}
	for i, f := range result.Facts {
		key := strings.ToLower(memory.Normalize(f.Subject)) + "\x00" + strings.ToLower(memory.Normalize(f.Predicate)) +
			"\x00" + strings.ToLower(memory.Normalize(f.Object)) + "\x00" + string(f.Type) + fmt.Sprint(f.Negated) + "\x00" + f.Quote
		if index, ok := seen[key]; ok {
			indexes[i] = index
			continue
		}
		indexes[i] = len(unique)
		seen[key] = len(unique)
		unique = append(unique, f)
	}
	for i, r := range result.Relations {
		if r.Fact < 0 || r.Fact >= len(indexes) {
			return result, fmt.Errorf("%w: extracted relation references an unknown fact", memory.ErrInvalid)
		}
		result.Relations[i].Fact = indexes[r.Fact]
	}
	result.Facts = unique
	if err := ValidateEnrichment(input, result); err != nil {
		return result, err
	}
	for i, f := range result.Facts {
		if f.Type != memory.EpistemicFact {
			continue
		}
		predicate := strings.ToLower(memory.Normalize(f.Predicate))
		for _, verb := range []string{"decided", "decides", "chose", "chooses", "selected", "selects"} {
			if predicate == verb || strings.HasPrefix(predicate, verb+" ") {
				profile, err := EffectiveExtractionProfile(input.Profile)
				if err != nil {
					return result, err
				}
				for _, typ := range profile.FactTypes {
					if typ == memory.EpistemicDecision {
						result.Facts[i].ModelType = f.Type
						result.Facts[i].Type = memory.EpistemicDecision
					}
				}
				break
			}
		}
	}
	for i, f := range result.Facts {
		if f.Type != memory.EpistemicFact {
			continue
		}
		for j, m := range input.Candidates {
			a, err := memory.ReadAssertion(m)
			if err != nil {
				return result, err
			}
			if a == nil || a.EpistemicType != memory.EpistemicFact || !strings.EqualFold(memory.Normalize(a.Subject), memory.Normalize(f.Subject)) ||
				!strings.EqualFold(memory.Normalize(a.Predicate), memory.Normalize(f.Predicate)) || !strings.EqualFold(memory.Normalize(a.Object), memory.Normalize(f.Object)) {
				continue
			}
			unconditional := true
			for key := range a.Conditions {
				if key != "polarity" {
					unconditional = false
				}
			}
			if !unconditional {
				continue
			}
			polarity := a.Conditions["polarity"]
			if polarity != "positive" && polarity != "negative" {
				continue
			}
			if (polarity == "negative") == f.Negated {
				continue
			}
			exists := false
			for _, r := range result.Relations {
				if r.Fact == i && r.Candidate == j && r.Relation == "contradicts" {
					exists = true
				}
			}
			if !exists && len(result.Relations) < 8 {
				result.Relations = append(result.Relations, ExtractedRelation{Fact: i, Candidate: j, Relation: "contradicts"})
			}
		}
	}
	return result, nil
}

type ExtractedRelation struct {
	Fact      int    `json:"fact"`
	Candidate int    `json:"candidate"`
	Relation  string `json:"relation"`
}

type Enrichment struct {
	Excerpts  []string            `json:"excerpts,omitempty"`
	Facts     []ExtractedFact     `json:"facts"`
	Entities  []ExtractedEntity   `json:"entities,omitempty"`
	Relations []ExtractedRelation `json:"relations"`
	Provider  string              `json:"provider,omitempty"`
	Model     string              `json:"model,omitempty"`
	Notes     []string            `json:"normalization_notes,omitempty"`
	Issues    []ExtractionIssue   `json:"issues,omitempty"`
}

// ValidateEnrichment enforces source grounding and bounded local references.
// A source quote proves provenance, not that an extracted interpretation is true.
func ValidateEnrichment(input EnrichmentInput, result Enrichment) error {
	if len(result.Excerpts) > 4 || len(result.Excerpts) > 0 && (len(result.Facts) > 0 || len(result.Entities) > 0 || len(result.Relations) > 0) {
		return fmt.Errorf("%w: excerpts require at most four source spans and no structured annotations", memory.ErrInvalid)
	}
	seenExcerpts := map[string]bool{}
	for _, text := range result.Excerpts {
		if strings.TrimSpace(text) == "" || len(text) > 2048 || !strings.Contains(input.Text, text) || seenExcerpts[text] {
			return fmt.Errorf("%w: excerpt must be a unique verbatim source span of at most 2048 bytes", memory.ErrInvalid)
		}
		seenExcerpts[text] = true
	}
	if err := validateExtractedEntities(input, result); err != nil {
		return err
	}
	if result.Facts == nil || result.Relations == nil || len(result.Facts) > MaxEnrichmentFacts || len(result.Relations) > 8 {
		return fmt.Errorf("%w: enrichment needs facts (0..%d) and relations (0..8) arrays", memory.ErrInvalid, MaxEnrichmentFacts)
	}
	seen := map[string]bool{}
	for _, f := range result.Facts {
		if strings.TrimSpace(f.Quote) == "" || !strings.Contains(input.Text, f.Quote) ||
			len(f.Quote) > 2048 || strings.TrimSpace(f.Subject) == "" || strings.TrimSpace(f.Predicate) == "" ||
			strings.TrimSpace(f.Object) == "" || len(f.Subject) > 200 || len(f.Predicate) > 100 || len(f.Object) > 500 {
			return fmt.Errorf("%w: extracted fact needs a verbatim source quote and bounded assertion", memory.ErrInvalid)
		}
		// Entities must actually occur in the evidence; predicates may normalize
		// wording (e.g. "uses"). Canonical alias resolution is deliberately separate.
		quote := strings.ToLower(f.Quote)
		if !strings.Contains(quote, strings.ToLower(f.Subject)) || !strings.Contains(quote, strings.ToLower(f.Object)) {
			return fmt.Errorf("%w: extracted entity absent from quoted evidence", memory.ErrInvalid)
		}
		switch f.Type {
		case memory.EpistemicFact, memory.EpistemicObservation, memory.EpistemicBelief,
			memory.EpistemicHypothesis, memory.EpistemicDecision, memory.EpistemicPreference:
		default:
			return fmt.Errorf("%w: unsupported extracted epistemic type", memory.ErrInvalid)
		}
		key := strings.ToLower(memory.Normalize(f.Subject)) + "\x00" + strings.ToLower(memory.Normalize(f.Predicate)) + "\x00" + strings.ToLower(memory.Normalize(f.Object)) +
			"\x00" + string(f.Type) + fmt.Sprint(f.Negated) + "\x00" + f.Quote
		if seen[key] {
			return fmt.Errorf("%w: duplicate extracted assertion", memory.ErrInvalid)
		}
		seen[key] = true
	}
	for _, r := range result.Relations {
		if r.Fact < 0 || r.Fact >= len(result.Facts) || r.Candidate < 0 || r.Candidate >= len(input.Candidates) {
			return fmt.Errorf("%w: extracted relationship references an unknown candidate", memory.ErrInvalid)
		}
		switch r.Relation {
		case "supports", "contradicts", "extends", "depends_on", "related_to":
		default:
			return fmt.Errorf("%w: unsupported extracted relationship", memory.ErrInvalid)
		}
	}
	return nil
}
