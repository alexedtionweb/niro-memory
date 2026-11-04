package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Enricher extracts grounded annotations, not authority to replace assertions.
// Candidate indexes are request-local: models never invent persistent IDs.
type Enricher interface {
	Enrich(context.Context, EnrichmentInput) (Enrichment, error)
}

type EnrichmentInput struct {
	Text       string          `json:"text"`
	Candidates []memory.Memory `json:"-"`
}

type ExtractedFact struct {
	Quote     string               `json:"quote"`
	Subject   string               `json:"subject"`
	Predicate string               `json:"predicate"`
	Object    string               `json:"object"`
	Type      memory.EpistemicType `json:"type"`
	Negated   bool                 `json:"negated"`
	ModelType memory.EpistemicType `json:"model_type,omitempty"`
}

// NormalizeEnrichment classifies explicit decision predicates and adds only
// opposite-polarity matches against an already structured unconditional fact.
func NormalizeEnrichment(input EnrichmentInput, result Enrichment) (Enrichment, error) {
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
				result.Facts[i].ModelType = f.Type
				result.Facts[i].Type = memory.EpistemicDecision
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
	Facts     []ExtractedFact     `json:"facts"`
	Relations []ExtractedRelation `json:"relations"`
	Provider  string              `json:"provider,omitempty"`
	Model     string              `json:"model,omitempty"`
}

// ValidateEnrichment enforces source grounding and bounded local references.
// A source quote proves provenance, not that an extracted interpretation is true.
func ValidateEnrichment(input EnrichmentInput, result Enrichment) error {
	if result.Facts == nil || result.Relations == nil || len(result.Facts) > 4 || len(result.Relations) > 8 {
		return fmt.Errorf("%w: enrichment needs facts (0..4) and relations (0..8) arrays", memory.ErrInvalid)
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
		key := f.Subject + "\x00" + f.Predicate + "\x00" + f.Object
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
