package postgres

import (
	"strconv"
	"time"

	"github.com/alexedtionweb/niro-memory/store/sqlgen"
)

// dialect adapts sqlgen to PostgreSQL: timestamptz times, jsonb lists and
// metadata, and an entities join table.
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

func (dialect) Param(n int) string          { return "$" + strconv.Itoa(n) }
func (dialect) Column(f string) string      { return columns[f] }
func (dialect) Time(t time.Time) any        { return utc(t) }
func (dialect) Bool(b bool) any             { return b }
func (dialect) Lower(e string) string       { return "lower(" + e + ")" }
func (dialect) Contains(h, n string) string { return "strpos(" + h + ", " + n + ") > 0" }

// Meta inlines the key, which sqlgen guarantees matches [A-Za-z0-9_-]. The
// value expression is NULL unless the type matches, so casts never fail.
func (dialect) Meta(key string, k sqlgen.Kind) (string, string) {
	ref := "m.metadata->'" + key + "'"
	txt := "(m.metadata->>'" + key + "')"
	switch k {
	case sqlgen.KindNumber:
		g := "jsonb_typeof(" + ref + ") = 'number'"
		return g, "(CASE WHEN " + g + " THEN " + txt + "::float8 END)"
	case sqlgen.KindBool:
		g := "jsonb_typeof(" + ref + ") = 'boolean'"
		return g, "(CASE WHEN " + g + " THEN " + txt + "::boolean END)"
	}
	g := "jsonb_typeof(" + ref + ") = 'string'"
	return g, "(CASE WHEN " + g + " THEN " + txt + " END)"
}

const entityHas = "EXISTS (SELECT 1 FROM memory_entities me JOIN entities e ON e.tenant = me.tenant AND e.id = me.entity_id WHERE me.tenant = m.tenant AND me.memory_id = m.id"

func (dialect) ListHas(field, p string) string {
	if field == "entities" {
		return entityHas + " AND e.key = " + p + ")"
	}
	return "(m." + field + " ? " + p + "::text)"
}

func (dialect) ListPresent(field string) string {
	if field == "entities" {
		return entityHas + ")"
	}
	return "jsonb_array_length(m." + field + ") > 0"
}
