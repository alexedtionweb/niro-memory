// Package knowledge plans and validates provider-neutral semantic memory
// transitions.
//
// The Deterministic planner handles explicit operations, structured duplicate
// reinforcement, slot updates, conditional branches, empty-input deferral, and
// new non-empty text as ADD. It never interprets free text, infers semantic
// relationships, or emits multiple operations; semantic interpretation and
// richer proposals require an optional Planner implementation. Validate remains
// the deterministic authorization and reference-checking boundary.
//
// AdmissionDecider is the bounded, provider-neutral admission interface for
// asynchronous callers:
//
//	DecideAdmission(context.Context, AdmissionInput) (AdmissionResult, error)
//
// AdmissionDeterministic is the default policy. It processes non-empty input,
// skips empty non-explicit input, always processes explicit requests, observes
// context cancellation, and enforces a 32 KiB default text limit (configurable
// up to 1 MiB).
package knowledge
