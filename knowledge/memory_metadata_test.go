package knowledge

import (
	"math"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestKnowledgeMetadataRoundTripAndCanonicalHash(t *testing.T) {
	first := memory.Assertion{
		Subject: "user", Predicate: "works_at", Object: "Acme",
		EpistemicType: memory.EpistemicFact, Confidence: 0.8,
		Conditions:  map[string]string{"country": "PT", "role": "engineer"},
		EvidenceIDs: []string{"ep-1"},
	}
	second := first
	second.Conditions = map[string]string{"role": "engineer", "country": "PT"}
	meta, err := memory.WithKnowledgeMetadata(map[string]any{"custom": "kept"}, &first, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := memory.Memory{Text: "different text", Metadata: meta}
	got, err := memory.ReadAssertion(m)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Object != first.Object || m.Metadata["custom"] != "kept" {
		t.Fatalf("metadata round trip failed: %#v", got)
	}
	m2 := memory.Memory{Metadata: map[string]any{memory.AssertionMetadataKey: second}}
	if memory.KnowledgeHash(&m) != memory.KnowledgeHash(&m2) {
		t.Fatal("conditions map ordering changed canonical knowledge hash")
	}
	differentType := first
	differentType.EpistemicType = memory.EpistemicBelief
	differentTypeMeta, err := memory.WithKnowledgeMetadata(nil, &differentType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if memory.KnowledgeHash(&m2) == memory.KnowledgeHash(&memory.Memory{Metadata: differentTypeMeta}) {
		t.Fatal("epistemic type was omitted from canonical knowledge hash")
	}

	textOnly := memory.Memory{Text: "  I like tea "}
	if memory.KnowledgeHash(&textOnly) != memory.Hash(textOnly.Text) {
		t.Fatal("text-only hash behavior changed")
	}
}

func TestKnowledgeHashIncludesCanonicalDecisionMetadata(t *testing.T) {
	assertion := memory.Assertion{
		Subject: "project", Predicate: "deployment", Object: "replicas",
		EpistemicType: memory.EpistemicDecision, Confidence: 0.9,
		Conditions: map[string]string{"region": "west", "environment": "prod"},
	}
	decision := memory.DecisionMetadata{
		Subject: "project deployment", Outcome: "use replicas", Status: memory.DecisionActive,
		Rationale: []string{"availability", "failover"}, EvidenceIDs: []string{"ep-2", "ep-1"},
		Authority: "operator",
	}
	firstMeta, err := memory.WithKnowledgeMetadata(nil, &assertion, &decision)
	if err != nil {
		t.Fatal(err)
	}
	reorderedAssertion := assertion
	reorderedAssertion.Conditions = map[string]string{"environment": "prod", "region": "west"}
	reorderedDecision := decision
	reorderedDecision.EvidenceIDs = []string{"ep-1", "ep-2"}
	reorderedMeta, err := memory.WithKnowledgeMetadata(nil, &reorderedAssertion, &reorderedDecision)
	if err != nil {
		t.Fatal(err)
	}
	first, reordered := memory.Memory{Metadata: firstMeta}, memory.Memory{Metadata: reorderedMeta}
	if memory.KnowledgeHash(&first) != memory.KnowledgeHash(&reordered) {
		t.Fatal("condition or evidence order changed canonical knowledge hash")
	}
	decision.Outcome = "use a single replica"
	changedMeta, err := memory.WithKnowledgeMetadata(nil, &assertion, &decision)
	if err != nil {
		t.Fatal(err)
	}
	if memory.KnowledgeHash(&first) == memory.KnowledgeHash(&memory.Memory{Metadata: changedMeta}) {
		t.Fatal("decision metadata was omitted from canonical knowledge hash")
	}
}

func TestKnowledgeHashIgnoresConfidenceAndEvidenceProvenance(t *testing.T) {
	assertion := memory.Assertion{
		Subject: "user", Predicate: "works_at", Object: "Acme",
		EpistemicType: memory.EpistemicFact, Confidence: 0.6,
		Conditions:  map[string]string{"country": "PT"},
		EvidenceIDs: []string{"ep-old"},
	}
	decision := memory.DecisionMetadata{
		Subject: "deployment", Outcome: "use replicas", Status: memory.DecisionActive,
		Rationale: []string{"availability"}, EvidenceIDs: []string{"decision-ep-old"},
	}
	firstMetadata, err := memory.WithKnowledgeMetadata(nil, &assertion, &decision)
	if err != nil {
		t.Fatal(err)
	}
	updatedAssertion := assertion
	updatedAssertion.Confidence = 0.99
	updatedAssertion.EvidenceIDs = []string{"ep-new", "ep-supporting"}
	updatedDecision := decision
	updatedDecision.EvidenceIDs = []string{"decision-ep-new"}
	updatedMetadata, err := memory.WithKnowledgeMetadata(nil, &updatedAssertion, &updatedDecision)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := memory.KnowledgeHash(&memory.Memory{Metadata: firstMetadata})
	updatedHash := memory.KnowledgeHash(&memory.Memory{Metadata: updatedMetadata})
	if firstHash != updatedHash {
		t.Fatalf("supporting evidence changed semantic hash: first=%s updated=%s", firstHash, updatedHash)
	}

	changedCondition := updatedAssertion
	changedCondition.Conditions = map[string]string{"country": "US"}
	conditionMetadata, err := memory.WithKnowledgeMetadata(nil, &changedCondition, &updatedDecision)
	if err != nil {
		t.Fatal(err)
	}
	if memory.KnowledgeHash(&memory.Memory{Metadata: conditionMetadata}) == firstHash {
		t.Fatal("condition change did not change semantic hash")
	}

	changedType := updatedAssertion
	changedType.EpistemicType = memory.EpistemicBelief
	typeMetadata, err := memory.WithKnowledgeMetadata(nil, &changedType, &updatedDecision)
	if err != nil {
		t.Fatal(err)
	}
	if memory.KnowledgeHash(&memory.Memory{Metadata: typeMetadata}) == firstHash {
		t.Fatal("epistemic type change did not change semantic hash")
	}

	changedOutcome := updatedDecision
	changedOutcome.Outcome = "use one replica"
	outcomeMetadata, err := memory.WithKnowledgeMetadata(nil, &updatedAssertion, &changedOutcome)
	if err != nil {
		t.Fatal(err)
	}
	if memory.KnowledgeHash(&memory.Memory{Metadata: outcomeMetadata}) == firstHash {
		t.Fatal("decision outcome change did not change semantic hash")
	}
}

func TestWithKnowledgeMetadataDeepCopiesInputs(t *testing.T) {
	assertion := memory.Assertion{
		Subject: "user", Predicate: "likes", Object: "tea",
		EpistemicType: memory.EpistemicFact, Confidence: 0.8,
		Conditions:  map[string]string{"place": "home"},
		EvidenceIDs: []string{"ep-1"},
	}
	decision := memory.DecisionMetadata{
		Subject: "deployment", Outcome: "replicas", Status: memory.DecisionProposed,
		Rationale: []string{"availability"},
	}
	inputMetadata := map[string]any{
		"nested": map[string]any{
			"list": []any{"unchanged", map[string]any{"value": "before"}},
		},
	}
	output, err := memory.WithKnowledgeMetadata(inputMetadata, &assertion, &decision)
	if err != nil {
		t.Fatal(err)
	}
	assertion.Conditions["place"] = "office"
	assertion.EvidenceIDs[0] = "changed"
	decision.Rationale[0] = "changed"
	inputMetadata["nested"].(map[string]any)["list"].([]any)[1].(map[string]any)["value"] = "after"
	inputMetadata["nested"].(map[string]any)["list"].([]any)[0] = "changed"

	gotAssertion, err := memory.ReadAssertion(memory.Memory{Metadata: output})
	if err != nil {
		t.Fatal(err)
	}
	if gotAssertion.Conditions["place"] != "home" || gotAssertion.EvidenceIDs[0] != "ep-1" {
		t.Fatalf("assertion metadata aliased caller input: %#v", gotAssertion)
	}
	gotDecision, err := memory.ReadDecision(memory.Memory{Metadata: output})
	if err != nil || gotDecision.Rationale[0] != "availability" {
		t.Fatalf("decision metadata aliased caller input: %#v, %v", gotDecision, err)
	}
	nested := output["nested"].(map[string]any)
	list := nested["list"].([]any)
	if list[0] != "unchanged" || list[1].(map[string]any)["value"] != "before" {
		t.Fatalf("custom metadata aliased caller input: %#v", output["nested"])
	}
}

func TestDecisionMetadataValidation(t *testing.T) {
	decision := memory.DecisionMetadata{
		Subject: "deployment", Outcome: "use replicas", Status: memory.DecisionActive,
		Rationale: []string{"availability"}, EvidenceIDs: []string{"ep-2"},
		Authority: "operator",
	}
	meta, err := memory.WithKnowledgeMetadata(nil, nil, &decision)
	if err != nil {
		t.Fatal(err)
	}
	got, err := memory.ReadDecision(memory.Memory{Metadata: meta})
	if err != nil || got == nil || got.Status != memory.DecisionActive {
		t.Fatalf("decision round trip = %#v, %v", got, err)
	}
	decision.Status = "guessed"
	if _, err := memory.WithKnowledgeMetadata(nil, nil, &decision); err == nil {
		t.Fatal("invalid decision status was accepted")
	}
}

func TestAssertionConfidenceMustBeFiniteAndBounded(t *testing.T) {
	for _, confidence := range []float64{-0.1, 1.1, math.Inf(1), math.NaN()} {
		assertion := memory.Assertion{
			Subject: "user", Predicate: "likes", Object: "tea",
			EpistemicType: memory.EpistemicFact, Confidence: confidence,
		}
		if _, err := memory.WithKnowledgeMetadata(nil, &assertion, nil); err == nil {
			t.Fatalf("confidence %v accepted", confidence)
		}
	}
}

func TestValidateKnowledgeRejectsMalformedAssertion(t *testing.T) {
	m := memory.Memory{Metadata: map[string]any{
		memory.AssertionMetadataKey: map[string]any{
			"subject": " ", "predicate": "likes", "object": "tea",
			"epistemic_type": "fact", "confidence": 0.4,
		},
	}}
	err := memory.ValidateKnowledge(&m)
	if err == nil || !strings.Contains(err.Error(), "subject is empty") {
		t.Fatalf("error = %v, want empty subject", err)
	}
}
