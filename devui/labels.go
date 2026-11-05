//go:build devui

package devui

import (
	"fmt"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
)

type ActionLabel struct {
	Title    string
	Sentence string
}

func ReconcilerActionLabel(action memory.Action) ActionLabel {
	switch action {
	case memory.ActionAdd:
		return ActionLabel{"New memory created", "This is a new independent memory."}
	case memory.ActionEvidence:
		return ActionLabel{"Existing memory reinforced", "Same content already existed; this source was added as evidence."}
	case memory.ActionSupersede:
		return ActionLabel{"Current value updated", "Previous value closed and kept in history."}
	case memory.ActionUpdate:
		return ActionLabel{"Earlier value corrected", "The previous value was marked as wrong, not deleted."}
	case memory.ActionInvalidate:
		return ActionLabel{"Current value ended", "This property no longer has a current value."}
	default:
		return ActionLabel{string(action), "(unmapped action)"}
	}
}

type SearchTraceLabel struct {
	Sentence string
	LinkText string
	LinkURL  string
}

func SearchTraceDescription(step search.Step) SearchTraceLabel {
	reason := strings.ToLower(step.Reason)
	if step.Strategy == "vector" && !step.Ran && (strings.Contains(reason, "no embedder") || strings.Contains(reason, "no vector index")) {
		return SearchTraceLabel{
			Sentence: "Semantic search is off: no embedding model configured. Only shared words and known entities match; a query in another language may miss results.",
			LinkText: "Configure semantic search",
			LinkURL:  "/_dev/#/models?view=embeddings",
		}
	}
	if step.Strategy == "vector" && !step.Ran && (strings.Contains(reason, "budget") || strings.Contains(reason, "timeout")) {
		return SearchTraceLabel{Sentence: "Semantic search skipped: the query embedding was not ready within the policy time limit."}
	}
	if step.Ran {
		name := step.Strategy
		if known := map[string]string{
			"exact": "Exact match", "filter": "Filter", "lexical": "Keyword search", "entity": "Entity search",
			"vector": "Semantic search", "relationships": "Relationship search", "rank": "Ranking",
		}[step.Strategy]; known != "" {
			name = known
		}
		return SearchTraceLabel{Sentence: fmt.Sprintf("%s found %d candidates.", name, step.Candidates)}
	}
	if step.Reason != "" {
		return SearchTraceLabel{Sentence: step.Reason}
	}
	return SearchTraceLabel{Sentence: fmt.Sprintf("%s did not run.", step.Strategy)}
}

func MemoryKindLabel(kind memory.Kind) string {
	switch kind {
	case memory.Working:
		return "Short-term"
	case memory.Episodic:
		return "Event"
	case memory.Semantic:
		return "Fact"
	case memory.Procedural:
		return "Procedure"
	default:
		return string(kind)
	}
}

func MemoryLayerLabel(layer memory.Layer) string {
	switch layer {
	case memory.Atom:
		return "Atom"
	case memory.Scene:
		return "Scene"
	case memory.Profile:
		return "Profile"
	default:
		return fmt.Sprintf("Layer %d", layer)
	}
}
