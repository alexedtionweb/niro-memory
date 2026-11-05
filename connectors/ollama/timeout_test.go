package ollama

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
)

type timeoutTransport func(*http.Request) (*http.Response, error)

func (f timeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestEmbeddingHTTPTimeoutDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name          string
		timeout, want time.Duration
	}{
		{"default", 0, 5 * time.Minute},
		{"configured", 10 * time.Minute, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := New(Options{Model: "test", Dims: 2, Timeout: tc.timeout})
			if err != nil {
				t.Fatal(err)
			}
			if e.o.HTTP.Timeout != tc.want {
				t.Fatalf("HTTP timeout=%s want %s", e.o.HTTP.Timeout, tc.want)
			}
			e.o.HTTP.Transport = timeoutTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) <= time.Minute || time.Until(deadline) > tc.want {
					t.Errorf("request deadline did not use configured timeout: %v", deadline)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"embeddings":[[1,2]]}`)), Header: make(http.Header)}, nil
			})
			vectors, _, err := e.Embed(context.Background(), embed.Query, []embed.Input{{Text: "generated fixture"}})
			if err != nil || len(vectors) != 1 {
				t.Fatalf("embed: %v %v", vectors, err)
			}
		})
	}
	client := &http.Client{Timeout: 17 * time.Second}
	e, err := New(Options{Model: "test", Dims: 2, Timeout: 10 * time.Minute, HTTP: client})
	if err != nil || e.o.HTTP != client || client.Timeout != 17*time.Second {
		t.Fatalf("custom client was replaced or changed: %v", err)
	}
	if _, err := New(Options{Model: "test", Dims: 2, Timeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestEmbeddingTimeoutAndCallerCancellationPreserveCause(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "http-timeout", true: "caller-deadline"}[callerDeadline], func(t *testing.T) {
			e, err := New(Options{Model: "test", Dims: 2, Timeout: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if callerDeadline {
				e.o.HTTP.Timeout = time.Minute
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			e.o.HTTP.Transport = timeoutTransport(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			_, _, err = e.Embed(ctx, embed.Document, []embed.Input{{Text: "private text must not appear in error"}})
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "model test, batch 1 inputs, HTTP timeout") || strings.Contains(err.Error(), "private text") {
				t.Fatalf("timeout cause or safe diagnostics lost: %v", err)
			}
		})
	}
}
