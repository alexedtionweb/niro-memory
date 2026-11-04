// Package store defines the contract between the engine and primary-store
// connectors (SQLite, PostgreSQL, or your own).
//
// A connector implements [Store]: one atomic Write of a [Batch], a handful of
// reads, and a leased job [Queue]. Everything else is optional and
// discovered by type assertion — [Lexical], [Entities], [VectorCatalog],
// vector.Index, [Admin], [SQLConsole] — so a backend implements only what it
// can do efficiently and the planner skips the rest.
//
// The tenant is an explicit argument of every call and must be enforced by
// every query. The storetest package verifies this for any connector.
package store

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Store is the required contract of a primary-store connector.
type Store interface {
	Writer
	Reader
	Queue
	io.Closer
}

// Writer applies changes atomically.
type Writer interface {
	// Write applies the whole batch in one transaction or nothing at all.
	// It returns memory.ErrConflict when a conditional change (Close, Cursor,
	// a second current value for a slot) fails, so callers can re-read and retry.
	Write(ctx context.Context, b *Batch) (*Receipt, error)
}

// Reader reads records of one tenant.
type Reader interface {
	// Get returns the memories with the given ids, in any order; unknown ids
	// are skipped. Entities are populated.
	Get(ctx context.Context, tenant string, ids []string) ([]memory.Memory, error)
	// Find returns memories matching q.
	Find(ctx context.Context, q Query) ([]memory.Memory, error)
	// History returns the events of one memory, oldest first.
	History(ctx context.Context, tenant, id string) ([]memory.Event, error)
	// Episodes returns episodes matching q.
	Episodes(ctx context.Context, q EpisodeQuery) ([]memory.Episode, error)
	// Run returns run state or memory.ErrNotFound.
	Run(ctx context.Context, tenant, run string) (memory.Run, error)
	// Settings returns the tenant's settings document, or nil if none.
	Settings(ctx context.Context, tenant string) ([]byte, error)
}

// Batch is a unit of work. Replay is resolved first, then Checks, before any
// domain mutations. Relations follow memory insertion and precede deletion.
type Batch struct {
	Tenant         string
	OperationKey   string
	OperationHash  string
	Result         json.RawMessage
	Checks         []Check
	Relations      []relations.Link
	RelationCloses []relations.Close

	// Memories are inserted. The store resolves EntityRefs to entity rows
	// (creating them by key) and links them. Inserting a current memory whose
	// Slot already has a current value in the same tenant/user/agent fails
	// with memory.ErrConflict.
	Memories []memory.Memory
	// Closes end validity or invalidate existing memories. Applied before
	// Memories so a supersession can close the old slot value and insert the
	// new one in the same batch.
	Closes []Close
	// Touches append evidence and add heat to existing memories (dedup hits).
	Touches []Touch
	// Deletes hard-delete memories with their entity links and vectors.
	Deletes []string
	// Events are appended to memory histories.
	Events []memory.Event

	// Episodes are appended to their runs. The store assigns Seq and ID.
	// An episode whose (run, ClientMsgID) already exists is not inserted;
	// the Receipt returns the stored one with Duplicate set.
	Episodes []memory.Episode
	// EpisodeEdits change status and/or parts of existing episodes.
	EpisodeEdits []EpisodeEdit
	// DeleteRuns hard-deletes runs with all their episodes.
	DeleteRuns []string
	// Cursors advance run processing cursors with compare-and-set.
	Cursors []Cursor
	// Summaries replace run summaries.
	Summaries []Summary

	// Vectors are upserted (the store is the source of truth for vectors).
	Vectors []vector.Point
	// VectorDeletes remove vectors of targets, for every model.
	VectorDeletes []vector.Target

	// Aliases attach alias names to entities (entity resolution/merging).
	Aliases []Alias

	// Jobs are enqueued. A job whose Key matches a pending job of the same
	// tenant and kind is merged into it: the pending job keeps its payload
	// and takes the later NotBefore (debounce).
	Jobs []Job

	// Settings, when non-nil, replaces the tenant's settings document.
	Settings []byte
}

// Empty reports whether the batch changes nothing.
func (b *Batch) Empty() bool {
	return len(b.Memories)+len(b.Closes)+len(b.Touches)+len(b.Deletes)+len(b.Events)+
		len(b.Episodes)+len(b.EpisodeEdits)+len(b.DeleteRuns)+len(b.Cursors)+len(b.Summaries)+
		len(b.Vectors)+len(b.VectorDeletes)+len(b.Aliases)+len(b.Jobs)+len(b.Relations)+len(b.RelationCloses)+len(b.Checks) == 0 && b.Settings == nil && b.OperationKey == ""
}

// Check rejects a stale plan before any mutations. Open requires an open row.
type Check struct {
	ID      string
	Version int
	Open    bool
}

// Operations reads committed operation receipts, or memory.ErrNotFound.
type Operations interface {
	Operation(context.Context, string, string) (*Receipt, error)
}

// Close ends a memory version. It fails with memory.ErrConflict unless the
// memory is still open (InvalidatedAt and ValidTo both unset).
type Close struct {
	ID            string
	ValidTo       *time.Time // world change: the fact stopped being true
	InvalidatedAt *time.Time // correction: the fact was never true
	ClosedAt      *time.Time // system time ValidTo was learned; nil uses the store clock
}

// Touch records that an existing memory was seen again.
type Touch struct {
	ID       string
	Evidence []memory.Evidence
	Heat     int64
}

// EpisodeEdit changes an episode. Nil Parts keeps the parts.
type EpisodeEdit struct {
	ID     string
	Status memory.EpisodeStatus
	Parts  []memory.Part
}

// Cursor moves a run's processed sequence from From to To. It fails with
// memory.ErrConflict when the stored value is not From.
type Cursor struct {
	Run      string
	From, To int64
}

// Summary sets the rolling summary of a run.
type Summary struct {
	Run  string
	Text string
	Upto int64
}

// Alias makes Name an alias of the entity with key EntityKey.
type Alias struct {
	EntityKey string
	Name      string
}

// Receipt reports what a Write did.
type Receipt struct {
	Episodes      []EpisodeResult
	Result        json.RawMessage
	Replayed      bool
	OperationHash string
}

// EpisodeResult is the stored form of one appended episode.
type EpisodeResult struct {
	Episode   memory.Episode
	Duplicate bool
}

// Query selects memories of one tenant.
type Query struct {
	Tenant string
	// Filter must already include scope and visibility conditions.
	Filter memory.Filter
	// ValidAt and KnownAt select a bi-temporal point of view. Zero means now.
	// A memory matches when it was true at ValidAt according to what the
	// engine knew at KnownAt.
	ValidAt, KnownAt time.Time
	// AllVersions disables the temporal condition (history views).
	AllVersions bool
	// ShowExpired includes memories past ExpiresAt.
	ShowExpired bool
	Order       Order
	Limit       int
	Offset      int
}

// Order is a result ordering.
type Order string

// Orderings.
const (
	NewestFirst  Order = ""
	OldestFirst  Order = "oldest"
	HottestFirst Order = "heat"
)

// EpisodeQuery selects episodes of one tenant.
type EpisodeQuery struct {
	Tenant         string
	IDs            []string
	Run            string
	User, Agent    string
	Roles          []string
	AfterSeq       int64 // exclusive
	BeforeSeq      int64 // exclusive; 0 = no bound
	Since, Until   time.Time
	BlobSHA        string // episodes referencing this blob
	IncludeDeleted bool
	Desc           bool // newest first
	Limit          int
}

// Lexical is implemented by stores with full-text search.
type Lexical interface {
	// SearchMemories ranks memories matching q.Query by text relevance.
	SearchMemories(ctx context.Context, q TextQuery) ([]Hit, error)
	// SearchEpisodes ranks episodes by text relevance.
	SearchEpisodes(ctx context.Context, q EpisodeTextQuery) ([]EpisodeHit, error)
}

// TextQuery is a full-text search over memories.
type TextQuery struct {
	Query
	// Terms are normalized search terms (see textutil.Terms). Stores match
	// any term and rank by relevance. Terms of PrefixMin or more characters
	// also match words they prefix ("live" matches "lives"), a
	// language-neutral approximation of stemming.
	Terms []string
}

// EpisodeTextQuery is a full-text search over episodes.
type EpisodeTextQuery struct {
	EpisodeQuery
	Terms []string
}

// PrefixMin is the minimum term length (in characters) for prefix matching.
const PrefixMin = 4

// Hit is a ranked memory id. Score is the raw backend score, higher is
// better; for BM25 backends it is the (positive) BM25 score.
type Hit struct {
	ID    string
	Score float64
}

// EpisodeHit is a ranked episode.
type EpisodeHit struct {
	Episode memory.Episode
	Score   float64
}

// Entities is implemented by stores that index entities.
type Entities interface {
	// FindEntities returns entities whose key or an alias key is in keys.
	FindEntities(ctx context.Context, tenant string, keys []string) ([]memory.Entity, error)
	// EntityMemories returns links between the entities and memories, at
	// most limit per entity, most recent first. Mentions on the returned
	// entities count all links.
	EntityMemories(ctx context.Context, tenant string, entityIDs []string, limit int) ([]Link, error)
	// ListEntities lists entities whose key starts with prefix.
	ListEntities(ctx context.Context, tenant, prefix string, limit int) ([]memory.Entity, error)
}

// Link connects an entity and a memory.
type Link struct {
	EntityID, MemoryID string
}

// VectorCatalog is implemented by stores that persist vectors.
type VectorCatalog interface {
	// Vectors pages through stored points of a model (key > after).
	Vectors(ctx context.Context, tenant, model, after string, limit int) ([]vector.Point, error)
	// Missing lists memory targets that have no vector for model.
	Missing(ctx context.Context, tenant, model string, limit int) ([]vector.Target, error)
	// Coverage counts current memories with and without a vector for model.
	Coverage(ctx context.Context, tenant, model string) (have, total int, err error)
}

// Admin is implemented by stores that can describe themselves (dev console).
type Admin interface {
	Tenants(ctx context.Context) ([]TenantStats, error)
}

// TenantStats summarizes one tenant.
type TenantStats struct {
	Tenant   string `json:"tenant"`
	Memories int    `json:"memories"`
	Episodes int    `json:"episodes"`
	Entities int    `json:"entities"`
	Jobs     int    `json:"jobs"`
}

// SQLConsole is implemented by SQL stores to run read-only queries for the
// dev console. Implementations must make writes impossible, not just unlikely.
type SQLConsole interface {
	QueryReadOnly(ctx context.Context, sql string, maxRows int) (cols []string, rows [][]any, err error)
}

// Capabilities describes fine-grained features for the planner and docs.
type Capabilities struct {
	BM25 string `json:"bm25"` // "exact", "approx" or "" (none)
}

// Describer is implemented by stores that report Capabilities.
type Describer interface {
	Capabilities() Capabilities
}

// JobState is the lifecycle state of a job.
type JobState string

// Job states. Completed jobs are deleted.
const (
	JobPending JobState = "pending"
	JobLeased  JobState = "leased"
	JobDead    JobState = "dead"
)

// Job is a durable unit of background work.
type Job struct {
	ID         string          `json:"id"`
	Tenant     string          `json:"tenant"`
	Kind       string          `json:"kind"`
	Key        string          `json:"key"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	State      JobState        `json:"state"`
	Attempts   int             `json:"attempts"`
	NotBefore  time.Time       `json:"not_before"`
	LeaseUntil time.Time       `json:"lease_until,omitempty"`
	Worker     string          `json:"worker,omitempty"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Queue is a durable job queue with leases.
type Queue interface {
	// Claim leases up to n due jobs for worker. Claims are fair across
	// tenants, and a job is not claimed while another job with the same
	// tenant, kind and key holds a live lease.
	Claim(ctx context.Context, worker string, n int, lease time.Duration) ([]Job, error)
	// Heartbeat extends a lease, saving checkpoint when non-nil. It fails
	// with memory.ErrConflict if the worker no longer holds the lease.
	Heartbeat(ctx context.Context, id, worker string, lease time.Duration, checkpoint json.RawMessage) error
	// Finish ends a leased job: done deletes it, retry makes it pending at
	// r.RetryAt, dead parks it for inspection.
	Finish(ctx context.Context, id, worker string, r Outcome) error
	// Jobs lists jobs for inspection.
	Jobs(ctx context.Context, q JobQuery) ([]Job, error)
}

// Outcome is the result of running a job.
type Outcome struct {
	Done    bool
	Dead    bool
	RetryAt time.Time
	Error   string
}

// JobQuery filters jobs for inspection. Empty fields match all.
type JobQuery struct {
	Tenant string
	State  JobState
	Kind   string
	Key    string
	Limit  int
}
