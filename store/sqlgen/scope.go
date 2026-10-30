package sqlgen

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Scope tokens let a full-text index narrow matches to one tenant, user or
// run inside the index itself (an AND of posting lists), instead of matching
// every tenant's documents and filtering rows afterwards. Tokens are opaque
// hashes made of letters and digits, so every tokenizer keeps them whole and
// they never collide with words.

func token(kind string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "zz" + kind + hex.EncodeToString(sum[:8])
}

// TenantToken identifies a tenant's documents.
func TenantToken(tenant string) string { return token("t", tenant) }

// UserToken identifies a user's documents inside a tenant ("" is a user).
func UserToken(tenant, user string) string { return token("u", tenant, user) }

// RunToken identifies a run's episodes inside a tenant.
func RunToken(tenant, run string) string { return token("r", tenant, run) }

// MemoryTokens returns the scope tokens stored with a memory.
func MemoryTokens(m *memory.Memory) string {
	return TenantToken(m.Tenant) + " " + UserToken(m.Tenant, m.User)
}

// EpisodeTokens returns the scope tokens stored with an episode.
func EpisodeTokens(tenant string, e *memory.Episode) string {
	return TenantToken(tenant) + " " + UserToken(tenant, e.User) + " " + RunToken(tenant, e.Run)
}

// OwnerUser returns the user a filter is restricted to, when the filter is
// a conjunction containing user_id = X (as the engine builds for scoped
// reads). Such a restriction can be pushed into the text index.
func OwnerUser(f memory.Filter) (string, bool) {
	if f.Field == "user_id" && f.Op == memory.OpEq {
		u, ok := f.Value.(string)
		return u, ok
	}
	for _, sub := range f.And {
		if u, ok := OwnerUser(sub); ok {
			return u, true
		}
	}
	return "", false
}
