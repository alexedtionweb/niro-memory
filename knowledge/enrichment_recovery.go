package knowledge

import (
	"fmt"
	"reflect"

	"github.com/alexedtionweb/niro-memory/memory"
)

const MaxEnrichmentFacts = 64
const MaxEnrichmentEntities = 320

type ExtractionIssue struct {
	Stage  string `json:"stage"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// RecoverEnrichment validates independent records without allowing a malformed
// optional annotation to discard unrelated grounded assertions.
func RecoverEnrichment(input EnrichmentInput, result Enrichment) (Enrichment, error) {
	if len(result.Excerpts) > 0 {
		return result, ValidateEnrichment(input, result)
	}
	out := Enrichment{Facts: []ExtractedFact{}, Entities: []ExtractedEntity{}, Relations: []ExtractedRelation{}, Provider: result.Provider, Model: result.Model, Notes: result.Notes, Issues: append([]ExtractionIssue(nil), result.Issues...)}
	issue := func(stage string, index int, err error) {
		out.Issues = append(out.Issues, ExtractionIssue{Stage: stage, Index: index, Reason: err.Error()})
	}
	if result.Facts == nil || result.Relations == nil {
		issue("schema", 0, fmt.Errorf("%w: facts, entities and relations arrays are required", memory.ErrInvalid))
	}
	if len(result.Facts) > MaxEnrichmentFacts || len(result.Entities) > MaxEnrichmentEntities || len(result.Relations) > 8 {
		return out, fmt.Errorf("%w: extraction recovery bounds exceeded", memory.ErrInvalid)
	}
	indexes := map[int]int{}
	types := map[string]string{}
	attributes := map[string]map[string]any{}
	compatibleAttributes := func(name string, values map[string]any, stage string, index int) map[string]any {
		if len(values) == 0 {
			return values
		}
		key := memory.EntityKey(name)
		if attributes[key] == nil {
			attributes[key] = map[string]any{}
		}
		kept := map[string]any{}
		for attribute, value := range values {
			if old, ok := attributes[key][attribute]; ok && !reflect.DeepEqual(old, value) {
				issue(stage, index, fmt.Errorf("conflicting %q attribute for %q excluded", attribute, name))
				continue
			}
			attributes[key][attribute], kept[attribute] = value, value
		}
		return kept
	}
	for i, f := range result.Facts {
		single := Enrichment{Facts: []ExtractedFact{f}, Entities: []ExtractedEntity{}, Relations: []ExtractedRelation{}}
		normalized, err := NormalizeEnrichment(input, single)
		if err != nil {
			// Named-entity classifications are optional, not part of the claim.
			f.SubjectType, f.ObjectType = "", ""
			f.SubjectAttributes, f.ObjectAttributes = nil, nil
			single.Facts = []ExtractedFact{f}
			normalized, err = NormalizeEnrichment(input, single)
			if err != nil {
				issue("fact", i, err)
				continue
			}
			issue("fact_annotations", i, fmt.Errorf("invalid entity annotations removed; grounded assertion retained"))
		}
		if len(normalized.Facts) == 0 {
			continue
		}
		f = normalized.Facts[0]
		for _, annotation := range []struct {
			name       string
			typ        *string
			attributes *map[string]any
		}{{f.Subject, &f.SubjectType, &f.SubjectAttributes}, {f.Object, &f.ObjectType, &f.ObjectAttributes}} {
			key := memory.EntityKey(annotation.name)
			if old := types[key]; old != "" && *annotation.typ != "" && old != *annotation.typ {
				issue("fact_annotations", i, fmt.Errorf("conflicting entity classification for %q excluded", annotation.name))
				*annotation.typ = ""
				*annotation.attributes = nil
			} else if *annotation.typ != "" {
				types[key] = *annotation.typ
				*annotation.attributes = compatibleAttributes(annotation.name, *annotation.attributes, "fact_annotations", i)
			}
		}
		indexes[i] = len(out.Facts)
		out.Facts = append(out.Facts, f)
	}
	entityNames := map[string]ExtractedEntity{}
	for i, entity := range result.Entities {
		if err := validateExtractedEntities(input, Enrichment{Entities: []ExtractedEntity{entity}}); err != nil {
			issue("entity", i, err)
			continue
		}
		key := memory.EntityKey(entity.Name)
		if old := types[key]; old != "" && old != entity.Type {
			issue("entity", i, fmt.Errorf("conflicting entity classification excluded"))
			continue
		}
		entity.Attributes = compatibleAttributes(entity.Name, entity.Attributes, "entity", i)
		if old, ok := entityNames[key]; ok {
			if old.Type != entity.Type || !reflect.DeepEqual(old.Attributes, entity.Attributes) {
				issue("entity", i, fmt.Errorf("conflicting duplicate entity annotation excluded"))
			}
			continue
		}
		types[key] = entity.Type
		entityNames[key] = entity
		out.Entities = append(out.Entities, entity)
	}
	for i, r := range result.Relations {
		index, ok := indexes[r.Fact]
		if !ok {
			issue("relation", i, fmt.Errorf("relationship depends on a rejected fact"))
			continue
		}
		r.Fact = index
		if err := ValidateEnrichment(input, Enrichment{Facts: out.Facts, Relations: []ExtractedRelation{r}}); err != nil {
			issue("relation", i, err)
			continue
		}
		out.Relations = append(out.Relations, r)
	}
	// Normalize handles duplicate facts/entities and synthesizes grounded inline
	// annotations only after the independent records have been validated.
	return NormalizeEnrichment(input, out)
}
