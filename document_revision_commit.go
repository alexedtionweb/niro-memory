package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

func cloneDocumentMemory(m memory.Memory) (memory.Memory, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return memory.Memory{}, err
	}
	var result memory.Memory
	err = json.Unmarshal(raw, &result)
	return result, err
}

func documentIDsFilter(ids []string, op memory.Op) memory.Filter {
	var groups []memory.Filter
	for start := 0; start < len(ids); start += 1000 {
		groups = append(groups, memory.Cmp("id", op, ids[start:min(start+1000, len(ids))]))
	}
	if len(groups) == 0 {
		return memory.Cmp("id", op, []string{})
	}
	if op == memory.OpNin {
		return memory.And(groups...)
	}
	return memory.Or(groups...)
}

func documentAssets(doc string, ids []string) memory.Filter {
	return memory.And(memory.Or(memory.Eq("metadata.document_id", doc), documentIDsFilter(ids, memory.OpIn)),
		memory.Cmp("source", memory.OpIn, []string{"document", "knowledge_enriched", "media_interpreted"}))
}

func (e *Engine) prepareDocumentRevision(ctx context.Context, tenant string, r DocumentRevision) (documentRevisionDraft, error) {
	as := memory.Principal{Admin: true}
	ep, err := e.documentSource(ctx, tenant, as, r.DocumentID)
	if err != nil {
		return documentRevisionDraft{}, err
	}
	status, err := e.DocumentStatus(ctx, tenant, as, r.DocumentID)
	if err != nil {
		return documentRevisionDraft{}, err
	}
	root, active, err := e.activeDocumentRevision(ctx, tenant, ep, *status)
	if err != nil {
		return documentRevisionDraft{}, err
	}
	if active.ID != r.Base {
		return documentRevisionDraft{}, fmt.Errorf("%w: active revision changed; request a fresh reprocessing", memory.ErrConflict)
	}
	activeIDs := append(append([]string{}, active.Chunks...), active.Enriched...)
	filter := memory.And(documentAssets(r.DocumentID, activeIDs), memory.Eq("user_id", ep.User), memory.Eq("agent_id", ep.Agent))
	if err := filter.Check(); err != nil {
		return documentRevisionDraft{}, err
	}
	previous, err := e.o.Store.Find(ctx, store.Query{Tenant: tenant, Limit: 4097, Filter: filter})
	if err != nil {
		return documentRevisionDraft{}, err
	}
	if len(previous) > 4096 {
		return documentRevisionDraft{}, fmt.Errorf("%w: document revision exceeds 4096 assets", memory.ErrInvalid)
	}
	draft := documentRevisionDraft{Revision: r, Manifest: root, Previous: previous}
	byID := make(map[string]memory.Memory, len(previous))
	mapping := map[string]string{}
	var ids []string
	for _, m := range previous {
		if m.Visibility != memory.Private || len(m.Grants) > 0 {
			return draft, fmt.Errorf("%w: document assets with changed audience require review before snapshotting", memory.ErrInvalid)
		}
		byID[m.ID] = m
		mapping[m.ID] = "revision-item-" + memory.Hash(r.ID+":"+m.ID)
		ids = append(ids, m.ID)
	}
	if slices.Contains(r.Stages, "chunks") {
		if e.o.Documents == nil {
			return draft, fmt.Errorf("%w: document parser not configured", memory.ErrInvalid)
		}
		data, err := e.Blob(ctx, tenant, ep.Parts[0].Blob.SHA256)
		if err != nil {
			return draft, err
		}
		pages, err := e.o.Documents.Extract(ctx, ep.Parts[0].Name, data)
		if err != nil {
			return draft, err
		}
		if len(pages) == 0 || len(pages) > 100 {
			return draft, fmt.Errorf("%w: document must have 1..100 pages", memory.ErrInvalid)
		}
		size := 0
		lastSection, lastHeading := "", ""
		for i, page := range pages {
			size += len(page.Text)
			if size > 2<<20 || page.Number < 1 || !utf8.ValidString(page.Text) {
				return draft, fmt.Errorf("%w: invalid or oversized parsed document", memory.ErrInvalid)
			}
			for _, chunk := range documentChunks(page.Text) {
				if chunk.heading == "" {
					chunk.section, chunk.heading = lastSection, lastHeading
				} else {
					lastSection, lastHeading = chunk.section, chunk.heading
				}
				id := "revision-chunk-" + memory.Hash(fmt.Sprintf("%s:%d:%d", r.ID, i, chunk.start))
				ev := memory.Episode{ID: id + "-evidence", Scope: ep.Scope, Role: "event", Status: memory.Final, ClientMsgID: id + "-evidence", OccurredAt: r.CreatedAt,
					Metadata: map[string]any{"document_id": r.DocumentID, "revision_id": r.ID, "page": page.Number},
					Parts:    []memory.Part{{Kind: memory.PartText, Text: page.Text[chunk.start:chunk.end]}}}
				m := memory.Memory{ID: id, Scope: ep.Scope, Text: ev.Parts[0].Text, Title: chunk.heading, Visibility: memory.Private, Confidence: 1, Importance: .5,
					Metadata: map[string]any{"document_id": r.DocumentID, "revision_id": r.ID, "filename": ep.Parts[0].Name, "page": page.Number,
						"chunk": len(draft.Chunks) + 1, "section": chunk.section, "section_title": chunk.heading, "page_start": chunk.start, "page_end": chunk.end,
						"continuation": chunk.continuation, "navigation_only": chunk.navigation},
					Source: memory.Source{Kind: "document", Trust: memory.TrustNormal, Evidence: []memory.Evidence{{Episode: ev.ID, Part: 0, End: len(ev.Parts[0].Text)}}}}
				m.Entities = chunk.entities(r.DocumentID)
				pipeline.Prepare(&m, r.CreatedAt)
				if err := m.Validate(e.o.Limits); err != nil {
					return draft, err
				}
				draft.Chunks = append(draft.Chunks, m)
				draft.Episodes = append(draft.Episodes, ev)
				if len(draft.Chunks) > 4096 {
					return draft, fmt.Errorf("%w: document revision exceeds 4096 assets", memory.ErrInvalid)
				}
			}
		}
		if len(draft.Chunks) == 0 {
			return draft, fmt.Errorf("%w: document has no readable text", memory.ErrInvalid)
		}
	} else {
		for _, id := range active.Chunks {
			m, ok := byID[id]
			if !ok {
				return draft, fmt.Errorf("%w: an imported chunk was deleted, expired or superseded; rechunk before reprocessing", memory.ErrInvalid)
			}
			copy, err := cloneDocumentMemory(m)
			if err != nil {
				return draft, err
			}
			copy.ID, copy.Supersedes, copy.Version = mapping[id], id, m.Version+1
			copy.Metadata["document_id"], copy.Metadata["revision_id"] = r.DocumentID, r.ID
			if slices.Contains(r.Stages, "knowledge") && r.RepairOf == "" && copy.Metadata["extracted_entities"] != nil {
				raw, err := json.Marshal(copy.Metadata["extracted_entities"])
				if err != nil {
					return draft, err
				}
				var entities []knowledge.ExtractedEntity
				if err := json.Unmarshal(raw, &entities); err != nil {
					return draft, fmt.Errorf("%w: invalid prior entity annotations: %v", memory.ErrInvalid, err)
				}
				copy.Entities = slices.DeleteFunc(copy.Entities, func(ref memory.EntityRef) bool {
					return slices.ContainsFunc(entities, func(old knowledge.ExtractedEntity) bool {
						return memory.EntityKey(old.Name) == memory.EntityKey(ref.Name)
					})
				})
				delete(copy.Metadata, "extracted_entities")
				delete(copy.Metadata, "extraction")
			}
			ref := memory.EntityRef{Name: r.DocumentID, Type: "document"}
			if !slices.Contains(copy.Entities, ref) {
				copy.Entities = append(copy.Entities, ref)
			}
			pipeline.Prepare(&copy, r.CreatedAt)
			draft.Chunks = append(draft.Chunks, copy)
		}
	}
	for _, m := range previous {
		if slices.Contains(active.Chunks, m.ID) {
			continue
		}
		if slices.Contains(r.Stages, "knowledge") && r.RepairOf == "" {
			continue
		}
		if slices.Contains(r.Stages, "chunks") {
			return draft, fmt.Errorf("%w: rechunking requires regeneration of existing knowledge", memory.ErrInvalid)
		}
		copy, err := cloneDocumentMemory(m)
		if err != nil {
			return draft, err
		}
		copy.ID, copy.Supersedes, copy.Version = mapping[m.ID], m.ID, m.Version+1
		copy.Metadata["document_id"], copy.Metadata["revision_id"] = r.DocumentID, r.ID
		if meta, ok := copy.Metadata["enrichment"].(map[string]any); ok {
			if old, ok := meta["source_id"].(string); ok {
				meta["source_id"] = mapping[old]
			}
		}
		a, err := memory.ReadAssertion(copy)
		if err != nil {
			return draft, err
		}
		decision, err := memory.ReadDecision(copy)
		if err != nil {
			return draft, err
		}
		if a != nil {
			for i, id := range a.EvidenceIDs {
				if next := mapping[id]; next != "" {
					a.EvidenceIDs[i] = next
				}
			}
		}
		if decision != nil {
			for i, id := range decision.EvidenceIDs {
				if next := mapping[id]; next != "" {
					decision.EvidenceIDs[i] = next
				}
			}
		}
		if a != nil || decision != nil {
			meta, err := memory.WithKnowledgeMetadata(copy.Metadata, a, decision)
			if err != nil {
				return draft, err
			}
			copy.Metadata = meta
		}
		pipeline.Prepare(&copy, r.CreatedAt)
		draft.Enriched = append(draft.Enriched, copy)
	}
	if rs, ok := e.o.Store.(relations.Store); ok && len(ids) > 0 {
		draft.PreviousRelations, err = rs.RelationshipSets(ctx, tenant, ids)
		if err != nil {
			return draft, err
		}
		if len(draft.PreviousRelations) >= relations.MaxReadLinks {
			return draft, fmt.Errorf("%w: relationship snapshot exceeded read bound", memory.ErrInvalid)
		}
		present := map[string]bool{}
		for _, m := range append(append([]memory.Memory{}, draft.Chunks...), draft.Enriched...) {
			present[m.ID] = true
		}
		externalIDs := map[string]bool{}
		for _, l := range draft.PreviousRelations {
			if l.ValidTo != nil || l.InvalidatedAt != nil {
				continue
			}
			for _, id := range []string{l.Source, l.Target} {
				if mapping[id] == "" {
					externalIDs[id] = true
				}
			}
		}
		var externalTargets []string
		for id := range externalIDs {
			externalTargets = append(externalTargets, id)
		}
		external, err := e.o.Store.Get(ctx, tenant, externalTargets)
		if err != nil {
			return draft, err
		}
		validExternal := map[string]bool{}
		for _, m := range external {
			if current(m, e.o.Now()) && m.User == ep.User && m.Agent == ep.Agent && m.Visibility == memory.Private && len(m.Grants) == 0 {
				validExternal[m.ID] = true
				draft.ExternalEndpoints = append(draft.ExternalEndpoints, m)
			}
		}
		endpoint := func(id string) string {
			if next := mapping[id]; present[next] {
				return next
			}
			if validExternal[id] {
				return id
			}
			return ""
		}
		for _, l := range draft.PreviousRelations {
			if l.ValidTo != nil || l.InvalidatedAt != nil {
				continue
			}
			if externalIDs[l.Source] && !validExternal[l.Source] || externalIDs[l.Target] && !validExternal[l.Target] {
				return draft, fmt.Errorf("%w: an external relationship endpoint changed audience or is no longer current", memory.ErrInvalid)
			}
			source, target := endpoint(l.Source), endpoint(l.Target)
			if source == "" || target == "" {
				continue
			}
			if slices.Contains(r.Stages, "relationships") && l.Relation != "derived_from" && !externalIDs[l.Source] && !externalIDs[l.Target] {
				continue
			}
			l.ID, l.Source, l.Target = memory.Hash(r.ID+":"+l.ID), source, target
			l.At, l.ValidFrom, l.OperationKey = r.CreatedAt, r.CreatedAt, r.ID
			draft.Relations = append(draft.Relations, l)
		}
	}
	if !slices.Contains(r.Stages, "chunks") {
		reader := e.o.Store.(store.VectorTargets)
		present := map[string]bool{}
		for _, m := range append(append([]memory.Memory{}, draft.Chunks...), draft.Enriched...) {
			present[m.ID] = true
		}
		for start := 0; start < len(ids); start += 50 {
			var targets []vector.Target
			for _, id := range ids[start:min(start+50, len(ids))] {
				targets = append(targets, vector.Target{Kind: vector.TargetMemory, ID: id})
			}
			points, err := reader.TargetVectors(ctx, tenant, targets)
			if err != nil {
				return draft, err
			}
			for _, p := range points {
				if !present[mapping[p.Target.ID]] {
					continue
				}
				if slices.Contains(r.Stages, "embeddings") && r.RepairOf == "" && p.Model == e.o.Embedder.Model().ID {
					continue
				}
				p.Target.ID = mapping[p.Target.ID]
				draft.CopiedVectors = append(draft.CopiedVectors, p)
			}
		}
	}
	return draft, nil
}

func (e *Engine) revisionKnowledge(source memory.Memory, out knowledge.Enrichment, r DocumentRevision) ([]memory.Memory, []relations.Link, error) {
	var ms []memory.Memory
	var links []relations.Link
	for i, f := range out.Facts {
		a := &memory.Assertion{Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, EpistemicType: f.Type, Confidence: .5, EvidenceIDs: []string{source.ID}}
		if f.Negated {
			a.Conditions = map[string]string{"polarity": "negative"}
		} else {
			a.Conditions = map[string]string{"polarity": "positive"}
		}
		refs, entities := entityAnnotations(f, out.Entities)
		var decision *memory.DecisionMetadata
		if f.Type == memory.EpistemicDecision {
			decision = &memory.DecisionMetadata{Subject: f.Subject, Outcome: f.Quote, Status: memory.DecisionProposed, EvidenceIDs: []string{source.ID}}
		}
		meta, err := memory.WithKnowledgeMetadata(map[string]any{"document_id": r.DocumentID, "revision_id": r.ID, "extracted_entities": entities,
			"enrichment": map[string]any{"source_id": source.ID, "source_version": source.Version, "quote": f.Quote, "provider": out.Provider, "model": out.Model, "review": "unreviewed", "mode": "structured"}}, a, decision)
		if err != nil {
			return nil, nil, err
		}
		m := memory.Memory{ID: "revision-fact-" + memory.Hash(r.ID+":"+source.ID+":"+strconv.Itoa(i)), Scope: source.Scope, Text: f.Quote, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			Visibility: source.Visibility, Grants: source.Grants, Confidence: .5, Importance: source.Importance, Entities: refs, Metadata: meta, ValidFrom: source.ValidFrom, ExpiresAt: source.ExpiresAt,
			Source: memory.Source{Kind: "knowledge_enriched", Trust: source.Source.Trust, DerivedBy: out.Provider + "/" + out.Model, Evidence: source.Source.Evidence}}
		pipeline.Prepare(&m, r.CreatedAt)
		if err := m.Validate(e.o.Limits); err != nil {
			return nil, nil, err
		}
		ms = append(ms, m)
		links = append(links, relations.Link{ID: memory.Hash(m.ID + ":derived"), Tenant: source.Tenant, Source: m.ID, Target: source.ID, Relation: "derived_from",
			Reason: f.Quote, Confidence: .5, Provider: m.Source.DerivedBy, Origin: "inferred", At: r.CreatedAt, ValidFrom: r.CreatedAt, OperationKey: r.ID, Evidence: source.Source.Evidence})
	}
	return ms, links, nil
}

func (e *Engine) activateDocumentRevision(ctx context.Context, tenant string, r DocumentRevision, draft documentRevisionDraft, points []vector.Point) error {
	options, err := e.knowledgeOptions(ctx, tenant)
	if err != nil {
		return err
	}
	if string(payload(r.Models)) != string(payload(revisionModelNames(options, e))) {
		return e.revisionReview(ctx, tenant, r, fmt.Errorf("model or extraction schema changed during revision processing"))
	}
	now := e.o.Now()
	if r.Incomplete && r.RepairOf == "" {
		r.RepairRevisionID = "revision-" + memory.Hash(r.DocumentID+":repair:"+r.ID)
	}
	r.State, r.CompletedAt = "committed", &now
	r.Chunks, r.Enriched = []string{}, []string{}
	for _, m := range draft.Chunks {
		r.Chunks = append(r.Chunks, m.ID)
	}
	for _, m := range draft.Enriched {
		r.Enriched = append(r.Enriched, m.ID)
	}
	b := &store.Batch{Tenant: tenant, OperationKey: documentRevisionKey(r.ID) + ":complete", OperationHash: r.ID, Result: payload(r), Episodes: draft.Episodes,
		Memories: append(append([]memory.Memory{}, draft.Chunks...), draft.Enriched...), Relations: draft.Relations}
	if r.RepairRevisionID != "" {
		b.Jobs = append(b.Jobs, store.Job{Kind: JobDocumentRepair, Key: r.ID, Payload: payload(map[string]string{"document_id": r.DocumentID, "revision_id": r.ID})})
		e.o.Logger.Warn("document revision committing validated partial knowledge", "document_id", r.DocumentID, "revision_id", r.ID, "failed_sources", len(r.Issues), "repair_revision_id", r.RepairRevisionID)
	}
	for i := range b.Memories {
		pipeline.Prepare(&b.Memories[i], now)
	}
	for i := range b.Relations {
		b.Relations[i].At = now
	}
	var previousIDs []string
	for _, m := range draft.ExternalEndpoints {
		b.Checks = append(b.Checks, store.Check{ID: m.ID, Version: m.Version, Open: true})
	}
	for _, m := range draft.Previous {
		previousIDs = append(previousIDs, m.ID)
		b.Checks = append(b.Checks, store.Check{ID: m.ID, Version: m.Version, Open: true})
		b.Closes = append(b.Closes, store.Close{ID: m.ID, ValidTo: &now})
	}
	unexpected := memory.And(documentAssets(r.DocumentID, previousIDs), documentIDsFilter(previousIDs, memory.OpNin),
		memory.Eq("user_id", draft.Chunks[0].User), memory.Eq("agent_id", draft.Chunks[0].Agent),
		memory.Cmp("valid_to", memory.OpExists, false), memory.Cmp("invalidated_at", memory.OpExists, false))
	b.Checks = append(b.Checks, store.Check{NoMatch: &unexpected})
	root := memory.Memory{ID: "document-entity-" + memory.Hash(r.ID), Scope: draft.Chunks[0].Scope, Text: "Document: " + fmt.Sprint(draft.Chunks[0].Metadata["filename"]),
		Title: fmt.Sprint(draft.Chunks[0].Metadata["filename"]), Slot: documentManifestSlot(r.DocumentID), Visibility: memory.Private, Confidence: 1, Importance: .5,
		Source:   memory.Source{Kind: "document_manifest", Trust: memory.TrustNormal},
		Entities: []memory.EntityRef{{Name: r.DocumentID, Type: "document"}},
		Metadata: map[string]any{"document_id": r.DocumentID, "revision_id": r.ID}}
	if draft.Manifest != nil {
		b.Checks = append(b.Checks, store.Check{ID: draft.Manifest.ID, Version: draft.Manifest.Version, Open: true})
		b.Closes = append(b.Closes, store.Close{ID: draft.Manifest.ID, ValidTo: &now})
		root.Version, root.Supersedes = draft.Manifest.Version+1, draft.Manifest.ID
	} else {
		filter := memory.And(memory.Eq("slot", root.Slot), memory.Eq("user_id", root.User), memory.Eq("agent_id", root.Agent),
			memory.Cmp("valid_to", memory.OpExists, false), memory.Cmp("invalidated_at", memory.OpExists, false))
		b.Checks = append(b.Checks, store.Check{NoMatch: &filter})
	}
	pipeline.Prepare(&root, now)
	b.Memories = append(b.Memories, root)
	for _, l := range draft.PreviousRelations {
		if l.InvalidatedAt == nil && l.ValidTo == nil {
			b.RelationCloses = append(b.RelationCloses, relations.Close{ID: l.ID, At: now})
		}
	}
	for _, m := range b.Memories {
		if err := m.Validate(e.o.Limits); err != nil {
			return e.revisionReview(ctx, tenant, r, err)
		}
		b.Events = append(b.Events, memory.Event{MemoryID: m.ID, Action: memory.ActionAdd, New: m.Text, Reason: "document revision " + r.ID, At: now})
	}
	e.addPoints(b, points)
	_, err = e.o.Store.Write(ctx, b)
	if errors.Is(err, memory.ErrConflict) {
		return e.revisionReview(ctx, tenant, r, fmt.Errorf("source changed while preparing revision; previous knowledge remains active: %w", err))
	}
	return err
}
