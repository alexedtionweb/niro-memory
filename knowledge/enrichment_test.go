package knowledge

import (
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestDecisionPredicateClassificationPreservesOriginalModelType(t *testing.T) {
	input := EnrichmentInput{Text: "We decided to use PostgreSQL for storage."}
	original := ExtractedFact{Quote: input.Text, Subject: "We", Predicate: "decided to use", Object: "PostgreSQL", Type: memory.EpistemicFact}
	out, err := NormalizeEnrichment(input, Enrichment{Facts: []ExtractedFact{original}, Relations: []ExtractedRelation{}})
	if err != nil || out.Facts[0].Type != memory.EpistemicDecision || out.Facts[0].ModelType != memory.EpistemicFact || out.Facts[0].Quote != input.Text {
		t.Fatalf("decision normalization lost provenance: %+v %v", out, err)
	}
	original.Type = memory.EpistemicHypothesis
	out, err = NormalizeEnrichment(input, Enrichment{Facts: []ExtractedFact{original}, Relations: []ExtractedRelation{}})
	if err != nil || out.Facts[0].Type != memory.EpistemicHypothesis {
		t.Fatal("uncertain claim promoted to decision")
	}
}

func TestGroundedEnrichmentValidation(t *testing.T) {
	input := EnrichmentInput{Text: "Atlas uses Redis.", Candidates: []memory.Memory{{Text: "Atlas uses MySQL."}}}
	good := func() Enrichment {
		return Enrichment{Facts: []ExtractedFact{{Quote: input.Text, Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact}}, Relations: []ExtractedRelation{}}
	}

	t.Run("ExplicitPolarityConflictsNeedSameStructuredUnconditionalClaim", func(t *testing.T) {
		base := memory.Assertion{Subject: "Atlas", Predicate: "uses", Object: "Redis", EpistemicType: memory.EpistemicFact, Confidence: 1,
			Conditions: map[string]string{"polarity": "positive"}}
		input := EnrichmentInput{Text: "Atlas does not use Redis.", Candidates: []memory.Memory{{Text: "Atlas uses Redis.", Metadata: map[string]any{"assertion": base}}}}
		makeResult := func() Enrichment {
			return Enrichment{Facts: []ExtractedFact{{Quote: input.Text, Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, Negated: true}}, Relations: []ExtractedRelation{}}
		}
		out, err := NormalizeEnrichment(input, makeResult())
		if err != nil || len(out.Relations) != 1 || out.Relations[0].Relation != "contradicts" {
			t.Fatalf("explicit denial missing: %+v %v", out, err)
		}
		out, err = NormalizeEnrichment(input, out)
		if err != nil || len(out.Relations) != 1 {
			t.Fatal("duplicate conflict annotation")
		}
		for _, kind := range []string{"same polarity", "different condition", "different object", "unstructured"} {
			t.Run(kind, func(t *testing.T) {
				candidate := base
				candidate.Conditions = map[string]string{"polarity": "positive"}
				switch kind {
				case "same polarity":
					candidate.Conditions["polarity"] = "negative"
				case "different condition":
					candidate.Conditions["environment"] = "testing"
				case "different object":
					candidate.Object = "MySQL"
				}
				m := memory.Memory{Text: "context", Metadata: map[string]any{"assertion": candidate}}
				if kind == "unstructured" {
					m.Metadata = nil
				}
				in := input
				in.Candidates = []memory.Memory{m}
				out, err := NormalizeEnrichment(in, makeResult())
				if err != nil || len(out.Relations) != 0 {
					t.Fatalf("invented conflict: %+v %v", out, err)
				}
			})
		}
	})
	if err := ValidateEnrichment(input, good()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Enrichment){
		"missing arrays":    func(o *Enrichment) { o.Relations = nil },
		"invented evidence": func(o *Enrichment) { o.Facts[0].Quote = "Not recorded." },
		"invented entity":   func(o *Enrichment) { o.Facts[0].Object = "PostgreSQL" },
		"invalid type":      func(o *Enrichment) { o.Facts[0].Type = "authoritative" },
		"invalid candidate": func(o *Enrichment) { o.Relations = []ExtractedRelation{{Fact: 0, Candidate: 10, Relation: "supports"}} },
		"invalid fact":      func(o *Enrichment) { o.Relations = []ExtractedRelation{{Fact: 10, Candidate: 0, Relation: "supports"}} },
		"authority edge": func(o *Enrichment) {
			o.Relations = []ExtractedRelation{{Fact: 0, Candidate: 0, Relation: "supersedes"}}
		},
		"duplicate facts": func(o *Enrichment) { o.Facts = append(o.Facts, o.Facts[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			out := good()
			mutate(&out)
			if err := ValidateEnrichment(input, out); err == nil {
				t.Fatal("invalid extraction accepted")
			}
		})
	}
}
