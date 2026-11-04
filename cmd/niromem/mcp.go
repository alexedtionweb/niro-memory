package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/mcp"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/search"
)

type mcpBackend struct {
	engine *niromem.Engine
	tenant string
	as     memory.Principal
}

func (b mcpBackend) EvaluateKnowledge(ctx context.Context, req niromem.KnowledgeRequest) (*niromem.KnowledgeResult, error) {
	req.Input.Scope = memory.Scope{Tenant: b.tenant, User: b.as.User, Agent: b.as.Agent}
	req.As, req.Input.Authority = b.as, false
	return b.engine.EvaluateKnowledge(ctx, req)
}

func (b mcpBackend) KnowledgeState(ctx context.Context, req niromem.KnowledgeRequest) (knowledge.State, error) {
	req.Input.Scope = memory.Scope{Tenant: b.tenant, User: b.as.User, Agent: b.as.Agent}
	req.As, req.Input.Authority = b.as, false
	return b.engine.KnowledgeState(ctx, req)
}

func (b mcpBackend) KnowledgeTrace(ctx context.Context, key string) (*niromem.KnowledgeResult, error) {
	return b.engine.KnowledgeTrace(ctx, b.tenant, b.as, key)
}

func (b mcpBackend) Search(ctx context.Context, req client.SearchRequest) (*search.Result, error) {
	req.Tenant, req.As = b.tenant, b.as
	if req.Filter != nil {
		raw, err := json.Marshal(req.Filter)
		if err != nil {
			return nil, err
		}
		f, err := memory.ParseFilter(raw)
		if err != nil {
			return nil, err
		}
		req.SearchRequest.Filter = memory.And(req.SearchRequest.Filter, f)
	}
	return b.engine.Search(ctx, req.SearchRequest)
}

func (b mcpBackend) Remember(ctx context.Context, facts ...niromem.Fact) ([]pipeline.Change, error) {
	for i := range facts {
		facts[i].Tenant = b.tenant
	}
	return b.engine.Remember(ctx, facts...)
}

func mcpHandler(e *niromem.Engine, auth api.Authenticator, address, user, agent string) (http.Handler, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("-mcp requires a loopback listen address, got %q", address)
	}
	if strings.TrimSpace(user) == "" {
		return nil, errors.New("-mcp requires -mcp-user or NIROMEM_USER_ID; choose a project-specific identity")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := auth.Authenticate(r)
		if err != nil {
			http.Error(w, "missing or invalid API key", http.StatusUnauthorized)
			return
		}
		p := id.Principal
		if !p.Admin && ((p.User != "" && p.User != user) || (p.Agent != "" && p.Agent != agent)) {
			http.Error(w, "API key binding does not match the configured MCP user/agent", http.StatusForbidden)
			return
		}
		p.User, p.Agent, p.Admin = user, agent, false
		s, err := mcp.New(mcpBackend{engine: e, tenant: id.Tenant, as: p}, user, agent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.HTTPHandler(port, "").ServeHTTP(w, r)
	}), nil
}
