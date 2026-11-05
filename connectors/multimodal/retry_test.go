package multimodal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func isTemporary(err error) bool {
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func TestRetryableHTTPStatus(t *testing.T) {
	for _, status := range []int{400, 401, 404, 408, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", status)
			}))
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL})
			var result any
			err := p.post(context.Background(), "/api/show", nil, &result)
			want := status == 429 || status >= 500
			if err == nil || isTemporary(err) != want {
				t.Fatalf("status=%d temporary=%t want=%t error=%v", status, isTemporary(err), want, err)
			}
		})
	}
}

type failingBody struct {
	err error
}

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingBody) Close() error             { return nil }

func TestRetryableTransportPreservesCause(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("connection reset")} {
		for _, onRead := range []bool{false, true} {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if onRead {
					return &http.Response{StatusCode: 200, Body: failingBody{err: cause}, Header: make(http.Header)}, nil
				}
				return nil, cause
			})}
			p, _ := New(Options{HTTP: client})
			var result any
			err := p.post(context.Background(), "/api/show", nil, &result)
			if !isTemporary(err) || !errors.Is(err, cause) {
				t.Fatalf("read=%t cause=%v got=%v", onRead, cause, err)
			}
		}
	}
}

func TestPermanentResponseFailures(t *testing.T) {
	for _, body := range []io.ReadCloser{
		io.NopCloser(strings.NewReader(`{"done":`)),
		io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20+1))),
		failingBody{err: io.ErrUnexpectedEOF},
	} {
		p, _ := New(Options{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
		})}})
		var result any
		err := p.post(context.Background(), "/api/chat", nil, &result)
		if err == nil || isTemporary(err) {
			t.Fatalf("invalid response must be permanent: %v", err)
		}
	}
	if err := Validate("bad.png", []byte("not png")); err == nil || isTemporary(err) {
		t.Fatalf("invalid media must be permanent: %v", err)
	}
	if err := require(nil, "vision"); err == nil || isTemporary(err) {
		t.Fatalf("unsupported capability must be permanent: %v", err)
	}
}
