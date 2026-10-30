package memory

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewIDIsOrderedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 2000; i++ {
		id := NewID()
		if len(id) != 36 || id[14] != '7' {
			t.Fatalf("not a uuidv7: %s", id)
		}
		if seen[id] || id <= prev {
			t.Fatalf("id %s not unique/increasing after %s", id, prev)
		}
		seen[id], prev = true, id
	}
}

func TestNormalizeHashEntityKey(t *testing.T) {
	if got := Normalize("  a\t\tb\n c\x00 "); got != "a b c" {
		t.Fatalf("Normalize = %q", got)
	}
	if Hash("User  lives in Lisbon") != Hash("user lives in lisbon ") {
		t.Fatal("hash must ignore case and whitespace")
	}
	if got := EntityKey("  Acme-Corp, Inc. "); got != "acme corp inc" {
		t.Fatalf("EntityKey = %q", got)
	}
	if got := EntityKey("São Paulo"); got != "são paulo" {
		t.Fatalf("EntityKey unicode = %q", got)
	}
}

func sample() *Memory {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return &Memory{
		ID: "m1", Scope: Scope{Tenant: "t", User: "alice", Agent: "a1"},
		Kind: Semantic, Layer: Atom, Text: "Alice lives in Lisbon", Slot: "user.location",
		Tags: []string{"travel", "home"}, Metadata: map[string]any{"city": "Lisbon", "n": 3.0},
		Entities:   []EntityRef{{Type: "place", Name: "Lisbon"}},
		Confidence: 0.9, Visibility: Team, ValidFrom: at, RecordedAt: at, Version: 1,
		Source: Source{Kind: "explicit", Trust: TrustHigh},
	}
}

func TestParseFilterAndMatch(t *testing.T) {
	m := sample()
	cases := []struct {
		json string
		want bool
	}{
		{`{"user_id":"alice"}`, true},
		{`{"user_id":"bob"}`, false},
		{`{"city":"Lisbon"}`, true}, // bare unknown key = metadata
		{`{"metadata.n":{"gte":3}}`, true},
		{`{"metadata.n":{"gt":3}}`, false},
		{`{"tags":{"contains":"travel"}}`, true},
		{`{"tags":{"in":["x","home"]}}`, true},
		{`{"tags":{"nin":["home"]}}`, false},
		{`{"text":{"icontains":"LISBON"}}`, true},
		{`{"created_at":{"gte":"2026-02-01"}}`, true},
		{`{"valid_to":{"exists":false}}`, true},
		{`{"entities":"lisbon"}`, true},
		{`{"OR":[{"user_id":"bob"},{"kind":"semantic"}]}`, true},
		{`{"NOT":{"kind":"semantic"}}`, false},
		{`{"AND":[{"layer":1},{"confidence":{"lt":0.95}}]}`, true},
		{`{"missing":{"ne":"x"}}`, true},
	}
	for _, c := range cases {
		f, err := ParseFilter([]byte(c.json))
		if err != nil {
			t.Fatalf("%s: %v", c.json, err)
		}
		if got := f.Match(m); got != c.want {
			t.Errorf("%s: got %v want %v", c.json, got, c.want)
		}
	}
}

func TestParseFilterRejects(t *testing.T) {
	bad := []string{
		`{"user_id":{"regex":"a"}}`,
		`{"tags":{"gt":"a"}}`,
		`{"layer":{"in":"x"}}`,
		`{"metadata.a b":"x"}`,
		`{"valid_from":{"gte":"yesterday"}}`,
		`{"OR":5}`,
		`[1,2]`,
		`{"expires_at":{"exists":"yes"}}`,
	}
	for _, b := range bad {
		if _, err := ParseFilter([]byte(b)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", b, err)
		}
	}
	deep := strings.Repeat(`{"NOT":`, 12) + `{"kind":"x"}` + strings.Repeat(`}`, 12)
	if _, err := ParseFilter([]byte(deep)); !errors.Is(err, ErrInvalid) {
		t.Errorf("deep filter accepted: %v", err)
	}
}

func TestReadable(t *testing.T) {
	m := sample()
	alice := Principal{User: "alice", Agent: "a1"}
	bob := Principal{User: "bob", Agent: "a1", Roles: []string{"support"}}
	check := func(name string, p Principal, want bool) {
		t.Helper()
		f := Readable(p)
		if err := f.Check(); err != nil {
			t.Fatal(err)
		}
		if got := f.Match(m); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	check("team visible to bob", bob, true)
	m.Visibility = Private
	check("private visible to owner", alice, true)
	check("private hidden from bob", bob, false)
	check("admin sees all", Principal{Admin: true}, true)
	m.Visibility = Restricted
	check("restricted hidden without grant", bob, false)
	m.Grants = []string{"role:support"}
	check("restricted visible by role", bob, true)
}

func TestValidate(t *testing.T) {
	m := sample()
	if err := m.Validate(DefaultLimits); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Memory){
		"tenant":     func(m *Memory) { m.Tenant = "" },
		"empty":      func(m *Memory) { m.Text = "  " },
		"kind":       func(m *Memory) { m.Kind = "x" },
		"confidence": func(m *Memory) { m.Confidence = 2 },
		"grant":      func(m *Memory) { m.Grants = []string{"bob"} },
		"window":     func(m *Memory) { t := m.ValidFrom.Add(-time.Hour); m.ValidTo = &t },
	} {
		c := *sample()
		mut(&c)
		if err := c.Validate(DefaultLimits); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestCurrent(t *testing.T) {
	m := sample()
	now := m.ValidFrom.Add(time.Hour)
	if !m.Current(now) {
		t.Fatal("should be current")
	}
	end := now.Add(-time.Minute)
	m.ValidTo = &end
	if m.Current(now) || !m.Current(end.Add(-time.Second)) {
		t.Fatal("validity window wrong")
	}
}
