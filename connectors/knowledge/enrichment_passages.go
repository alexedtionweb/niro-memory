package knowledge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

var extractionNumericValue = regexp.MustCompile(`[0-9]+(?:[.,][0-9]+)*(?:\s*%)?`)

func omittedNumericValues(out domain.Enrichment) []string {
	assertions := map[string]string{}
	for _, fact := range out.Facts {
		assertions[fact.Quote] += " " + fact.Subject + " " + fact.Predicate + " " + fact.Object
	}
	values := func(text string) map[string]bool {
		result := map[string]bool{}
		for _, value := range extractionNumericValue.FindAllString(text, -1) {
			result[strings.Join(strings.Fields(value), "")] = true
		}
		return result
	}
	var missing []string
	checked := map[string]bool{}
	for _, fact := range out.Facts {
		present := values(assertions[fact.Quote])
		for _, value := range extractionNumericValue.FindAllString(fact.Quote, -1) {
			value = strings.Join(strings.Fields(value), "")
			if !present[value] && !checked[value] {
				missing = append(missing, value)
				checked[value] = true
			}
		}
	}
	return missing
}

// Each bounded unit gets its own evidence IDs. Merging uses resolved quotes,
// never indexes from another unit or model-generated summaries.
func (c *Connector) enrichWindows(ctx context.Context, input domain.EnrichmentInput) (domain.Enrichment, error) {
	spans := sourceEvidence(input.Text)
	out := domain.Enrichment{Facts: []domain.ExtractedFact{}, Entities: []domain.ExtractedEntity{}, Relations: []domain.ExtractedRelation{}, Provider: c.options.Protocol, Model: c.options.Model}
	for start := 0; start < len(spans); start++ {
		if start >= 16 {
			out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "coverage", Index: start, Reason: "Passage exceeds 16 evidence spans; split the source further to extract remaining details."})
			break
		}
		unit := input
		unit.Text = input.Text[spans[start].start:spans[start].end]
		result, err := c.enrichUnit(ctx, unit)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			var review *domain.ExtractionReviewRequired
			if !errors.Is(err, memory.ErrInvalid) && !errors.As(err, &review) {
				if len(out.Facts) > 0 || len(out.Entities) > 0 {
					out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "provider", Index: start, Reason: fmt.Sprintf("Extraction interrupted after validated windows; this and remaining windows need repair: %v", err)})
					return domain.RecoverEnrichment(input, out)
				}
				return out, err
			}
			out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "window", Index: start, Reason: err.Error()})
			continue
		}
		offset := len(out.Facts)
		out.Facts = append(out.Facts, result.Facts...)
		out.Entities = append(out.Entities, result.Entities...)
		for _, r := range result.Relations {
			r.Fact += offset
			out.Relations = append(out.Relations, r)
		}
		for _, issue := range result.Issues {
			issue.Index = start
			out.Issues = append(out.Issues, issue)
		}
		out.Notes = append(out.Notes, result.Notes...)
	}
	out.Notes = append(out.Notes, fmt.Sprintf("Extracted independently from %d bounded source windows.", min(len(spans), 16)))
	if len(out.Relations) > 8 {
		out.Relations = out.Relations[:8]
		out.Issues = append(out.Issues, domain.ExtractionIssue{Stage: "relations", Index: 8, Reason: "More than eight candidate links; remaining proposals require a separate relationship pass."})
	}
	return domain.RecoverEnrichment(input, out)
}
