package knowledge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
)

// ExtractionProfile customizes the bounded contract, never its evidence or
// authority rules. Attributes describe entity properties, not executable prompts.
type ExtractionProfile struct {
	EntityTypes      []string                   `json:"entity_types"`
	FactTypes        []memory.EpistemicType     `json:"fact_types"`
	EntityAttributes map[string]AttributeSchema `json:"entity_attributes,omitempty"`
}

type AttributeSchema struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

func (p *ExtractionProfile) UnmarshalJSON(raw []byte) error {
	type profileJSON ExtractionProfile
	var decoded profileJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("%w: extraction profile: %v", memory.ErrInvalid, err)
	}
	out := ExtractionProfile(decoded)
	if err := out.Validate(); err != nil {
		return err
	}
	*p = out
	return nil
}

func DefaultExtractionProfile() ExtractionProfile {
	return ExtractionProfile{
		EntityTypes: []string{"person", "organization", "team", "project", "service", "technology",
			"database", "location", "document", "concept", "event", "other"},
		FactTypes: []memory.EpistemicType{memory.EpistemicFact, memory.EpistemicObservation,
			memory.EpistemicBelief, memory.EpistemicHypothesis, memory.EpistemicDecision, memory.EpistemicPreference},
	}
}

func EffectiveExtractionProfile(p *ExtractionProfile) (ExtractionProfile, error) {
	if p == nil {
		return DefaultExtractionProfile(), nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ExtractionProfile{}, fmt.Errorf("%w: invalid extraction profile", memory.ErrInvalid)
	}
	var out ExtractionProfile
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

var profileName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

func (p ExtractionProfile) Validate() error {
	fail := func(message string) error {
		return fmt.Errorf("%w: extraction profile: %s", memory.ErrInvalid, message)
	}
	if len(p.EntityTypes) == 0 || len(p.EntityTypes) > 24 || len(p.FactTypes) == 0 || len(p.FactTypes) > 6 || len(p.EntityAttributes) > 8 {
		return fail("require 1..24 entity types, 1..6 fact types and at most 8 entity attributes")
	}
	seen := map[string]bool{}
	for _, name := range p.EntityTypes {
		if !profileName.MatchString(name) || seen[name] {
			return fail("entity types must be unique lowercase identifiers")
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for _, typ := range p.FactTypes {
		if !slices.Contains(DefaultExtractionProfile().FactTypes, typ) || seen[string(typ)] {
			return fail("unsupported or repeated fact type")
		}
		seen[string(typ)] = true
	}
	for name, a := range p.EntityAttributes {
		if !profileName.MatchString(name) || len(a.Description) > 200 {
			return fail("invalid entity attribute name or description")
		}
		if a.Type != "string" && a.Type != "number" && a.Type != "boolean" {
			return fail("attribute types must be string, number or boolean")
		}
		if len(a.Enum) > 16 || len(a.Enum) > 0 && a.Type != "string" {
			return fail("enum requires a string attribute with at most 16 values")
		}
		values := map[string]bool{}
		for _, v := range a.Enum {
			if strings.TrimSpace(v) == "" || len(v) > 100 || values[v] {
				return fail("invalid or duplicate attribute enum value")
			}
			values[v] = true
		}
	}
	return nil
}

type ExtractedEntity struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Quote      string         `json:"quote"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

func entityPronoun(name string) bool {
	if len([]rune(name)) > 1 && name == strings.ToUpper(name) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "i", "we", "you", "he", "she", "it", "they", "me", "us", "them", "eu", "nós", "nos", "você", "vocês", "ele", "ela", "eles", "elas":
		return true
	}
	return false
}

func validateEntityAttributes(profile ExtractionProfile, attributes map[string]any) error {
	for name, value := range attributes {
		a, ok := profile.EntityAttributes[name]
		if !ok {
			return fmt.Errorf("%w: unknown entity attribute %q", memory.ErrInvalid, name)
		}
		valid := false
		switch a.Type {
		case "string":
			v, ok := value.(string)
			valid = ok && len(v) <= 500 && (len(a.Enum) == 0 || slices.Contains(a.Enum, v))
		case "number":
			v, ok := value.(float64)
			valid = ok && !math.IsNaN(v) && !math.IsInf(v, 0)
		case "boolean":
			_, valid = value.(bool)
		}
		if !valid {
			return fmt.Errorf("%w: invalid entity attribute %q", memory.ErrInvalid, name)
		}
	}
	return nil
}

func validateExtractedEntities(input EnrichmentInput, result Enrichment) error {
	profile, err := EffectiveExtractionProfile(input.Profile)
	if err != nil {
		return err
	}
	if len(result.Entities) > MaxEnrichmentEntities {
		return fmt.Errorf("%w: at most %d extracted entities", memory.ErrInvalid, MaxEnrichmentEntities)
	}
	seen := map[string]bool{}
	for i, e := range result.Entities {
		if strings.TrimSpace(e.Name) == "" || len(e.Name) > 200 {
			return fmt.Errorf("%w: entity %d needs a name of 1..200 bytes", memory.ErrInvalid, i)
		}
		if !slices.Contains(profile.EntityTypes, e.Type) {
			return fmt.Errorf("%w: entity %d type %q is not allowed by the extraction profile", memory.ErrInvalid, i, e.Type)
		}
		if len(e.Quote) > 2048 || strings.TrimSpace(e.Quote) == "" || !strings.Contains(input.Text, e.Quote) {
			return fmt.Errorf("%w: entity %d needs verbatim source evidence (select a valid evidence_id)", memory.ErrInvalid, i)
		}
		if !strings.Contains(strings.ToLower(e.Quote), strings.ToLower(e.Name)) {
			return fmt.Errorf("%w: entity %d name is absent from its selected source evidence", memory.ErrInvalid, i)
		}
		key := memory.EntityKey(e.Name)
		if seen[key] {
			return fmt.Errorf("%w: duplicate extracted entity", memory.ErrInvalid)
		}
		seen[key] = true
		if err := validateEntityAttributes(profile, e.Attributes); err != nil {
			return err
		}
	}
	for _, f := range result.Facts {
		if !slices.Contains(profile.FactTypes, f.Type) {
			return fmt.Errorf("%w: fact type excluded by extraction profile", memory.ErrInvalid)
		}
		for _, typ := range []string{f.SubjectType, f.ObjectType} {
			if typ != "" && !slices.Contains(profile.EntityTypes, typ) {
				return fmt.Errorf("%w: inline entity type excluded by extraction profile", memory.ErrInvalid)
			}
			if len(f.SubjectAttributes) > 0 && f.SubjectType == "" || len(f.ObjectAttributes) > 0 && f.ObjectType == "" {
				return fmt.Errorf("%w: entity attributes require a named entity type", memory.ErrInvalid)
			}
			for _, attributes := range []map[string]any{f.SubjectAttributes, f.ObjectAttributes} {
				if err := validateEntityAttributes(profile, attributes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ExtractionJSONSchema describes resolved extraction results. Provider modes
// may use smaller contracts whose source references are resolved by the server.
func ExtractionJSONSchema(p *ExtractionProfile) (json.RawMessage, error) {
	return extractionJSONSchema(p, 0, -1)
}

// EvidenceExtractionJSONSchema replaces model-written quotes with references
// to server-supplied source spans. Persisted enrichments still contain quotes.
func EvidenceExtractionJSONSchema(p *ExtractionProfile, spans int, candidates ...int) (json.RawMessage, error) {
	if spans < 1 {
		return nil, fmt.Errorf("%w: extraction requires at least one source evidence span", memory.ErrInvalid)
	}
	count := -1
	if len(candidates) > 0 {
		count = candidates[0]
	}
	return extractionJSONSchema(p, spans, count)
}

func extractionJSONSchema(p *ExtractionProfile, spans, candidates int) (json.RawMessage, error) {
	profile, err := EffectiveExtractionProfile(p)
	if err != nil {
		return nil, err
	}
	str := func() map[string]any { return map[string]any{"type": "string"} }
	object := func(properties any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	array := func(items any, max int) map[string]any {
		return map[string]any{"type": "array", "maxItems": max, "items": items}
	}
	attributes := map[string]any{}
	for name, a := range profile.EntityAttributes {
		property := map[string]any{"type": a.Type}
		if a.Type == "string" {
			property["maxLength"] = 500
		}
		if a.Description != "" {
			property["description"] = a.Description
		}
		if len(a.Enum) > 0 {
			property["enum"] = a.Enum
		}
		attributes[name] = property
	}
	entityTypes := append([]string{""}, profile.EntityTypes...)
	entityType := map[string]any{"type": "string", "enum": entityTypes}
	var attributeSchema any
	if len(attributes) > 0 {
		attributeSchema = object(attributes, []string{})
	}
	var quoteSchema any = str()
	var evidenceSchema any
	quoteField := "quote"
	if spans > 0 {
		quoteSchema = nil
		evidenceSchema = map[string]any{"type": "integer", "minimum": 0, "maximum": spans - 1}
		quoteField = "evidence_id"
	}
	fact := object(struct {
		Quote             any `json:"quote,omitempty"`
		EvidenceID        any `json:"evidence_id,omitempty"`
		Subject           any `json:"subject"`
		Predicate         any `json:"predicate"`
		Object            any `json:"object"`
		Type              any `json:"type"`
		Negated           any `json:"negated"`
		SubjectType       any `json:"subject_type"`
		ObjectType        any `json:"object_type"`
		SubjectAttributes any `json:"subject_attributes,omitempty"`
		ObjectAttributes  any `json:"object_attributes,omitempty"`
	}{quoteSchema, evidenceSchema, str(), str(), str(), map[string]any{"type": "string", "enum": profile.FactTypes}, map[string]any{"type": "boolean"}, entityType, entityType, attributeSchema, attributeSchema},
		[]string{quoteField, "subject", "subject_type", "predicate", "object", "object_type", "type", "negated"})
	if spans == 0 {
		quoteSchema = map[string]any{"type": "string", "minLength": 1, "maxLength": 2048}
	}
	entity := object(struct {
		Name       any `json:"name"`
		Type       any `json:"type"`
		Quote      any `json:"quote,omitempty"`
		EvidenceID any `json:"evidence_id,omitempty"`
		Attributes any `json:"attributes"`
	}{map[string]any{"type": "string", "minLength": 1, "maxLength": 200}, map[string]any{"type": "string", "enum": profile.EntityTypes},
		quoteSchema, evidenceSchema, object(attributes, []string{})}, []string{"name", "type", quoteField})
	candidateRef := map[string]any{"type": "integer", "minimum": 0}
	if candidates > 0 {
		candidateRef["maximum"] = candidates - 1
	}
	factIndexMax := 3
	if spans == 0 {
		factIndexMax = MaxEnrichmentFacts - 1
	}
	relation := object(map[string]any{"fact": map[string]any{"type": "integer", "minimum": 0, "maximum": factIndexMax},
		"candidate": candidateRef,
		"relation":  map[string]any{"type": "string", "enum": []string{"supports", "contradicts", "extends", "depends_on", "related_to"}}},
		[]string{"fact", "candidate", "relation"})
	// Field order is part of the constrained generation prompt for small local
	// models: extract claims before entity annotations, matching the examples.
	relationLimit := 8
	if candidates == 0 {
		relationLimit = 0
	}
	var excerpts any
	if spans == 0 {
		excerpts = array(map[string]any{"type": "string", "minLength": 1, "maxLength": 2048}, 4)
	}
	factLimit, entityLimit := 4, 12
	if spans == 0 {
		factLimit, entityLimit = MaxEnrichmentFacts, MaxEnrichmentEntities
	}
	root := object(struct {
		Facts     any `json:"facts"`
		Entities  any `json:"entities"`
		Relations any `json:"relations"`
		Excerpts  any `json:"excerpts,omitempty"`
	}{array(fact, factLimit), array(entity, entityLimit), array(relation, relationLimit), excerpts}, []string{"facts", "entities", "relations"})
	return json.Marshal(root)
}
