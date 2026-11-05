package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestExtractionProfileSchemaAndGrounding(t *testing.T) {
	p := DefaultExtractionProfile()
	p.EntityTypes = []string{"person", "service"}
	p.EntityAttributes = map[string]AttributeSchema{"role": {Type: "string", Enum: []string{"owner", "reviewer"}}, "active": {Type: "boolean"}, "budget": {Type: "number"}}
	schema, err := ExtractionJSONSchema(&p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Index(schema, []byte(`"facts":`)) > bytes.Index(schema, []byte(`"entities":`)) {
		t.Fatal("schema order no longer matches compact CPU examples")
	}
	var decoded map[string]any
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatal(err)
	}
	properties := decoded["properties"].(map[string]any)
	factProperties := properties["facts"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if factProperties["subject_attributes"] == nil || factProperties["object_attributes"] == nil {
		t.Fatal("configured inline entity attributes missing from provider schema")
	}
	entity := properties["entities"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	types := entity["type"].(map[string]any)["enum"].([]any)
	if len(types) != 2 || types[0] != "person" {
		t.Fatalf("profile not reflected in schema: %s", schema)
	}
	attr := entity["attributes"].(map[string]any)
	if attr["additionalProperties"] != false || len(attr["properties"].(map[string]any)) != 3 {
		t.Fatal("unbounded entity attributes")
	}
	input := EnrichmentInput{Text: "Alice owns Atlas.", Profile: &p}
	good := Enrichment{Facts: []ExtractedFact{}, Relations: []ExtractedRelation{}, Entities: []ExtractedEntity{{Name: "Alice", Type: "person", Quote: input.Text, Attributes: map[string]any{"role": "owner", "active": true, "budget": float64(20)}}}}
	if err := ValidateEnrichment(input, good); err != nil {
		t.Fatal(err)
	}
	for _, entity := range []ExtractedEntity{
		{Name: "Bob", Type: "person", Quote: input.Text},
		{Name: "Alice", Type: "database", Quote: input.Text},
		{Name: "Alice", Type: "person", Quote: "Alice owns Jupiter."},
		{Name: "Alice", Type: "person", Quote: input.Text, Attributes: map[string]any{"secret": "value"}},
		{Name: "Alice", Type: "person", Quote: input.Text, Attributes: map[string]any{"active": "yes"}},
		{Name: "Alice", Type: "person", Quote: input.Text, Attributes: map[string]any{"role": "administrator"}},
	} {
		if err := ValidateEnrichment(input, Enrichment{Facts: []ExtractedFact{}, Relations: []ExtractedRelation{}, Entities: []ExtractedEntity{entity}}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("invalid entity accepted: %+v %v", entity, err)
		}
	}
}

func TestStatementQuoteAlignmentNeverInventsEvidence(t *testing.T) {
	for _, test := range []struct {
		text, quote, want string
		changed           bool
	}{
		{"Atlas uses Redis!", "Atlas uses Redis.", "Atlas uses Redis!", true},
		{"Atlas uses Redis.", "Atlas uses Redis!", "Atlas uses Redis.", true},
		{"EU AMO CACHORRO QUENTE", "EU AMO CACHORRO QUENTE.", "EU AMO CACHORRO QUENTE", true},
		{"Atlas uses Redis \n", "Atlas uses Redis.", "Atlas uses Redis", true},
		{"Atlas uses Redis in tests", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas uses Redis? \n", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas uses Redis! Atlas uses Redis", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas uses Redis?", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas does not use Redis!", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas uses MySQL!", "Atlas uses Redis.", "Atlas uses Redis.", false},
		{"Atlas uses Redis! Atlas uses Redis!", "Atlas uses Redis.", "Atlas uses Redis.", false},
	} {
		got, changed := sourceQuote(test.text, test.quote)
		if got != test.want || changed != test.changed {
			t.Fatalf("quote alignment %+v -> %q %v", test, got, changed)
		}
	}
	f := ExtractedFact{Quote: "Atlas uses Redis!", Subject: "Atlas", SubjectType: "project", Predicate: "uses", Object: "Redis", ObjectType: "database", Type: memory.EpistemicFact}
	duplicate := f
	duplicate.Quote = "Atlas uses Redis."
	out, err := NormalizeEnrichment(EnrichmentInput{Text: f.Quote}, Enrichment{Facts: []ExtractedFact{f, duplicate}, Relations: []ExtractedRelation{}})
	if err != nil || len(out.Facts) != 1 || out.Facts[0].Quote != f.Quote || len(out.Notes) != 1 {
		t.Fatalf("grounded duplicate normalization: %+v %v", out, err)
	}
}

func TestMissingTerminalPunctuationRetainsExactEvidence(t *testing.T) {
	text := "EU AMO CACHORRO QUENTE"
	out, err := NormalizeEnrichment(EnrichmentInput{Text: text}, Enrichment{
		Facts:     []ExtractedFact{{Quote: text + ".", Subject: "EU", Predicate: "amo", Object: "CACHORRO QUENTE", Type: memory.EpistemicPreference}},
		Entities:  []ExtractedEntity{{Name: "CACHORRO QUENTE", Type: "concept", Quote: text + "."}},
		Relations: []ExtractedRelation{},
	})
	if err != nil || len(out.Notes) != 2 || out.Facts[0].Quote != text || out.Entities[0].Quote != text {
		t.Fatalf("source punctuation alignment: %+v %v", out, err)
	}
	if err := ValidateEnrichment(EnrichmentInput{Text: text}, out); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateEchoIsNeverAdmittedAsNewSourceEvidence(t *testing.T) {
	source := ExtractedFact{Quote: "Atlas does not use Redis.", Subject: "Atlas", SubjectType: "project", Predicate: "uses", Object: "Redis", ObjectType: "database", Type: memory.EpistemicFact, Negated: true}
	echo := source
	echo.Quote = "Atlas uses Redis."
	echo.Negated = false
	input := EnrichmentInput{Text: source.Quote, Candidates: []memory.Memory{{Text: echo.Quote}}}
	out, err := NormalizeEnrichment(input, Enrichment{Facts: []ExtractedFact{echo, source}, Relations: []ExtractedRelation{{Fact: 1, Candidate: 0, Relation: "contradicts"}}})
	if err != nil || len(out.Facts) != 1 || !out.Facts[0].Negated || len(out.Entities) != 2 || out.Relations[0].Fact != 0 || len(out.Notes) != 1 {
		t.Fatalf("candidate echo: %+v %v", out, err)
	}
	for _, facts := range [][]ExtractedFact{{echo}, {source, {Quote: "Atlas uses Jupiter.", Subject: "Atlas", Predicate: "uses", Object: "Jupiter", Type: memory.EpistemicFact}}} {
		if _, err := NormalizeEnrichment(input, Enrichment{Facts: facts, Relations: []ExtractedRelation{}}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatal("ungrounded or candidate-only output became successful extraction")
		}
	}
	if _, err := NormalizeEnrichment(input, Enrichment{Facts: []ExtractedFact{echo, source}, Relations: []ExtractedRelation{{Fact: 0, Candidate: 0, Relation: "supports"}}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("relation to discarded candidate claim accepted")
	}
}

func TestExtractionProfileBoundsAndIsolation(t *testing.T) {
	for _, raw := range []string{
		`{"entity_types":["person"],"fact_types":["fact"],"properties":{}}`,
		`{"entity_types":["person"],"fact_types":["fact"],"entity_attributes":{"role":{"type":"string","unsupported":true}}}`,
	} {
		var profile ExtractionProfile
		if err := json.Unmarshal([]byte(raw), &profile); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("uncontrolled schema fields ignored: %s %v", raw, err)
		}
	}
	for _, change := range []func(*ExtractionProfile){
		func(p *ExtractionProfile) { p.EntityTypes = nil },
		func(p *ExtractionProfile) { p.EntityTypes = []string{"person", "person"} },
		func(p *ExtractionProfile) { p.EntityTypes = []string{"Person"} },
		func(p *ExtractionProfile) { p.FactTypes = []memory.EpistemicType{"command"} },
		func(p *ExtractionProfile) { p.EntityAttributes = map[string]AttributeSchema{"x": {Type: "object"}} },
		func(p *ExtractionProfile) {
			p.EntityAttributes = map[string]AttributeSchema{"x": {Type: "boolean", Enum: []string{"yes"}}}
		},
	} {
		p := DefaultExtractionProfile()
		change(&p)
		if _, err := ExtractionJSONSchema(&p); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("invalid profile accepted: %+v %v", p, err)
		}
	}
	p := DefaultExtractionProfile()
	copy, err := EffectiveExtractionProfile(&p)
	if err != nil {
		t.Fatal(err)
	}
	copy.EntityTypes[0] = "changed"
	if p.EntityTypes[0] == "changed" || !slices.Contains(p.EntityTypes, "database") {
		t.Fatal("profile aliased caller or missing defaults")
	}
}

func TestNormalizeDuplicateFactsPreservesPolarityAndRelationIndexes(t *testing.T) {
	f := ExtractedFact{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact}
	negative := f
	negative.Quote = "Atlas does not use Redis."
	negative.Negated = true
	input := EnrichmentInput{Text: f.Quote + " " + negative.Quote, Candidates: []memory.Memory{{Text: f.Quote}}}
	out, err := NormalizeEnrichment(input, Enrichment{Facts: []ExtractedFact{f, f, negative}, Relations: []ExtractedRelation{{Fact: 1, Candidate: 0, Relation: "supports"}}})
	if err != nil || len(out.Facts) != 2 || out.Relations[0].Fact != 0 || !out.Facts[1].Negated {
		t.Fatalf("duplicate normalization: %+v %v", out, err)
	}

}

func TestInlineEntityNormalizationRetainsAttributesAndExplainsPronouns(t *testing.T) {
	p := DefaultExtractionProfile()
	p.EntityAttributes = map[string]AttributeSchema{"role": {Type: "string"}}
	f := ExtractedFact{Quote: "We chose Redis.", Subject: "We", SubjectType: "organization", Predicate: "chose", Object: "Redis", ObjectType: "database", ObjectAttributes: map[string]any{"role": "cache"}, Type: memory.EpistemicDecision}
	out, err := NormalizeEnrichment(EnrichmentInput{Text: f.Quote, Profile: &p}, Enrichment{Facts: []ExtractedFact{f}, Entities: []ExtractedEntity{
		{Name: "Redis", Type: "database", Quote: f.Quote, Attributes: map[string]any{"role": "cache"}},
	}, Relations: []ExtractedRelation{}})
	if err != nil || len(out.Entities) != 1 || out.Entities[0].Name != "Redis" || out.Entities[0].Attributes["role"] != "cache" ||
		out.Facts[0].SubjectType != "" || len(out.Notes) != 1 {
		t.Fatalf("inline annotations: %+v %v", out, err)
	}
	invalid := f
	invalid.ObjectType = "unsupported"
	if _, err := NormalizeEnrichment(EnrichmentInput{Text: f.Quote}, Enrichment{Facts: []ExtractedFact{invalid}, Relations: []ExtractedRelation{}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("unsupported inline type accepted")
	}
	invalid = f
	invalid.ObjectAttributes = map[string]any{"unknown": "value"}
	if _, err := NormalizeEnrichment(EnrichmentInput{Text: f.Quote, Profile: &p}, Enrichment{Facts: []ExtractedFact{invalid}, Relations: []ExtractedRelation{}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("unknown inline attribute accepted")
	}
	if _, err := NormalizeEnrichment(EnrichmentInput{Text: f.Quote, Profile: &p}, Enrichment{Facts: []ExtractedFact{f}, Entities: []ExtractedEntity{
		{Name: "Redis", Type: "database", Quote: f.Quote, Attributes: map[string]any{"role": "storage"}},
	}, Relations: []ExtractedRelation{}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("inconsistent inline/list entity attributes silently merged")
	}
}
