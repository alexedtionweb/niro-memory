package pipeline

import (
	"context"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
)

// SourceExtractor preserves text as evidence before asynchronous interpretation.
// It makes no model calls and assigns no interpreted assertions.
type SourceExtractor struct{}

func (SourceExtractor) Name() string { return "source" }

func (SourceExtractor) Extract(_ context.Context, _ memory.Run, episodes []memory.Episode) ([]Proposal, error) {
	var out []Proposal
	for _, ep := range episodes {
		if ep.Role == "system" {
			continue
		}
		for i, part := range ep.Parts {
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			trust := memory.TrustNormal
			if ep.Role == "tool" || ep.Role == "event" {
				trust = memory.TrustLow
			}
			out = append(out, Proposal{Memory: memory.Memory{
				Scope: ep.Scope, Text: part.Text, Kind: memory.Episodic, Layer: memory.Atom,
				Confidence: 1, Importance: .5, ValidFrom: ep.OccurredAt,
				Source: memory.Source{Actor: actor(ep), Role: ep.Role, Trust: trust,
					Evidence: []memory.Evidence{{Episode: ep.ID, Part: i, Start: 0, End: len(part.Text)}}},
			}})
		}
	}
	return out, nil
}
