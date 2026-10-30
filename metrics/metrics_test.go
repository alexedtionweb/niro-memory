package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	r := New()
	for i := 1; i <= 100; i++ {
		r.Observe("search", time.Duration(i)*time.Millisecond, nil)
	}
	r.Observe("search", time.Second, errors.New("x"))
	s := r.Snapshot()
	if len(s) != 1 || s[0].Count != 101 || s[0].Errors != 1 || s[0].MaxMS != 1000 {
		t.Fatalf("%+v", s)
	}
	if s[0].P50MS < 35 || s[0].P50MS > 65 || s[0].P99MS < 75 {
		t.Fatalf("percentiles off: %+v", s[0])
	}
	var b strings.Builder
	if err := r.WritePrometheus(&b); err != nil || !strings.Contains(b.String(), `niromem_op_seconds_count{op="search"} 101`) {
		t.Fatalf("%v\n%s", err, b.String())
	}
}
