package knowledge

import (
	"strings"
	"testing"
)

func TestValidateExtractiveExcerpts(t *testing.T) {
	for name, excerpts := range map[string][]string{
		"fabricated": {"MongoDB is used."},
		"blank":      {""},
		"duplicate":  {"Atlas uses Redis.", "Atlas uses Redis."},
		"oversized":  {strings.Repeat("x", 2049)},
		"too-many":   {"A", "B", "C", "D", "E"},
	} {
		t.Run(name, func(t *testing.T) {
			result := Enrichment{Facts: []ExtractedFact{}, Entities: []ExtractedEntity{}, Relations: []ExtractedRelation{}, Excerpts: excerpts}
			if err := ValidateEnrichment(EnrichmentInput{Text: "Atlas uses Redis. ABCDE " + strings.Repeat("x", 2050)}, result); err == nil {
				t.Fatal("invalid source excerpt accepted")
			}
		})
	}
	valid := Enrichment{Facts: []ExtractedFact{}, Entities: []ExtractedEntity{}, Relations: []ExtractedRelation{}, Excerpts: []string{"Atlas uses Redis."}}
	if err := ValidateEnrichment(EnrichmentInput{Text: "Atlas uses Redis."}, valid); err != nil {
		t.Fatal(err)
	}
	valid.Relations = []ExtractedRelation{{Fact: 0, Candidate: 0, Relation: "supports"}}
	if err := ValidateEnrichment(EnrichmentInput{Text: "Atlas uses Redis."}, valid); err == nil {
		t.Fatal("structured annotations mixed with source-only selection")
	}
}
