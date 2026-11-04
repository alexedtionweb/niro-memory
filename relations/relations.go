// Package relations defines provider-neutral, inferred links between memories.
// Inferred links are annotations, never authoritative facts or retrieval rules.
package relations

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Input is a bounded, single-owner set of memories the provider may analyze.
type Input struct {
	Source     memory.Memory   `json:"source"`
	Candidates []memory.Memory `json:"candidates"`
}

// Proposal identifies a link to a supplied candidate, not an arbitrary entity.
type Proposal struct {
	Target     string  `json:"target_id"`
	Relation   string  `json:"relation"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}

// Inferer is implemented by optional connectors or application-owned models.
type Inferer interface {
	Name() string
	Infer(context.Context, Input) ([]Proposal, error)
}

// Link is a derived annotation supported by two immutable memory IDs.
type Link struct {
	ID            string            `json:"id"`
	Tenant        string            `json:"tenant"`
	Source        string            `json:"source_id"`
	Target        string            `json:"target_id"`
	Relation      string            `json:"relation"`
	Reason        string            `json:"reason"`
	Confidence    float64           `json:"confidence"`
	Provider      string            `json:"provider"`
	At            time.Time         `json:"at"`
	Evidence      []memory.Evidence `json:"evidence,omitempty"`
	ValidFrom     time.Time         `json:"valid_from"`
	ValidTo       *time.Time        `json:"valid_to,omitempty"`
	InvalidatedAt *time.Time        `json:"invalidated_at,omitempty"`
	OperationKey  string            `json:"operation_key,omitempty"`
	Origin        string            `json:"origin,omitempty"`
}

// Close records the system time at which a link ceased to be current.
type Close struct {
	ID string
	At time.Time
}

// MaxReadLinks is the fixed history-read bound, newest assertions first.
const MaxReadLinks = 4000

// Store reads historical links mentioning any supplied anchor, including
// system-closed links, newest first and capped at MaxReadLinks.
type Store interface {
	RelationshipSets(context.Context, string, []string) ([]Link, error)
}

// ValidRelation restricts model output to supported, explicitly labeled types.
func ValidRelation(value string) bool {
	switch value {
	case "supports", "contradicts", "extends", "depends_on", "derived_from", "related_to":
		return true
	}
	return false
}
