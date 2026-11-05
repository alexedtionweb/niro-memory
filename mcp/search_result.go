package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/alexedtionweb/niro-memory/search"
)

const searchResultBytes = 24000

type compactMemory struct {
	ID            string         `json:"id"`
	Title         string         `json:"title,omitempty"`
	Text          string         `json:"text"`
	SourceKind    string         `json:"source_kind"`
	References    map[string]any `json:"references,omitempty"`
	TextTruncated bool           `json:"text_truncated"`
}

type compactHit struct {
	Memory  compactMemory   `json:"memory"`
	Score   float64         `json:"score"`
	Explain *search.Explain `json:"explain,omitempty"`
}

type boundedSearch struct {
	Hits      []json.RawMessage `json:"hits"`
	Trace     []search.Step     `json:"trace"`
	Took      int64             `json:"took_ns"`
	Mode      string            `json:"response_mode"`
	Truncated bool              `json:"truncated"`
	Omitted   int               `json:"omitted_hits"`
	Note      string            `json:"note"`
}

func boundedSearchResult(raw json.RawMessage, mode string, textMax int) (boundedSearch, error) {
	var native search.Result
	if err := json.Unmarshal(raw, &native); err != nil {
		return boundedSearch{}, fmt.Errorf("decode search response: %w", err)
	}
	out := boundedSearch{Hits: []json.RawMessage{}, Trace: native.Trace, Took: int64(native.Took), Mode: mode,
		Note: "Scores are ranking signals, not confidence. Text previews may be incomplete; use get_memory with the ID for full evidence."}
	for i, hit := range native.Hits {
		var value any = hit
		previewTruncated := false
		if mode == "compact" {
			m := hit.Memory
			text := []rune(m.Text)
			if len(text) > textMax {
				text = text[:textMax]
				previewTruncated = true
			}
			refs := make(map[string]any)
			for _, key := range []string{"document_id", "filename", "page", "section", "section_title", "chunk", "page_start", "page_end", "continuation", "interpretation"} {
				if v, ok := m.Metadata[key]; ok {
					refs[key] = v
				}
			}
			if v, ok := m.Metadata["enrichment"].(map[string]any); ok {
				for _, key := range []string{"source_id", "source_version", "start", "end", "review", "mode"} {
					if value, ok := v[key]; ok {
						refs[key] = value
					}
				}
			}
			value = compactHit{Memory: compactMemory{ID: m.ID, Title: m.Title, Text: string(text), SourceKind: m.Source.Kind, References: refs, TextTruncated: previewTruncated}, Score: hit.Score, Explain: hit.Explain}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return boundedSearch{}, fmt.Errorf("encode search hit: %w", err)
		}
		out.Hits = append(out.Hits, encoded)
		data, err := json.Marshal(out)
		if err != nil {
			return boundedSearch{}, fmt.Errorf("encode search response: %w", err)
		}
		// Reserve room for final truncation flags and omitted-hit count.
		if len(data) > searchResultBytes-128 {
			out.Hits = out.Hits[:len(out.Hits)-1]
			out.Omitted = len(native.Hits) - i
			out.Truncated = true
			break
		}
		out.Truncated = out.Truncated || previewTruncated
	}
	data, err := json.Marshal(out)
	if err != nil {
		return boundedSearch{}, fmt.Errorf("encode bounded search response: %w", err)
	}
	if len(data) > searchResultBytes {
		return boundedSearch{}, fmt.Errorf("search trace exceeds response budget; narrow the query")
	}
	return out, nil
}
