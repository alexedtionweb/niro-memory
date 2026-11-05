package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

const JobDocumentReprocess = "document.reprocess"
const JobDocumentRepair = "document.repair"

type DocumentReprocessRequest struct {
	RequestID     string   `json:"request_id"`
	Stages        []string `json:"stages"`
	repairOf      string
	repairTargets []string
}

func revisionModelNames(options KnowledgeOptions, e *Engine) map[string]string {
	out := map[string]string{"knowledge_mode": options.Mode}
	if e.o.Embedder != nil {
		out["embeddings"] = e.o.Embedder.Model().ID
	}
	if p, ok := options.Planner.(interface{ KnowledgeProvider() (string, string) }); ok {
		provider, model := p.KnowledgeProvider()
		out["knowledge"] = provider + "/" + model
	}
	if p, ok := options.Planner.(interface{ KnowledgeExtractionMode() string }); ok {
		out["extraction_mode"] = p.KnowledgeExtractionMode()
	}
	if e.o.Relationships != nil {
		out["relationships"] = e.o.Relationships.Name()
	}
	out["schema"] = memory.Hash(string(payload(options.Extraction)))
	return out
}

// ReprocessDocument queues a new immutable candidate revision. Existing assets
// remain open until all selected work validates and a single activation commits.
func (e *Engine) ReprocessDocument(ctx context.Context, tenant string, as memory.Principal, doc string, req DocumentReprocessRequest) (*DocumentRevision, error) {
	ep, err := e.documentSource(ctx, tenant, as, doc)
	if err != nil {
		return nil, err
	}
	if req.RequestID == "" || len(req.RequestID) > 128 || len(req.Stages) == 0 || len(req.Stages) > 4 {
		return nil, fmt.Errorf("%w: request_id (1..128 bytes) and 1..4 unique stages required", memory.ErrInvalid)
	}
	seen := map[string]bool{}
	for _, s := range req.Stages {
		if !slices.Contains([]string{"chunks", "embeddings", "knowledge", "relationships"}, s) || seen[s] {
			return nil, fmt.Errorf("%w: unsupported or duplicate document stage %q", memory.ErrInvalid, s)
		}
		seen[s] = true
	}
	slices.Sort(req.Stages)
	rid := "revision-" + memory.Hash(doc+":"+req.RequestID)
	if prior, err := e.DocumentRevision(ctx, tenant, as, doc, rid); err == nil {
		if !slices.Equal(prior.Stages, req.Stages) {
			return nil, fmt.Errorf("%w: request_id was used with different stages", memory.ErrConflict)
		}
		return &prior, nil
	} else if !errors.Is(err, memory.ErrNotFound) {
		return nil, err
	}
	status, err := e.DocumentStatus(ctx, tenant, as, doc)
	if err != nil {
		return nil, err
	}
	if status.Status != "ingested" {
		return nil, fmt.Errorf("%w: import must finish before reprocessing", memory.ErrInvalid)
	}
	_, active, err := e.activeDocumentRevision(ctx, tenant, ep, *status)
	if err != nil {
		return nil, err
	}
	if req.repairOf != "" && active.ID != req.repairOf {
		return nil, fmt.Errorf("%w: repair base is no longer active", memory.ErrConflict)
	}
	options, err := e.knowledgeOptions(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if seen["knowledge"] {
		if options.Mode != "enforced" {
			return nil, fmt.Errorf("%w: document revision knowledge activation requires enforced mode", memory.ErrInvalid)
		}
		if _, ok := options.Planner.(knowledge.Enricher); !ok {
			return nil, fmt.Errorf("%w: structured enrichment provider is not configured", memory.ErrInvalid)
		}
		if p, ok := options.Planner.(interface{ KnowledgeExtractionMode() string }); ok && p.KnowledgeExtractionMode() == "extractive" {
			return nil, fmt.Errorf("%w: select a structured extraction provider before requesting knowledge reprocessing", memory.ErrInvalid)
		}
	}
	if seen["relationships"] && e.o.Relationships == nil {
		return nil, fmt.Errorf("%w: relationship inference provider is not configured", memory.ErrInvalid)
	}
	if seen["embeddings"] && e.o.Embedder == nil {
		return nil, fmt.Errorf("%w: embedder is not configured", memory.ErrInvalid)
	}
	if seen["chunks"] && ep.Metadata["media"] == true {
		return nil, fmt.Errorf("%w: media reinterpretation is not a document chunking stage", memory.ErrInvalid)
	}
	if seen["chunks"] && e.o.Embedder != nil && !seen["embeddings"] {
		return nil, fmt.Errorf("%w: rechunking requires the embeddings stage when an embedder is configured", memory.ErrInvalid)
	}
	if seen["chunks"] && len(active.Enriched) > 0 && !seen["knowledge"] {
		return nil, fmt.Errorf("%w: rechunking an enriched revision also requires knowledge regeneration", memory.ErrInvalid)
	}
	if _, ok := e.o.Store.(store.VectorTargets); !ok {
		return nil, fmt.Errorf("%w: revision snapshots require target-level vector reads", memory.ErrInvalid)
	}
	out := DocumentRevision{ID: rid, DocumentID: doc, Base: active.ID, State: "queued", Stages: req.Stages, CreatedAt: e.o.Now(), Models: revisionModelNames(options, e)}
	out.RepairOf, out.RepairTargets = req.repairOf, req.repairTargets
	scope := ep.Scope
	scope.Run = "document-revisions:" + doc
	event := memory.Episode{ID: rid, Scope: scope, Role: "event", Status: memory.Final, ClientMsgID: rid, OccurredAt: out.CreatedAt,
		Metadata: map[string]any{"document_id": doc, "revision_id": rid}, Parts: []memory.Part{{Kind: memory.PartText, Text: "Document revision requested: " + strings.Join(req.Stages, ", ")}}}
	b := &store.Batch{Tenant: tenant, OperationKey: documentRevisionKey(rid), OperationHash: rid, Result: payload(out), Episodes: []memory.Episode{event},
		Jobs: []store.Job{{Kind: JobDocumentReprocess, Key: rid, Payload: payload(map[string]string{"document_id": doc, "revision_id": rid})}}}
	rec, err := e.o.Store.Write(ctx, b)
	if err != nil {
		return nil, err
	}
	if rec.Replayed {
		if err := json.Unmarshal(rec.Result, &out); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

type documentRevisionDraft struct {
	Revision          DocumentRevision
	Manifest          *memory.Memory
	Previous          []memory.Memory
	Chunks            []memory.Memory
	Enriched          []memory.Memory
	Episodes          []memory.Episode
	Relations         []relations.Link
	PreviousRelations []relations.Link
	ExternalEndpoints []memory.Memory
	CopiedVectors     []vector.Point
}

func (e *Engine) revisionReview(ctx context.Context, tenant string, r DocumentRevision, err error) error {
	r.State, r.Error = "needs_review", err.Error()
	now := e.o.Now()
	r.CompletedAt = &now
	e.o.Logger.Warn("document revision needs review", "document_id", r.DocumentID, "revision_id", r.ID, "err", err)
	_, writeErr := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant, OperationKey: documentRevisionKey(r.ID) + ":review", OperationHash: r.ID, Result: payload(r)})
	return writeErr
}

func (e *Engine) handleDocumentReprocess(ctx context.Context, job store.Job) error {
	var body struct {
		DocumentID string `json:"document_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.DocumentID == "" || body.RevisionID == "" {
		return jobs.Permanent(fmt.Errorf("%w: invalid document revision job", memory.ErrInvalid))
	}
	as := memory.Principal{Admin: true}
	r, err := e.DocumentRevision(ctx, job.Tenant, as, body.DocumentID, body.RevisionID)
	if err != nil {
		return err
	}
	if r.State == "committed" || r.State == "needs_review" {
		return nil
	}
	options, err := e.knowledgeOptions(ctx, job.Tenant)
	if err != nil {
		return err
	}
	if string(payload(r.Models)) != string(payload(revisionModelNames(options, e))) {
		return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("model or extraction schema changed after this revision was queued; request a new revision"))
	}
	var draft documentRevisionDraft
	planKey := documentRevisionKey(r.ID) + ":draft"
	planned, err := e.replayOperation(ctx, job.Tenant, planKey, r.ID, &draft)
	if err != nil {
		return err
	}
	if !planned {
		draft, err = e.prepareDocumentRevision(ctx, job.Tenant, r)
		if err != nil {
			if errors.Is(err, memory.ErrInvalid) || errors.Is(err, memory.ErrConflict) {
				return e.revisionReview(ctx, job.Tenant, r, err)
			}
			return err
		}
		rec, err := e.o.Store.Write(ctx, &store.Batch{Tenant: job.Tenant, OperationKey: planKey, OperationHash: r.ID, Result: payload(draft)})
		if err != nil {
			return err
		}
		if rec.Replayed {
			if err := json.Unmarshal(rec.Result, &draft); err != nil {
				return err
			}
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	if slices.Contains(r.Stages, "knowledge") {
		for i, source := range draft.Chunks {
			if r.RepairOf != "" && !slices.Contains(r.RepairTargets, source.Supersedes) {
				continue
			}
			key := documentRevisionKey(r.ID) + ":knowledge:" + strconv.Itoa(i)
			var output knowledge.Enrichment
			done, err := e.replayOperation(ctx, job.Tenant, key, r.ID, &output)
			if err != nil {
				return err
			}
			if !done {
				if time.Now().After(deadline) {
					return jobs.Yield()
				}
				if options.Admission != nil {
					admissionKey := key + ":admission"
					var decision knowledge.AdmissionResult
					admitted, err := e.replayOperation(ctx, job.Tenant, admissionKey, r.ID, &decision)
					if err != nil {
						return err
					}
					if !admitted {
						callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
						decision, err = options.Admission.DecideAdmission(callCtx, knowledge.AdmissionInput{Scope: source.Scope, Text: source.Text, Trust: source.Source.Trust})
						cancel()
						if err != nil {
							return fmt.Errorf("document revision admission: %w", err)
						}
						if decision != knowledge.AdmissionProcess {
							return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("knowledge admission returned %q; prior revision remains active", decision))
						}
						if _, err := e.o.Store.Write(ctx, &store.Batch{Tenant: job.Tenant, OperationKey: admissionKey, OperationHash: r.ID, Result: payload(decision)}); err != nil {
							return err
						}
					}
				}
				callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
				output, err = options.Planner.(knowledge.Enricher).Enrich(callCtx, knowledge.EnrichmentInput{Text: source.Text, Profile: options.Extraction})
				cancel()
				if err != nil {
					var review *knowledge.ExtractionReviewRequired
					if errors.Is(err, memory.ErrInvalid) || errors.As(err, &review) {
						e.o.Logger.Warn("document chunk extraction incomplete", "document_id", r.DocumentID, "source_id", source.ID, "err", err)
						output = knowledge.Enrichment{Facts: []knowledge.ExtractedFact{}, Entities: []knowledge.ExtractedEntity{}, Relations: []knowledge.ExtractedRelation{},
							Issues: []knowledge.ExtractionIssue{{Stage: "extraction", Index: i, Reason: err.Error()}}}
					} else {
						return fmt.Errorf("knowledge chunk %d: %w", i+1, err)
					}
				}
				output, err = knowledge.RecoverEnrichment(knowledge.EnrichmentInput{Text: source.Text, Profile: options.Extraction}, output)
				if err != nil {
					return e.revisionReview(ctx, job.Tenant, r, err)
				}
				if len(output.Excerpts) > 0 {
					return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("structured revision received excerpts, not structured data"))
				}
				rec, err := e.saveDocumentStage(ctx, source.Scope, r, key, "knowledge", i+1, len(draft.Chunks), output)
				if err != nil {
					return err
				}
				if rec.Replayed {
					if err := json.Unmarshal(rec.Result, &output); err != nil {
						return err
					}
				}
			}
			derived, links, err := e.revisionKnowledge(source, output, r)
			if err != nil {
				return e.revisionReview(ctx, job.Tenant, r, err)
			}
			for _, m := range derived {
				if slices.ContainsFunc(draft.Enriched, func(old memory.Memory) bool {
					meta, ok := old.Metadata["enrichment"].(map[string]any)
					return ok && meta["source_id"] == source.ID && old.Hash == m.Hash && old.Text == m.Text
				}) {
					continue
				}
				draft.Enriched = append(draft.Enriched, m)
			}
			if len(draft.Chunks)+len(draft.Enriched) > 4096 {
				return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("%w: document revision exceeds 4096 assets", memory.ErrInvalid))
			}
			for _, link := range links {
				if slices.ContainsFunc(draft.Enriched, func(m memory.Memory) bool { return m.ID == link.Source }) {
					draft.Relations = append(draft.Relations, link)
				}
			}
			entities := append([]knowledge.ExtractedEntity(nil), output.Entities...)
			if r.RepairOf != "" && source.Metadata["extracted_entities"] != nil {
				raw, err := json.Marshal(source.Metadata["extracted_entities"])
				if err != nil {
					return err
				}
				var previous []knowledge.ExtractedEntity
				if err := json.Unmarshal(raw, &previous); err != nil {
					return fmt.Errorf("%w: invalid prior entity annotations: %v", memory.ErrInvalid, err)
				}
				for _, entity := range previous {
					if !slices.ContainsFunc(entities, func(next knowledge.ExtractedEntity) bool {
						return memory.EntityKey(next.Name) == memory.EntityKey(entity.Name)
					}) {
						entities = append(entities, entity)
					}
				}
			}
			for _, entity := range entities {
				ref := memory.EntityRef{Name: entity.Name, Type: entity.Type}
				draft.Chunks[i].Entities = slices.DeleteFunc(draft.Chunks[i].Entities, func(old memory.EntityRef) bool {
					return memory.EntityKey(old.Name) == memory.EntityKey(ref.Name) && old.Type != ref.Type && old.Type != "document"
				})
				if !slices.Contains(draft.Chunks[i].Entities, ref) {
					draft.Chunks[i].Entities = append(draft.Chunks[i].Entities, ref)
				}
			}
			draft.Chunks[i].Metadata["extracted_entities"] = entities
			factCount := 0
			for _, m := range draft.Enriched {
				if meta, ok := m.Metadata["enrichment"].(map[string]any); ok && meta["source_id"] == source.ID {
					factCount++
				}
			}
			extractionStatus := "extracted"
			if factCount == 0 && len(entities) == 0 {
				extractionStatus = "empty"
			}
			if len(output.Issues) > 0 {
				extractionStatus = "partial"
			}
			if r.RepairOf != "" && len(output.Facts) == 0 && len(output.Entities) == 0 && len(output.Issues) == 0 {
				output.Issues = append(output.Issues, knowledge.ExtractionIssue{Stage: "coverage", Reason: "Repair returned no structured records; prior extraction gaps remain unresolved."})
				extractionStatus = "partial"
			}
			draft.Chunks[i].Metadata["extraction"] = map[string]any{"provider": output.Provider, "model": output.Model, "review": "unreviewed", "notes": output.Notes, "status": extractionStatus, "fact_count": factCount}
			draft.Chunks[i].Metadata["extraction_issues"] = output.Issues
		}
	}
	r.Incomplete, r.Issues = false, nil
	r.EmptySources, r.ExtractedFacts = 0, 0
	for _, source := range draft.Chunks {
		if extraction, ok := source.Metadata["extraction"].(map[string]any); ok {
			if extraction["status"] == "empty" {
				r.EmptySources++
			}
			raw, err := json.Marshal(extraction["fact_count"])
			if err != nil {
				return err
			}
			var count int
			if err := json.Unmarshal(raw, &count); err != nil {
				return err
			}
			r.ExtractedFacts += count
		}
		if source.Metadata["extraction_issues"] == nil {
			continue
		}
		raw, err := json.Marshal(source.Metadata["extraction_issues"])
		if err != nil {
			return err
		}
		var details []knowledge.ExtractionIssue
		if err := json.Unmarshal(raw, &details); err != nil {
			return err
		}
		if len(details) > 0 {
			r.Incomplete = true
			r.Issues = append(r.Issues, DocumentExtractionIssue{SourceID: source.ID, Details: details})
		}
	}
	all := append(append([]memory.Memory{}, draft.Chunks...), draft.Enriched...)
	if slices.Contains(r.Stages, "relationships") {
		for i, source := range all {
			var candidates []memory.Memory
			for _, m := range all {
				if m.ID != source.ID {
					candidates = append(candidates, m)
				}
				if len(candidates) == 20 {
					break
				}
			}
			if len(candidates) == 0 {
				continue
			}
			key := documentRevisionKey(r.ID) + ":relations:" + strconv.Itoa(i)
			var proposals []relations.Proposal
			done, err := e.replayOperation(ctx, job.Tenant, key, r.ID, &proposals)
			if err != nil {
				return err
			}
			if !done {
				if time.Now().After(deadline) {
					return jobs.Yield()
				}
				callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
				proposals, err = e.o.Relationships.Infer(callCtx, relations.Input{Source: source, Candidates: candidates})
				cancel()
				if err != nil {
					return err
				}
				if err := validateRevisionRelations(proposals, candidates); err != nil {
					return e.revisionReview(ctx, job.Tenant, r, err)
				}
				rec, err := e.saveDocumentStage(ctx, source.Scope, r, key, "relationships", i+1, len(all), proposals)
				if err != nil {
					return err
				}
				if rec.Replayed {
					if err := json.Unmarshal(rec.Result, &proposals); err != nil {
						return err
					}
				}
			}
			for j, p := range proposals {
				draft.Relations = append(draft.Relations, relations.Link{ID: memory.Hash(key + ":" + strconv.Itoa(j)), Tenant: job.Tenant, Source: source.ID, Target: p.Target,
					Relation: p.Relation, Reason: p.Reason, Confidence: p.Confidence, Provider: e.o.Relationships.Name(), Origin: "inferred", At: r.CreatedAt, ValidFrom: r.CreatedAt,
					OperationKey: r.ID, Evidence: source.Source.Evidence})
			}
		}
	}
	points := draft.CopiedVectors
	if slices.Contains(r.Stages, "embeddings") {
		embeddingAssets := all
		if r.RepairOf != "" {
			embeddingAssets = nil
			for _, m := range all {
				if !slices.ContainsFunc(points, func(p vector.Point) bool { return p.Target.ID == m.ID && p.Model == e.o.Embedder.Model().ID }) {
					embeddingAssets = append(embeddingAssets, m)
				}
			}
		}
		for start := 0; start < len(embeddingAssets); start += 16 {
			end := min(start+16, len(embeddingAssets))
			key := documentRevisionKey(r.ID) + ":vectors:" + strconv.Itoa(start)
			var vs [][]float32
			done, err := e.replayOperation(ctx, job.Tenant, key, r.ID, &vs)
			if err != nil {
				return err
			}
			if !done {
				if time.Now().After(deadline) {
					return jobs.Yield()
				}
				input := make([]embed.Input, end-start)
				for i, m := range embeddingAssets[start:end] {
					input[i] = embed.Input{Modality: "text", Text: m.Title + "\n" + m.Text}
				}
				vs, _, err = e.o.Embedder.Embed(ctx, embed.Document, input)
				if err != nil {
					return err
				}
				if len(vs) != len(input) {
					return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vs), len(input)))
				}
				model := e.o.Embedder.Model()
				for _, v := range vs {
					if len(v) != model.Dims {
						return e.revisionReview(ctx, job.Tenant, r, vector.ErrDims)
					}
					copy := append([]float32(nil), v...)
					if err := vector.Normalize(copy); err != nil {
						return e.revisionReview(ctx, job.Tenant, r, fmt.Errorf("invalid embedding: %w", err))
					}
				}
				rec, err := e.saveDocumentStage(ctx, embeddingAssets[start].Scope, r, key, "embeddings", end, len(embeddingAssets), vs)
				if err != nil {
					return err
				}
				if rec.Replayed {
					if err := json.Unmarshal(rec.Result, &vs); err != nil {
						return err
					}
				}
			}
			for i, m := range embeddingAssets[start:end] {
				points = append(points, vector.Point{Tenant: job.Tenant, Model: e.o.Embedder.Model().ID, Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, Modality: "text",
					User: m.User, Agent: m.Agent, Run: m.Run, Values: vs[i]})
			}
		}
	}
	return e.activateDocumentRevision(ctx, job.Tenant, r, draft, points)
}

func (e *Engine) handleDocumentRepair(ctx context.Context, job store.Job) error {
	var body struct {
		DocumentID string `json:"document_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.DocumentID == "" || body.RevisionID == "" {
		return jobs.Permanent(fmt.Errorf("%w: invalid document repair payload", memory.ErrInvalid))
	}
	parent, err := e.DocumentRevision(ctx, job.Tenant, memory.Principal{Admin: true}, body.DocumentID, body.RevisionID)
	if err != nil {
		return err
	}
	var targets []string
	for _, issue := range parent.Issues {
		targets = append(targets, issue.SourceID)
	}
	if !parent.Incomplete || len(targets) == 0 {
		return nil
	}
	stages := []string{"knowledge"}
	if e.o.Embedder != nil {
		stages = append(stages, "embeddings")
	}
	_, err = e.ReprocessDocument(ctx, job.Tenant, memory.Principal{Admin: true}, body.DocumentID, DocumentReprocessRequest{RequestID: "repair:" + parent.ID, Stages: stages, repairOf: parent.ID, repairTargets: targets})
	if errors.Is(err, memory.ErrConflict) {
		e.o.Logger.Warn("document repair skipped after active revision changed", "document_id", body.DocumentID, "revision_id", parent.ID, "err", err)
		return nil
	}
	return err
}

func (e *Engine) saveDocumentStage(ctx context.Context, scope memory.Scope, r DocumentRevision, key, stage string, completed, total int, value any) (*store.Receipt, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("serialize document stage: %w", err)
	}
	scope.Run = "document-progress:" + r.ID
	id := "progress-" + memory.Hash(key)
	progress := DocumentProgress{Stage: stage, Completed: completed, Total: total}
	return e.o.Store.Write(ctx, &store.Batch{Tenant: scope.Tenant, OperationKey: key, OperationHash: r.ID, Result: raw,
		Episodes: []memory.Episode{{ID: id, ClientMsgID: id, Scope: scope, Role: "event", Status: memory.Final, OccurredAt: e.o.Now(),
			Metadata: map[string]any{"document_id": r.DocumentID, "revision_id": r.ID, "progress": progress},
			Parts:    []memory.Part{{Kind: memory.PartText, Text: fmt.Sprintf("%s: %d/%d", stage, completed, total)}}}}})
}

func validateRevisionRelations(proposals []relations.Proposal, candidates []memory.Memory) error {
	if len(proposals) > 20 {
		return fmt.Errorf("%w: more than 20 relationships", memory.ErrInvalid)
	}
	for _, p := range proposals {
		found := false
		for _, c := range candidates {
			found = found || c.ID == p.Target
		}
		if !found || !relations.ValidRelation(p.Relation) || strings.TrimSpace(p.Reason) == "" || len(p.Reason) > 2000 || math.IsNaN(p.Confidence) || p.Confidence <= 0 || p.Confidence > 1 {
			return fmt.Errorf("%w: invalid relationship target, type, evidence or confidence", memory.ErrInvalid)
		}
	}
	return nil
}
