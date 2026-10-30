package sqlgen

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

type d struct{}

func (d) Param(n int) string                     { return "$" + strconv.Itoa(n) }
func (d) Column(f string) string                 { return "m." + f }
func (d) Time(t time.Time) any                   { return t }
func (d) Bool(b bool) any                        { return b }
func (d) Meta(k string, _ Kind) (string, string) { return "g(" + k + ")", "v(" + k + ")" }
func (d) ListHas(f, p string) string             { return "has(" + f + "," + p + ")" }
func (d) ListPresent(f string) string            { return "len(" + f + ")>0" }
func (d) Lower(e string) string                  { return "lower(" + e + ")" }
func (d) Contains(h, n string) string            { return "contains(" + h + "," + n + ")" }

func TestWhereBindsValues(t *testing.T) {
	f, err := memory.ParseFilter([]byte(`{"user_id":"x'); DROP TABLE m;--","metadata.k":{"ne":"v"},"tags":{"in":["a","b"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{D: d{}}
	sql, err := b.Where(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "DROP") || len(b.Args) != 4 {
		t.Fatalf("values must be bound: %s %v", sql, b.Args)
	}
	if _, err := (&Builder{D: d{}}).Where(memory.Filter{Field: "metadata.bad key", Op: memory.OpEq, Value: "x"}); err == nil {
		t.Fatal("unchecked metadata key accepted")
	}
}
