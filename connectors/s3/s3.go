// Package s3 is a blob.Store connector for S3 and S3-compatible storage
// (AWS S3, MinIO, Cloudflare R2, Ceph, …), signing requests with AWS
// Signature Version 4 on the standard library — no AWS SDK.
//
// Objects are content-addressed: <prefix><tenant>/<sha[0:2]>/<sha>.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
)

// Options configures the store.
type Options struct {
	// Endpoint is the service URL, e.g. https://s3.us-east-1.amazonaws.com
	// or http://localhost:9000 (MinIO).
	Endpoint string
	Region   string
	Bucket   string
	// Prefix is prepended to object keys (e.g. "niromem/").
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	// PathStyle uses endpoint/bucket/key instead of bucket.endpoint/key
	// (required by MinIO and most S3-compatible services).
	PathStyle bool
	HTTP      *http.Client
	Now       func() time.Time
}

// Store is an S3-backed blob.Store.
type Store struct {
	o  Options
	ep *url.URL
}

var _ blob.Store = (*Store)(nil)

// New validates options. It performs no I/O.
func New(o Options) (*Store, error) {
	ep, err := url.Parse(o.Endpoint)
	if err != nil || ep.Host == "" || o.Bucket == "" || o.Region == "" {
		return nil, fmt.Errorf("s3: endpoint, region and bucket are required")
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Store{o: o, ep: ep}, nil
}

func (s *Store) objectURL(tenant, sha string) *url.URL {
	key := s.o.Prefix + tenant + "/" + sha[:2] + "/" + sha
	u := *s.ep
	if s.o.PathStyle {
		u.Path = "/" + s.o.Bucket + "/" + key
	} else {
		u.Host = s.o.Bucket + "." + s.ep.Host
		u.Path = "/" + key
	}
	return &u
}

func (s *Store) do(ctx context.Context, method string, u *url.URL, body []byte, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	Sign(req, body, s.o.AccessKey, s.o.SecretKey, s.o.SessionToken, s.o.Region, "s3", s.o.Now())
	return s.o.HTTP.Do(req)
}

func failure(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("s3: %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// Put uploads data (idempotent: the key is the content hash).
func (s *Store) Put(ctx context.Context, tenant string, data []byte, mime string) (memory.BlobRef, error) {
	ref := blob.Ref(data, mime)
	if err := blob.ValidKey(tenant, ref.SHA256); err != nil {
		return ref, err
	}
	h := http.Header{}
	if mime != "" {
		h.Set("Content-Type", mime)
	}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(tenant, ref.SHA256), data, h)
	if err != nil {
		return ref, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return ref, failure(resp)
	}
	return ref, nil
}

// Get downloads a blob or returns memory.ErrNotFound.
func (s *Store) Get(ctx context.Context, tenant, sha string) ([]byte, error) {
	if err := blob.ValidKey(tenant, sha); err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(tenant, sha), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, memory.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return nil, failure(resp)
	}
	return io.ReadAll(resp.Body)
}

// Delete removes a blob; deleting a missing blob is not an error.
func (s *Store) Delete(ctx context.Context, tenant, sha string) error {
	if err := blob.ValidKey(tenant, sha); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(tenant, sha), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return failure(resp)
	}
	return nil
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncode encodes a path or query component as SigV4 requires (RFC 3986
// unreserved characters kept; '/' kept in paths).
func uriEncode(s string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' || (path && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Sign adds AWS Signature Version 4 headers to req. Every header already
// on the request is signed, plus host, x-amz-date and x-amz-content-sha256.
func Sign(req *http.Request, body []byte, accessKey, secretKey, token, region, service string, now time.Time) {
	t := now.UTC()
	amzDate, day := t.Format("20060102T150405Z"), t.Format("20060102")
	payload := sha(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var query []string
	for _, k := range keys {
		for _, v := range q[k] {
			query = append(query, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	path := req.URL.EscapedPath()
	if p, err := url.PathUnescape(path); err == nil {
		path = uriEncode(p, true)
	}
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, strings.Join(query, "&"), canonHeaders.String(), signed, payload}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha([]byte(canonical))
	key := hmacSHA(hmacSHA(hmacSHA(hmacSHA([]byte("AWS4"+secretKey), day), region), service), "aws4_request")
	sig := hex.EncodeToString(hmacSHA(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}
