package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
)

func TestDeterministicPlannerBranchesDifferentConditions(t *testing.T) {
	old := assertionMemory("work-hours", "user", "works_at", "Acme", map[string]string{"country": "PT"})
	input := baseInput()
	input.Slot = "work-hours"
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "works_at", Object: "Acme",
		EpistemicType: memory.EpistemicFact, Confidence: 0.9,
		Conditions: map[string]string{"country": "US"},
	}
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{old}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Operations[0].Type; got != BRANCH {
		t.Fatalf("operation = %s, want BRANCH", got)
	}
}

func TestDeterministicPlannerUsesExplicitSlotWithoutInterpretingText(t *testing.T) {
	old := assertionMemory("work-hours", "user", "works_at", "Acme", nil)
	old.Slot = "employer"
	input := baseInput()
	input.Text, input.Slot = "The user now works at Globex", "employer"
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{old}})
	if err != nil {
		t.Fatal(err)
	}
	if op := plan.Operations[0]; op.Type != SUPERSEDE || op.TargetID != old.ID {
		t.Fatalf("slot update = %#v", op)
	}
}

func TestDeterministicPlannerCorrectionAndAmbiguity(t *testing.T) {
	target := assertionMemory("preference", "user", "likes", "tea", nil)
	input := baseInput()
	input.Intent, input.TargetID, input.Authority = CORRECT, target.ID, true
	input.Trust = memory.TrustHigh
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{target}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operations[0].Type != CORRECT {
		t.Fatalf("explicit correction became %s", plan.Operations[0].Type)
	}

	input.TargetID = ""
	plan, err = (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{target}})
	if err != nil {
		t.Fatal(err)
	}
	if op := plan.Operations[0]; op.Type != DEFER || op.ReasonCode != "target_required" {
		t.Fatalf("ambiguous correction = %#v", op)
	}
}

func TestValidateDefersLowTrustDestructiveRecommendation(t *testing.T) {
	target := assertionMemory("fact", "user", "lives_in", "Lisbon", nil)
	target.Source.Trust = memory.TrustHigh
	input := baseInput()
	input.Trust, input.Authority, input.Intent, input.TargetID = memory.TrustLow, false, SUPERSEDE, target.ID
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{target}})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := Validate(input, plan, State{Current: []memory.Memory{target}}, memory.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if op := validated.Operations[0]; op.Type != DEFER || op.ReasonCode != "authority_required" {
		t.Fatalf("unauthorized recommendation = %#v", op)
	}
}

func TestValidateDefersLowTrustCorrection(t *testing.T) {
	target := assertionMemory("fact", "user", "lives_in", "Lisbon", nil)
	target.Source.Trust = memory.TrustHigh
	input := baseInput()
	input.Trust, input.Authority, input.Intent, input.TargetID = memory.TrustLow, true, CORRECT, target.ID
	state := State{Current: []memory.Memory{target}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := Validate(input, plan, state, memory.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if op := validated.Operations[0]; op.Type != DEFER || op.ReasonCode != "authority_required" {
		t.Fatalf("low-trust correction = %#v", op)
	}
}

func TestConditionalBranchSurvivesValidation(t *testing.T) {
	old := assertionMemory("work-hours", "user", "works_at", "Acme", map[string]string{"country": "PT"})
	input := baseInput()
	input.Slot = "work-hours"
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "works_at", Object: "Acme",
		EpistemicType: memory.EpistemicFact, Confidence: 0.9,
		Conditions: map[string]string{"country": "US"},
	}
	state := State{Current: []memory.Memory{old}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := Validate(input, plan, state, memory.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if op := validated.Operations[0]; op.Type != BRANCH || op.TargetID != old.ID {
		t.Fatalf("validated branch = %#v", op)
	}
}

func TestActiveAndProposedDecisionAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		trust     memory.Trust
		authority bool
		want      OperationType
	}{
		{name: "proposed without authority", status: memory.DecisionProposed, trust: memory.TrustNormal, want: ADD},
		{name: "active without authority", status: memory.DecisionActive, trust: memory.TrustHigh, want: DEFER},
		{name: "active with authority", status: memory.DecisionActive, trust: memory.TrustHigh, authority: true, want: ADD},
		{name: "active low trust with authority", status: memory.DecisionActive, trust: memory.TrustLow, authority: true, want: DEFER},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := baseInput()
			input.Trust, input.Authority = tc.trust, tc.authority
			input.Decision = &memory.DecisionMetadata{
				Subject: "deployment", Outcome: "use replicas", Status: tc.status,
			}
			plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
			if err != nil {
				t.Fatal(err)
			}
			validated, err := Validate(input, plan, State{}, memory.Principal{})
			if err != nil {
				t.Fatal(err)
			}
			if got := validated.Operations[0].Type; got != tc.want {
				t.Fatalf("operation = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidateDefersUnauthoritativeActiveDecision(t *testing.T) {
	input := baseInput()
	input.Decision = &memory.DecisionMetadata{
		Subject: "deployment", Outcome: "use replicas", Status: memory.DecisionActive,
	}
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := Validate(input, plan, State{}, memory.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if op := validated.Operations[0]; op.Type != DEFER || op.ReasonCode != "authority_required" {
		t.Fatalf("unauthorized active decision = %#v", op)
	}
}

func TestValidateRejectsUnknownTargetAndVersion(t *testing.T) {
	input := baseInput()
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].Type = REINFORCE
	plan.Operations[0].TargetID = "missing"
	plan.Operations[0].ExpectedVersion = 1
	_, err = Validate(input, plan, State{}, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "unknown_reference" {
		t.Fatalf("error = %v, want unknown_reference", err)
	}
}

func TestValidateRequiresReadableExactScopeAndEvidence(t *testing.T) {
	target := assertionMemory("target", "user", "lives_in", "Porto", nil)
	target.Version = 3
	target.Visibility = memory.Private
	input := baseInput()
	input.Intent, input.TargetID, input.Trust = REINFORCE, target.ID, memory.TrustNormal
	input.Scope.User = "other"
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{target}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(input, plan, State{Current: []memory.Memory{target}}, memory.Principal{User: "other"})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "scope_mismatch" {
		t.Fatalf("error = %v, want scope_mismatch", err)
	}

	input = baseInput()
	input.Evidence = []memory.Evidence{{Episode: "ep-1", Part: 0, Start: 0, End: 10}}
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "likes", Object: "coffee",
		EpistemicType: memory.EpistemicFact, Confidence: 0.8,
		EvidenceIDs: []string{"ep-not-authorized"},
	}
	plan, err = (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(input, plan, State{}, memory.Principal{})
	if !errors.As(err, &validation) || validation.Code != "evidence_reference" {
		t.Fatalf("error = %v, want evidence_reference", err)
	}
}

func TestValidatePreventsPrivateToTeamVisibilityLeakage(t *testing.T) {
	input := baseInput()
	input.Visibility = memory.Private
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "prefers", Object: "tea",
		EpistemicType: memory.EpistemicPreference, Confidence: 0.9,
	}
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operations[0].Memory.Visibility != memory.Private {
		t.Fatalf("planner visibility = %q, want private", plan.Operations[0].Memory.Visibility)
	}
	plan.Operations[0].Memory.Visibility = memory.Team
	if _, err := Validate(input, plan, State{}, memory.Principal{User: input.Scope.User}); err == nil {
		t.Fatal("private input was accepted as a team-visible output")
	}

	plan.Operations[0].Memory.Visibility = ""
	validated, err := Validate(input, plan, State{}, memory.Principal{User: input.Scope.User})
	if err != nil {
		t.Fatalf("omitted visibility should inherit input policy: %v", err)
	}
	if got := validated.Operations[0].Memory.Visibility; got != memory.Private {
		t.Fatalf("normalized output visibility = %q, want private", got)
	}
}

func TestCitedPrivateEvidenceConstrainsOutputVisibility(t *testing.T) {
	privateEvidence := assertionMemory("private-source", "user", "prefers", "tea", nil)
	privateEvidence.Visibility = memory.Private
	privateEvidence.Source.Evidence = []memory.Evidence{{Episode: "private-episode", Part: 0, Start: 0, End: 10}}
	input := baseInput()
	input.Evidence = []memory.Evidence{{Episode: "private-episode", Part: 0, Start: 0, End: 10}}
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "prefers", Object: "tea",
		EpistemicType: memory.EpistemicPreference, Confidence: 0.9,
		EvidenceIDs: []string{"private-episode"},
	}
	state := State{SupportingEvidence: []memory.Memory{privateEvidence}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operations[0].Memory.Visibility != memory.Private {
		t.Fatalf("planner broadened private evidence to %q", plan.Operations[0].Memory.Visibility)
	}
	principal := memory.Principal{User: input.Scope.User}
	if _, err := Validate(input, plan, state, principal); err != nil {
		t.Fatalf("private output rejected: %v", err)
	}
	plan.Operations[0].Memory.Visibility = memory.Team
	if _, err := Validate(input, plan, state, principal); err == nil {
		t.Fatal("team-visible output citing private evidence was accepted")
	}
}

func TestRestrictedOutputCannotBroadenCitedGrants(t *testing.T) {
	source := assertionMemory("restricted-source", "user", "prefers", "tea", nil)
	source.Visibility = memory.Restricted
	source.Grants = []string{"user:alice"}
	source.Source.Evidence = []memory.Evidence{{Episode: "restricted-episode", Part: 0, Start: 0, End: 5}}
	input := baseInput()
	input.Evidence = []memory.Evidence{{Episode: "restricted-episode", Part: 0, Start: 0, End: 5}}
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "prefers", Object: "tea",
		EpistemicType: memory.EpistemicPreference, Confidence: 0.9,
		EvidenceIDs: []string{"restricted-episode"},
	}
	state := State{SupportingEvidence: []memory.Memory{source}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operations[0].Memory.Visibility != memory.Restricted ||
		len(plan.Operations[0].Memory.Grants) != 1 || plan.Operations[0].Memory.Grants[0] != "user:alice" {
		t.Fatalf("derived visibility policy = %q %v", plan.Operations[0].Memory.Visibility, plan.Operations[0].Memory.Grants)
	}
	plan.Operations[0].Memory.Grants = []string{"user:alice", "user:bob"}
	if _, err := Validate(input, plan, state, memory.Principal{User: input.Scope.User}); err == nil {
		t.Fatal("output with broader grants than cited source was accepted")
	}
}

func TestValidatePlanBindingAndUnknownOperation(t *testing.T) {
	input := baseInput()
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].Type = OperationType("DELETE")
	_, err = Validate(input, plan, State{}, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "unknown_operation" {
		t.Fatalf("error = %v, want unknown_operation", err)
	}
	plan.Operations[0].Type = ADD
	plan.InputHash = "replayed"
	_, err = Validate(input, plan, State{}, memory.Principal{})
	if !errors.As(err, &validation) || validation.Code != "plan_binding" {
		t.Fatalf("error = %v, want plan_binding", err)
	}
}

func TestValidateRejectsUnknownOptionalTargetReference(t *testing.T) {
	input := baseInput()
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].TargetID = "unseen-memory"
	plan.Operations[0].ExpectedVersion = 2
	_, err = Validate(input, plan, State{}, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "unknown_reference" {
		t.Fatalf("error = %v, want unknown_reference", err)
	}
}

func TestValidateRejectsInvalidRelationEndpoint(t *testing.T) {
	target := assertionMemory("known", "user", "likes", "tea", nil)
	input := baseInput()
	state := State{Current: []memory.Memory{target}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	op := &plan.Operations[0]
	op.Type, op.TargetID, op.ExpectedVersion = RELATE, target.ID, target.Version
	op.Relations = []relations.Link{{
		Tenant: input.Scope.Tenant, Source: target.ID, Target: "unknown",
		Relation: "related_to", Reason: "related", Confidence: 0.8,
	}}
	_, err = Validate(input, plan, state, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "invalid_relationship" {
		t.Fatalf("error = %v, want invalid_relationship", err)
	}
}

func TestValidateAcceptsOnlyInputOrStateEvidence(t *testing.T) {
	inputEvidence := memory.Evidence{Episode: "ep-input", Part: 0, Start: 0, End: 10}
	stateEvidence := memory.Evidence{Episode: "ep-state", Part: 1, Start: 2, End: 8}
	target := assertionMemory("support", "user", "likes", "coffee", nil)
	target.Source.Evidence = []memory.Evidence{stateEvidence}
	input := baseInput()
	input.Evidence = []memory.Evidence{inputEvidence}
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "likes", Object: "coffee",
		EpistemicType: memory.EpistemicFact, Confidence: 0.8,
		EvidenceIDs: []string{"ep-state"},
	}
	input.Intent, input.TargetID = REINFORCE, target.ID
	state := State{SupportingEvidence: []memory.Memory{target}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(input, plan, state, memory.Principal{}); err != nil {
		t.Fatalf("authorized state evidence rejected: %v", err)
	}
	input.Assertion.EvidenceIDs = []string{"ep-outside"}
	plan, err = (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(input, plan, state, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "evidence_reference" {
		t.Fatalf("error = %v, want evidence_reference", err)
	}
	input.Assertion.EvidenceIDs = []string{"ep-state"}
	plan, err = (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].Memory.Source.Evidence = nil
	_, err = Validate(input, plan, state, memory.Principal{})
	if !errors.As(err, &validation) || validation.Code != "evidence_reference" {
		t.Fatalf("error = %v, want source evidence preservation error", err)
	}
}

func TestValidateRejectsADDThatWouldReplaceOccupiedSlot(t *testing.T) {
	old := assertionMemory("language", "service", "uses", "Go", nil)
	old.Slot = "project.language"
	input := baseInput()
	input.Intent, input.Slot = ADD, "project.language"
	state := State{Current: []memory.Memory{old}}
	plan, err := (Deterministic{}).Plan(context.Background(), input, state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(input, plan, state, memory.Principal{})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "occupied_slot" {
		t.Fatalf("error = %v, want occupied_slot", err)
	}
}

func TestDeterministicPlannerConservativeUnspecifiedText(t *testing.T) {
	input := baseInput()
	input.Assertion = nil
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	if op := plan.Operations[0]; op.Type != ADD || op.ReasonCode != "new_knowledge" {
		t.Fatalf("unstructured text operation = %#v", op)
	}
	input.Text = " \n\t"
	plan, err = (Deterministic{}).Plan(context.Background(), input, State{})
	if err != nil {
		t.Fatal(err)
	}
	if op := plan.Operations[0]; op.Type != DEFER || op.ReasonCode != "insufficient_input" {
		t.Fatalf("empty text operation = %#v", op)
	}
}

func TestDeterministicPlannerRejectsInvalidInputConfidence(t *testing.T) {
	input := baseInput()
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "likes", Object: "tea",
		EpistemicType: memory.EpistemicFact, Confidence: math.Inf(1),
	}
	if _, err := (Deterministic{}).Plan(context.Background(), input, State{}); err == nil {
		t.Fatal("infinite model confidence accepted")
	}
}

func TestDeterministicPlannerReinforcesClaimDespiteNewEvidenceAndConfidence(t *testing.T) {
	old := assertionMemory("claim", "user", "works_at", "Acme", map[string]string{"country": "PT"})
	oldAssertion, err := memory.ReadAssertion(old)
	if err != nil {
		t.Fatal(err)
	}
	oldAssertion.Confidence = 0.6
	oldAssertion.EvidenceIDs = []string{"ep-old"}
	old.Metadata, err = memory.WithKnowledgeMetadata(nil, oldAssertion, nil)
	if err != nil {
		t.Fatal(err)
	}

	input := baseInput()
	input.Assertion = &memory.Assertion{
		Subject: "user", Predicate: "works_at", Object: "Acme",
		EpistemicType: memory.EpistemicFact, Confidence: 0.95,
		Conditions:  map[string]string{"country": "PT"},
		EvidenceIDs: []string{"ep-new"},
	}
	plan, err := (Deterministic{}).Plan(context.Background(), input, State{Current: []memory.Memory{old}})
	if err != nil {
		t.Fatal(err)
	}
	if op := plan.Operations[0]; op.Type != REINFORCE || op.TargetID != old.ID {
		t.Fatalf("new evidence created duplicate instead of reinforcement: %#v", op)
	}
}

func TestDeterministicAdmissionIsBoundedAndExplicitSafe(t *testing.T) {
	decider := AdmissionDeterministic{MaxTextBytes: 16}
	got, err := decider.DecideAdmission(context.Background(), AdmissionInput{Text: "incoming"})
	if err != nil || got != AdmissionProcess {
		t.Fatalf("normal admission = %s, %v", got, err)
	}
	got, err = decider.DecideAdmission(context.Background(), AdmissionInput{Explicit: true})
	if err != nil || got != AdmissionProcess {
		t.Fatalf("explicit admission = %s, %v", got, err)
	}
	got, err = decider.DecideAdmission(context.Background(), AdmissionInput{})
	if err != nil || got != AdmissionSkip {
		t.Fatalf("empty background admission = %s, %v", got, err)
	}
	if _, err := decider.DecideAdmission(context.Background(), AdmissionInput{Text: "this input is oversized"}); err == nil {
		t.Fatal("oversized admission input accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := decider.DecideAdmission(ctx, AdmissionInput{Text: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission error = %v", err)
	}
}

func TestKnowledgeStateJSONIncludesSupportingAndUnresolvedFields(t *testing.T) {
	state := State{
		SupportingEvidence: []memory.Memory{{ID: "evidence-1"}},
		Unresolved: []relations.Link{{
			ID: "link-1", Tenant: "tenant-1", Source: "a", Target: "b",
			Relation: "contradicts",
		}},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["supporting_evidence"]; !ok {
		t.Fatal("supporting_evidence field missing from JSON")
	}
	if _, ok := decoded["unresolved_relationships"]; !ok {
		t.Fatal("unresolved_relationships field missing from JSON")
	}
}

func baseInput() Input {
	return Input{
		OperationKey: "op-1",
		Scope:        memory.Scope{Tenant: "tenant-1", User: "user-1"},
		Text:         "The user likes coffee.",
		Trust:        memory.TrustNormal,
	}
}

func assertionMemory(id, subject, predicate, object string, conditions map[string]string) memory.Memory {
	assertion := memory.Assertion{
		Subject: subject, Predicate: predicate, Object: object,
		EpistemicType: memory.EpistemicFact, Conditions: conditions, Confidence: 0.9,
	}
	metadata, err := memory.WithKnowledgeMetadata(nil, &assertion, nil)
	if err != nil {
		panic(err)
	}
	return memory.Memory{
		ID: id, Scope: memory.Scope{Tenant: "tenant-1", User: "user-1"},
		Kind: memory.Semantic, Layer: memory.Atom, Text: subject + " " + predicate + " " + object,
		Subject: subject, Predicate: predicate, Object: object, Metadata: metadata,
		Confidence: assertion.Confidence, Visibility: memory.Team, Version: 1,
		Source: memory.Source{Trust: memory.TrustNormal},
	}
}
