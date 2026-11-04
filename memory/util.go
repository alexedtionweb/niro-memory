package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
)

var idMu sync.Mutex
var idLast int64

// NewID returns a UUIDv7 string: time-ordered (good B-tree locality) and
// globally unique without coordination, so any stateless node can mint IDs.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("memory: crypto/rand failed: " + err.Error())
	}
	// Keep IDs strictly increasing within a process even inside one millisecond.
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= idLast {
		ms = idLast + 1
	}
	idLast = ms
	idMu.Unlock()
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(ms))
	copy(b[0:6], t[2:8])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Normalize trims text, removes control characters and collapses runs of
// whitespace into one space. It is the canonical form used for hashing.
func Normalize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = sb.Len() > 0
		case unicode.IsControl(r):
		default:
			if space {
				sb.WriteByte(' ')
				space = false
			}
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// Hash returns the deduplication hash of a memory text: the first 16 bytes
// of SHA-256 over the lower-cased normalized text, hex encoded.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(Normalize(text))))
	return hex.EncodeToString(sum[:16])
}

// EntityKey normalizes an entity name for matching: lower case, letters and
// digits only, single spaces. "  Acme-Corp, Inc. " becomes "acme corp inc".
func EntityKey(name string) string {
	var sb strings.Builder
	space := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			space = false
			sb.WriteRune(r)
			continue
		}
		space = true
	}
	return sb.String()
}

// Current reports whether m is believed true at time t: not invalidated,
// inside its validity window, and not expired.
func (m *Memory) Current(t time.Time) bool {
	if m.InvalidatedAt != nil && !m.InvalidatedAt.After(t) {
		return false
	}
	if m.ValidFrom.After(t) || (m.ValidTo != nil && !m.ValidTo.After(t)) {
		return false
	}
	return m.ExpiresAt == nil || m.ExpiresAt.After(t)
}

// Text returns all searchable text of an episode: part texts, captions,
// derived texts and file names, separated by newlines.
func (e *Episode) Text() string {
	var sb strings.Builder
	for _, p := range e.Parts {
		for _, s := range [...]string{p.Text, p.Caption, p.DerivedText, p.Name} {
			if s == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// Errors returned by validation. Wrap-compatible with errors.Is.
var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Limits bound the size of what callers may write.
type Limits struct {
	MaxTextBytes  int // per memory text and per episode part text
	MaxTags       int
	MaxParts      int
	MaxPartBytes  int64 // per binary part
	MaxMetaBytes  int   // encoded metadata
	MaxTurnsBatch int
}

// DefaultLimits are conservative defaults suitable for chat agents.
var DefaultLimits = Limits{
	MaxTextBytes:  32 << 10,
	MaxTags:       32,
	MaxParts:      32,
	MaxPartBytes:  20 << 20,
	MaxMetaBytes:  16 << 10,
	MaxTurnsBatch: 256,
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// Validate checks a memory before it is written. It never mutates m.
func (m *Memory) Validate(l Limits) error {
	switch {
	case m.Tenant == "":
		return invalid("tenant is required")
	case strings.TrimSpace(m.Text) == "":
		return invalid("text is empty")
	case len(m.Text) > l.MaxTextBytes:
		return invalid("text is %d bytes, limit %d", len(m.Text), l.MaxTextBytes)
	case len(m.Tags) > l.MaxTags:
		return invalid("%d tags, limit %d", len(m.Tags), l.MaxTags)
	case math.IsNaN(m.Confidence) || math.IsInf(m.Confidence, 0) || m.Confidence < 0 || m.Confidence > 1:
		return invalid("confidence %v outside [0,1]", m.Confidence)
	case math.IsNaN(m.Importance) || math.IsInf(m.Importance, 0) || m.Importance < 0 || m.Importance > 1:
		return invalid("importance %v outside [0,1]", m.Importance)
	case m.Layer < Atom || m.Layer > Profile:
		return invalid("layer %d outside 1..3", m.Layer)
	case m.ValidTo != nil && m.ValidTo.Before(m.ValidFrom):
		return invalid("valid_to before valid_from")
	}
	switch m.Kind {
	case Working, Episodic, Semantic, Procedural:
	default:
		return invalid("unknown kind %q", m.Kind)
	}
	switch m.Visibility {
	case Team, Private, Restricted:
	default:
		return invalid("unknown visibility %q", m.Visibility)
	}
	for _, g := range m.Grants {
		if k, v, ok := strings.Cut(g, ":"); !ok || v == "" || (k != "user" && k != "agent" && k != "role") {
			return invalid("grant %q must be user:<id>, agent:<id> or role:<name>", g)
		}
	}
	return ValidateKnowledge(m)
}
