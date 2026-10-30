// Package memory defines the domain model of the memory engine.
//
// Every logical memory category (working, episodic, semantic, procedural)
// and every distillation layer (atom, scene, profile) is one record type:
// [Memory]. Raw conversation turns, events and tool outputs are [Episode]s.
//
// Memories are bi-temporal: ValidFrom/ValidTo say when a fact is true in the
// world, RecordedAt/InvalidatedAt say when the engine believed it. Nothing is
// overwritten; corrections and changes create new versions.
//
// The package has no dependencies beyond the standard library and knows
// nothing about storage, HTTP or AI.
package memory
