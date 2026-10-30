// Package client is a small Go SDK for the niromem HTTP API, for services
// that use a remote memory server instead of embedding the engine.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/search"
)

// Client calls a niromem server. It is safe for concurrent use.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

// New returns a client with a 30s timeout.
func New(baseURL, key string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Key: key, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("niromem: %d %s", e.Status, e.Message) }

// Is maps HTTP statuses to memory errors, so errors.Is(err, memory.ErrNotFound) works.
func (e *Error) Is(target error) bool {
	switch e.Status {
	case 400:
		return target == memory.ErrInvalid
	case 404:
		return target == memory.ErrNotFound
	case 409:
		return target == memory.ErrConflict
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Remember writes facts (the tenant comes from the key).
func (c *Client) Remember(ctx context.Context, facts ...niromem.Fact) ([]pipeline.Change, error) {
	var out struct {
		Changes []pipeline.Change `json:"changes"`
	}
	err := c.do(ctx, "POST", "/v1/facts", map[string]any{"facts": facts}, &out)
	return out.Changes, err
}

// SearchRequest is niromem.SearchRequest plus a filter in the JSON filter
// language (see memory.ParseFilter).
type SearchRequest struct {
	niromem.SearchRequest
	Filter map[string]any `json:"filter,omitempty"`
}

// Search retrieves memories.
func (c *Client) Search(ctx context.Context, r SearchRequest) (*search.Result, error) {
	var out search.Result
	err := c.do(ctx, "POST", "/v1/search", r, &out)
	return &out, err
}

// Context returns the layered context and its rendered prompt block.
func (c *Client) Context(ctx context.Context, r niromem.ContextRequest) (*niromem.Context, string, error) {
	r.Timeout /= time.Millisecond // the wire format carries milliseconds
	var out struct {
		Context  niromem.Context `json:"context"`
		Rendered string          `json:"rendered"`
	}
	err := c.do(ctx, "POST", "/v1/context", r, &out)
	return &out.Context, out.Rendered, err
}

// Append adds turns to a run.
func (c *Client) Append(ctx context.Context, run, user, agent string, turns []niromem.Turn, wait bool) (*niromem.AppendResult, error) {
	var out niromem.AppendResult
	err := c.do(ctx, "POST", "/v1/runs/"+url.PathEscape(run)+"/turns",
		map[string]any{"user_id": user, "agent_id": agent, "turns": turns, "wait": wait}, &out)
	return &out, err
}

// Window returns the conversation window of a run.
func (c *Client) Window(ctx context.Context, run string, tokens int) (*niromem.Window, error) {
	var out niromem.Window
	err := c.do(ctx, "GET", "/v1/runs/"+url.PathEscape(run)+"/window?tokens="+strconv.Itoa(tokens), nil, &out)
	return &out, err
}

// Get returns one memory.
func (c *Client) Get(ctx context.Context, id string) (memory.Memory, error) {
	var m memory.Memory
	err := c.do(ctx, "GET", "/v1/memories/"+url.PathEscape(id), nil, &m)
	return m, err
}

// List lists memories of a user (and optionally agent).
func (c *Client) List(ctx context.Context, user, agent string, limit int) ([]memory.Memory, error) {
	q := url.Values{"user_id": {user}, "agent_id": {agent}, "limit": {strconv.Itoa(limit)}}
	var out struct {
		Memories []memory.Memory `json:"memories"`
	}
	err := c.do(ctx, "GET", "/v1/memories?"+q.Encode(), nil, &out)
	return out.Memories, err
}

// Update corrects a memory, creating a new version.
func (c *Client) Update(ctx context.Context, id string, p niromem.Patch) (memory.Memory, error) {
	var m memory.Memory
	err := c.do(ctx, "PATCH", "/v1/memories/"+url.PathEscape(id), p, &m)
	return m, err
}

// Delete removes a memory permanently.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/memories/"+url.PathEscape(id), nil, nil)
}

// History returns a memory's lifecycle events.
func (c *Client) History(ctx context.Context, id string) ([]memory.Event, error) {
	var out struct {
		Events []memory.Event `json:"events"`
	}
	err := c.do(ctx, "GET", "/v1/memories/"+url.PathEscape(id)+"/history", nil, &out)
	return out.Events, err
}

// Forget deletes everything of a user/agent/run.
func (c *Client) Forget(ctx context.Context, scope memory.Scope) (niromem.ForgetResult, error) {
	var out niromem.ForgetResult
	err := c.do(ctx, "POST", "/v1/forget", scope, &out)
	return out, err
}
