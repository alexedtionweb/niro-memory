package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// NewLocal calls an authenticated API handler in-process, without a listener.
// The handler must enforce the same authorization as its network counterpart.
func NewLocal(handler http.Handler) *Client {
	c := New("http://niromem.local", "")
	c.HTTP.Transport = localTransport{handler}
	return c
}

// WorkspaceRequest is the JSON bridge used by the allowlisted MCP workspace tools.
func (c *Client) WorkspaceRequest(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, method, path, body, &out)
	return out, err
}

type localTransport struct{ handler http.Handler }

func (t localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	w := &localResponse{header: make(http.Header)}
	t.handler.ServeHTTP(w, req)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return &http.Response{StatusCode: w.status, Header: w.header, Body: io.NopCloser(bytes.NewReader(w.body.Bytes())), Request: req}, nil
}

type localResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *localResponse) Header() http.Header { return w.header }
func (w *localResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *localResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}
