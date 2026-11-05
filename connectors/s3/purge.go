package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/alexedtionweb/niro-memory/blob"
)

// PurgeTenantBatch removes a bounded page of current objects. Versioned bucket
// history and object-lock retention remain the bucket administrator's concern.
func (s *Store) PurgeTenantBatch(ctx context.Context, tenant string, limit int) (int, bool, error) {
	if limit < 1 || limit > 1000 {
		return 0, false, fmt.Errorf("s3: purge batch must be 1..1000")
	}
	if err := blob.ValidKey(tenant, strings.Repeat("0", 64)); err != nil {
		return 0, false, err
	}
	u := s.objectURL(tenant, strings.Repeat("0", 64))
	u.Path = "/"
	if s.o.PathStyle {
		u.Path = "/" + s.o.Bucket
	}
	q := u.Query()
	prefix := s.o.Prefix + tenant + "/"
	q.Set("list-type", "2")
	q.Set("prefix", prefix)
	q.Set("max-keys", fmt.Sprint(limit))
	u.RawQuery = q.Encode()
	resp, err := s.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, false, failure(resp)
	}
	var list struct {
		XMLName   xml.Name `xml:"ListBucketResult"`
		Truncated bool     `xml:"IsTruncated"`
		Contents  []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&list); err != nil {
		return 0, false, err
	}
	if len(list.Contents) > limit {
		return 0, false, fmt.Errorf("s3: oversized listing")
	}
	if list.Truncated && len(list.Contents) == 0 {
		return 0, false, fmt.Errorf("s3: truncated listing made no progress")
	}
	n := 0
	for _, object := range list.Contents {
		if !strings.HasPrefix(object.Key, prefix) {
			return n, false, fmt.Errorf("s3: object outside tenant prefix")
		}
		target := *u
		target.RawQuery = ""
		target.Path = "/" + object.Key
		if s.o.PathStyle {
			target.Path = "/" + s.o.Bucket + "/" + object.Key
		}
		result, err := s.do(ctx, http.MethodDelete, &target, nil, nil)
		if err != nil {
			return n, false, err
		}
		if result.StatusCode >= 300 && result.StatusCode != http.StatusNotFound {
			err = failure(result)
		}
		result.Body.Close()
		if err != nil {
			return n, false, err
		}
		n++
	}
	return n, !list.Truncated, nil
}
