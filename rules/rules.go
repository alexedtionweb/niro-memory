// Package rules is the deterministic extraction engine. A JSON rules file
// declares regular expressions that turn conversation text and structured
// tool output into memory candidates — no AI involved.
//
// Safety: Go regexps are RE2 (linear time), so a rules file cannot cause
// catastrophic backtracking; pattern size and rule counts are capped.
// Correctness: every rule may carry examples, which run at load time; a file
// whose examples fail is rejected.
//
// See docs/rules.md for the file format and configs/rules.example.json.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Limits on rule files.
const (
	MaxRules      = 500
	MaxPatternLen = 2000
	MaxFileBytes  = 1 << 20
)

// File is the JSON document.
type File struct {
	Version    int          `json:"version"`
	Skip       []string     `json:"skip,omitempty"`
	Flags      []FlagRule   `json:"flags,omitempty"`
	Entities   []EntityRule `json:"entities,omitempty"`
	Times      []TimeRule   `json:"time,omitempty"`
	Facts      []FactRule   `json:"facts,omitempty"`
	Structured []DataRule   `json:"structured,omitempty"`
}

// FlagRule marks matching text with a flag (e.g. "instruction-like") and
// lowers its trust. Flags never block storage; they label it.
type FlagRule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// EntityRule recognizes entities. The capture group "name" (or the whole
// match) is the entity name.
type EntityRule struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
}

// TimeRule resolves time expressions against the observation time of the
// turn. Either Offset ("-24h", "-168h") or captures named year, month and
// day (and optionally hour, minute) must be given.
type TimeRule struct {
	Pattern string `json:"pattern"`
	Offset  string `json:"offset,omitempty"`
}

// FactRule turns a pattern match into a memory.
type FactRule struct {
	ID       string    `json:"id"`
	Roles    []string  `json:"roles,omitempty"` // default ["user"]
	Pattern  string    `json:"pattern"`
	Emit     Emit      `json:"emit"`
	Examples []Example `json:"examples,omitempty"`
}

// DataRule turns structured parts (tool results, events) into memories.
// Source matches the part name (tool or event name); Vars map template
// names to dot paths in the JSON data ("order.id", "items.0.sku").
type DataRule struct {
	ID       string            `json:"id"`
	Source   string            `json:"source"`
	Vars     map[string]string `json:"vars"`
	Require  []string          `json:"require,omitempty"`
	Emit     Emit              `json:"emit"`
	Examples []DataExample     `json:"examples,omitempty"`
}

// Emit describes the memory a rule produces. Strings may contain {name}
// placeholders for captures/vars and the built-ins {user}, {agent},
// {actor}, {role} and {date} (observation date, YYYY-MM-DD).
type Emit struct {
	// Op is "add" (default) or "invalidate": end the open value of Slot,
	// optionally only when its text contains Match (case-insensitive).
	Op         string      `json:"op,omitempty"`
	Match      string      `json:"match,omitempty"`
	Kind       string      `json:"kind,omitempty"`  // default semantic
	Layer      int         `json:"layer,omitempty"` // default 1
	Title      string      `json:"title,omitempty"`
	Text       string      `json:"text,omitempty"`
	Subject    string      `json:"subject,omitempty"`
	Predicate  string      `json:"predicate,omitempty"`
	Object     string      `json:"object,omitempty"`
	Slot       string      `json:"slot,omitempty"`
	Tags       []string    `json:"tags,omitempty"`
	Entities   []EntityRef `json:"entities,omitempty"`
	Confidence float64     `json:"confidence,omitempty"` // default 0.8
	Importance float64     `json:"importance,omitempty"` // default 0.5
	Visibility string      `json:"visibility,omitempty"`
	TTL        string      `json:"ttl,omitempty"` // e.g. "72h": sets ExpiresAt
}

// EntityRef names an entity from a template ("{place}" or literal).
type EntityRef struct {
	Type string `json:"type,omitempty"`
	From string `json:"from"`
}

// Example is a self-test of a fact rule. Want lists expected captures; a
// null Want asserts that the input must not match.
type Example struct {
	In   string            `json:"in"`
	Want map[string]string `json:"want"`
}

// DataExample is a self-test of a structured rule: Data must produce Text.
type DataExample struct {
	Data json.RawMessage `json:"data"`
	Text string          `json:"text"`
}

// Set is a compiled, validated rules file. It is immutable and safe for
// concurrent use.
type Set struct {
	Version  string // "rules@<hash>": recorded as DerivedBy on every memory
	file     File
	skip     []*regexp.Regexp
	flags    []flag
	entities []entity
	times    []timeRule
	facts    []fact
	data     []dataRule
}

type flag struct {
	name string
	re   *regexp.Regexp
}

type entity struct {
	typ string
	re  *regexp.Regexp
}

type timeRule struct {
	re     *regexp.Regexp
	offset time.Duration
}

type fact struct {
	FactRule
	re  *regexp.Regexp
	ttl time.Duration
}

type dataRule struct {
	DataRule
	ttl time.Duration
}

// LoadFile reads and compiles a rules file.
func LoadFile(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse compiles a rules document and runs its examples.
func Parse(data []byte) (*Set, error) {
	if len(data) > MaxFileBytes {
		return nil, invalid("rules file larger than %d bytes", MaxFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, invalid("rules: %v", err)
	}
	if f.Version != 1 {
		return nil, invalid("rules: unsupported version %d (want 1)", f.Version)
	}
	if n := len(f.Skip) + len(f.Flags) + len(f.Entities) + len(f.Times) + len(f.Facts) + len(f.Structured); n > MaxRules {
		return nil, invalid("rules: %d rules, limit %d", n, MaxRules)
	}
	sum := sha256.Sum256(data)
	s := &Set{Version: "rules@" + hex.EncodeToString(sum[:4]), file: f}
	for i, p := range f.Skip {
		re, err := compile(p, fmt.Sprintf("skip[%d]", i))
		if err != nil {
			return nil, err
		}
		s.skip = append(s.skip, re)
	}
	for i, fr := range f.Flags {
		re, err := compile(fr.Pattern, fmt.Sprintf("flags[%d]", i))
		if err != nil {
			return nil, err
		}
		if fr.Name == "" {
			return nil, invalid("rules: flags[%d] needs a name", i)
		}
		s.flags = append(s.flags, flag{fr.Name, re})
	}
	for i, er := range f.Entities {
		re, err := compile(er.Pattern, fmt.Sprintf("entities[%d]", i))
		if err != nil {
			return nil, err
		}
		s.entities = append(s.entities, entity{er.Type, re})
	}
	for i, tr := range f.Times {
		re, err := compile(tr.Pattern, fmt.Sprintf("time[%d]", i))
		if err != nil {
			return nil, err
		}
		t := timeRule{re: re}
		if tr.Offset != "" {
			if t.offset, err = time.ParseDuration(tr.Offset); err != nil {
				return nil, invalid("rules: time[%d]: %v", i, err)
			}
		} else if re.SubexpIndex("year") < 0 || re.SubexpIndex("month") < 0 || re.SubexpIndex("day") < 0 {
			return nil, invalid("rules: time[%d] needs offset or year/month/day captures", i)
		}
		s.times = append(s.times, t)
	}
	ids := map[string]bool{}
	for _, fr := range f.Facts {
		if fr.ID == "" || ids[fr.ID] {
			return nil, invalid("rules: fact id %q missing or duplicated", fr.ID)
		}
		ids[fr.ID] = true
		re, err := compile(fr.Pattern, "fact "+fr.ID)
		if err != nil {
			return nil, err
		}
		ft := fact{FactRule: fr, re: re}
		if len(ft.Roles) == 0 {
			ft.Roles = []string{"user"}
		}
		if ft.ttl, err = checkEmit(fr.ID, fr.Emit, re.SubexpNames()); err != nil {
			return nil, err
		}
		s.facts = append(s.facts, ft)
	}
	for _, dr := range f.Structured {
		if dr.ID == "" || ids[dr.ID] || dr.Source == "" {
			return nil, invalid("rules: structured rule id %q missing, duplicated, or without source", dr.ID)
		}
		ids[dr.ID] = true
		names := []string{}
		for name := range dr.Vars {
			names = append(names, name)
		}
		d := dataRule{DataRule: dr}
		var err error
		if d.ttl, err = checkEmit(dr.ID, dr.Emit, names); err != nil {
			return nil, err
		}
		s.data = append(s.data, d)
	}
	return s, s.selfTest()
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{memory.ErrInvalid}, a...)...)
}

func compile(p, where string) (*regexp.Regexp, error) {
	if p == "" || len(p) > MaxPatternLen {
		return nil, invalid("rules: %s: pattern empty or longer than %d", where, MaxPatternLen)
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, invalid("rules: %s: %v", where, err)
	}
	return re, nil
}

var builtins = map[string]bool{"user": true, "agent": true, "actor": true, "role": true, "date": true}

var placeholder = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func checkEmit(id string, e Emit, names []string) (time.Duration, error) {
	known := map[string]bool{}
	for _, n := range names {
		if n != "" {
			known[n] = true
		}
	}
	tmpl := []string{e.Title, e.Text, e.Subject, e.Predicate, e.Object, e.Slot, e.Match}
	for _, en := range e.Entities {
		tmpl = append(tmpl, en.From)
	}
	tmpl = append(tmpl, e.Tags...)
	for _, t := range tmpl {
		for _, m := range placeholder.FindAllStringSubmatch(t, -1) {
			if !known[m[1]] && !builtins[m[1]] {
				return 0, invalid("rules: %s uses unknown placeholder {%s}", id, m[1])
			}
		}
	}
	switch e.Op {
	case "", "add":
		if e.Text == "" {
			return 0, invalid("rules: %s: emit.text is required", id)
		}
	case "invalidate":
		if e.Slot == "" {
			return 0, invalid("rules: %s: invalidate needs emit.slot", id)
		}
	default:
		return 0, invalid("rules: %s: unknown op %q", id, e.Op)
	}
	switch memory.Kind(e.Kind) {
	case "", memory.Working, memory.Episodic, memory.Semantic, memory.Procedural:
	default:
		return 0, invalid("rules: %s: unknown kind %q", id, e.Kind)
	}
	switch memory.Visibility(e.Visibility) {
	case "", memory.Team, memory.Private, memory.Restricted:
	default:
		return 0, invalid("rules: %s: unknown visibility %q", id, e.Visibility)
	}
	if e.Layer < 0 || e.Layer > 3 || e.Confidence < 0 || e.Confidence > 1 || e.Importance < 0 || e.Importance > 1 {
		return 0, invalid("rules: %s: layer/confidence/importance out of range", id)
	}
	if e.TTL == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(e.TTL)
	if err != nil || d <= 0 {
		return 0, invalid("rules: %s: bad ttl %q", id, e.TTL)
	}
	return d, nil
}

// selfTest runs every example; the first failure rejects the file.
func (s *Set) selfTest() error {
	for _, f := range s.facts {
		for i, ex := range f.Examples {
			m := f.re.FindStringSubmatch(ex.In)
			if ex.Want == nil {
				if m != nil {
					return invalid("rules: %s example %d: %q must not match", f.ID, i, ex.In)
				}
				continue
			}
			if m == nil {
				return invalid("rules: %s example %d: %q does not match", f.ID, i, ex.In)
			}
			for name, want := range ex.Want {
				idx := f.re.SubexpIndex(name)
				if idx < 0 || strings.TrimSpace(m[idx]) != want {
					got := ""
					if idx >= 0 {
						got = m[idx]
					}
					return invalid("rules: %s example %d: capture %s = %q, want %q", f.ID, i, name, got, want)
				}
			}
		}
	}
	for _, d := range s.data {
		for i, ex := range d.Examples {
			cs := s.Structured(Part{Name: d.Source, Data: ex.Data}, Context{})
			found := false
			for _, c := range cs {
				if c.Rule == d.ID && c.Text == ex.Text {
					found = true
				}
			}
			if !found {
				return invalid("rules: %s example %d: did not produce %q", d.ID, i, ex.Text)
			}
		}
	}
	return nil
}

// Rules returns the parsed document (for display and editing).
func (s *Set) Rules() File { return s.file }
