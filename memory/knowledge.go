package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// EpistemicType describes the kind of claim represented by an assertion.
// It is orthogonal to Memory.Kind.
type EpistemicType string

const (
	EpistemicFact        EpistemicType = "fact"
	EpistemicObservation EpistemicType = "observation"
	EpistemicBelief      EpistemicType = "belief"
	EpistemicHypothesis  EpistemicType = "hypothesis"
	EpistemicDecision    EpistemicType = "decision"
	EpistemicPreference  EpistemicType = "preference"
)

// Assertion is the optional structured meaning of a memory.
type Assertion struct {
	Subject       string            `json:"subject"`
	Predicate     string            `json:"predicate"`
	Object        string            `json:"object"`
	EpistemicType EpistemicType     `json:"epistemic_type"`
	Conditions    map[string]string `json:"conditions,omitempty"`
	Confidence    float64           `json:"confidence"`
	EvidenceIDs   []string          `json:"evidence_ids,omitempty"`
}

const (
	DecisionProposed   = "proposed"
	DecisionActive     = "active"
	DecisionSuperseded = "superseded"
	DecisionRetracted  = "retracted"
	DecisionUnresolved = "unresolved"
)

// DecisionMetadata stores decision context without duplicating memory fields.
type DecisionMetadata struct {
	Subject           string   `json:"subject"`
	Outcome           string   `json:"outcome"`
	Rationale         []string `json:"rationale,omitempty"`
	Alternatives      []string `json:"alternatives,omitempty"`
	Constraints       []string `json:"constraints,omitempty"`
	EvidenceIDs       []string `json:"evidence_ids,omitempty"`
	RevisitConditions []string `json:"revisit_conditions,omitempty"`
	Authority         string   `json:"authority,omitempty"`
	Status            string   `json:"status"`
}

const (
	AssertionMetadataKey = "assertion"
	DecisionMetadataKey  = "decision"
)

// ReadAssertion reads and validates the optional structured assertion.
func ReadAssertion(m Memory) (*Assertion, error) {
	raw, ok := m.Metadata[AssertionMetadataKey]
	if !ok || raw == nil {
		return nil, nil
	}
	var assertion Assertion
	if err := decodeKnowledgeMetadata(raw, &assertion); err != nil {
		return nil, fmt.Errorf("memory: decode assertion: %w", err)
	}
	if err := validateAssertion(assertion); err != nil {
		return nil, err
	}
	return &assertion, nil
}

// ReadDecision reads and validates optional decision metadata.
func ReadDecision(m Memory) (*DecisionMetadata, error) {
	raw, ok := m.Metadata[DecisionMetadataKey]
	if !ok || raw == nil {
		return nil, nil
	}
	var decision DecisionMetadata
	if err := decodeKnowledgeMetadata(raw, &decision); err != nil {
		return nil, fmt.Errorf("memory: decode decision: %w", err)
	}
	if err := validateDecision(decision); err != nil {
		return nil, err
	}
	return &decision, nil
}

// WithKnowledgeMetadata returns a copy of meta with supplied knowledge values
// stored under reserved keys. A nil value leaves that key unchanged.
func WithKnowledgeMetadata(meta map[string]any, assertion *Assertion, decision *DecisionMetadata) (map[string]any, error) {
	out := make(map[string]any, len(meta)+2)
	for key, value := range meta {
		copy, err := cloneMetadataValue(value)
		if err != nil {
			return nil, fmt.Errorf("memory: clone metadata %q: %w", key, err)
		}
		out[key] = copy
	}
	if assertion != nil {
		if err := validateAssertion(*assertion); err != nil {
			return nil, err
		}
		copy := *assertion
		copy.EvidenceIDs = append([]string(nil), assertion.EvidenceIDs...)
		copy.Conditions = make(map[string]string, len(assertion.Conditions))
		for key, value := range assertion.Conditions {
			copy.Conditions[key] = value
		}
		out[AssertionMetadataKey] = copy
	}
	if decision != nil {
		if err := validateDecision(*decision); err != nil {
			return nil, err
		}
		copy := *decision
		copy.Rationale = append([]string(nil), decision.Rationale...)
		copy.Alternatives = append([]string(nil), decision.Alternatives...)
		copy.Constraints = append([]string(nil), decision.Constraints...)
		copy.EvidenceIDs = append([]string(nil), decision.EvidenceIDs...)
		copy.RevisitConditions = append([]string(nil), decision.RevisitConditions...)
		out[DecisionMetadataKey] = copy
	}
	return out, nil
}

func cloneMetadataValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var copy any
	if err := decoder.Decode(&copy); err != nil {
		return nil, err
	}
	return copy, nil
}

// ValidateKnowledge validates optional knowledge metadata and the memory's
// temporal interval. Text-only memories remain valid.
func ValidateKnowledge(m *Memory) error {
	if m == nil {
		return fmt.Errorf("%w: nil memory", ErrInvalid)
	}
	if m.ValidTo != nil && m.ValidTo.Before(m.ValidFrom) {
		return invalid("valid_to before valid_from")
	}
	assertion, err := ReadAssertion(*m)
	if err != nil {
		return err
	}
	if assertion != nil {
		for _, evidenceID := range assertion.EvidenceIDs {
			if strings.TrimSpace(evidenceID) == "" {
				return invalid("assertion contains empty evidence reference")
			}
		}
	}
	_, err = ReadDecision(*m)
	return err
}

// KnowledgeHash hashes canonical assertion and decision metadata when present.
// For text-only memories it deliberately retains the original text-only Hash
// behavior.
func KnowledgeHash(m *Memory) string {
	if m == nil {
		return Hash("")
	}
	assertion, err := ReadAssertion(*m)
	if err != nil {
		return Hash(m.Text)
	}
	decision, err := ReadDecision(*m)
	if err != nil || (assertion == nil && decision == nil) {
		return Hash(m.Text)
	}
	var canonicalAssert *canonicalAssertion
	if assertion != nil {
		conditions := make([][2]string, 0, len(assertion.Conditions))
		for key, value := range assertion.Conditions {
			conditions = append(conditions, [2]string{Normalize(key), Normalize(value)})
		}
		sort.Slice(conditions, func(i, j int) bool {
			if conditions[i][0] == conditions[j][0] {
				return conditions[i][1] < conditions[j][1]
			}
			return conditions[i][0] < conditions[j][0]
		})
		canonicalAssert = &canonicalAssertion{
			Subject: Normalize(assertion.Subject), Predicate: Normalize(assertion.Predicate),
			Object: Normalize(assertion.Object), EpistemicType: assertion.EpistemicType,
			Conditions: conditions,
		}
	}
	canonicalDecision := canonicalizeDecision(decision)
	canonical := struct {
		Assertion *canonicalAssertion `json:"assertion,omitempty"`
		Decision  *DecisionMetadata   `json:"decision,omitempty"`
	}{
		Assertion: canonicalAssert,
		Decision:  canonicalDecision,
	}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16])
}

type canonicalAssertion struct {
	Subject       string        `json:"subject"`
	Predicate     string        `json:"predicate"`
	Object        string        `json:"object"`
	EpistemicType EpistemicType `json:"epistemic_type"`
	Conditions    [][2]string   `json:"conditions,omitempty"`
}

func canonicalizeDecision(decision *DecisionMetadata) *DecisionMetadata {
	if decision == nil {
		return nil
	}
	canonical := *decision
	canonical.Subject = Normalize(canonical.Subject)
	canonical.Outcome = Normalize(canonical.Outcome)
	canonical.Authority = Normalize(canonical.Authority)
	canonical.Rationale = normalizeList(canonical.Rationale)
	canonical.Alternatives = normalizeList(canonical.Alternatives)
	canonical.Constraints = normalizeList(canonical.Constraints)
	canonical.RevisitConditions = normalizeList(canonical.RevisitConditions)
	canonical.EvidenceIDs = nil
	return &canonical
}

func normalizeList(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = Normalize(value)
	}
	return out
}

func decodeKnowledgeMetadata(value any, out any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func validateAssertion(a Assertion) error {
	if strings.TrimSpace(a.Subject) == "" {
		return invalid("assertion subject is empty")
	}
	if strings.TrimSpace(a.Predicate) == "" {
		return invalid("assertion predicate is empty")
	}
	if len(a.Subject) > 4096 || len(a.Predicate) > 4096 || len(a.Object) > 4096 {
		return invalid("assertion field exceeds 4096 bytes")
	}
	switch a.EpistemicType {
	case EpistemicFact, EpistemicObservation, EpistemicBelief, EpistemicHypothesis, EpistemicDecision, EpistemicPreference:
	default:
		return invalid("unknown epistemic type %q", a.EpistemicType)
	}
	if math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) || a.Confidence < 0 || a.Confidence > 1 {
		return invalid("assertion confidence %v outside [0,1]", a.Confidence)
	}
	if len(a.Conditions) > 64 {
		return invalid("assertion has %d conditions, limit 64", len(a.Conditions))
	}
	for key, value := range a.Conditions {
		if strings.TrimSpace(key) == "" || len(key) > 256 || len(value) > 1024 {
			return invalid("invalid assertion condition")
		}
	}
	if len(a.EvidenceIDs) > 256 {
		return invalid("assertion has %d evidence references, limit 256", len(a.EvidenceIDs))
	}
	for _, id := range a.EvidenceIDs {
		if strings.TrimSpace(id) == "" || len(id) > 1024 {
			return invalid("invalid assertion evidence reference")
		}
	}
	return nil
}

func validateDecision(d DecisionMetadata) error {
	if strings.TrimSpace(d.Subject) == "" || strings.TrimSpace(d.Outcome) == "" {
		return invalid("decision subject and outcome are required")
	}
	if len(d.Subject) > 4096 || len(d.Outcome) > 4096 || len(d.Authority) > 1024 {
		return invalid("decision field exceeds size limit")
	}
	switch d.Status {
	case DecisionProposed, DecisionActive, DecisionSuperseded, DecisionRetracted, DecisionUnresolved:
	default:
		return invalid("unknown decision status %q", d.Status)
	}
	for _, values := range [][]string{d.Rationale, d.Alternatives, d.Constraints, d.EvidenceIDs, d.RevisitConditions} {
		if len(values) > 256 {
			return invalid("decision list exceeds 256 values")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 4096 {
				return invalid("invalid decision metadata value")
			}
		}
	}
	return nil
}
