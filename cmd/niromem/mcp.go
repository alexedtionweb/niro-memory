package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/mcp"
)

type mcpIdentity struct{ id api.Identity }

func (a mcpIdentity) Authenticate(*http.Request) (api.Identity, error) {
	return a.id, nil
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
		writeUser, writeAgent := user, agent
		if !p.Admin {
			if p.User != "" {
				writeUser = p.User
			}
			if p.Agent != "" {
				writeAgent = p.Agent
			}
		}
		// Reuse native API authorization with the already authenticated identity;
		// never grant admin or accept a tenant selected by tool arguments.
		backend := client.NewLocal((&api.Server{Engine: e, Auth: mcpIdentity{id}}).Handler())
		backend.HTTP.Timeout = mcp.SearchTimeout + 5*time.Second
		s, err := mcp.NewWorkspace(backend, writeUser, writeAgent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.HTTPHandler(port, "").ServeHTTP(w, r)
	}), nil
}
