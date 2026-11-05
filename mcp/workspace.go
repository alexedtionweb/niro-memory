package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

// WorkspaceBackend reuses the authenticated native API, not a bypass around it.
// Only the fixed endpoints declared in workspaceTools are dispatched.
type WorkspaceBackend interface {
	WorkspaceRequest(context.Context, string, string, any) (json.RawMessage, error)
}

func (s *Server) maxRequestBytes() int {
	if s.workspace {
		return 25 << 20
	}
	return 1 << 20
}

// NewWorkspace exposes the authenticated UI/API workspace. user/agent choose
// default write ownership, not read restrictions; API-key bindings still apply.
// New retains the legacy project-isolated tool set for existing integrations.
func NewWorkspace(backend Backend, user, agent string) (*Server, error) {
	s, err := New(backend, user, agent)
	if err != nil {
		return nil, err
	}
	if _, ok := backend.(WorkspaceBackend); !ok {
		return nil, fmt.Errorf("MCP workspace requires an authenticated workspace backend")
	}
	s.workspace = true
	return s, nil
}

func (s *Server) callWorkspace(ctx context.Context, name string, raw json.RawMessage) (any, *rpcError) {
	var spec *workspaceTool
	for _, tool := range workspaceTools() {
		if tool.name == name {
			spec = &tool
			break
		}
	}
	if spec == nil {
		return nil, &rpcError{-32602, "unknown tool"}
	}
	fail := func(err error) (any, *rpcError) {
		return toolResult(name+" failed: "+err.Error(), true), nil
	}
	var args map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return fail(fmt.Errorf("invalid tool arguments: %w", err))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail(fmt.Errorf("tool arguments must contain exactly one JSON object"))
	}
	if args == nil {
		return fail(fmt.Errorf("tool arguments must be an object"))
	}
	schema := map[string]any{"type": "object", "properties": spec.properties, "required": spec.required, "additionalProperties": false}
	if err := validateToolValue("arguments", args, schema); err != nil {
		return fail(err)
	}
	if spec.confirm && args["confirm"] != true {
		return fail(fmt.Errorf("confirm:true requires explicit user approval"))
	}
	if name == "control_job" && args["action"] == "priority" && args["priority"] == nil {
		return fail(fmt.Errorf("priority is required for the priority action"))
	}
	if name == "update_memory" && len(args["patch"].(map[string]any)) == 0 {
		return fail(fmt.Errorf("patch must contain at least one correction"))
	}
	delete(args, "confirm")
	timeout := 5 * time.Second
	if name == "search_memory" || name == "propose_memory_update" || name == "commit_memory_update" {
		timeout = SearchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backend := s.client.(WorkspaceBackend)
	writeScope := map[string]any{"user_id": s.user, "agent_id": s.agent}
	if name == "remember_memory" || name == "import_document" || name == "import_media" || name == "propose_memory_update" || name == "commit_memory_update" {
		rawIdentity, err := backend.WorkspaceRequest(ctx, http.MethodGet, "/v1/whoami", nil)
		if err != nil {
			return fail(err)
		}
		var identity struct {
			Principal memory.Principal `json:"principal"`
		}
		if err := json.Unmarshal(rawIdentity, &identity); err != nil {
			return fail(fmt.Errorf("decode identity: %w", err))
		}
		if !identity.Principal.Admin {
			if identity.Principal.User != "" {
				writeScope["user_id"] = identity.Principal.User
			}
			if identity.Principal.Agent != "" {
				writeScope["agent_id"] = identity.Principal.Agent
			}
		}
	}
	if name == "workspace_info" {
		out := make(map[string]json.RawMessage)
		for _, item := range []struct{ key, path string }{
			{"identity", "/v1/whoami"}, {"capabilities", "/v1/capabilities"},
			{"embeddings", "/v1/vectors/config"}, {"knowledge", "/v1/knowledge/config"},
		} {
			result, err := backend.WorkspaceRequest(ctx, http.MethodGet, item.path, nil)
			if err != nil {
				return fail(err)
			}
			out[item.key] = result
		}
		return jsonToolResult(out)
	}
	if name == "import_document" || name == "import_media" {
		decoded, err := base64.StdEncoding.Strict().DecodeString(args["bytes"].(string))
		if err != nil || len(decoded) == 0 || len(decoded) > 16<<20 {
			return fail(fmt.Errorf("bytes must be valid base64 encoding of 1..16 MiB of original data"))
		}
	}
	if name == "delete_all_memories" {
		identity, err := backend.WorkspaceRequest(ctx, http.MethodGet, "/v1/whoami", nil)
		if err != nil {
			return fail(err)
		}
		var id struct {
			Tenant string `json:"tenant"`
		}
		if err := json.Unmarshal(identity, &id); err != nil {
			return fail(fmt.Errorf("decode identity: %w", err))
		}
		if args["confirmation"] != id.Tenant || id.Tenant == "" {
			return fail(fmt.Errorf("confirmation must match the exact authenticated tenant name"))
		}
		if len(args["request_id"].(string)) > 128 {
			return fail(fmt.Errorf("request_id must be at most 128 bytes"))
		}
	}
	path := spec.path
	if spec.pathArg != "" {
		value := args[spec.pathArg].(string)
		if len(value) > 256 || value == "." || value == ".." {
			return fail(fmt.Errorf("invalid %s", spec.pathArg))
		}
		path = strings.Replace(path, "{"+spec.pathArg+"}", url.PathEscape(value), 1)
		delete(args, spec.pathArg)
	}
	if name == "remember_memory" || name == "import_document" || name == "import_media" {
		for key, value := range writeScope {
			if _, ok := args[key]; !ok && value != "" {
				args[key] = value
			}
		}
	}
	var body any = args
	switch name {
	case "remember_memory":
		args["actor"] = "claude-code-mcp"
		args["metadata"] = map[string]any{"source": "claude-code-mcp"}
		body = map[string]any{"facts": []any{args}}
	case "propose_memory_update", "commit_memory_update":
		body = workspaceKnowledgeInput(writeScope, args)
	case "embedding_status":
		identity, err := backend.WorkspaceRequest(ctx, http.MethodGet, "/v1/whoami", nil)
		if err != nil {
			return fail(err)
		}
		config, err := backend.WorkspaceRequest(ctx, http.MethodGet, "/v1/vectors/config", nil)
		if err != nil {
			return fail(err)
		}
		model, _ := args["model"].(string)
		if model == "" {
			var selection struct {
				Active string `json:"active"`
			}
			if err := json.Unmarshal(config, &selection); err != nil {
				return fail(fmt.Errorf("decode model configuration: %w", err))
			}
			model = selection.Active
		}
		if model == "" {
			return jsonToolResult(map[string]any{"identity": identity, "configuration": config, "coverage_available": false, "reason": "No active embedding model configured."})
		}
		coverage, err := backend.WorkspaceRequest(ctx, http.MethodGet, "/v1/vectors/coverage?model="+url.QueryEscape(model), nil)
		if err != nil {
			return fail(err)
		}
		return jsonToolResult(map[string]any{"identity": identity, "configuration": config, "model": model, "coverage": coverage, "coverage_available": true,
			"coverage_scope": "All current memories in the authenticated tenant; not a document-specific count. Compare the same tenant and model in the UI."})
	}
	if spec.bodyArg != "" {
		body = args[spec.bodyArg]
	}
	if spec.method == http.MethodGet {
		q := make(url.Values)
		for key, value := range args {
			if object, ok := value.(map[string]any); ok {
				encoded, err := json.Marshal(object)
				if err != nil {
					return fail(err)
				}
				q.Set(key, string(encoded))
			} else {
				q.Set(key, fmt.Sprint(value))
			}
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		body = nil
	}
	mode, textMax := "compact", 1200
	if name == "search_memory" {
		if value, ok := args["response_mode"].(string); ok {
			mode = value
		}
		if value, ok := args["text_max_chars"].(json.Number); ok {
			textMax, _ = strconv.Atoi(string(value))
		}
		delete(args, "response_mode")
		delete(args, "text_max_chars")
		if _, ok := args["limit"]; !ok {
			args["limit"] = 5
		}
	}
	result, err := backend.WorkspaceRequest(ctx, spec.method, path, body)
	if err != nil {
		return fail(err)
	}
	if name == "search_memory" {
		response, err := boundedSearchResult(result, mode, textMax)
		if err != nil {
			return fail(err)
		}
		return jsonToolResult(response)
	}
	if len(result) > 2<<20 {
		return fail(fmt.Errorf("result exceeds 2 MiB; use a smaller limit or narrower filter (the operation may already have committed)"))
	}
	return jsonToolResult(result)
}

func workspaceKnowledgeInput(scope map[string]any, args map[string]any) any {
	for _, key := range []string{"user_id", "agent_id", "run_id"} {
		if value, ok := args[key]; ok {
			scope[key] = value
			delete(args, key)
		}
	}
	input := cloneFields(args)
	input["scope"] = scope
	input["trust"] = "normal"
	if input["subject"] != nil || input["predicate"] != nil || input["object"] != nil {
		input["assertion"] = map[string]any{
			"subject": input["subject"], "predicate": input["predicate"], "object": input["object"],
			"epistemic_type": "fact", "confidence": 1,
		}
		delete(input, "subject")
		delete(input, "predicate")
		delete(input, "object")
	}
	return map[string]any{"input": input}
}

func validateToolValue(path string, value any, schema map[string]any) error {
	invalid := func() error { return fmt.Errorf("%s must be %s", path, schema["type"]) }
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		for _, key := range schemaStrings(schema["required"]) {
			if _, ok := object[key]; !ok {
				return fmt.Errorf("%s.%s is required", path, key)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range object {
			if childSchema, ok := properties[key].(map[string]any); ok {
				if err := validateToolValue(path+"."+key, child, childSchema); err != nil {
					return err
				}
			} else if schema["additionalProperties"] == false {
				return fmt.Errorf("unknown argument %s.%s", path, key)
			}
		}
	case "array":
		values, ok := value.([]any)
		if !ok {
			return invalid()
		}
		if len(values) > 100 {
			return fmt.Errorf("%s exceeds 100 items", path)
		}
		for _, child := range values {
			if err := validateToolValue(path+"[]", child, schema["items"].(map[string]any)); err != nil {
				return err
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return invalid()
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s must not be empty; omit unused optional fields", path)
		}
		if max, ok := schema["maxLength"].(int); ok && len(text) > max {
			return fmt.Errorf("%s exceeds %d bytes", path, max)
		}
		if choices := schemaStrings(schema["enum"]); len(choices) > 0 {
			found := false
			for _, choice := range choices {
				found = found || choice == text
			}
			if !found {
				return fmt.Errorf("%s is not an allowed value", path)
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return invalid()
		}
	case "integer", "number":
		number, ok := value.(json.Number)
		if !ok {
			return invalid()
		}
		v, err := number.Float64()
		if err != nil {
			return invalid()
		}
		if schema["type"] == "integer" {
			if _, err := strconv.ParseInt(string(number), 10, 64); err != nil {
				return invalid()
			}
		}
		for _, bound := range []string{"minimum", "maximum"} {
			if limit, ok := schema[bound].(int); ok && (bound == "minimum" && v < float64(limit) || bound == "maximum" && v > float64(limit)) {
				return fmt.Errorf("%s violates %s %d", path, bound, limit)
			}
		}
	}
	return nil
}

func schemaStrings(value any) []string {
	values, _ := value.([]string)
	return values
}
