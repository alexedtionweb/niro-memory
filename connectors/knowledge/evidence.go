package knowledge

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

type evidenceSpan struct {
	ID         int    `json:"id"`
	Text       string `json:"text"`
	start, end int
}

// Spans are source slices, never summaries. Segmentation does not classify
// claims; it only gives the model bounded evidence handles to select.
func sourceEvidence(text string) []evidenceSpan {
	var spans []evidenceSpan
	for start := 0; start < len(text); {
		end := start
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if end+size-start > 2048 {
				break
			}
			end += size
			if r == '\n' || (r == '.' || r == '!' || r == '?') && (end == len(text) || startsWhitespace(text[end:])) {
				break
			}
		}
		quote := strings.TrimSpace(text[start:end])
		if quote != "" {
			offset := start + strings.Index(text[start:end], quote)
			spans = append(spans, evidenceSpan{ID: len(spans), Text: quote, start: offset, end: offset + len(quote)})
		}
		start = end
	}
	return spans
}

func startsWhitespace(text string) bool {
	r, _ := utf8.DecodeRuneInString(text)
	return unicode.IsSpace(r)
}

type evidenceFact struct {
	domain.ExtractedFact
	EvidenceID *int `json:"evidence_id"`
}

type evidenceEntity struct {
	domain.ExtractedEntity
	EvidenceID *int `json:"evidence_id"`
}

func marshalEvidence(value any, id *int) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "quote")
	fields["evidence_id"], err = json.Marshal(id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (f evidenceFact) MarshalJSON() ([]byte, error) {
	return marshalEvidence(f.ExtractedFact, f.EvidenceID)
}

func (e evidenceEntity) MarshalJSON() ([]byte, error) {
	return marshalEvidence(e.ExtractedEntity, e.EvidenceID)
}

type evidenceResponse struct {
	domain.Enrichment
	Facts    []evidenceFact   `json:"facts"`
	Entities []evidenceEntity `json:"entities"`
}

func (r evidenceResponse) resolve(spans []evidenceSpan) (domain.Enrichment, error) {
	out := r.Enrichment
	if r.Facts != nil {
		out.Facts = make([]domain.ExtractedFact, len(r.Facts))
	}
	if r.Entities != nil {
		out.Entities = make([]domain.ExtractedEntity, len(r.Entities))
	}
	resolve := func(id *int, quote, field string) (string, error) {
		// Legacy exact-quote responses remain supported, but pass the unchanged
		// grounding validator. A fabricated quote never becomes source evidence.
		if id == nil {
			return quote, nil
		}
		if *id < 0 || *id >= len(spans) {
			return "", fmt.Errorf("%w: %s evidence_id %d is not a supplied source span", memory.ErrInvalid, field, *id)
		}
		if quote != "" && quote != spans[*id].Text {
			return "", fmt.Errorf("%w: %s quote conflicts with selected source span %d; return evidence_id only", memory.ErrInvalid, field, *id)
		}
		return spans[*id].Text, nil
	}
	for i, fact := range r.Facts {
		quote, err := resolve(fact.EvidenceID, fact.Quote, fmt.Sprintf("fact %d", i))
		if err != nil {
			out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "fact_evidence", Index: i, Reason: err.Error()})
			// Keep the index for relation validation, but never reuse a quote
			// that conflicts with the selected evidence.
			out.Facts[i] = fact.ExtractedFact
			out.Facts[i].Quote = ""
			continue
		}
		out.Facts[i] = fact.ExtractedFact
		out.Facts[i].Quote = quote
	}
	for i, entity := range r.Entities {
		quote, err := resolve(entity.EvidenceID, entity.Quote, fmt.Sprintf("entity %d", i))
		if err != nil {
			out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "entity_evidence", Index: i, Reason: err.Error()})
			out.Entities[i] = entity.ExtractedEntity
			out.Entities[i].Quote = ""
			continue
		}
		out.Entities[i] = entity.ExtractedEntity
		out.Entities[i].Quote = quote
	}
	return out, nil
}
