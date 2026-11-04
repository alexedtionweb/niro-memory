package client

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/relations"
)

func (c *Client) EnrichmentStatus(ctx context.Context, id string) (*niromem.EnrichmentResult, error) {
	var out niromem.EnrichmentResult
	err := c.do(ctx, "GET", "/v1/knowledge/enrichment/"+url.PathEscape(id), nil, &out)
	return &out, err
}

func (c *Client) EvaluateKnowledge(ctx context.Context, req niromem.KnowledgeRequest) (*niromem.KnowledgeResult, error) {
	var out niromem.KnowledgeResult
	err := c.do(ctx, "POST", "/v1/knowledge/evaluate", req, &out)
	return &out, err
}

func (c *Client) PlanKnowledge(ctx context.Context, req niromem.KnowledgeRequest) (*niromem.KnowledgeResult, error) {
	var out niromem.KnowledgeResult
	err := c.do(ctx, "POST", "/v1/knowledge/plan", req, &out)
	return &out, err
}

func (c *Client) CommitKnowledge(ctx context.Context, req niromem.KnowledgeRequest) (*niromem.KnowledgeResult, error) {
	var out niromem.KnowledgeResult
	err := c.do(ctx, "POST", "/v1/knowledge/commit", req, &out)
	return &out, err
}

func (c *Client) KnowledgeState(ctx context.Context, req niromem.KnowledgeRequest) (knowledge.State, error) {
	q := url.Values{}
	if req.Input.Scope.User != "" {
		q.Set("user_id", req.Input.Scope.User)
	}
	if req.Input.Scope.Agent != "" {
		q.Set("agent_id", req.Input.Scope.Agent)
	}
	if req.Input.OperationKey != "" {
		q.Set("operation_key", req.Input.OperationKey)
	}
	if req.Input.TargetID != "" {
		q.Set("target_id", req.Input.TargetID)
	}
	if req.Input.Text != "" {
		q.Set("text", req.Input.Text)
	}
	if req.Input.Slot != "" {
		q.Set("slot", req.Input.Slot)
	}
	if req.Input.Intent != "" {
		q.Set("intent", string(req.Input.Intent))
	}
	if req.Input.Scope.Run != "" {
		q.Set("run_id", req.Input.Scope.Run)
	}
	if !req.ValidAt.IsZero() {
		q.Set("valid_at", req.ValidAt.Format(time.RFC3339Nano))
	}
	if !req.KnownAt.IsZero() {
		q.Set("known_at", req.KnownAt.Format(time.RFC3339Nano))
	}
	if req.AllVersions {
		q.Set("all_versions", "true")
	}
	if req.MaxCandidates != 0 {
		q.Set("max_candidates", strconv.Itoa(req.MaxCandidates))
	}
	if req.Input.Assertion != nil {
		q.Set("subject", req.Input.Assertion.Subject)
		q.Set("predicate", req.Input.Assertion.Predicate)
		q.Set("object", req.Input.Assertion.Object)
	}
	var out knowledge.State
	err := c.do(ctx, "GET", "/v1/knowledge/state?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) KnowledgeRelations(ctx context.Context, req niromem.RelationRequest) ([]relations.Link, error) {
	q := url.Values{}
	if req.ListRequest.User != "" {
		q.Set("user_id", req.ListRequest.User)
	}
	if req.ListRequest.Agent != "" {
		q.Set("agent_id", req.ListRequest.Agent)
	}
	if req.ListRequest.Run != "" {
		q.Set("run_id", req.ListRequest.Run)
	}
	if req.ListRequest.AllVersions {
		q.Set("all_versions", "true")
	}
	if req.ListRequest.ShowExpired {
		q.Set("show_expired", "true")
	}
	if !req.ListRequest.ValidAt.IsZero() {
		q.Set("valid_at", req.ListRequest.ValidAt.Format(time.RFC3339Nano))
	}
	if !req.ListRequest.KnownAt.IsZero() {
		q.Set("known_at", req.ListRequest.KnownAt.Format(time.RFC3339Nano))
	}
	if req.ListRequest.Limit != 0 {
		q.Set("limit", strconv.Itoa(req.ListRequest.Limit))
	}
	if req.ListRequest.Offset != 0 {
		q.Set("offset", strconv.Itoa(req.ListRequest.Offset))
	}
	if req.ListRequest.Order != "" {
		q.Set("order", string(req.ListRequest.Order))
	}
	filter := req.ListRequest.Filter
	if filter.Field != "" || filter.Op != "" || len(filter.And) > 0 || len(filter.Or) > 0 || filter.Not != nil {
		if raw, err := json.Marshal(filter); err == nil {
			q.Set("filter", string(raw))
		} else {
			return nil, err
		}
	}
	if req.Direction != "" {
		q.Set("direction", req.Direction)
	}
	if len(req.Types) > 0 {
		for _, typ := range req.Types {
			q.Add("types", typ)
		}
	}
	if req.Depth != 0 {
		q.Set("depth", strconv.Itoa(req.Depth))
	}
	if req.MaxEdges != 0 {
		q.Set("max_edges", strconv.Itoa(req.MaxEdges))
	}
	var out struct {
		Relations []relations.Link `json:"relations"`
	}
	err := c.do(ctx, "GET", "/v1/knowledge/relations/"+url.PathEscape(req.MemoryID)+"?"+q.Encode(), nil, &out)
	return out.Relations, err
}

func (c *Client) KnowledgeTrace(ctx context.Context, key string) (*niromem.KnowledgeResult, error) {
	var out niromem.KnowledgeResult
	err := c.do(ctx, "GET", "/v1/knowledge/traces/"+url.PathEscape(key), nil, &out)
	return &out, err
}
