package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

type DocumentNode struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Label       string         `json:"label"`
	MemoryID    string         `json:"memory_id,omitempty"`
	Model       string         `json:"model,omitempty"`
	Dims        int            `json:"dims,omitempty"`
	Modality    string         `json:"modality,omitempty"`
	ActiveModel bool           `json:"active_model,omitempty"`
	Version     int            `json:"version,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type DocumentEdge struct {
	Source   string `json:"source_id"`
	Target   string `json:"target_id"`
	Relation string `json:"relation"`
	Origin   string `json:"origin"`
}

type DocumentGraph struct {
	Document         DocumentImport   `json:"document"`
	Revision         DocumentRevision `json:"revision"`
	Original         memory.BlobRef   `json:"original"`
	PreviewURL       string           `json:"preview_url"`
	DownloadURL      string           `json:"download_url"`
	Nodes            []DocumentNode   `json:"nodes"`
	Edges            []DocumentEdge   `json:"edges"`
	Next             string           `json:"next_after_id,omitempty"`
	VectorsAvailable bool             `json:"vectors_available"`
	VectorReason     string           `json:"vector_reason,omitempty"`
	ActiveModel      string           `json:"active_model,omitempty"`
	TotalItems       int              `json:"total_items"`
	ReturnedItems    int              `json:"returned_items"`
	TotalChunks      int              `json:"total_chunks"`
	TotalEnriched    int              `json:"total_enriched"`
	IsCurrent        bool             `json:"is_current"`
}

type MemoryDocumentLink struct {
	DocumentID string `json:"document_id"`
	RevisionID string `json:"revision_id,omitempty"`
	GraphURL   string `json:"graph_url"`
}

func (e *Engine) MemoryDocument(ctx context.Context, tenant string, as memory.Principal, id string) (MemoryDocumentLink, error) {
	m, err := e.Get(ctx, tenant, as, id)
	if err != nil {
		return MemoryDocumentLink{}, err
	}
	for depth := 0; depth < 4; depth++ {
		if doc, ok := m.Metadata["document_id"].(string); ok && doc != "" {
			if _, err := e.documentSource(ctx, tenant, as, doc); err != nil {
				return MemoryDocumentLink{}, err
			}
			revision, _ := m.Metadata["revision_id"].(string)
			return MemoryDocumentLink{DocumentID: doc, RevisionID: revision, GraphURL: "/v1/documents/" + url.PathEscape(doc) + "/graph"}, nil
		}
		meta, ok := m.Metadata["enrichment"].(map[string]any)
		if !ok {
			return MemoryDocumentLink{}, memory.ErrNotFound
		}
		source, ok := meta["source_id"].(string)
		if !ok || source == m.ID {
			return MemoryDocumentLink{}, memory.ErrNotFound
		}
		m, err = e.Get(ctx, tenant, as, source)
		if err != nil {
			return MemoryDocumentLink{}, err
		}
	}
	return MemoryDocumentLink{}, fmt.Errorf("%w: document provenance chain exceeds four hops", memory.ErrInvalid)
}

func documentManifestSlot(id string) string { return "document:" + id }
func documentRevisionKey(id string) string  { return "document.revision:" + id }

// DocumentGraph reconstructs provenance for legacy and new imports. No writes
// or inference are needed; vectors are read from the primary store, not inferred
// from configured capabilities.
func (e *Engine) DocumentGraph(ctx context.Context, tenant string, as memory.Principal, id, revision, after string, limit int) (*DocumentGraph, error) {
	ep, err := e.documentSource(ctx, tenant, as, id)
	if err != nil {
		return nil, err
	}
	status, err := e.DocumentStatus(ctx, tenant, as, id)
	if err != nil {
		return nil, err
	}
	_, currentRevision, err := e.activeDocumentRevision(ctx, tenant, ep, *status)
	if err != nil {
		return nil, err
	}
	var selected DocumentRevision
	if revision == "original" {
		selected, err = e.originalDocumentRevision(ctx, tenant, ep, *status)
	} else if revision != "" {
		selected, err = e.DocumentRevision(ctx, tenant, as, id, revision)
		if err == nil && selected.State != "committed" {
			return nil, fmt.Errorf("%w: revision has not committed; inspect its status instead", memory.ErrInvalid)
		}
	} else {
		_, selected, err = e.activeDocumentRevision(ctx, tenant, ep, *status)
	}
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("%w: limit must be 1..50", memory.ErrInvalid)
	}
	ids := append(append([]string{}, selected.Chunks...), selected.Enriched...)
	start := 0
	if after != "" {
		index := slices.Index(ids, after)
		if index < 0 {
			return nil, fmt.Errorf("%w: cursor does not belong to this revision", memory.ErrInvalid)
		}
		start = index + 1
	}
	end := min(start+limit, len(ids))
	result := &DocumentGraph{Document: *status, Revision: selected, Original: *ep.Parts[0].Blob,
		PreviewURL: "/v1/documents/" + url.PathEscape(id) + "/preview", DownloadURL: "/v1/documents/" + url.PathEscape(id) + "/file",
		Nodes: []DocumentNode{{ID: id, Type: "document", Label: status.Name}}, Edges: []DocumentEdge{}, TotalItems: len(ids),
		TotalChunks: len(selected.Chunks), TotalEnriched: len(selected.Enriched), IsCurrent: selected.ID == currentRevision.ID}
	// The receipt's full ID list is unnecessary in a paginated graph response.
	result.Document.Memories = nil
	result.Revision.Chunks = nil
	result.Revision.Enriched = nil
	if end < len(ids) {
		result.Next = ids[end-1]
	}
	ms, err := e.o.Store.Get(ctx, tenant, ids[start:end])
	if err != nil {
		return nil, err
	}
	settings, err := e.Settings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	allowed := memory.And(memory.Readable(as), bindingFilter(settings, as))
	if err := allowed.Check(); err != nil {
		return nil, err
	}
	byID := make(map[string]memory.Memory, len(ms))
	var targets []vector.Target
	visible := map[string]bool{}
	for _, m := range ms {
		if !allowed.Match(&m) {
			continue
		}
		byID[m.ID] = m
		targets = append(targets, vector.Target{Kind: vector.TargetMemory, ID: m.ID})
		visible[m.ID] = true
	}
	for _, mid := range ids[start:end] {
		m, ok := byID[mid]
		if !ok {
			continue
		}
		kind := "chunk"
		if slices.Contains(selected.Enriched, mid) {
			kind = "enriched"
		}
		label := m.Title
		if label == "" {
			runes := []rune(m.Text)
			label = string(runes[:min(len(runes), 140)])
		}
		review := ""
		if kind == "enriched" {
			review = "unreviewed"
		}
		result.Nodes = append(result.Nodes, DocumentNode{ID: mid, Type: kind, Label: label, MemoryID: mid, Version: m.Version,
			Metadata: map[string]any{"page": m.Metadata["page"], "section": m.Metadata["section"], "chunk": m.Metadata["chunk"],
				"review": review, "valid_to": m.ValidTo, "invalidated_at": m.InvalidatedAt, "subject": m.Subject, "predicate": m.Predicate, "object": m.Object,
				"entities": m.Entities, "assertion": m.Metadata["assertion"], "extracted_entities": m.Metadata["extracted_entities"], "extraction": m.Metadata["extraction"], "extraction_issues": m.Metadata["extraction_issues"]}})
		relation := "contains"
		if kind == "enriched" {
			relation = "has_interpretation"
		}
		result.Edges = append(result.Edges, DocumentEdge{Source: id, Target: mid, Relation: relation, Origin: "provenance"})
		result.ReturnedItems++
	}
	config, err := e.VectorConfiguration(ctx, tenant)
	if err != nil {
		return nil, err
	}
	result.ActiveModel = config.Active
	if reader, ok := e.o.Store.(store.VectorTargets); ok {
		result.VectorsAvailable = true
		points, err := reader.TargetVectors(ctx, tenant, targets)
		if err != nil {
			return nil, err
		}
		for _, p := range points {
			nodeID := "embedding-" + memory.Hash(p.Model+":"+p.Target.Key())
			result.Nodes = append(result.Nodes, DocumentNode{ID: nodeID, Type: "embedding", Label: p.Model, Model: p.Model, Dims: len(p.Values),
				Modality: p.Modality, ActiveModel: p.Model == config.Active, MemoryID: p.Target.ID})
			result.Edges = append(result.Edges, DocumentEdge{Source: p.Target.ID, Target: nodeID, Relation: "represented_by", Origin: "provenance"})
		}
	} else {
		result.VectorReason = "The store does not support target-level vector inspection; absence cannot be inferred."
	}
	if reader, ok := e.o.Store.(relations.Store); ok && len(targets) > 0 {
		links, err := reader.RelationshipSets(ctx, tenant, ids[start:end])
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if visible[l.Source] && visible[l.Target] && l.InvalidatedAt == nil && l.ValidTo == nil {
				result.Edges = append(result.Edges, DocumentEdge{Source: l.Source, Target: l.Target, Relation: l.Relation, Origin: l.Origin})
			}
		}
	}
	return result, nil
}

func (e *Engine) activeDocumentRevision(ctx context.Context, tenant string, ep *memory.Episode, status DocumentImport) (*memory.Memory, DocumentRevision, error) {
	rows, err := e.o.Store.Find(ctx, store.Query{Tenant: tenant, Limit: 1, Filter: memory.And(memory.Eq("slot", documentManifestSlot(status.ID)),
		memory.Eq("user_id", ep.User), memory.Eq("agent_id", ep.Agent))})
	if err != nil {
		return nil, DocumentRevision{}, err
	}
	if len(rows) == 0 {
		baseline, err := e.originalDocumentRevision(ctx, tenant, ep, status)
		return nil, baseline, err
	}
	rid, ok := rows[0].Metadata["revision_id"].(string)
	if !ok {
		return nil, DocumentRevision{}, fmt.Errorf("document manifest has no revision ID")
	}
	var revision DocumentRevision
	replayed, err := e.replayOperation(ctx, tenant, documentRevisionKey(rid)+":complete", rid, &revision)
	if err != nil {
		return nil, revision, err
	}
	if !replayed {
		return nil, revision, fmt.Errorf("document manifest references missing committed revision")
	}
	return &rows[0], revision, nil
}

func (e *Engine) originalDocumentRevision(ctx context.Context, tenant string, ep *memory.Episode, status DocumentImport) (DocumentRevision, error) {
	baseline := DocumentRevision{ID: "original", DocumentID: status.ID, State: "committed", CreatedAt: ep.OccurredAt, Chunks: status.Memories, Stages: []string{"import"}}
	if len(status.Memories) == 0 {
		return baseline, nil
	}
	filter := memory.And(memory.Eq("source", "knowledge_enriched"), memory.Eq("user_id", ep.User), memory.Eq("agent_id", ep.Agent))
	if err := filter.Check(); err != nil {
		return baseline, err
	}
	derived, err := e.o.Store.Find(ctx, store.Query{Tenant: tenant, AllVersions: true, ShowExpired: true, Limit: 4097, Filter: filter})
	if err != nil {
		return baseline, err
	}
	if len(derived) > 4096 {
		return baseline, fmt.Errorf("%w: document exceeds 4096 original derived records", memory.ErrInvalid)
	}
	for _, m := range derived {
		if meta, ok := m.Metadata["enrichment"].(map[string]any); ok {
			if source, ok := meta["source_id"].(string); ok && slices.Contains(status.Memories, source) {
				baseline.Enriched = append(baseline.Enriched, m.ID)
			}
		}
	}
	return baseline, nil
}

type DocumentRevision struct {
	ID               string                    `json:"id"`
	DocumentID       string                    `json:"document_id"`
	Base             string                    `json:"base_revision"`
	Stages           []string                  `json:"stages"`
	State            string                    `json:"state"`
	Error            string                    `json:"error,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	CompletedAt      *time.Time                `json:"completed_at,omitempty"`
	Chunks           []string                  `json:"chunk_ids,omitempty"`
	Enriched         []string                  `json:"enriched_ids,omitempty"`
	Models           map[string]string         `json:"models,omitempty"`
	Progress         *DocumentProgress         `json:"progress,omitempty"`
	Incomplete       bool                      `json:"incomplete,omitempty"`
	Issues           []DocumentExtractionIssue `json:"issues,omitempty"`
	RepairOf         string                    `json:"repair_of,omitempty"`
	RepairTargets    []string                  `json:"repair_targets,omitempty"`
	RepairRevisionID string                    `json:"repair_revision_id,omitempty"`
	EmptySources     int                       `json:"empty_sources,omitempty"`
	ExtractedFacts   int                       `json:"extracted_facts,omitempty"`
}

type DocumentExtractionIssue struct {
	SourceID string                      `json:"source_id"`
	Details  []knowledge.ExtractionIssue `json:"details"`
}

type DocumentProgress struct {
	Stage     string `json:"stage"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

func (e *Engine) DocumentRevision(ctx context.Context, tenant string, as memory.Principal, doc, id string) (DocumentRevision, error) {
	ep, err := e.documentSource(ctx, tenant, as, doc)
	if err != nil {
		return DocumentRevision{}, err
	}
	if id == "original" {
		status, err := e.DocumentStatus(ctx, tenant, as, doc)
		if err != nil {
			return DocumentRevision{}, err
		}
		return e.originalDocumentRevision(ctx, tenant, ep, *status)
	}
	var result DocumentRevision
	for _, suffix := range []string{":complete", ":review", ""} {
		ok, err := e.replayOperation(ctx, tenant, documentRevisionKey(id)+suffix, id, &result)
		if err != nil {
			return result, err
		}
		if ok {
			if result.DocumentID != doc {
				return DocumentRevision{}, memory.ErrNotFound
			}
			if suffix == "" {
				jobs, err := e.o.Store.Jobs(ctx, store.JobQuery{Tenant: tenant, Kind: JobDocumentReprocess, Key: id, Limit: 1})
				if err != nil {
					return result, err
				}
				for _, job := range jobs {
					result.State = string(job.State)
					result.Error = job.LastError
					if job.State == store.JobLeased {
						result.State = "processing"
					}
				}
			}
			progress, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, Run: "document-progress:" + id, Limit: 1, Desc: true, Roles: []string{"event"}})
			if err != nil {
				return result, err
			}
			if len(progress) > 0 {
				raw, err := json.Marshal(progress[0].Metadata["progress"])
				if err != nil {
					return result, err
				}
				if err := json.Unmarshal(raw, &result.Progress); err != nil {
					return result, err
				}
			}
			return result, nil
		}
	}
	return result, memory.ErrNotFound
}

func (e *Engine) DocumentRevisions(ctx context.Context, tenant string, as memory.Principal, doc string) ([]DocumentRevision, error) {
	if _, err := e.documentSource(ctx, tenant, as, doc); err != nil {
		return nil, err
	}
	events, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, Run: "document-revisions:" + doc, Limit: 20, Desc: true, Roles: []string{"event"}})
	if err != nil {
		return nil, err
	}
	out := []DocumentRevision{}
	for _, ep := range events {
		id, ok := ep.Metadata["revision_id"].(string)
		if !ok {
			return nil, errors.New("document revision event has no ID")
		}
		r, err := e.DocumentRevision(ctx, tenant, as, doc, id)
		if err != nil {
			return nil, err
		}
		r.Chunks, r.Enriched = nil, nil
		out = append(out, r)
	}
	return out, nil
}
