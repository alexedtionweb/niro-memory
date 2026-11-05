package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

const JobKnowledgeEnrich = "knowledge.enrich"

type EnrichmentResult struct {
	SourceID   string                       `json:"source_id"`
	Status     string                       `json:"status"`
	Mode       string                       `json:"mode"`
	Extraction *knowledge.Enrichment        `json:"extraction,omitempty"`
	Changes    []pipeline.Change            `json:"changes,omitempty"`
	Relations  []relations.Link             `json:"relations,omitempty"`
	Error      string                       `json:"error,omitempty"`
	Reason     string                       `json:"reason,omitempty"`
	Duration   time.Duration                `json:"duration_ns,omitempty"`
	Profile    *knowledge.ExtractionProfile `json:"extraction_profile,omitempty"`
	Schema     json.RawMessage              `json:"extraction_schema,omitempty"`
}

func enrichmentKey(id string) string { return "enrichment:" + id }

// EnrichmentStatus is authorized against the source before reading any receipt
// or job. Derived IDs and job errors are never exposed across source audiences.
func (e *Engine) EnrichmentStatus(ctx context.Context, tenant string, as memory.Principal, id string) (*EnrichmentResult, error) {
	if _, err := e.Get(ctx, tenant, as, id); err != nil {
		return nil, err
	}
	options, err := e.knowledgeOptions(ctx, tenant)
	if err != nil {
		return nil, err
	}
	result := &EnrichmentResult{SourceID: id, Mode: options.Mode, Status: "not_scheduled"}
	if ops, ok := e.o.Store.(store.Operations); ok {
		rec, err := ops.Operation(ctx, tenant, enrichmentKey(id))
		if err == nil {
			if err := json.Unmarshal(rec.Result, result); err != nil {
				return nil, err
			}
			return result, nil
		}
		if !errors.Is(err, memory.ErrNotFound) {
			return nil, err
		}
	}
	js, err := e.o.Store.Jobs(ctx, store.JobQuery{Tenant: tenant, Kind: JobKnowledgeEnrich, Key: id, Limit: 1})
	if err != nil {
		return nil, err
	}
	for _, j := range js {
		if j.Key != id {
			continue
		}
		result.Status = "queued"
		if j.State == store.JobLeased {
			result.Status = "processing"
		}
		if j.State == store.JobDead {
			result.Status = "failed"
		}
		if j.LastError != "" {
			result.Error = j.LastError
		}
		return result, nil
	}
	if options.Mode == "disabled" {
		result.Status = "disabled"
	}
	return result, nil
}

func (e *Engine) handleKnowledgeEnrich(ctx context.Context, j store.Job) error {
	options, err := e.knowledgeOptions(ctx, j.Tenant)
	if err != nil {
		return err
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(j.Payload, &body); err != nil || body.ID == "" {
		return jobs.Permanent(fmt.Errorf("%w: enrichment needs a source ID", memory.ErrInvalid))
	}
	key := enrichmentKey(body.ID)
	hash := memory.Hash(body.ID)
	var result EnrichmentResult
	if replayed, err := e.replayOperation(ctx, j.Tenant, key, hash, &result); replayed || err != nil {
		return err
	}
	ms, err := e.o.Store.Get(ctx, j.Tenant, []string{body.ID})
	if err != nil {
		return err
	}
	if len(ms) != 1 || !current(ms[0], e.o.Now()) {
		result = EnrichmentResult{SourceID: body.ID, Mode: options.Mode, Status: "needs_review"}
		_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result)})
		if err != nil {
			return err
		}
		return nil
	}
	source := ms[0]
	result = EnrichmentResult{SourceID: source.ID, Mode: options.Mode}
	if options.Mode == "disabled" {
		result.Status = "disabled"
		_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result)})
		return err
	}
	enricher, ok := options.Planner.(knowledge.Enricher)
	if !ok {
		return jobs.Permanent(fmt.Errorf("selected knowledge planner does not support background enrichment"))
	}
	if profile, ok := options.Planner.(interface{ KnowledgeExtractionMode() string }); ok &&
		profile.KnowledgeExtractionMode() == "extractive" && source.Source.Kind == "document" {
		result.Status = "source_only"
		result.Reason = "Document passages are already indexed with section context; extractive copies would add no knowledge. No model call or derived memories."
		_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result),
			Checks: []store.Check{{ID: source.ID, Version: source.Version, Open: true}}})
		return err
	}
	if options.Admission != nil {
		callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
		decision, err := options.Admission.DecideAdmission(callCtx, knowledge.AdmissionInput{
			Scope: source.Scope, Text: source.Text, Trust: source.Source.Trust, Explicit: source.Source.Kind == "explicit"})
		cancel()
		if err != nil {
			return fmt.Errorf("enrichment admission: %w", err)
		}
		if decision != knowledge.AdmissionProcess {
			switch decision {
			case knowledge.AdmissionSkip, knowledge.AdmissionDefer, knowledge.AdmissionRequireConfirmation:
				result.Status = "admission:" + string(decision)
				_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result)})
				return err
			default:
				return jobs.Permanent(fmt.Errorf("%w: unsupported enrichment admission %q", memory.ErrInvalid, decision))
			}
		}
	}
	if len(source.Text) > 6000 {
		return jobs.Permanent(fmt.Errorf("%w: enrichment source exceeds 6000 bytes; split the input", memory.ErrInvalid))
	}
	input := knowledge.EnrichmentInput{Text: source.Text, Candidates: []memory.Memory{}, Profile: options.Extraction}
	// Candidate selection and every resulting link preserve exact owner/audience.
	candidates, err := e.o.Store.Find(ctx, store.Query{Tenant: j.Tenant, Limit: 64,
		Filter: memory.And(memory.Eq("user_id", source.User), memory.Eq("agent_id", source.Agent)), Order: store.NewestFirst})
	if err != nil {
		return err
	}
	contextBytes := 0
	for _, m := range candidates {
		if m.ID == source.ID || len(m.Text) > 1000 || !knowledgeAudienceMatches(m, knowledge.Input{Visibility: source.Visibility, Grants: source.Grants}) {
			continue
		}
		if contextBytes+len(m.Text) > 2000 {
			continue
		}
		contextBytes += len(m.Text)
		input.Candidates = append(input.Candidates, m)
		if len(input.Candidates) == 6 || len(input.Candidates) >= options.MaxCandidates {
			break
		}
	}
	// Persist the exact input and output together. A retried worker cannot
	// reinterpret candidate indexes after concurrent writes change their order.
	var saved struct {
		InputText  string                       `json:"text"`
		Candidates []memory.Memory              `json:"candidates"`
		Output     knowledge.Enrichment         `json:"output"`
		Duration   time.Duration                `json:"duration_ns"`
		Profile    *knowledge.ExtractionProfile `json:"profile,omitempty"`
		Schema     json.RawMessage              `json:"schema,omitempty"`
	}
	planKey := key + ":plan"
	planned, err := e.replayOperation(ctx, j.Tenant, planKey, hash, &saved)
	if err != nil {
		return err
	}
	start := time.Now()
	if !planned {
		callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
		out, err := enricher.Enrich(callCtx, input)
		cancel()
		if err == nil {
			out, err = knowledge.RecoverEnrichment(input, out)
			if err == nil && len(out.Facts) == 0 && len(out.Entities) == 0 && len(out.Issues) > 0 {
				err = fmt.Errorf("%w: no valid extraction records: %s", memory.ErrInvalid, out.Issues[0].Reason)
			}
		}
		if err != nil {
			var review *knowledge.ExtractionReviewRequired
			if errors.As(err, &review) || errors.Is(err, memory.ErrInvalid) {
				result.Status = "needs_review"
				result.Error = err.Error()
				result.Duration = time.Since(start)
				_, writeErr := e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result),
					Checks: []store.Check{{ID: source.ID, Version: source.Version, Open: true}}})
				if writeErr != nil {
					return writeErr
				}
				e.o.Logger.Warn("knowledge extraction needs review", "source_id", source.ID, "job", j.ID, "err", err)
				return nil
			}
			return fmt.Errorf("extract knowledge: %w", err)
		}
		saved.InputText, saved.Candidates, saved.Output = input.Text, input.Candidates, out
		saved.Profile = input.Profile
		saved.Schema, err = knowledge.ExtractionJSONSchema(input.Profile)
		if err != nil {
			return jobs.Permanent(err)
		}
		saved.Duration = time.Since(start)
		checks := []store.Check{{ID: source.ID, Version: source.Version, Open: true}}
		for _, m := range input.Candidates {
			checks = append(checks, store.Check{ID: m.ID, Version: m.Version, Open: true})
		}
		rec, err := e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: planKey, OperationHash: hash, Result: payload(saved), Checks: checks})
		if err != nil {
			return err
		}
		if rec.Replayed {
			return memory.ErrConflict
		}
	} else {
		input.Text, input.Candidates = saved.InputText, saved.Candidates
		input.Profile = saved.Profile
	}
	if err := knowledge.ValidateEnrichment(input, saved.Output); err != nil {
		return jobs.Permanent(err)
	}
	result.Extraction = &saved.Output
	result.Profile = saved.Profile
	result.Schema = saved.Schema
	result.Duration = saved.Duration
	result.Status = "completed"
	if len(saved.Output.Facts) == 0 && len(saved.Output.Entities) == 0 && len(saved.Output.Excerpts) == 0 {
		result.Status = "empty"
		result.Reason = "The model returned no structured records. Source text remains searchable; this is not proof the source contains no useful information."
	}
	if len(saved.Output.Issues) > 0 {
		result.Status = "partial"
		result.Reason = "Grounded results saved; rejected extraction records require review or document repair."
		e.o.Logger.Warn("knowledge extraction partially recovered", "source_id", source.ID, "issues", len(saved.Output.Issues))
	}
	for _, r := range saved.Output.Relations {
		target := input.Candidates[r.Candidate]
		currentTargets, err := e.o.Store.Get(ctx, j.Tenant, []string{target.ID})
		if err != nil {
			return err
		}
		if len(currentTargets) != 1 || !current(currentTargets[0], e.o.Now()) || currentTargets[0].Version != target.Version {
			result.Status = "needs_review"
			_, err = e.o.Store.Write(ctx, &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash, Result: payload(result),
				Checks: []store.Check{{ID: source.ID, Version: source.Version, Open: true}}})
			return err
		}
	}
	b := &store.Batch{Tenant: j.Tenant, OperationKey: key, OperationHash: hash,
		Checks: []store.Check{{ID: source.ID, Version: source.Version, Open: true}}}
	if options.Mode == "shadow" {
		result.Status = "shadow"
	} else if len(saved.Output.Excerpts) > 0 {
		proposals := make([]pipeline.Proposal, 0, len(saved.Output.Excerpts))
		for i, text := range saved.Output.Excerpts {
			m := memory.Memory{ID: memory.Hash(key + ":excerpt:" + strconv.Itoa(i)), Scope: source.Scope, Text: text,
				Confidence: source.Confidence, Importance: source.Importance, Visibility: source.Visibility, Grants: append([]string(nil), source.Grants...),
				ValidFrom: source.ValidFrom, ValidTo: source.ValidTo, ExpiresAt: source.ExpiresAt,
				Source: memory.Source{Kind: "knowledge_enriched", Actor: source.Source.Actor, Role: source.Source.Role, Trust: source.Source.Trust,
					DerivedBy: saved.Output.Provider + "/" + saved.Output.Model, Evidence: source.Source.Evidence},
				Metadata: map[string]any{"document_id": source.Metadata["document_id"], "revision_id": source.Metadata["revision_id"], "enrichment": map[string]any{"source_id": source.ID, "source_version": source.Version, "quote": text,
					"start": strings.Index(source.Text, text), "end": strings.Index(source.Text, text) + len(text),
					"provider": saved.Output.Provider, "model": saved.Output.Model, "review": "unreviewed", "mode": "extractive"}}}
			proposals = append(proposals, pipeline.Proposal{Memory: m})
		}
		result.Changes, err = e.rec.Plan(ctx, j.Tenant, proposals, b)
		if err != nil {
			return err
		}
		for _, change := range result.Changes {
			if change.Action == pipeline.ActionReject {
				return jobs.Permanent(fmt.Errorf("%w: extractive memory rejected: %s", memory.ErrInvalid, change.Reason))
			}
		}
		e.followUp(b, result.Changes)
	} else {
		facts := append([]knowledge.ExtractedFact(nil), saved.Output.Facts...)
		for _, entity := range saved.Output.Entities {
			associated := false
			for _, f := range saved.Output.Facts {
				if strings.Contains(strings.ToLower(f.Quote), strings.ToLower(entity.Name)) {
					associated = true
					break
				}
			}
			if !associated {
				facts = append(facts, knowledge.ExtractedFact{Subject: entity.Name, Predicate: "mentioned_as", Object: entity.Type,
					Quote: entity.Quote, Type: memory.EpistemicObservation})
			}
		}
		for i, f := range facts {
			now := e.o.Now()
			polarity := "positive"
			if f.Negated {
				polarity = "negative"
			}
			a := &memory.Assertion{Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, EpistemicType: f.Type, Confidence: .5, EvidenceIDs: []string{source.ID}, Conditions: map[string]string{"polarity": polarity}}
			var decision *memory.DecisionMetadata
			if f.Type == memory.EpistemicDecision {
				decision = &memory.DecisionMetadata{Subject: f.Subject, Outcome: f.Quote, Status: memory.DecisionProposed, EvidenceIDs: []string{source.ID}}
			}
			refs, entities := entityAnnotations(f, saved.Output.Entities)
			if i >= len(saved.Output.Facts) {
				refs = nil
				for _, entity := range entities {
					refs = append(refs, memory.EntityRef{Name: entity.Name, Type: entity.Type})
				}
			}
			meta, err := memory.WithKnowledgeMetadata(map[string]any{"document_id": source.Metadata["document_id"], "revision_id": source.Metadata["revision_id"], "extracted_entities": entities, "enrichment": map[string]any{
				"source_id": source.ID, "source_version": source.Version, "quote": f.Quote, "start": strings.Index(source.Text, f.Quote),
				"end":      strings.Index(source.Text, f.Quote) + len(f.Quote),
				"provider": saved.Output.Provider, "model": saved.Output.Model, "review": "unreviewed",
				"model_type":     f.ModelType,
				"entity_mention": i >= len(saved.Output.Facts),
			}}, a, decision)
			if err != nil {
				return jobs.Permanent(err)
			}
			m := memory.Memory{ID: memory.Hash(key + ":" + strconv.Itoa(i)), Scope: source.Scope, Text: f.Quote, Metadata: meta,
				Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, Confidence: .5, Importance: source.Importance,
				Visibility: source.Visibility, Grants: append([]string(nil), source.Grants...),
				ValidFrom: source.ValidFrom, ValidTo: source.ValidTo, ExpiresAt: source.ExpiresAt,
				Entities: refs,
				Source: memory.Source{Kind: "knowledge_enriched", Actor: source.Source.Actor, Role: source.Source.Role,
					Trust: source.Source.Trust, DerivedBy: saved.Output.Provider + "/" + saved.Output.Model, Evidence: source.Source.Evidence}}
			pipeline.Prepare(&m, now)
			if err := m.Validate(e.o.Limits); err != nil {
				return jobs.Permanent(err)
			}
			change, err := e.reconcileEnriched(ctx, b, m, f, entities, source)
			if err != nil {
				if errors.Is(err, memory.ErrInvalid) {
					return jobs.Permanent(err)
				}
				return err
			}
			result.Changes = append(result.Changes, change)
			b.Relations = append(b.Relations, relations.Link{ID: memory.Hash(key + ":source:" + strconv.Itoa(i)), Tenant: j.Tenant,
				Source: change.Memory.ID, Target: source.ID, Relation: "derived_from", Origin: "inferred", Confidence: .5, Reason: f.Quote,
				Provider: m.Source.DerivedBy, OperationKey: key, At: now, ValidFrom: m.ValidFrom, Evidence: m.Source.Evidence})
		}
		for i, r := range saved.Output.Relations {
			target := input.Candidates[r.Candidate]
			fact := result.Changes[r.Fact].Memory
			if fact.ID == target.ID {
				continue
			}
			b.Checks = append(b.Checks, store.Check{ID: target.ID, Version: target.Version, Open: true})
			b.Relations = append(b.Relations, relations.Link{ID: memory.Hash(key + ":relation:" + strconv.Itoa(i)), Tenant: j.Tenant,
				Source: fact.ID, Target: target.ID, Relation: r.Relation, Origin: "inferred", Confidence: .5, Reason: fact.Text,
				Provider: fact.Source.DerivedBy, OperationKey: key, At: e.o.Now(), ValidFrom: fact.ValidFrom, Evidence: fact.Source.Evidence})
		}
		e.followUp(b, result.Changes)
		result.Relations = b.Relations
	}
	b.Result = payload(result)
	_, err = e.o.Store.Write(ctx, b)
	return err
}
