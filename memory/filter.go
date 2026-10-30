package memory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Op is a filter comparison operator.
type Op string

// Filter operators. They follow the Mem0 filter language.
const (
	OpEq        Op = "eq"
	OpNe        Op = "ne"
	OpGt        Op = "gt"
	OpGte       Op = "gte"
	OpLt        Op = "lt"
	OpLte       Op = "lte"
	OpIn        Op = "in"
	OpNin       Op = "nin"
	OpContains  Op = "contains"  // substring for text, membership for lists
	OpIContains Op = "icontains" // case-insensitive substring
	OpExists    Op = "exists"    // value true/false: field is set / not set
)

// FieldType is the type of a filterable field.
type FieldType int

// Field types.
const (
	FieldString FieldType = iota
	FieldNumber
	FieldTime
	FieldList
	FieldMeta // metadata.<key>; dynamically typed
)

// Fields lists the filterable core fields of a [Memory]. Any other key is a
// metadata key ("metadata.<key>" or a bare unknown key, as in Mem0).
var Fields = map[string]FieldType{
	"id": FieldString, "kind": FieldString, "title": FieldString, "text": FieldString,
	"subject": FieldString, "predicate": FieldString, "object": FieldString,
	"slot": FieldString, "hash": FieldString, "visibility": FieldString,
	"user_id": FieldString, "agent_id": FieldString, "run_id": FieldString,
	"actor": FieldString, "role": FieldString, "source": FieldString,
	"trust": FieldString, "derived_by": FieldString, "supersedes": FieldString,
	"layer": FieldNumber, "confidence": FieldNumber, "importance": FieldNumber,
	"heat": FieldNumber, "version": FieldNumber,
	"valid_from": FieldTime, "valid_to": FieldTime, "recorded_at": FieldTime,
	"created_at": FieldTime, "invalidated_at": FieldTime, "expires_at": FieldTime,
	"tags": FieldList, "grants": FieldList, "entities": FieldList,
}

// Filter is a boolean expression over memory fields. A node is either a leaf
// (Field, Op, Value) or exactly one of And, Or, Not.
//
// After [Filter.Check], leaf values are normalized: times are time.Time,
// numbers float64, lists []any.
type Filter struct {
	Field string   `json:"field,omitempty"`
	Op    Op       `json:"op,omitempty"`
	Value any      `json:"value,omitempty"`
	And   []Filter `json:"and,omitempty"`
	Or    []Filter `json:"or,omitempty"`
	Not   *Filter  `json:"not,omitempty"`
}

// Eq builds a field = value leaf.
func Eq(field string, v any) Filter { return Filter{Field: field, Op: OpEq, Value: v} }

// Cmp builds a leaf with any operator.
func Cmp(field string, op Op, v any) Filter { return Filter{Field: field, Op: op, Value: v} }

// And combines filters; nil-safe and flattening.
func And(fs ...Filter) Filter { return Filter{And: fs} }

// Or combines filters.
func Or(fs ...Filter) Filter { return Filter{Or: fs} }

// Not negates a filter.
func Not(f Filter) Filter { return Filter{Not: &f} }

// IsZero reports whether f is an empty filter (matches everything).
func (f *Filter) IsZero() bool {
	return f == nil || (f.Field == "" && f.And == nil && f.Or == nil && f.Not == nil)
}

// FieldTypeOf resolves a field name to its canonical name and type.
// "created_at" is an alias of "recorded_at"; unknown names are metadata keys.
func FieldTypeOf(name string) (string, FieldType) {
	if name == "created_at" {
		return "recorded_at", FieldTime
	}
	if t, ok := Fields[name]; ok {
		return name, t
	}
	return "metadata." + strings.TrimPrefix(name, "metadata."), FieldMeta
}

// ParseFilter parses the Mem0-style JSON filter language:
//
//	{"user_id": "alice", "created_at": {"gte": "2026-01-01"},
//	 "OR": [{"tags": {"contains": "travel"}}, {"metadata.city": {"in": ["Lisbon", "Porto"]}}]}
//
// Keys of one object are ANDed; "AND", "OR" take arrays and "NOT" takes an
// object or array. A plain value means eq.
func ParseFilter(data []byte) (Filter, error) {
	if len(strings.TrimSpace(string(data))) == 0 || string(data) == "null" {
		return Filter{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return Filter{}, invalid("filter: %v", err)
	}
	f, err := FilterFromMap(m)
	if err != nil {
		return Filter{}, err
	}
	return f, f.Check()
}

// FilterFromMap converts a decoded JSON object into a Filter (see [ParseFilter]).
func FilterFromMap(m map[string]any) (Filter, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output
	var out []Filter
	for _, k := range keys {
		v := m[k]
		switch strings.ToUpper(strings.TrimPrefix(k, "$")) {
		case "AND", "OR":
			subs, err := filterList(v)
			if err != nil {
				return Filter{}, err
			}
			if strings.EqualFold(strings.TrimPrefix(k, "$"), "AND") {
				out = append(out, Filter{And: subs})
			} else {
				out = append(out, Filter{Or: subs})
			}
			continue
		case "NOT":
			subs, err := filterList(v)
			if err != nil {
				return Filter{}, err
			}
			inner := Filter{And: subs}
			out = append(out, Filter{Not: &inner})
			continue
		}
		ops, ok := v.(map[string]any)
		if !ok {
			out = append(out, Eq(k, v))
			continue
		}
		opKeys := make([]string, 0, len(ops))
		for op := range ops {
			opKeys = append(opKeys, op)
		}
		sort.Strings(opKeys)
		for _, op := range opKeys {
			out = append(out, Cmp(k, Op(strings.TrimPrefix(op, "$")), ops[op]))
		}
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return Filter{And: out}, nil
}

func filterList(v any) ([]Filter, error) {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case map[string]any:
		items = []any{t}
	default:
		return nil, invalid("filter: AND/OR/NOT need an object or array")
	}
	out := make([]Filter, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, invalid("filter: logical operands must be objects")
		}
		f, err := FilterFromMap(m)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Filter complexity limits keep queries cheap and stop abusive filters.
const (
	maxFilterDepth = 8
	maxFilterNodes = 128
	maxFilterList  = 1000
)

// Check validates operators and normalizes leaf values in place.
func (f *Filter) Check() error {
	nodes := 0
	return f.check(0, &nodes)
}

func (f *Filter) check(depth int, nodes *int) error {
	*nodes++
	if depth > maxFilterDepth || *nodes > maxFilterNodes {
		return invalid("filter too complex")
	}
	n := 0
	if f.Field != "" {
		n++
	}
	if f.And != nil {
		n++
	}
	if f.Or != nil {
		n++
	}
	if f.Not != nil {
		n++
	}
	if n > 1 {
		return invalid("filter node mixes field and logical operators")
	}
	for i := range f.And {
		if err := f.And[i].check(depth+1, nodes); err != nil {
			return err
		}
	}
	for i := range f.Or {
		if err := f.Or[i].check(depth+1, nodes); err != nil {
			return err
		}
	}
	if f.Not != nil {
		if err := f.Not.check(depth+1, nodes); err != nil {
			return err
		}
	}
	if f.Field == "" {
		return nil
	}
	name, typ := FieldTypeOf(f.Field)
	f.Field = name
	if typ == FieldMeta && !ValidMetaKey(strings.TrimPrefix(name, "metadata.")) {
		return invalid("filter: bad metadata key %q", name)
	}
	switch f.Op {
	case OpExists:
		b, ok := f.Value.(bool)
		if !ok {
			return invalid("filter: exists needs true or false")
		}
		f.Value = b
		return nil
	case OpIn, OpNin:
		list, ok := f.Value.([]any)
		if !ok {
			if s, isStr := f.Value.([]string); isStr {
				for _, v := range s {
					list = append(list, v)
				}
			} else {
				return invalid("filter: %s on %s needs an array", f.Op, f.Field)
			}
		}
		if len(list) > maxFilterList {
			return invalid("filter: list longer than %d", maxFilterList)
		}
		norm := make([]any, len(list))
		for i, v := range list {
			nv, err := normValue(typ, v)
			if err != nil {
				return fmt.Errorf("%w (field %s)", err, f.Field)
			}
			if f.Field == "entities" {
				nv = EntityKey(nv.(string))
			}
			norm[i] = nv
		}
		f.Value = norm
		return nil
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpContains, OpIContains:
	default:
		return invalid("filter: unknown operator %q", f.Op)
	}
	if (f.Op == OpContains || f.Op == OpIContains) && typ != FieldString && typ != FieldList && typ != FieldMeta {
		return invalid("filter: %s not supported on %s", f.Op, f.Field)
	}
	if typ == FieldList && f.Op != OpEq && f.Op != OpNe && f.Op != OpContains {
		return invalid("filter: %s not supported on list field %s", f.Op, f.Field)
	}
	nv, err := normValue(typ, f.Value)
	if err != nil {
		return fmt.Errorf("%w (field %s)", err, f.Field)
	}
	f.Value = nv
	if f.Field == "entities" {
		f.Value = EntityKey(nv.(string))
	}
	return nil
}

// ValidMetaKey reports whether k may be used as a metadata filter key:
// 1-64 ASCII letters, digits, "_" or "-". SQL connectors rely on this to
// embed keys in JSON paths safely.
func ValidMetaKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func normValue(t FieldType, v any) (any, error) {
	switch t {
	case FieldString, FieldList:
		switch x := v.(type) {
		case string:
			if t == FieldList && x == "" {
				return nil, invalid("filter: empty list value")
			}
			return x, nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		}
		return nil, invalid("filter: expected string, got %T", v)
	case FieldNumber:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int:
			return float64(x), nil
		case int64:
			return float64(x), nil
		case string:
			n, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return nil, invalid("filter: expected number, got %q", x)
			}
			return n, nil
		}
		return nil, invalid("filter: expected number, got %T", v)
	case FieldTime:
		return ParseTime(v)
	default: // metadata: keep JSON scalars
		switch x := v.(type) {
		case string, float64, bool:
			return x, nil
		case int:
			return float64(x), nil
		case int64:
			return float64(x), nil
		}
		return nil, invalid("filter: metadata values must be scalars, got %T", v)
	}
}

// ParseTime accepts RFC 3339 timestamps, plain dates (2006-01-02),
// unix seconds as numbers, and time.Time.
func ParseTime(v any) (time.Time, error) {
	switch x := v.(type) {
	case time.Time:
		return x.UTC(), nil
	case float64:
		return time.Unix(int64(x), 0).UTC(), nil
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, invalid("filter: bad time %q", x)
	}
	return time.Time{}, invalid("filter: expected time, got %T", v)
}

// Match evaluates the filter against a memory in Go. Stores that cannot push
// a filter down use it, and the engine re-checks hits from external indexes.
func (f *Filter) Match(m *Memory) bool {
	if f.IsZero() {
		return true
	}
	switch {
	case f.And != nil:
		for i := range f.And {
			if !f.And[i].Match(m) {
				return false
			}
		}
		return true
	case f.Or != nil:
		for i := range f.Or {
			if f.Or[i].Match(m) {
				return true
			}
		}
		return false
	case f.Not != nil:
		return !f.Not.Match(m)
	}
	v, ok := m.FieldValue(f.Field)
	if f.Op == OpExists {
		return ok == f.Value.(bool)
	}
	if !ok {
		return f.Op == OpNe || f.Op == OpNin
	}
	if list, isList := v.([]string); isList {
		has := func(x any) bool {
			s, _ := x.(string)
			for _, e := range list {
				if e == s {
					return true
				}
			}
			return false
		}
		switch f.Op {
		case OpEq, OpContains:
			return has(f.Value)
		case OpNe:
			return !has(f.Value)
		case OpIn:
			for _, x := range f.Value.([]any) {
				if has(x) {
					return true
				}
			}
			return false
		case OpNin:
			for _, x := range f.Value.([]any) {
				if has(x) {
					return false
				}
			}
			return true
		}
		return false
	}
	switch f.Op {
	case OpIn, OpNin:
		found := false
		for _, x := range f.Value.([]any) {
			if compare(v, x) == 0 {
				found = true
				break
			}
		}
		return found == (f.Op == OpIn)
	case OpContains, OpIContains:
		s, ok1 := v.(string)
		sub, ok2 := f.Value.(string)
		if !ok1 || !ok2 {
			return false
		}
		if f.Op == OpIContains {
			return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
		}
		return strings.Contains(s, sub)
	}
	c := compare(v, f.Value)
	switch f.Op {
	case OpEq:
		return c == 0
	case OpNe:
		return c != 0
	case OpGt:
		return c == 1
	case OpGte:
		return c == 0 || c == 1
	case OpLt:
		return c == -1
	case OpLte:
		return c == 0 || c == -1
	}
	return false
}

// compare returns -1, 0, 1, or 2 when the values are not comparable.
func compare(a, b any) int {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		if !ok {
			return 2
		}
		return strings.Compare(x, y)
	case float64:
		y, ok := b.(float64)
		if !ok {
			return 2
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case bool:
		y, ok := b.(bool)
		if !ok {
			return 2
		}
		if x == y {
			return 0
		}
		return 2
	case time.Time:
		y, ok := b.(time.Time)
		if !ok {
			return 2
		}
		return x.Compare(y)
	}
	return 2
}

// FieldValue returns the value of a canonical filter field. The boolean is
// false when the field is unset (empty string, nil time, missing metadata).
func (m *Memory) FieldValue(field string) (any, bool) {
	str := func(s string) (any, bool) { return s, s != "" }
	tm := func(t *time.Time) (any, bool) {
		if t == nil {
			return nil, false
		}
		return t.UTC(), true
	}
	switch field {
	case "id":
		return str(m.ID)
	case "kind":
		return str(string(m.Kind))
	case "title":
		return str(m.Title)
	case "text":
		return str(m.Text)
	case "subject":
		return str(m.Subject)
	case "predicate":
		return str(m.Predicate)
	case "object":
		return str(m.Object)
	case "slot":
		return str(m.Slot)
	case "hash":
		return str(m.Hash)
	case "visibility":
		return str(string(m.Visibility))
	case "user_id":
		return m.User, true // empty owner is a value: "no user"
	case "agent_id":
		return m.Agent, true
	case "run_id":
		return m.Run, true
	case "actor":
		return str(m.Source.Actor)
	case "role":
		return str(m.Source.Role)
	case "source":
		return str(m.Source.Kind)
	case "trust":
		return str(string(m.Source.Trust))
	case "derived_by":
		return str(m.Source.DerivedBy)
	case "supersedes":
		return str(m.Supersedes)
	case "layer":
		return float64(m.Layer), true
	case "confidence":
		return m.Confidence, true
	case "importance":
		return m.Importance, true
	case "heat":
		return float64(m.Heat), true
	case "version":
		return float64(m.Version), true
	case "valid_from":
		return m.ValidFrom.UTC(), true
	case "recorded_at":
		return m.RecordedAt.UTC(), true
	case "valid_to":
		return tm(m.ValidTo)
	case "invalidated_at":
		return tm(m.InvalidatedAt)
	case "expires_at":
		return tm(m.ExpiresAt)
	case "tags":
		return m.Tags, len(m.Tags) > 0
	case "grants":
		return m.Grants, len(m.Grants) > 0
	case "entities":
		keys := make([]string, len(m.Entities))
		for i, e := range m.Entities {
			keys[i] = EntityKey(e.Name)
		}
		return keys, len(keys) > 0
	}
	if k, ok := strings.CutPrefix(field, "metadata."); ok {
		v, ok := m.Metadata[k]
		if !ok || v == nil {
			return nil, false
		}
		switch x := v.(type) {
		case int:
			return float64(x), true
		case int64:
			return float64(x), true
		}
		return v, true
	}
	return nil, false
}

// Readable returns the visibility filter for principal p, or the zero filter
// for admins. Engines AND it with every user filter before querying, so ACLs
// are enforced inside the store, before limits are applied.
func Readable(p Principal) Filter {
	if p.Admin {
		return Filter{}
	}
	owner := And(
		Or(Eq("user_id", ""), Eq("user_id", p.User)),
		Or(Eq("agent_id", ""), Eq("agent_id", p.Agent)),
	)
	var grants []any
	if p.User != "" {
		grants = append(grants, "user:"+p.User)
	}
	if p.Agent != "" {
		grants = append(grants, "agent:"+p.Agent)
	}
	for _, r := range p.Roles {
		grants = append(grants, "role:"+r)
	}
	or := []Filter{Eq("visibility", string(Team)), And(Cmp("visibility", OpIn, []any{string(Private), string(Restricted)}), owner)}
	if len(grants) > 0 {
		or = append(or, And(Eq("visibility", string(Restricted)), Cmp("grants", OpIn, grants)))
	}
	return Or(or...)
}
