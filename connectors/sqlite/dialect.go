package sqlite

import (
	"strconv"
	"time"

	"github.com/alexedtionweb/niro-memory/store/sqlgen"
)

// dialect adapts sqlgen to SQLite: times are unix milliseconds, lists and
// metadata are JSON text, and entities live in a join table.
type dialect struct{}

var columns = map[string]string{
	"id": "m.id", "kind": "m.kind", "title": "m.title", "text": "m.text", "subject": "m.subject",
	"predicate": "m.predicate", "object": "m.object", "slot": "m.slot", "hash": "m.hash",
	"visibility": "m.visibility", "user_id": "m.user_id", "agent_id": "m.agent_id", "run_id": "m.run_id",
	"actor": "m.actor", "role": "m.role", "source": "m.src_kind", "trust": "m.trust",
	"derived_by": "m.derived_by", "supersedes": "m.supersedes", "layer": "m.layer",
	"confidence": "m.confidence", "importance": "m.importance", "heat": "m.heat", "version": "m.version",
	"valid_from": "m.valid_from", "valid_to": "m.valid_to", "recorded_at": "m.recorded_at",
	"invalidated_at": "m.invalidated_at", "expires_at": "m.expires_at",
}

func (dialect) Param(n int) string          { return "?" + strconv.Itoa(n) }
func (dialect) Column(f string) string      { return columns[f] }
func (dialect) Time(t time.Time) any        { return t.UnixMilli() }
func (dialect) Lower(e string) string       { return "go_lower(" + e + ")" }
func (dialect) Contains(h, n string) string { return "instr(" + h + ", " + n + ") > 0" }

func (dialect) Bool(b bool) any {
	if b {
		return 1
	}
	return 0
}

// Meta inlines the key into a JSON path; sqlgen only passes keys accepted by
// memory.ValidMetaKey ([A-Za-z0-9_-]).
func (dialect) Meta(key string, k sqlgen.Kind) (string, string) {
	path := `'$."` + key + `"'`
	typ := "json_type(m.metadata, " + path + ")"
	val := "json_extract(m.metadata, " + path + ")"
	switch k {
	case sqlgen.KindNumber:
		return typ + " IN ('integer','real')", val
	case sqlgen.KindBool:
		return typ + " IN ('true','false')", val
	}
	return typ + " = 'text'", val
}

const entityHas = "EXISTS (SELECT 1 FROM memory_entities me JOIN entities e ON e.tenant = me.tenant AND e.id = me.entity_id WHERE me.tenant = m.tenant AND me.memory_id = m.id"

func (dialect) ListHas(field, p string) string {
	if field == "entities" {
		return entityHas + " AND e.key = " + p + ")"
	}
	return "EXISTS (SELECT 1 FROM json_each(m." + field + ") WHERE json_each.value = " + p + ")"
}

func (dialect) ListPresent(field string) string {
	if field == "entities" {
		return entityHas + ")"
	}
	return "json_array_length(m." + field + ") > 0"
}
