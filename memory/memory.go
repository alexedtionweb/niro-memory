package memory

import (
	"encoding/json"
	"time"
)

// Kind is the logical memory category.
type Kind string

// Memory kinds. They are labels on one record type, not separate stores.
const (
	Working    Kind = "working"    // short-lived task context; usually has ExpiresAt
	Episodic   Kind = "episodic"   // something that happened
	Semantic   Kind = "semantic"   // a fact, preference or relationship
	Procedural Kind = "procedural" // a workflow, strategy or skill
)

// Layer is the distillation level of a memory (L0 is the raw [Episode]).
type Layer int

// Distillation layers.
const (
	Atom    Layer = 1 // one fact, preference, constraint or event
	Scene   Layer = 2 // a block of knowledge about a topic or project
	Profile Layer = 3 // long-term profile of a user, agent or team
)

// Visibility controls who may read a memory inside its tenant.
type Visibility string

// Visibility levels.
const (
	Team       Visibility = "team"       // any principal of the tenant (default)
	Private    Visibility = "private"    // only the owning user and/or agent
	Restricted Visibility = "restricted" // owner plus principals listed in Grants
)

// Trust is how much the engine trusts the origin of a memory.
// Retrieved memories are always data, never instructions; trust only labels them.
type Trust string

// Trust levels.
const (
	TrustHigh   Trust = "high"   // written explicitly by an authenticated agent or operator
	TrustNormal Trust = "normal" // derived from user or assistant turns
	TrustLow    Trust = "low"    // tool output, external content, or flagged as suspicious
)

// Scope says who owns a record. Tenant is mandatory and is a hard isolation
// boundary; User, Agent and Run narrow ownership inside the tenant.
type Scope struct {
	Tenant string `json:"tenant,omitempty"`
	User   string `json:"user_id,omitempty"`
	Agent  string `json:"agent_id,omitempty"`
	Run    string `json:"run_id,omitempty"`
}

// Memory is the single record type for every kind and layer of memory.
//
// Records are append-only: a change in the world closes ValidTo of the old
// version, a correction sets InvalidatedAt, and the new version points back
// through Supersedes. Only Delete removes data.
type Memory struct {
	ID string `json:"id"`
	Scope

	Kind  Kind   `json:"kind"`
	Layer Layer  `json:"layer"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`

	// Subject, Predicate and Object optionally describe the memory as a
	// relationship ("user" "lives_in" "Lisbon"). Subject and Object are names
	// of entities; such memories are the edges of the entity graph.
	Subject   string `json:"subject,omitempty"`
	Predicate string `json:"predicate,omitempty"`
	Object    string `json:"object,omitempty"`

	// Slot names a single-valued property ("user.location"). Writing a new
	// value to the same slot in the same scope supersedes the current one.
	Slot string `json:"slot,omitempty"`

	// Hash is the content hash used for deduplication. Set by the engine.
	Hash     string         `json:"hash,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Entities []EntityRef    `json:"entities,omitempty"`

	Confidence float64 `json:"confidence"`
	Importance float64 `json:"importance"`
	Heat       int64   `json:"heat"`

	Visibility Visibility `json:"visibility"`
	// Grants lists extra readers of a Restricted memory as "user:<id>",
	// "agent:<id>" or "role:<name>".
	Grants []string `json:"grants,omitempty"`

	ValidFrom time.Time  `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
	// ClosedAt is when the end of validity was learned, not when it happened.
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	RecordedAt    time.Time  `json:"recorded_at"`
	InvalidatedAt *time.Time `json:"invalidated_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`

	Version    int    `json:"version"`
	Supersedes string `json:"supersedes,omitempty"`

	Source Source `json:"source"`
}

// Source is the provenance of a memory.
type Source struct {
	// Kind is "explicit", "rules", "import" or "ext:<name>".
	Kind  string `json:"kind"`
	Actor string `json:"actor,omitempty"` // who wrote or said it
	Role  string `json:"role,omitempty"`  // role of the originating turn, if any
	Trust Trust  `json:"trust"`
	// DerivedBy identifies the exact derivation, e.g. "rules@3f2a9c1b", so
	// memories can be re-derived when rules change.
	DerivedBy string     `json:"derived_by,omitempty"`
	Evidence  []Evidence `json:"evidence,omitempty"`
	// Flags carries ingestion warnings such as "instruction-like".
	Flags []string `json:"flags,omitempty"`
}

// Evidence points at the exact place a memory came from.
type Evidence struct {
	Episode string `json:"episode"`
	Part    int    `json:"part"`
	Start   int    `json:"start"` // byte offsets in the part's text
	End     int    `json:"end"`
}

// EntityRef names an entity mentioned by a memory.
type EntityRef struct {
	Type string `json:"type,omitempty"`
	Name string `json:"name"`
}

// Entity is a resolved entity. Key is the normalized name used for matching.
type Entity struct {
	ID       string   `json:"id"`
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Type     string   `json:"type,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
	Mentions int64    `json:"mentions"`
}

// Action is a lifecycle event recorded in a memory's history.
type Action string

// History actions.
const (
	ActionAdd        Action = "add"
	ActionUpdate     Action = "update"     // corrected; old version invalidated
	ActionSupersede  Action = "supersede"  // world changed; old version closed
	ActionInvalidate Action = "invalidate" // validity ended without a replacement
	ActionEvidence   Action = "evidence"   // same fact seen again
	ActionDelete     Action = "delete"
)

// Event is one entry of a memory's history.
type Event struct {
	MemoryID string    `json:"memory_id"`
	Action   Action    `json:"action"`
	Old      string    `json:"old,omitempty"`
	New      string    `json:"new,omitempty"`
	Related  string    `json:"related,omitempty"` // the other version involved
	Actor    string    `json:"actor,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// Episode is one raw turn, event or tool output (layer L0). Episodes are the
// evidence every derived memory points back to.
type Episode struct {
	ID string `json:"id"`
	Scope
	Seq         int64          `json:"seq"`
	ClientMsgID string         `json:"client_msg_id,omitempty"`
	Role        string         `json:"role"` // user, assistant, system, tool, event
	Actor       string         `json:"actor,omitempty"`
	OccurredAt  time.Time      `json:"occurred_at"`
	RecordedAt  time.Time      `json:"recorded_at"`
	Status      EpisodeStatus  `json:"status"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// EpisodeStatus tracks edits and deletions of turns.
type EpisodeStatus string

// Episode statuses.
const (
	Final   EpisodeStatus = "final"
	Edited  EpisodeStatus = "edited"
	Deleted EpisodeStatus = "deleted"
)

// PartKind is the content type of an episode part. It mirrors niro.Part.
type PartKind string

// Part kinds.
const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartAudio      PartKind = "audio"
	PartVideo      PartKind = "video"
	PartFile       PartKind = "file"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	PartData       PartKind = "data"
)

// Part is one piece of an episode. Binary media live in a blob store and are
// referenced by Blob; Caption carries a caller-supplied caption or transcript
// and DerivedText is filled later by a describer extension.
type Part struct {
	Kind        PartKind        `json:"kind"`
	Text        string          `json:"text,omitempty"`
	Mime        string          `json:"mime,omitempty"`
	Blob        *BlobRef        `json:"blob,omitempty"`
	URL         string          `json:"url,omitempty"`
	Name        string          `json:"name,omitempty"` // file or tool name
	Caption     string          `json:"caption,omitempty"`
	DerivedText string          `json:"derived_text,omitempty"`
	DerivedBy   string          `json:"derived_by,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"` // tool args/results, structured events
	ToolCallID  string          `json:"tool_call_id,omitempty"`
}

// BlobRef is a content-addressed pointer to binary data.
type BlobRef struct {
	SHA256 string `json:"sha256"`
	Mime   string `json:"mime,omitempty"`
	Size   int64  `json:"size"`
}

// Run is the state of one conversation or task run.
type Run struct {
	Scope
	LastSeq      int64     `json:"last_seq"`
	ProcessedSeq int64     `json:"processed_seq"`
	Summary      string    `json:"summary,omitempty"`
	SummaryUpto  int64     `json:"summary_upto,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Principal is the identity a read is performed for. It drives visibility
// checks; the tenant itself is enforced separately by every store call.
type Principal struct {
	User  string   `json:"user_id,omitempty"`
	Agent string   `json:"agent_id,omitempty"`
	Roles []string `json:"roles,omitempty"`
	// Admin bypasses visibility rules (operators, migrations, the dev console).
	Admin bool `json:"admin,omitempty"`
}
