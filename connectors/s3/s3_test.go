package s3_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/connectors/s3"
	"github.com/alexedtionweb/niro-memory/memory"
)

// TestSignatureVector checks Sign against the example in the AWS S3
// documentation ("Example: GET Object", Signature Version 4).
func TestSignatureVector(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	s3.Sign(req, nil, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "", "us-east-1", "s3",
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", got, want)
	}
}

// Set NIROMEM_S3_ENDPOINT (+ _BUCKET, _ACCESS_KEY, _SECRET_KEY), e.g. the
// MinIO service of deploy/docker-compose.yml, to run against real storage.
func TestRoundTrip(t *testing.T) {
	ep := os.Getenv("NIROMEM_S3_ENDPOINT")
	if ep == "" {
		t.Skip("NIROMEM_S3_ENDPOINT not set")
	}
	s, err := s3.New(s3.Options{Endpoint: ep, Region: "us-east-1", Bucket: os.Getenv("NIROMEM_S3_BUCKET"), PathStyle: true,
		AccessKey: os.Getenv("NIROMEM_S3_ACCESS_KEY"), SecretKey: os.Getenv("NIROMEM_S3_SECRET_KEY"), Prefix: "test/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := s.Put(ctx, "t", []byte("hello"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(ctx, "t", ref.SHA256); err != nil || string(b) != "hello" {
		t.Fatalf("%s %v", b, err)
	}
	if err := s.Delete(ctx, "t", ref.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "t", ref.SHA256); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	if _, err := s3.New(s3.Options{Endpoint: "http://x"}); err == nil {
		t.Fatal("missing bucket accepted")
	}
	s, _ := s3.New(s3.Options{Endpoint: "http://127.0.0.1:1", Region: "r", Bucket: "b"})
	if _, err := s.Get(context.Background(), "../x", strings.Repeat("a", 64)); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("bad tenant accepted")
	}
}
