package store

import (
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Admits reports whether m satisfies the temporal and expiry conditions of q
// at time now (the filter is checked separately). Connectors that evaluate
// queries in Go, and the engine when re-checking external index hits, use it
// so every backend shares one definition of "current".
func (q *Query) Admits(m *memory.Memory, now time.Time) bool {
	if !q.ShowExpired && m.ExpiresAt != nil && !m.ExpiresAt.After(now) {
		return false
	}
	if q.AllVersions {
		return true
	}
	valid, known := q.ValidAt, q.KnownAt
	if valid.IsZero() {
		valid = now
	}
	if known.IsZero() {
		known = now
	}
	if m.RecordedAt.After(known) || (m.InvalidatedAt != nil && !m.InvalidatedAt.After(known)) {
		return false
	}
	return !m.ValidFrom.After(valid) && (m.ValidTo == nil || m.ValidTo.After(valid) ||
		(m.ClosedAt != nil && known.Before(*m.ClosedAt)))
}

// SlotKey is the uniqueness key of a slot: at most one open memory may hold
// a slot per tenant, user and agent (runs share slots).
func SlotKey(m *memory.Memory) string {
	if m.Slot == "" {
		return ""
	}
	return m.User + "\x00" + m.Agent + "\x00" + m.Slot
}

// Open reports whether a memory version is still open: neither closed nor
// invalidated. Only open versions can be closed or superseded.
func Open(m *memory.Memory) bool { return m.ValidTo == nil && m.InvalidatedAt == nil }
