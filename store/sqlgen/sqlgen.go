// Package sqlgen compiles memory filters and temporal conditions to SQL so
// every SQL connector shares one implementation with the exact semantics of
// memory.Filter.Match. Connectors supply a small [Dialect].
//
// Semantics mirrored from Match:
//   - A leaf on an unset field is false, except ne/nin which are true.
//   - Values of different types never compare equal or ordered.
//   - user_id, agent_id and run_id are always set ("" means "none").
//
// Only canonical field names and validated metadata keys are ever inlined
// into SQL; every value is a bound parameter.
package sqlgen

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

// Kind is the type class of a compared metadata value.
type Kind int

// Value kinds.
const (
	KindText Kind = iota
	KindNumber
	KindBool
)

// Dialect adapts SQL generation to one database.
type Dialect interface {
	// Param returns the placeholder of the n-th argument (1-based).
	Param(n int) string
	// Column returns the expression of a scalar core field ("kind", "heat",
	// "valid_to", "actor", …) of the memories table aliased "m".
	Column(field string) string
	// Time converts a time to the driver value stored in time columns.
	Time(t time.Time) any
	// Bool converts a boolean to the driver value compared with metadata.
	Bool(b bool) any
	// Meta returns a guard that is true when metadata key holds a value of
	// kind k, and the expression of that value.
	Meta(key string, k Kind) (guard, value string)
	// ListHas returns a predicate true when list field (tags, grants,
	// entities) contains the value bound to param.
	ListHas(field, param string) string
	// ListPresent returns a predicate true when the list is not empty.
	ListPresent(field string) string
	// Lower returns a Unicode-aware lower-case expression.
	Lower(expr string) string
	// Contains returns a case-sensitive substring predicate.
	Contains(hay, needle string) string
}

// Builder accumulates SQL arguments while compiling.
type Builder struct {
	D    Dialect
	Args []any
}

// Arg binds v and returns its placeholder.
func (b *Builder) Arg(v any) string {
	b.Args = append(b.Args, v)
	return b.D.Param(len(b.Args))
}

var alwaysSet = map[string]bool{"user_id": true, "agent_id": true, "run_id": true}

// Where compiles a checked filter. An empty filter compiles to "1=1".
func (b *Builder) Where(f memory.Filter) (string, error) {
	switch {
	case f.IsZero():
		return "1=1", nil
	case f.And != nil || f.Or != nil:
		subs, op, empty := f.And, " AND ", "1=1"
		if f.Or != nil {
			subs, op, empty = f.Or, " OR ", "1=0"
		}
		if len(subs) == 0 {
			return empty, nil
		}
		parts := make([]string, len(subs))
		for i, s := range subs {
			p, err := b.Where(s)
			if err != nil {
				return "", err
			}
			parts[i] = p
		}
		return "(" + strings.Join(parts, op) + ")", nil
	case f.Not != nil:
		p, err := b.Where(*f.Not)
		if err != nil {
			return "", err
		}
		return "(NOT " + p + ")", nil
	}
	return b.leaf(f)
}

func (b *Builder) leaf(f memory.Filter) (string, error) {
	name, typ := memory.FieldTypeOf(f.Field)
	if name != f.Field {
		return "", fmt.Errorf("%w: filter not checked (field %q)", memory.ErrInvalid, f.Field)
	}
	if typ == memory.FieldMeta && !memory.ValidMetaKey(strings.TrimPrefix(name, "metadata.")) {
		return "", fmt.Errorf("%w: bad metadata key", memory.ErrInvalid)
	}
	if f.Op == memory.OpExists {
		if v, _ := f.Value.(bool); v {
			return b.present(name, typ), nil
		}
		return "(NOT " + b.present(name, typ) + ")", nil
	}
	if typ == memory.FieldList {
		return b.list(name, f)
	}
	negate := f.Op == memory.OpNe || f.Op == memory.OpNin
	var pos string
	switch f.Op {
	case memory.OpIn, memory.OpNin:
		vals, _ := f.Value.([]any)
		ors := []string{"1=0"}
		for _, v := range vals {
			p, err := b.cmp(name, typ, "=", v)
			if err != nil {
				return "", err
			}
			ors = append(ors, p)
		}
		pos = "(" + strings.Join(ors, " OR ") + ")"
	case memory.OpContains, memory.OpIContains:
		s, ok := f.Value.(string)
		col, guard := b.textExpr(name, typ)
		if !ok || col == "" {
			return "1=0", nil
		}
		if f.Op == memory.OpIContains {
			pos = "(" + guard + " AND " + b.D.Contains(b.D.Lower(col), b.D.Lower(b.Arg(s))) + ")"
		} else {
			pos = "(" + guard + " AND " + b.D.Contains(col, b.Arg(s)) + ")"
		}
	default:
		op := map[memory.Op]string{memory.OpEq: "=", memory.OpNe: "=", memory.OpGt: ">", memory.OpGte: ">=", memory.OpLt: "<", memory.OpLte: "<="}[f.Op]
		if op == "" {
			return "", fmt.Errorf("%w: unknown operator %q", memory.ErrInvalid, f.Op)
		}
		p, err := b.cmp(name, typ, op, f.Value)
		if err != nil {
			return "", err
		}
		pos = p
	}
	// COALESCE turns SQL NULL (unset field) into false, matching Match.
	pos = "COALESCE(" + pos + ", 1=0)"
	if negate {
		return "(NOT " + pos + ")", nil
	}
	return pos, nil
}

// present is true when the field is set, mirroring Memory.FieldValue.
func (b *Builder) present(name string, typ memory.FieldType) string {
	switch typ {
	case memory.FieldMeta:
		key := strings.TrimPrefix(name, "metadata.")
		var gs []string
		for _, k := range []Kind{KindText, KindNumber, KindBool} {
			g, _ := b.D.Meta(key, k)
			gs = append(gs, g)
		}
		return "COALESCE(" + strings.Join(gs, " OR ") + ", 1=0)"
	case memory.FieldList:
		return b.D.ListPresent(name)
	case memory.FieldTime:
		return b.D.Column(name) + " IS NOT NULL"
	case memory.FieldString:
		if alwaysSet[name] {
			return "1=1"
		}
		return b.D.Column(name) + " <> ''"
	}
	return "1=1"
}

// textExpr returns the expression and presence guard of a text-valued field.
func (b *Builder) textExpr(name string, typ memory.FieldType) (string, string) {
	switch typ {
	case memory.FieldMeta:
		g, v := b.D.Meta(strings.TrimPrefix(name, "metadata."), KindText)
		return v, g
	case memory.FieldString:
		return b.D.Column(name), b.present(name, typ)
	}
	return "", ""
}

// cmp builds "field <op> value", guarded so that unset fields and values of
// another type yield false.
func (b *Builder) cmp(name string, typ memory.FieldType, op string, v any) (string, error) {
	switch typ {
	case memory.FieldString:
		s, ok := v.(string)
		if !ok {
			return "1=0", nil
		}
		return "(" + b.present(name, typ) + " AND " + b.D.Column(name) + " " + op + " " + b.Arg(s) + ")", nil
	case memory.FieldNumber:
		n, ok := v.(float64)
		if !ok {
			return "1=0", nil
		}
		return b.D.Column(name) + " " + op + " " + b.Arg(n), nil
	case memory.FieldTime:
		t, ok := v.(time.Time)
		if !ok {
			return "1=0", nil
		}
		return b.D.Column(name) + " " + op + " " + b.Arg(b.D.Time(t)), nil
	case memory.FieldMeta:
		var k Kind
		var arg any
		switch x := v.(type) {
		case string:
			k, arg = KindText, x
		case float64:
			k, arg = KindNumber, x
		case bool:
			if op != "=" {
				return "1=0", nil // booleans are only equal or not
			}
			k, arg = KindBool, b.D.Bool(x)
		default:
			return "1=0", nil
		}
		g, val := b.D.Meta(strings.TrimPrefix(name, "metadata."), k)
		return "(" + g + " AND " + val + " " + op + " " + b.Arg(arg) + ")", nil
	}
	return "", fmt.Errorf("%w: cannot compare field %s", memory.ErrInvalid, name)
}

func (b *Builder) list(name string, f memory.Filter) (string, error) {
	switch f.Op {
	case memory.OpEq, memory.OpContains, memory.OpNe:
		s, _ := f.Value.(string)
		p := b.D.ListHas(name, b.Arg(s))
		if f.Op == memory.OpNe {
			return "(NOT " + p + ")", nil
		}
		return p, nil
	case memory.OpIn, memory.OpNin:
		vals, _ := f.Value.([]any)
		ors := []string{"1=0"}
		for _, v := range vals {
			s, _ := v.(string)
			ors = append(ors, b.D.ListHas(name, b.Arg(s)))
		}
		p := "(" + strings.Join(ors, " OR ") + ")"
		if f.Op == memory.OpNin {
			return "(NOT " + p + ")", nil
		}
		return p, nil
	}
	return "", fmt.Errorf("%w: %s not supported on list %s", memory.ErrInvalid, f.Op, name)
}

// Temporal compiles the bi-temporal and expiry conditions of q, with the
// same meaning as store.Query.Admits.
func (b *Builder) Temporal(q store.Query, now time.Time) string {
	var conds []string
	col := b.D.Column
	if !q.ShowExpired {
		conds = append(conds, "("+col("expires_at")+" IS NULL OR "+col("expires_at")+" > "+b.Arg(b.D.Time(now))+")")
	}
	if !q.AllVersions {
		valid, known := q.ValidAt, q.KnownAt
		if valid.IsZero() {
			valid = now
		}
		if known.IsZero() {
			known = now
		}
		k, v := b.Arg(b.D.Time(known)), b.Arg(b.D.Time(valid))
		conds = append(conds,
			col("recorded_at")+" <= "+k,
			"("+col("invalidated_at")+" IS NULL OR "+col("invalidated_at")+" > "+k+")",
			col("valid_from")+" <= "+v,
			"("+col("valid_to")+" IS NULL OR "+col("valid_to")+" > "+v+")")
	}
	if len(conds) == 0 {
		return "1=1"
	}
	return strings.Join(conds, " AND ")
}

// OrderBy returns the ORDER BY expression of a store ordering. Memory IDs are
// UUIDv7, so ordering by id is ordering by creation time.
func OrderBy(o store.Order, d Dialect) string {
	switch o {
	case store.OldestFirst:
		return d.Column("id") + " ASC"
	case store.HottestFirst:
		return d.Column("heat") + " DESC, " + d.Column("id") + " DESC"
	}
	return d.Column("id") + " DESC"
}
