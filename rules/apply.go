package rules

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/textutil"
)

// Context is what the rules know about the text being processed.
type Context struct {
	User, Agent, Actor, Role string
	Observed                 time.Time // when the turn happened; anchors relative times
}

// Candidate is a memory proposed by a rule, before validation and
// reconciliation with existing knowledge.
type Candidate struct {
	Rule       string             `json:"rule"`
	Op         string             `json:"op"`              // "add" or "invalidate"
	Match      string             `json:"match,omitempty"` // invalidate: only when the open value contains this
	Kind       memory.Kind        `json:"kind"`
	Layer      memory.Layer       `json:"layer"`
	Title      string             `json:"title,omitempty"`
	Text       string             `json:"text,omitempty"`
	Subject    string             `json:"subject,omitempty"`
	Predicate  string             `json:"predicate,omitempty"`
	Object     string             `json:"object,omitempty"`
	Slot       string             `json:"slot,omitempty"`
	Tags       []string           `json:"tags,omitempty"`
	Entities   []memory.EntityRef `json:"entities,omitempty"`
	Confidence float64            `json:"confidence"`
	Importance float64            `json:"importance"`
	Visibility memory.Visibility  `json:"visibility"`
	ValidFrom  time.Time          `json:"valid_from"`
	ExpiresAt  *time.Time         `json:"expires_at,omitempty"`
	Flags      []string           `json:"flags,omitempty"`
	Start      int                `json:"start"` // byte span of the evidence in the input text
	End        int                `json:"end"`
}

// Part is a structured episode part (tool result or event).
type Part struct {
	Name string
	Data json.RawMessage
}

// Text applies skip, time, flag, entity and fact rules to one text,
// sentence by sentence. It is pure and allocation-light.
func (s *Set) Text(text string, c Context) []Candidate {
	var out []Candidate
	for _, sp := range textutil.Sentences(text) {
		sent := text[sp.Start:sp.End]
		if s.skipped(sent) {
			continue
		}
		var ents []memory.EntityRef
		var flags []string
		prepared := false
		for _, f := range s.facts {
			if !hasRole(f.Roles, c.Role) {
				continue
			}
			for _, loc := range f.re.FindAllStringSubmatchIndex(sent, -1) {
				if !prepared { // only pay for entities/flags when a fact matched
					ents, flags, prepared = s.Entities(sent), s.Flags(sent), true
				}
				vars := builtinVars(c)
				for i, name := range f.re.SubexpNames() {
					if name != "" && loc[2*i] >= 0 {
						vars[name] = strings.TrimSpace(sent[loc[2*i]:loc[2*i+1]])
					}
				}
				cand, ok := build(f.ID, f.Emit, f.ttl, vars, c, s.resolveTime(sent, c.Observed))
				if !ok {
					continue
				}
				cand.Entities = mergeEntities(cand.Entities, ents)
				cand.Flags = flags
				cand.Start, cand.End = sp.Start+loc[0], sp.Start+loc[1]
				out = append(out, cand)
			}
		}
	}
	return out
}

// Structured applies structured rules to a tool result or event part.
func (s *Set) Structured(p Part, c Context) []Candidate {
	var doc any
	parsed := false
	var out []Candidate
	for _, d := range s.data {
		if d.Source != p.Name {
			continue
		}
		if !parsed {
			if json.Unmarshal(p.Data, &doc) != nil {
				return nil
			}
			parsed = true
		}
		vars := builtinVars(c)
		for name, path := range d.Vars {
			if v, ok := lookup(doc, path); ok {
				vars[name] = v
			}
		}
		missing := false
		for _, r := range d.Require {
			if vars[r] == "" {
				missing = true
			}
		}
		if missing {
			continue
		}
		if cand, ok := build(d.ID, d.Emit, d.ttl, vars, c, c.Observed); ok {
			cand.End = len(p.Data)
			out = append(out, cand)
		}
	}
	return out
}

// Entities returns the entities recognized by entity rules, deduplicated by key.
func (s *Set) Entities(text string) []memory.EntityRef {
	var out []memory.EntityRef
	for _, e := range s.entities {
		idx := e.re.SubexpIndex("name")
		for _, m := range e.re.FindAllStringSubmatch(text, 16) {
			name := m[0]
			if idx > 0 {
				name = m[idx]
			}
			out = mergeEntities(out, []memory.EntityRef{{Type: e.typ, Name: strings.TrimSpace(name)}})
		}
	}
	return out
}

// Flags returns the names of flag rules matching text.
func (s *Set) Flags(text string) []string {
	var out []string
	for _, f := range s.flags {
		if f.re.MatchString(text) {
			out = append(out, f.name)
		}
	}
	return out
}

func (s *Set) skipped(sent string) bool {
	for _, re := range s.skip {
		if re.MatchString(sent) {
			return true
		}
	}
	return false
}

// resolveTime returns the time a sentence talks about, or observed.
func (s *Set) resolveTime(sent string, observed time.Time) time.Time {
	for _, t := range s.times {
		m := t.re.FindStringSubmatch(sent)
		if m == nil {
			continue
		}
		if t.offset != 0 {
			return observed.Add(t.offset)
		}
		get := func(name string, def int) int {
			if i := t.re.SubexpIndex(name); i > 0 && m[i] != "" {
				if n, err := strconv.Atoi(m[i]); err == nil {
					return n
				}
			}
			return def
		}
		y, mo, d := get("year", 0), get("month", 0), get("day", 0)
		if y < 1 || mo < 1 || mo > 12 || d < 1 || d > 31 {
			continue
		}
		return time.Date(y, time.Month(mo), d, get("hour", 0), get("minute", 0), 0, 0, time.UTC)
	}
	return observed
}

func builtinVars(c Context) map[string]string {
	date := ""
	if !c.Observed.IsZero() {
		date = c.Observed.UTC().Format("2006-01-02")
	}
	return map[string]string{"user": c.User, "agent": c.Agent, "actor": c.Actor, "role": c.Role, "date": date}
}

func fill(t string, vars map[string]string) string {
	if !strings.Contains(t, "{") {
		return t
	}
	return strings.TrimSpace(placeholder.ReplaceAllStringFunc(t, func(m string) string {
		return vars[m[1:len(m)-1]]
	}))
}

func build(id string, e Emit, ttl time.Duration, vars map[string]string, c Context, validFrom time.Time) (Candidate, bool) {
	cand := Candidate{
		Rule: id, Op: e.Op, Match: fill(e.Match, vars), Kind: memory.Kind(e.Kind), Layer: memory.Layer(e.Layer),
		Title: fill(e.Title, vars), Text: fill(e.Text, vars), Subject: fill(e.Subject, vars),
		Predicate: fill(e.Predicate, vars), Object: fill(e.Object, vars), Slot: fill(e.Slot, vars),
		Confidence: e.Confidence, Importance: e.Importance, Visibility: memory.Visibility(e.Visibility), ValidFrom: validFrom,
	}
	if cand.Op == "" {
		cand.Op = "add"
	}
	if cand.Kind == "" {
		cand.Kind = memory.Semantic
	}
	if cand.Layer == 0 {
		cand.Layer = memory.Atom
	}
	if cand.Confidence == 0 {
		cand.Confidence = 0.8
	}
	if cand.Importance == 0 {
		cand.Importance = 0.5
	}
	if cand.Visibility == "" {
		cand.Visibility = memory.Team
	}
	for _, t := range e.Tags {
		if v := fill(t, vars); v != "" {
			cand.Tags = append(cand.Tags, v)
		}
	}
	for _, en := range e.Entities {
		if v := fill(en.From, vars); v != "" {
			cand.Entities = mergeEntities(cand.Entities, []memory.EntityRef{{Type: en.Type, Name: v}})
		}
	}
	if ttl > 0 {
		exp := c.Observed.Add(ttl)
		cand.ExpiresAt = &exp
	}
	if cand.Op == "add" && cand.Text == "" {
		return cand, false
	}
	if cand.Op == "invalidate" && cand.Slot == "" {
		return cand, false
	}
	return cand, true
}

func mergeEntities(a, b []memory.EntityRef) []memory.EntityRef {
	for _, e := range b {
		k := memory.EntityKey(e.Name)
		if k == "" {
			continue
		}
		dup := false
		for _, x := range a {
			if memory.EntityKey(x.Name) == k {
				dup = true
				break
			}
		}
		if !dup {
			a = append(a, e)
		}
	}
	return a
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role || r == "*" {
			return true
		}
	}
	return false
}

// lookup resolves a dot path ("order.items.0.sku") in decoded JSON and
// renders scalars as text and objects/arrays as compact JSON.
func lookup(doc any, path string) (string, bool) {
	cur := doc
	for _, key := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[key]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(v) {
				return "", false
			}
			cur = v[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	}
	b, _ := json.Marshal(cur)
	return string(b), true
}
