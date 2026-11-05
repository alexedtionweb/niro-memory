package store

import (
	"context"
	"fmt"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Bounds for TargetVectors lookups.
const (
	// MaxVectorTargets caps distinct targets per TargetVectors call.
	MaxVectorTargets = 100
	// MaxTargetVectors caps the points returned across all models.
	MaxTargetVectors = 1000
)

// ErrTooManyTargetVectors is returned when explicit targets match more than
// MaxTargetVectors stored points; callers should request fewer targets.
var ErrTooManyTargetVectors = fmt.Errorf("%w: targets match more than %d vectors", memory.ErrInvalid, MaxTargetVectors)

// VectorTargets is an optional Store capability that reads persisted vectors
// of explicit targets across all models without scanning the tenant.
// Results are ordered by target key, then model. An empty target list returns
// no results.
type VectorTargets interface {
	TargetVectors(ctx context.Context, tenant string, targets []vector.Target) ([]vector.Point, error)
}

// ValidateVectorTargets checks a TargetVectors request: a tenant, at most
// MaxVectorTargets distinct targets, and well-formed Kind/ID/Part.
func ValidateVectorTargets(tenant string, targets []vector.Target) error {
	if tenant == "" {
		return fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	seen := make(map[string]struct{}, min(len(targets), MaxVectorTargets+1))
	for _, t := range targets {
		switch {
		case t.ID == "":
			return fmt.Errorf("%w: vector target id is required", memory.ErrInvalid)
		case t.Kind == vector.TargetMemory && t.Part != 0:
			return fmt.Errorf("%w: memory vector target %q has a part", memory.ErrInvalid, t.ID)
		case t.Kind == vector.TargetPart && t.Part < 0:
			return fmt.Errorf("%w: vector target %q has a negative part", memory.ErrInvalid, t.ID)
		case t.Kind != vector.TargetMemory && t.Kind != vector.TargetPart:
			return fmt.Errorf("%w: unknown vector target kind %q", memory.ErrInvalid, t.Kind)
		}
		seen[t.Key()] = struct{}{}
		if len(seen) > MaxVectorTargets {
			return fmt.Errorf("%w: more than %d distinct vector targets", memory.ErrInvalid, MaxVectorTargets)
		}
	}
	return nil
}
