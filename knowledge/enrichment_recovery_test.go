package knowledge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestRecoverEnrichmentIsolatesRecordsAndPreservesRelationIndexes(t *testing.T) {
	input := EnrichmentInput{Text: "Atlas uses Redis. Bruna works at Contoso.", Candidates: []memory.Memory{{Text: "Related context"}}}
	result := Enrichment{Facts: []ExtractedFact{
		{Quote: "invented evidence", Subject: "Atlas", Predicate: "uses", Object: "MongoDB", Type: memory.EpistemicFact},
		{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, SubjectType: "invented_type"},
		{Quote: "Bruna works at Contoso.", Subject: "Bruna", Predicate: "works at", Object: "Contoso", Type: memory.EpistemicFact},
	}, Entities: []ExtractedEntity{{Name: "", Type: "person", Quote: "Atlas uses Redis."}, {Name: "Bruna", Type: "person", Quote: "Bruna works at Contoso."}},
		Relations: []ExtractedRelation{{Fact: 0, Candidate: 0, Relation: "related_to"}, {Fact: 2, Candidate: 0, Relation: "related_to"}}}
	out, err := RecoverEnrichment(input, result)
	if err != nil || len(out.Facts) != 2 || len(out.Entities) != 1 || len(out.Relations) != 1 || out.Relations[0].Fact != 1 || len(out.Issues) != 4 {
		t.Fatalf("independent recovery: %+v %v", out, err)
	}
	if out.Facts[0].SubjectType != "" {
		t.Fatal("invalid annotation survived")
	}
	if err := ValidateEnrichment(input, out); err != nil {
		t.Fatal(err)
	}
	for _, fact := range out.Facts {
		if !strings.Contains(input.Text, fact.Quote) {
			t.Fatal("grounding bypass")
		}
	}
}

func TestRecoverySupportsMoreThanFourIndependentDocumentFacts(t *testing.T) {
	input := EnrichmentInput{}
	out := Enrichment{Facts: []ExtractedFact{}, Entities: []ExtractedEntity{}, Relations: []ExtractedRelation{}}
	for i := range 8 {
		subject := fmt.Sprintf("Item %d", i)
		quote := subject + " uses Redis."
		input.Text += quote + " "
		out.Facts = append(out.Facts, ExtractedFact{Quote: quote, Subject: subject, Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact})
	}

	got, err := RecoverEnrichment(input, out)
	if err != nil || len(got.Facts) != 8 {
		t.Fatalf("bounded merge/dedup failed: %+v %v", got, err)
	}
}

func TestRecoveryKeepsFactsWhenIndependentEntityAttributesConflict(t *testing.T) {
	profile := DefaultExtractionProfile()
	profile.EntityAttributes = map[string]AttributeSchema{"role": {Type: "string"}}
	input := EnrichmentInput{Text: "Bruna owns Atlas. Bruna reviews Atlas.", Profile: &profile}
	result := Enrichment{Facts: []ExtractedFact{
		{Quote: "Bruna owns Atlas.", Subject: "Bruna", SubjectType: "person", SubjectAttributes: map[string]any{"role": "owner"}, Predicate: "owns", Object: "Atlas", Type: memory.EpistemicFact},
		{Quote: "Bruna reviews Atlas.", Subject: "Bruna", SubjectType: "person", SubjectAttributes: map[string]any{"role": "reviewer"}, Predicate: "reviews", Object: "Atlas", Type: memory.EpistemicFact},
	}, Entities: []ExtractedEntity{{Name: "Bruna", Type: "person", Quote: "Bruna reviews Atlas.", Attributes: map[string]any{"role": "reviewer"}}}, Relations: []ExtractedRelation{}}
	got, err := RecoverEnrichment(input, result)
	if err != nil || len(got.Facts) != 2 || len(got.Entities) != 1 || got.Entities[0].Attributes["role"] != "owner" || len(got.Issues) == 0 {
		t.Fatalf("annotation conflicts erased grounded claims: %+v %v", got, err)
	}
	if result.Facts[1].SubjectAttributes["role"] != "reviewer" {
		t.Fatal("recovery mutated provider records")
	}
}
