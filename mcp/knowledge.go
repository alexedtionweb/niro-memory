package mcp

import (
	"context"
	"strings"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

type knowledgeBackend interface {
	EvaluateKnowledge(context.Context, niromem.KnowledgeRequest) (*niromem.KnowledgeResult, error)
	KnowledgeState(context.Context, niromem.KnowledgeRequest) (knowledge.State, error)
	KnowledgeTrace(context.Context, string) (*niromem.KnowledgeResult, error)
}

func knowledgeTools() []any {
	return []any{
		map[string]any{
			"name":        "inspect_enrichment",
			"description": "Read background extraction status, grounded assertions and inferred relationships for a saved source memory. No model call or mutation.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]string{"type": "string", "description": "Saved source memory ID"}}, "required": []string{"key"}, "additionalProperties": false},
			"annotations": map[string]bool{"readOnlyHint": true, "openWorldHint": true},
		},
		map[string]any{
			"name":        "inspect_knowledge",
			"description": "Inspect current knowledge state read-only for the fixed project scope.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":      map[string]string{"type": "string", "description": "Proposed statement to inspect"},
					"slot":      map[string]string{"type": "string", "description": "Exact slot to inspect"},
					"subject":   map[string]string{"type": "string"},
					"predicate": map[string]string{"type": "string"},
				},
				"additionalProperties": false,
			},
			"annotations": map[string]bool{"readOnlyHint": true, "openWorldHint": true},
		},
		map[string]any{
			"name":        "explain_memory",
			"description": "Read the knowledge trace for an operation key in the fixed project scope.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"key": map[string]string{"type": "string"}},
				"required":   []string{"key"}, "additionalProperties": false,
			},
			"annotations": map[string]bool{"readOnlyHint": true, "openWorldHint": true},
		},
		map[string]any{
			"name":        "propose_memory_update",
			"description": "Dry-run a proposed knowledge update. This tool never commits changes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation_key": map[string]string{"type": "string", "description": "Stable key for this proposed operation"},
					"text":          map[string]string{"type": "string"},
					"slot":          map[string]string{"type": "string"},
					"intent":        map[string]string{"type": "string"},
					"subject":       map[string]string{"type": "string"},
					"predicate":     map[string]string{"type": "string"},
					"object":        map[string]string{"type": "string"},
				},
				"required": []string{"operation_key", "text"}, "additionalProperties": false,
			},
			"annotations": map[string]bool{"readOnlyHint": true, "openWorldHint": true},
		},
	}
}

func (s *Server) callKnowledge(ctx context.Context, name string, args map[string]string) (any, bool, *rpcError) {
	if name == "inspect_enrichment" {
		backend, ok := s.client.(interface {
			EnrichmentStatus(context.Context, string) (*niromem.EnrichmentResult, error)
		})
		if !ok {
			return toolResult("Backend does not support enrichment inspection.", true), true, nil
		}
		if !validKnowledgeArgs(args, "key") || strings.TrimSpace(args["key"]) == "" || len(args["key"]) > 200 {
			return toolResult("Provide a source memory ID as key (at most 200 bytes).", true), true, nil
		}
		scoped, ok := s.client.(knowledgeBackend)
		if !ok {
			return toolResult("Backend does not support scoped enrichment inspection.", true), true, nil
		}
		req := scopedKnowledgeRequest(s.user, s.agent, nil, "")
		req.Input.TargetID = args["key"]
		req.AllVersions = true
		state, err := scoped.KnowledgeState(ctx, req)
		if err != nil {
			return toolResult("Enrichment source inspection failed: "+err.Error(), true), true, nil
		}
		allowed := false
		for _, m := range append(state.Current, state.Historical...) {
			if m.ID == args["key"] && m.User == s.user && m.Agent == s.agent {
				allowed = true
			}
		}
		if !allowed {
			return toolResult("Enrichment source not found in this project.", true), true, nil
		}
		result, err := backend.EnrichmentStatus(ctx, args["key"])
		if err != nil {
			return toolResult("Enrichment inspection failed: "+err.Error(), true), true, nil
		}
		out, rpcErr := jsonToolResult(result)
		return out, true, rpcErr
	}
	backend, ok := s.client.(knowledgeBackend)
	if !ok {
		return nil, false, nil
	}
	switch name {
	case "inspect_knowledge":
		if !validKnowledgeArgs(args, "text", "slot", "subject", "predicate") ||
			(strings.TrimSpace(args["text"]) == "" && args["slot"] == "" && args["subject"] == "" && args["predicate"] == "") ||
			len(args["text"]) > 8000 || len(args["slot"]) > 200 {
			return toolResult("Provide text or an exact slot/subject/predicate to inspect.", true), true, nil
		}
		req := scopedKnowledgeRequest(s.user, s.agent, args, "")
		result, err := backend.KnowledgeState(ctx, req)
		if err != nil {
			return toolResult("Knowledge inspection failed: "+err.Error(), true), true, nil
		}
		out, rpcErr := jsonToolResult(result)
		return out, true, rpcErr
	case "explain_memory":
		if !validKnowledgeArgs(args, "key") || strings.TrimSpace(args["key"]) == "" {
			return toolResult("Provide an operation key.", true), true, nil
		}
		if len(args["key"]) > 200 {
			return toolResult("Operation key must be at most 200 bytes.", true), true, nil
		}
		result, err := backend.KnowledgeTrace(ctx, args["key"])
		if err != nil {
			return toolResult("Knowledge trace failed: "+err.Error(), true), true, nil
		}
		out, rpcErr := jsonToolResult(result)
		return out, true, rpcErr
	case "propose_memory_update":
		if !validKnowledgeArgs(args, "operation_key", "text", "slot", "intent", "subject", "predicate", "object") ||
			strings.TrimSpace(args["operation_key"]) == "" || strings.TrimSpace(args["text"]) == "" ||
			len(args["text"]) > 8000 || len(args["operation_key"]) > 200 || len(args["slot"]) > 200 {
			return toolResult("Provide a stable operation_key and nonempty text (at most 8000 bytes). This is dry-run only.", true), true, nil
		}
		hasAssertion := args["subject"] != "" || args["predicate"] != "" || args["object"] != ""
		if hasAssertion && (args["subject"] == "" || args["predicate"] == "" || args["object"] == "") {
			return toolResult("Structured assertions require subject, predicate, and object.", true), true, nil
		}
		req := scopedKnowledgeRequest(s.user, s.agent, args, args["operation_key"])
		result, err := backend.EvaluateKnowledge(ctx, req)
		if err != nil {
			return toolResult("Knowledge proposal failed: "+err.Error(), true), true, nil
		}
		out, rpcErr := jsonToolResult(result)
		return out, true, rpcErr
	default:
		return nil, false, nil
	}
}

func scopedKnowledgeRequest(user, agent string, args map[string]string, operationKey string) niromem.KnowledgeRequest {
	req := niromem.KnowledgeRequest{
		Input: knowledge.Input{
			OperationKey: operationKey,
			Scope:        memory.Scope{User: user, Agent: agent},
			Text:         args["text"],
			Slot:         args["slot"],
			Trust:        memory.TrustNormal,
			Intent:       knowledge.OperationType(args["intent"]),
		},
	}
	if args["subject"] != "" || args["predicate"] != "" || args["object"] != "" {
		req.Input.Assertion = &memory.Assertion{
			Subject: args["subject"], Predicate: args["predicate"], Object: args["object"],
			EpistemicType: memory.EpistemicFact, Confidence: 1,
		}
	}
	return req
}

func validKnowledgeArgs(args map[string]string, allowed ...string) bool {
	for key := range args {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
