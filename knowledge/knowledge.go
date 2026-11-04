// Package knowledge provides provider-neutral planning and deterministic
// validation for semantic memory transitions.
package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
)

type OperationType string

const (
	ADD       OperationType = "ADD"
	REINFORCE OperationType = "REINFORCE"
	REFINE    OperationType = "REFINE"
	SUPERSEDE OperationType = "SUPERSEDE"
	CORRECT   OperationType = "CORRECT"
	RETRACT   OperationType = "RETRACT"
	BRANCH    OperationType = "BRANCH"
	RELATE    OperationType = "RELATE"
	DEFER     OperationType = "DEFER"
	IGNORE    OperationType = "IGNORE"
)

// Input is one proposed knowledge transition. Intent is explicit caller input;
// free text is never interpreted as an instruction by the deterministic planner.
type Input struct {
	OperationKey string                   `json:"operation_key"`
	Scope        memory.Scope             `json:"scope"`
	Text         string                   `json:"text"`
	Slot         string                   `json:"slot,omitempty"`
	Visibility   memory.Visibility        `json:"visibility,omitempty"`
	Grants       []string                 `json:"grants,omitempty"`
	Assertion    *memory.Assertion        `json:"assertion,omitempty"`
	Decision     *memory.DecisionMetadata `json:"decision,omitempty"`
	Evidence     []memory.Evidence        `json:"evidence,omitempty"`
	Trust        memory.Trust             `json:"trust"`
	Authority    bool                     `json:"authority"`
	Intent       OperationType            `json:"intent,omitempty"`
	TargetID     string                   `json:"target_id,omitempty"`
	ValidFrom    time.Time                `json:"valid_from,omitempty"`
}

// State contains only relevant records that the caller has already authorized.
type State struct {
	Current            []memory.Memory  `json:"current,omitempty"`
	Historical         []memory.Memory  `json:"historical,omitempty"`
	Conflicts          []memory.Memory  `json:"conflicts,omitempty"`
	Decisions          []memory.Memory  `json:"decisions,omitempty"`
	SupportingEvidence []memory.Memory  `json:"supporting_evidence,omitempty"`
	Relations          []relations.Link `json:"relations,omitempty"`
	Unresolved         []relations.Link `json:"unresolved_relationships,omitempty"`
}

type Operation struct {
	Type            OperationType    `json:"type"`
	TargetID        string           `json:"target_id,omitempty"`
	ExpectedVersion int              `json:"expected_version,omitempty"`
	Memory          memory.Memory    `json:"memory,omitempty"`
	Relations       []relations.Link `json:"relations,omitempty"`
	ReasonCode      string           `json:"reason_code"`
}

type Plan struct {
	ID            string      `json:"id"`
	OperationKey  string      `json:"operation_key"`
	InputHash     string      `json:"input_hash"`
	Provider      string      `json:"provider"`
	Model         string      `json:"model,omitempty"`
	PolicyVersion string      `json:"policy_version"`
	Operations    []Operation `json:"operations"`
}

type Planner interface {
	Plan(context.Context, Input, State) (Plan, error)
}

type AdmissionResult string

const (
	AdmissionProcess             AdmissionResult = "PROCESS"
	AdmissionSkip                AdmissionResult = "SKIP"
	AdmissionDefer               AdmissionResult = "DEFER"
	AdmissionRequireConfirmation AdmissionResult = "REQUIRE_CONFIRMATION"
)

type AdmissionInput struct {
	Scope    memory.Scope `json:"scope"`
	Text     string       `json:"text"`
	Trust    memory.Trust `json:"trust"`
	Explicit bool         `json:"explicit"`
}

// AdmissionDecider makes a bounded fast-path admission decision. Its result
// is an optimization, not authority to change knowledge or reconcile state.
type AdmissionDecider interface {
	DecideAdmission(context.Context, AdmissionInput) (AdmissionResult, error)
}

// AdmissionDeterministic is the default, provider-free admission policy. It
// processes all non-empty inputs, never probabilistically skips explicit
// requests, and rejects oversized input before it enters an async job queue.
type AdmissionDeterministic struct {
	MaxTextBytes int
}

const (
	defaultAdmissionMaxTextBytes = 32 << 10
	maxAdmissionTextBytes        = 1 << 20
)

func (d AdmissionDeterministic) DecideAdmission(ctx context.Context, input AdmissionInput) (AdmissionResult, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	limit := d.MaxTextBytes
	if limit <= 0 {
		limit = defaultAdmissionMaxTextBytes
	}
	if limit > maxAdmissionTextBytes {
		return "", fmt.Errorf("knowledge: admission text limit exceeds %d bytes", maxAdmissionTextBytes)
	}
	if len(input.Text) > limit {
		return "", fmt.Errorf("knowledge: admission input exceeds %d bytes", limit)
	}
	if !input.Explicit && strings.TrimSpace(input.Text) == "" {
		return AdmissionSkip, nil
	}
	return AdmissionProcess, nil
}

// Deterministic is a rules-only baseline planner. It does not infer meaning
// from free-form text. It supports explicit intents, structured duplicate
// reinforcement, slot updates, conditional branches, empty-input deferral,
// and ADD for non-empty unspecified text; semantic interpretation requires an
// optional planner provider.
type Deterministic struct{}

func (Deterministic) Plan(ctx context.Context, input Input, state State) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	hash, err := InputHash(input)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		ID: hash[:32], OperationKey: input.OperationKey, InputHash: hash,
		Provider: "deterministic", PolicyVersion: "1",
	}
	proposed, err := proposedMemory(input, state)
	if err != nil {
		return Plan{}, err
	}
	op := Operation{Memory: proposed}
	if input.Intent != "" {
		op.Type = input.Intent
		switch op.Type {
		case ADD:
			op.ReasonCode = "explicit_add"
		case RELATE:
			op.Type, op.ReasonCode = DEFER, "relation_details_required"
		case REINFORCE, REFINE, SUPERSEDE, CORRECT, RETRACT, BRANCH, DEFER, IGNORE:
			op.ReasonCode = "explicit_intent"
		default:
			op.Type, op.ReasonCode = DEFER, "unknown_intent"
		}
		if op.Type == SUPERSEDE || op.Type == CORRECT || op.Type == RETRACT || op.Type == REINFORCE ||
			op.Type == REFINE || op.Type == BRANCH || op.Type == RELATE {
			target, ok := findStateMemory(state, input.TargetID)
			if input.TargetID == "" || !ok {
				op.Type, op.TargetID, op.ExpectedVersion = DEFER, "", 0
				op.ReasonCode = "target_required"
			} else {
				op.TargetID, op.ExpectedVersion = target.ID, target.Version
			}
		}
		plan.Operations = []Operation{op}
		return plan, nil
	}

	if strings.TrimSpace(input.Text) == "" && input.Assertion == nil && input.Decision == nil {
		op.Type, op.ReasonCode = DEFER, "insufficient_input"
		plan.Operations = []Operation{op}
		return plan, nil
	}
	for _, current := range state.Current {
		if !sameScope(current.Scope, input.Scope) {
			continue
		}
		if memory.KnowledgeHash(&current) == memory.KnowledgeHash(&proposed) {
			op.Type, op.TargetID, op.ExpectedVersion = REINFORCE, current.ID, current.Version
			op.ReasonCode = "exact_duplicate"
			plan.Operations = []Operation{op}
			return plan, nil
		}
	}
	if input.Slot != "" {
		for _, current := range state.Current {
			if current.Slot != input.Slot || !sameScope(current.Scope, input.Scope) {
				continue
			}
			existing, readErr := memory.ReadAssertion(current)
			op.TargetID, op.ExpectedVersion = current.ID, current.Version
			switch {
			case input.Assertion != nil && readErr == nil && existing != nil &&
				!sameConditions(existing.Conditions, input.Assertion.Conditions):
				op.Type, op.ReasonCode = BRANCH, "conditional_assertion"
			case input.Assertion == nil || readErr != nil || existing == nil:
				op.Type, op.ReasonCode = SUPERSEDE, "same_slot_new_value"
			case input.Assertion.Object != existing.Object:
				op.Type, op.ReasonCode = SUPERSEDE, "same_slot_new_value"
			default:
				op.Type, op.ReasonCode = REINFORCE, "same_assertion"
			}
			plan.Operations = []Operation{op}
			return plan, nil
		}
	}
	if input.Assertion != nil {
		for _, current := range state.Current {
			if !sameScope(current.Scope, input.Scope) {
				continue
			}
			existing, readErr := memory.ReadAssertion(current)
			if readErr != nil || existing == nil ||
				!strings.EqualFold(memory.Normalize(existing.Subject), memory.Normalize(input.Assertion.Subject)) ||
				!strings.EqualFold(memory.Normalize(existing.Predicate), memory.Normalize(input.Assertion.Predicate)) {
				continue
			}
			op.TargetID, op.ExpectedVersion = current.ID, current.Version
			if !sameConditions(existing.Conditions, input.Assertion.Conditions) {
				op.Type, op.ReasonCode = BRANCH, "conditional_assertion"
			} else if input.Assertion.Object != existing.Object {
				op.Type, op.ReasonCode = SUPERSEDE, "same_slot_new_value"
			} else {
				op.Type, op.ReasonCode = REINFORCE, "same_assertion"
			}
			plan.Operations = []Operation{op}
			return plan, nil
		}
	}
	op.Type, op.ReasonCode = ADD, "new_knowledge"
	plan.Operations = []Operation{op}
	return plan, nil
}

// ValidationError identifies a deterministic plan validation failure.
type ValidationError struct {
	Code      string `json:"code"`
	Operation int    `json:"operation"`
	Message   string `json:"message"`
}

func (e *ValidationError) Error() string {
	if e.Operation >= 0 {
		return fmt.Sprintf("knowledge: %s (operation %d): %s", e.Code, e.Operation, e.Message)
	}
	return fmt.Sprintf("knowledge: %s: %s", e.Code, e.Message)
}

func (e *ValidationError) Unwrap() error { return memory.ErrInvalid }

type ValidatedPlan struct {
	Plan Plan
}

// Validate checks a provider plan against its input and authorized state.
// Unauthorized destructive recommendations are converted to DEFER, never
// silently applied as a weaker destructive transition.
func Validate(input Input, plan Plan, state State, principal memory.Principal) (Plan, error) {
	if strings.TrimSpace(input.Scope.Tenant) == "" {
		return Plan{}, validationError("scope_required", -1, "tenant is required")
	}
	if input.OperationKey == "" {
		return Plan{}, validationError("operation_key_required", -1, "operation key is required")
	}
	if !validTrust(input.Trust) {
		return Plan{}, validationError("invalid_trust", -1, "input trust must be high, normal, or low")
	}
	if !validVisibility(input.Visibility) {
		return Plan{}, validationError("invalid_visibility", -1, "input visibility must be team, private, or restricted")
	}
	if err := validateGrants(input.Grants); err != nil {
		return Plan{}, validationError("invalid_grants", -1, err.Error())
	}
	if len(plan.Operations) == 0 || len(plan.Operations) > 32 {
		return Plan{}, validationError("operation_count", -1, "plan must contain 1..32 operations")
	}
	expectedHash, err := InputHash(input)
	if err != nil {
		return Plan{}, validationError("input_hash", -1, err.Error())
	}
	if plan.OperationKey != input.OperationKey || plan.InputHash != expectedHash {
		return Plan{}, validationError("plan_binding", -1, "operation key or input hash does not match input")
	}
	out := plan
	out.Operations = append([]Operation(nil), plan.Operations...)
	totalRelations := 0
	targeted := make(map[string]bool)
	for i := range out.Operations {
		op := &out.Operations[i]
		if !validOperation(op.Type) {
			return Plan{}, validationError("unknown_operation", i, fmt.Sprintf("unknown operation %q", op.Type))
		}
		if !validReasonCode(op.ReasonCode) {
			return Plan{}, validationError("reason_code", i, "a concise lowercase reason code is required")
		}
		if op.ExpectedVersion < 0 {
			return Plan{}, validationError("version_mismatch", i, "expected version cannot be negative")
		}
		if op.Type == DEFER || op.Type == IGNORE {
			if op.TargetID != "" || op.ExpectedVersion != 0 || len(op.Relations) != 0 {
				return Plan{}, validationError("invalid_deferred_operation", i, "DEFER and IGNORE cannot carry targets or relationships")
			}
			continue
		}
		if len(op.Relations) > 64 {
			return Plan{}, validationError("relation_count", i, "operation exceeds 64 relationships")
		}
		totalRelations += len(op.Relations)
		if totalRelations > 64 {
			return Plan{}, validationError("relation_count", i, "plan exceeds 64 relationships")
		}
		if op.TargetID == "" && op.ExpectedVersion != 0 {
			return Plan{}, validationError("version_mismatch", i, "expected version requires a target")
		}
		if op.TargetID != "" {
			if targeted[op.TargetID] {
				return Plan{}, validationError("duplicate_target", i, "multiple operations target the same memory version")
			}
			targeted[op.TargetID] = true
		}
		target, hasTarget := findStateMemory(state, op.TargetID)
		targetRequired := op.Type == REINFORCE || op.Type == REFINE || op.Type == SUPERSEDE ||
			op.Type == CORRECT || op.Type == RETRACT || op.Type == BRANCH || op.Type == RELATE
		if targetRequired && (!hasTarget || op.TargetID == "") {
			return Plan{}, validationError("unknown_reference", i, "target is not in the supplied knowledge state")
		}
		if op.TargetID != "" && !hasTarget {
			return Plan{}, validationError("unknown_reference", i, "target is not in the supplied knowledge state")
		}
		if hasTarget {
			if target.Tenant != input.Scope.Tenant || !sameScope(target.Scope, input.Scope) {
				return Plan{}, validationError("scope_mismatch", i, "target is outside the input scope")
			}
			if !readable(target, principal) {
				return Plan{}, validationError("not_readable", i, "target is not readable by the principal")
			}
			if err := memory.ValidateKnowledge(&target); err != nil {
				return Plan{}, validationError("invalid_target", i, err.Error())
			}
			if op.TargetID != "" && op.ExpectedVersion != target.Version {
				return Plan{}, validationError("version_mismatch", i, "expected version does not match target")
			}
		}
		if op.Type == ADD && op.Memory.Slot != "" {
			for _, current := range state.Current {
				if current.Tenant == input.Scope.Tenant && current.Slot == op.Memory.Slot &&
					current.User == op.Memory.User && current.Agent == op.Memory.Agent {
					return Plan{}, validationError("occupied_slot", i, "ADD cannot implicitly replace an existing slot value")
				}
			}
		}
		if err := validateInputEvidence(input, state, op.Memory); err != nil {
			return Plan{}, validationError("evidence_reference", i, err.Error())
		}
		if op.Type != DEFER && op.Type != IGNORE {
			applyMissingVisibility(&op.Memory, input, state)
			if err := validateProposedMemory(input, state, op.Memory); err != nil {
				return Plan{}, validationError("memory_scope_or_payload", i, err.Error())
			}
			if op.Type == ADD || op.Type == BRANCH || op.Type == REFINE {
				if err := validateEvidenceVisibility(input, state, op.Memory, principal); err != nil {
					return Plan{}, validationError("visibility_broadened", i, err.Error())
				}
			}
		}
		if targetRequired && destructive(op.Type) {
			decision, decisionErr := memory.ReadDecision(target)
			if decisionErr != nil {
				return Plan{}, validationError("invalid_target", i, decisionErr.Error())
			}
			trustInsufficient := input.Trust == memory.TrustLow ||
				(target.Source.Trust == memory.TrustHigh && input.Trust != memory.TrustHigh)
			if !input.Authority || trustInsufficient || (decision != nil && decision.Status == memory.DecisionActive && !input.Authority) {
				op.Type, op.TargetID, op.ExpectedVersion = DEFER, "", 0
				op.Relations = nil
				op.ReasonCode = "authority_required"
			}
		}
		if op.Type != DEFER {
			decision, _ := memory.ReadDecision(op.Memory)
			if decision != nil && decision.Status == memory.DecisionActive &&
				(!input.Authority || input.Trust != memory.TrustHigh) {
				op.Type, op.TargetID, op.ExpectedVersion = DEFER, "", 0
				op.Relations = nil
				op.ReasonCode = "authority_required"
			}
		}
		for j, link := range op.Relations {
			if !relations.ValidRelation(link.Relation) || !validRelationEndpoints(link, state, op.Memory.ID) {
				return Plan{}, validationError("invalid_relationship", i, fmt.Sprintf("relationship %d has unknown type or reference", j))
			}
			if link.Tenant != input.Scope.Tenant {
				return Plan{}, validationError("scope_mismatch", i, "relationship belongs to another tenant")
			}
			if !validReasonCode(link.Reason) {
				return Plan{}, validationError("relationship_reason", i, "relationship reason must be a concise reason code")
			}
			for _, evidence := range link.Evidence {
				if !containsEvidence(allEvidence(input, state), evidence) {
					return Plan{}, validationError("evidence_reference", i, "relationship uses evidence outside authorized input/state")
				}
			}
			if math.IsNaN(link.Confidence) || math.IsInf(link.Confidence, 0) || link.Confidence < 0 || link.Confidence > 1 {
				return Plan{}, validationError("relationship_confidence", i, "relationship confidence is outside [0,1]")
			}
			for _, id := range []string{link.Source, link.Target} {
				endpoint, exists := findStateMemory(state, id)
				if exists && (endpoint.Tenant != input.Scope.Tenant || !sameScope(endpoint.Scope, input.Scope) || !readable(endpoint, principal)) {
					return Plan{}, validationError("invalid_relationship", i, "relationship endpoint is outside readable scope")
				}
				if exists {
					if err := memory.ValidateKnowledge(&endpoint); err != nil {
						return Plan{}, validationError("invalid_relationship", i, err.Error())
					}
				}
			}
			if link.ValidTo != nil && link.ValidTo.Before(link.ValidFrom) {
				return Plan{}, validationError("invalid_relationship_time", i, "relationship has an invalid temporal interval")
			}
		}
		if op.Type == RELATE && len(op.Relations) == 0 {
			return Plan{}, validationError("relationship_required", i, "RELATE requires at least one relationship")
		}
	}
	return out, nil
}

// InputHash returns the stable binding hash used to prevent plan replay.
func InputHash(input Input) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func proposedMemory(input Input, state State) (memory.Memory, error) {
	visibility, grants := requiredVisibility(input, state)
	m := memory.Memory{
		Scope: input.Scope, Kind: memory.Semantic, Layer: memory.Atom, Text: input.Text,
		Slot: input.Slot, Confidence: 1, Visibility: visibility,
		Grants: grants, ValidFrom: input.ValidFrom,
		Source: memory.Source{Kind: "knowledge", Trust: input.Trust, Evidence: append([]memory.Evidence(nil), input.Evidence...)},
	}
	if input.Assertion != nil {
		m.Confidence = input.Assertion.Confidence
		metadata, err := memory.WithKnowledgeMetadata(m.Metadata, input.Assertion, input.Decision)
		if err != nil {
			return memory.Memory{}, err
		}
		m.Metadata = metadata
		m.Subject, m.Predicate, m.Object = input.Assertion.Subject, input.Assertion.Predicate, input.Assertion.Object
	}
	if input.Decision != nil {
		metadata, err := memory.WithKnowledgeMetadata(m.Metadata, input.Assertion, input.Decision)
		if err != nil {
			return memory.Memory{}, err
		}
		m.Metadata = metadata
	}
	if strings.TrimSpace(m.Text) == "" && input.Assertion != nil {
		m.Text = strings.TrimSpace(input.Assertion.Subject + " " + input.Assertion.Predicate + " " + input.Assertion.Object)
	}
	if strings.TrimSpace(m.Text) == "" && input.Decision != nil {
		m.Text = input.Decision.Subject + ": " + input.Decision.Outcome
	}
	return m, nil
}

func effectiveVisibility(input Input) memory.Visibility {
	if input.Visibility == "" {
		return memory.Team
	}
	return input.Visibility
}

func applyMissingVisibility(proposed *memory.Memory, input Input, state State) {
	visibility, grants := requiredVisibility(input, state)
	if proposed.Visibility == "" {
		proposed.Visibility = visibility
	}
	if proposed.Grants == nil {
		proposed.Grants = append([]string(nil), grants...)
	}
}

func validateProposedMemory(input Input, state State, proposed memory.Memory) error {
	if proposed.Tenant != input.Scope.Tenant || !sameScope(proposed.Scope, input.Scope) {
		return errors.New("proposed memory scope must exactly match input scope")
	}
	if proposed.Text == "" {
		return errors.New("proposed memory text is required")
	}
	if proposed.Confidence < 0 || proposed.Confidence > 1 || math.IsNaN(proposed.Confidence) || math.IsInf(proposed.Confidence, 0) {
		return errors.New("proposed memory confidence is outside [0,1]")
	}
	if proposed.ValidTo != nil && proposed.ValidTo.Before(proposed.ValidFrom) {
		return errors.New("proposed memory has invalid temporal interval")
	}
	if !validVisibility(proposed.Visibility) {
		return errors.New("proposed memory has invalid visibility")
	}
	if err := validateGrants(proposed.Grants); err != nil {
		return err
	}
	expected, err := proposedMemory(input, state)
	if err != nil {
		return err
	}
	expectedJSON, _ := json.Marshal(expected)
	proposedJSON, _ := json.Marshal(proposed)
	if !bytes.Equal(proposedJSON, expectedJSON) {
		return errors.New("proposed memory changed the input payload or added unvalidated fields")
	}
	a, err := memory.ReadAssertion(proposed)
	if err != nil {
		return err
	}
	if input.Assertion == nil {
		if a != nil {
			return errors.New("proposed memory added an assertion not present in input")
		}
	} else if a == nil || !sameAssertion(*a, *input.Assertion) {
		return errors.New("proposed memory changed or removed the input assertion")
	}
	decision, err := memory.ReadDecision(proposed)
	if err != nil {
		return err
	}
	if !sameDecision(decision, input.Decision) {
		return errors.New("proposed memory changed or added decision metadata")
	}
	return nil
}

func validateEvidenceVisibility(input Input, state State, proposed memory.Memory, principal memory.Principal) error {
	if !visibilityAtLeastAsRestrictive(proposed.Visibility, proposed.Grants, effectiveVisibility(input), input.Grants) {
		return errors.New("proposed memory visibility is broader than input visibility")
	}
	for _, cited := range citedMemories(input, state) {
		if cited.Tenant != input.Scope.Tenant || !sameScope(cited.Scope, input.Scope) || !readable(cited, principal) {
			return fmt.Errorf("cited memory %q is outside the authorized input scope", cited.ID)
		}
		if !visibilityAtLeastAsRestrictive(proposed.Visibility, proposed.Grants, cited.Visibility, cited.Grants) {
			return fmt.Errorf("proposed memory visibility is broader than cited memory %q", cited.ID)
		}
	}
	return nil
}

func requiredVisibility(input Input, state State) (memory.Visibility, []string) {
	visibility := effectiveVisibility(input)
	grants := append([]string(nil), input.Grants...)
	for _, cited := range citedMemories(input, state) {
		switch cited.Visibility {
		case memory.Private:
			visibility, grants = memory.Private, nil
		case memory.Restricted:
			switch visibility {
			case memory.Team:
				visibility, grants = memory.Restricted, append([]string(nil), cited.Grants...)
			case memory.Restricted:
				grants = intersectGrants(grants, cited.Grants)
				if len(grants) == 0 {
					visibility = memory.Private
				}
			}
		}
	}
	sort.Strings(grants)
	return visibility, grants
}

func intersectGrants(a, b []string) []string {
	allowed := make(map[string]bool, len(b))
	for _, grant := range b {
		allowed[grant] = true
	}
	var out []string
	for _, grant := range a {
		if allowed[grant] {
			out = append(out, grant)
		}
	}
	return out
}

func citedMemories(input Input, state State) []memory.Memory {
	refs := make(map[string]bool)
	if input.Assertion != nil {
		for _, id := range input.Assertion.EvidenceIDs {
			refs[id] = true
		}
	}
	if input.Decision != nil {
		for _, id := range input.Decision.EvidenceIDs {
			refs[id] = true
		}
	}
	for _, evidence := range input.Evidence {
		refs[evidence.Episode] = true
		refs[evidence.Episode+"#"+strconv.Itoa(evidence.Part)+":"+strconv.Itoa(evidence.Start)+":"+strconv.Itoa(evidence.End)] = true
	}
	seen := make(map[string]bool)
	var cited []memory.Memory
	for _, list := range [][]memory.Memory{state.Current, state.Historical, state.Conflicts, state.Decisions, state.SupportingEvidence} {
		for _, candidate := range list {
			if seen[candidate.ID] {
				continue
			}
			match := refs[candidate.ID]
			for _, evidence := range candidate.Source.Evidence {
				key := evidence.Episode + "#" + strconv.Itoa(evidence.Part) + ":" + strconv.Itoa(evidence.Start) + ":" + strconv.Itoa(evidence.End)
				if refs[evidence.Episode] || refs[key] {
					match = true
					break
				}
			}
			if match {
				seen[candidate.ID] = true
				cited = append(cited, candidate)
			}
		}
	}
	return cited
}

func visibilityAtLeastAsRestrictive(output memory.Visibility, outputGrants []string, source memory.Visibility, sourceGrants []string) bool {
	if source == "" {
		source = memory.Team
	}
	switch source {
	case memory.Team:
		return validVisibility(output)
	case memory.Private:
		return output == memory.Private
	case memory.Restricted:
		if output == memory.Private {
			return true
		}
		if output != memory.Restricted {
			return false
		}
		allowed := make(map[string]bool, len(sourceGrants))
		for _, grant := range sourceGrants {
			allowed[grant] = true
		}
		for _, grant := range outputGrants {
			if !allowed[grant] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validVisibility(visibility memory.Visibility) bool {
	switch visibility {
	case "", memory.Team, memory.Private, memory.Restricted:
		return true
	default:
		return false
	}
}

func validateGrants(grants []string) error {
	if len(grants) > 256 {
		return errors.New("memory grants exceed 256 entries")
	}
	for _, grant := range grants {
		key, value, ok := strings.Cut(grant, ":")
		if !ok || value == "" || (key != "user" && key != "agent" && key != "role") {
			return fmt.Errorf("grant %q must be user:<id>, agent:<id> or role:<name>", grant)
		}
	}
	return nil
}

func validateInputEvidence(input Input, state State, proposed memory.Memory) error {
	evidence := allEvidence(input, state)
	allowed := make(map[string]bool, len(evidence)*2)
	for _, e := range evidence {
		if e.Episode == "" || e.Part < 0 || e.Start < 0 || e.End < e.Start {
			return errors.New("input contains invalid evidence")
		}
		allowed[e.Episode] = true
		allowed[e.Episode+"#"+strconv.Itoa(e.Part)+":"+strconv.Itoa(e.Start)+":"+strconv.Itoa(e.End)] = true
	}
	if !sameEvidenceList(proposed.Source.Evidence, input.Evidence) {
		return errors.New("operation must preserve all input source evidence")
	}
	if a, err := memory.ReadAssertion(proposed); err != nil {
		return err
	} else if a != nil {
		for _, id := range a.EvidenceIDs {
			if !allowed[id] {
				return fmt.Errorf("evidence %q was not supplied with input", id)
			}
		}
	}
	if d, err := memory.ReadDecision(proposed); err != nil {
		return err
	} else if d != nil {
		for _, id := range d.EvidenceIDs {
			if !allowed[id] {
				return fmt.Errorf("decision evidence %q was not supplied or authorized with state", id)
			}
		}
	}
	return nil
}

func allEvidence(input Input, state State) []memory.Evidence {
	return append(append([]memory.Evidence(nil), input.Evidence...), stateEvidence(state)...)
}

func stateEvidence(state State) []memory.Evidence {
	var out []memory.Evidence
	for _, list := range [][]memory.Memory{state.Current, state.Historical, state.Conflicts, state.Decisions, state.SupportingEvidence} {
		for _, m := range list {
			out = append(out, m.Source.Evidence...)
		}
	}
	return out
}

func sameEvidenceList(a, b []memory.Evidence) bool {
	if len(a) != len(b) {
		return false
	}
	remaining := append([]memory.Evidence(nil), b...)
	for _, item := range a {
		found := false
		for i, candidate := range remaining {
			if candidate == item {
				remaining = append(remaining[:i], remaining[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsEvidence(list []memory.Evidence, item memory.Evidence) bool {
	for _, candidate := range list {
		if candidate == item {
			return true
		}
	}
	return false
}

func findStateMemory(state State, id string) (memory.Memory, bool) {
	if id == "" {
		return memory.Memory{}, false
	}
	for _, list := range [][]memory.Memory{state.Current, state.Historical, state.Conflicts, state.Decisions, state.SupportingEvidence} {
		for _, m := range list {
			if m.ID == id {
				return m, true
			}
		}
	}
	return memory.Memory{}, false
}

func sameScope(a, b memory.Scope) bool {
	return a.Tenant == b.Tenant && a.User == b.User && a.Agent == b.Agent && a.Run == b.Run
}

func readable(m memory.Memory, p memory.Principal) bool {
	if p.Admin {
		return true
	}
	owner := (m.User == "" || m.User == p.User) && (m.Agent == "" || m.Agent == p.Agent)
	switch m.Visibility {
	case "", memory.Team:
		return true
	case memory.Private:
		return owner
	case memory.Restricted:
		if owner {
			return true
		}
		for _, grant := range m.Grants {
			switch {
			case grant == "user:"+p.User && p.User != "":
				return true
			case grant == "agent:"+p.Agent && p.Agent != "":
				return true
			case strings.HasPrefix(grant, "role:"):
				role := strings.TrimPrefix(grant, "role:")
				for _, have := range p.Roles {
					if have == role {
						return true
					}
				}
			}
		}
	}
	return false
}

func validOperation(op OperationType) bool {
	switch op {
	case ADD, REINFORCE, REFINE, SUPERSEDE, CORRECT, RETRACT, BRANCH, RELATE, DEFER, IGNORE:
		return true
	default:
		return false
	}
}

func validTrust(trust memory.Trust) bool {
	return trust == memory.TrustHigh || trust == memory.TrustNormal || trust == memory.TrustLow
}

func validReasonCode(code string) bool {
	if len(code) == 0 || len(code) > 64 || code[0] == '_' || code[len(code)-1] == '_' {
		return false
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func destructive(op OperationType) bool {
	return op == SUPERSEDE || op == CORRECT || op == RETRACT
}

func validRelationEndpoints(link relations.Link, state State, proposedID string) bool {
	if link.Source == "" || link.Target == "" {
		return false
	}
	_, sourceOK := findStateMemory(state, link.Source)
	_, targetOK := findStateMemory(state, link.Target)
	return (sourceOK || link.Source == proposedID) && (targetOK || link.Target == proposedID)
}

func sameDecision(a, b *memory.DecisionMetadata) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameConditions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func sameAssertion(a, b memory.Assertion) bool {
	if a.Subject != b.Subject || a.Predicate != b.Predicate || a.Object != b.Object || a.EpistemicType != b.EpistemicType ||
		a.Confidence != b.Confidence || !sameConditions(a.Conditions, b.Conditions) || len(a.EvidenceIDs) != len(b.EvidenceIDs) {
		return false
	}
	x, y := append([]string(nil), a.EvidenceIDs...), append([]string(nil), b.EvidenceIDs...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func validationError(code string, index int, message string) error {
	return &ValidationError{Code: code, Operation: index, Message: message}
}
