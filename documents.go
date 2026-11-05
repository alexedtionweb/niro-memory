package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/store"
)

const JobDocumentImport = "document.import"
const JobMediaInterpret = "media.interpret"
const MaxDocumentBytes = 20 << 20

// DocumentParser is implemented by the optional documents connector. The
// memory core does not depend on PDF, DOCX or OCR tools.
type DocumentParser interface {
	Validate(string, []byte) error
	Extract(context.Context, string, []byte) ([]DocumentPage, error)
}

// MediaInterpreter returns model-generated text, never verified observations.
type MediaInterpreter interface {
	DocumentParser
	Name() string
}

type DocumentPage struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
	Model  string `json:"model,omitempty"`
}

type DocumentImport struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Pages      int      `json:"pages"`
	Memories   []string `json:"memories"`
	Error      string   `json:"error,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Model      string   `json:"model,omitempty"`
	Unreviewed bool     `json:"unreviewed,omitempty"`
}

// ImportDocument atomically records the original evidence and its durable job.
// Parsing, OCR and models never run on the upload request.
func (e *Engine) ImportDocument(ctx context.Context, scope memory.Scope, name string, data []byte) (*DocumentImport, error) {
	return e.importSource(ctx, scope, name, data, false)
}

// ImportMedia queues interpretation while retaining the original media bytes.
func (e *Engine) ImportMedia(ctx context.Context, scope memory.Scope, name string, data []byte) (*DocumentImport, error) {
	return e.importSource(ctx, scope, name, data, true)
}

func (e *Engine) importSource(ctx context.Context, scope memory.Scope, name string, data []byte, media bool) (*DocumentImport, error) {
	parser := e.o.Documents
	if media {
		parser = e.o.Media
	}
	if parser == nil || e.o.Blob == nil {
		return nil, fmt.Errorf("%w: document parser and blob storage must be configured", memory.ErrInvalid)
	}
	if _, ok := e.o.Store.(store.Operations); !ok {
		return nil, fmt.Errorf("%w: document import requires durable operation receipts", memory.ErrInvalid)
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if scope.Tenant == "" || name == "." || len(name) > 240 || len(data) == 0 || len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: provide a filename and a document of 1..20 MiB", memory.ErrInvalid)
	}
	if err := parser.Validate(name, data); err != nil {
		return nil, fmt.Errorf("%w: %v", memory.ErrInvalid, err)
	}
	partKind, contentType := memory.PartFile, "application/octet-stream"
	if media {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".png":
			partKind, contentType = memory.PartImage, "image/png"
		case ".jpg", ".jpeg":
			partKind, contentType = memory.PartImage, "image/jpeg"
		case ".wav":
			partKind, contentType = memory.PartAudio, "audio/wav"
		case ".mp3":
			partKind, contentType = memory.PartAudio, "audio/mpeg"
		case ".m4a":
			partKind, contentType = memory.PartAudio, "audio/mp4"
		case ".ogg":
			partKind, contentType = memory.PartAudio, "audio/ogg"
		case ".mp4":
			partKind, contentType = memory.PartVideo, "video/mp4"
		case ".mov":
			partKind, contentType = memory.PartVideo, "video/quicktime"
		case ".webm":
			partKind, contentType = memory.PartVideo, "video/webm"
		}
	}
	ref := blob.Ref(data, contentType)
	identity := any(struct {
		Scope     memory.Scope
		Name, SHA string
	}{scope, name, ref.SHA256})
	if media {
		identity = struct {
			Source any
			Kind   string
		}{identity, "media"}
	}
	hash, err := operationHash(identity)
	if err != nil {
		return nil, err
	}
	id := "doc-" + hash
	scope.Run = id
	result := DocumentImport{ID: id, Name: name, Status: "queued", Memories: []string{}}
	kind := JobDocumentImport
	if media {
		result.Kind, result.Model, result.Unreviewed = "media", e.o.Media.Name(), true
		kind = JobMediaInterpret
	}
	var prior DocumentImport
	if replay, err := e.replayOperation(ctx, scope.Tenant, "document.upload:"+id, hash, &prior); replay || err != nil {
		return &prior, err
	}
	ref, err = e.o.Blob.Put(ctx, scope.Tenant, data, ref.Mime)
	if err != nil {
		return nil, err
	}
	now := e.o.Now()
	ep := memory.Episode{ID: id, Scope: scope, Role: "user", Status: memory.Final,
		ClientMsgID: id, OccurredAt: now, Metadata: map[string]any{"document": true, "media": media, "model": result.Model},
		Parts: []memory.Part{{Kind: partKind, Name: name, Mime: ref.Mime, Blob: &ref}}}
	rec, err := e.o.Store.Write(ctx, &store.Batch{Tenant: scope.Tenant, OperationKey: "document.upload:" + id,
		OperationHash: hash, Result: payload(result), Episodes: []memory.Episode{ep},
		Jobs: []store.Job{{Kind: kind, Key: id, Payload: payload(map[string]string{"id": id}), NotBefore: now}}})
	if err != nil {
		return nil, err
	}
	if rec.Replayed {
		if err := json.Unmarshal(rec.Result, &result); err != nil {
			return nil, err
		}
	}
	return &result, nil
}

func (e *Engine) documentSource(ctx context.Context, tenant string, as memory.Principal, id string) (*memory.Episode, error) {
	eps, err := e.o.Store.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, IDs: []string{id}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(eps) != 1 || eps[0].Metadata["document"] != true || eps[0].Status == memory.Deleted ||
		len(eps[0].Parts) != 1 || eps[0].Parts[0].Blob == nil ||
		!as.Admin && (as.User == "" && as.Agent == "" || as.User != "" && as.User != eps[0].User || as.Agent != "" && as.Agent != eps[0].Agent) {
		return nil, memory.ErrNotFound
	}
	return &eps[0], nil
}

// DocumentStatus reports chunk ingestion separately from model enrichment.
func (e *Engine) DocumentStatus(ctx context.Context, tenant string, as memory.Principal, id string) (*DocumentImport, error) {
	ep, err := e.documentSource(ctx, tenant, as, id)
	if err != nil {
		return nil, err
	}
	result := &DocumentImport{ID: id, Name: ep.Parts[0].Name, Status: "queued", Memories: []string{}}
	kind := JobDocumentImport
	if ep.Metadata["media"] == true {
		result.Kind, result.Unreviewed, kind = "media", true, JobMediaInterpret
		result.Model, _ = ep.Metadata["model"].(string)
	}
	ops, ok := e.o.Store.(store.Operations)
	if !ok {
		return nil, fmt.Errorf("document import requires operation receipts")
	}
	rec, err := ops.Operation(ctx, tenant, "document.complete:"+id)
	if err == nil {
		if err := json.Unmarshal(rec.Result, result); err != nil {
			return nil, err
		}
		return result, nil
	}
	if !errors.Is(err, memory.ErrNotFound) {
		return nil, err
	}
	js, err := e.o.Store.Jobs(ctx, store.JobQuery{Tenant: tenant, Kind: kind, Key: id, Limit: 1})
	if err != nil {
		return nil, err
	}
	for _, j := range js {
		if j.State == store.JobLeased {
			result.Status = "processing"
		}
		if j.State == store.JobDead {
			result.Status = "failed"
		}
		if j.State == store.JobPaused || j.State == store.JobCancelled {
			result.Status = string(j.State)
		}
		result.Error = j.LastError
	}
	return result, nil
}

// DocumentFile returns the original only after checking its source audience.
func (e *Engine) DocumentFile(ctx context.Context, tenant string, as memory.Principal, id string) ([]byte, string, error) {
	ep, err := e.documentSource(ctx, tenant, as, id)
	if err != nil {
		return nil, "", err
	}
	data, err := e.Blob(ctx, tenant, ep.Parts[0].Blob.SHA256)
	return data, ep.Parts[0].Name, err
}

func (e *Engine) handleDocumentImport(ctx context.Context, job store.Job) error {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(job.Payload, &body); err != nil || body.ID == "" {
		return jobs.Permanent(fmt.Errorf("%w: document job needs an ID", memory.ErrInvalid))
	}
	ep, err := e.documentSource(ctx, job.Tenant, memory.Principal{Admin: true}, body.ID)
	if err != nil {
		return jobs.Permanent(err)
	}
	parser := e.o.Documents
	media := ep.Metadata["media"] == true
	if media {
		parser = e.o.Media
	}
	if parser == nil {
		return jobs.Permanent(fmt.Errorf("source parser/interpreter is not configured on this worker"))
	}
	var result DocumentImport
	if done, err := e.replayOperation(ctx, job.Tenant, "document.complete:"+body.ID, body.ID, &result); done || err != nil {
		return err
	}
	var pages []DocumentPage
	planned, err := e.replayOperation(ctx, job.Tenant, "document.parse:"+body.ID, body.ID, &pages)
	if err != nil {
		return err
	}
	if !planned {
		data, err := e.Blob(ctx, job.Tenant, ep.Parts[0].Blob.SHA256)
		if err != nil {
			return err
		}
		pages, err = parser.Extract(ctx, ep.Parts[0].Name, data)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if media && errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("media interpretation timed out: %w", err)
			}
			var temporary interface{ Temporary() bool }
			if media && errors.As(err, &temporary) && temporary.Temporary() {
				return fmt.Errorf("media interpretation temporarily unavailable: %w", err)
			}
			return jobs.Permanent(fmt.Errorf("parse document: %w", err))
		}
		if len(pages) == 0 || len(pages) > 100 {
			return jobs.Permanent(fmt.Errorf("document must contain 1..100 pages"))
		}
		total := 0
		for _, page := range pages {
			total += len(page.Text)
			if page.Number < 1 || !utf8.ValidString(page.Text) || total > 2<<20 {
				return jobs.Permanent(fmt.Errorf("invalid or oversized document text (2 MiB maximum)"))
			}
		}
		rec, err := e.o.Store.Write(ctx, &store.Batch{Tenant: job.Tenant, OperationKey: "document.parse:" + body.ID,
			OperationHash: body.ID, Result: payload(pages)})
		if err != nil {
			return err
		}
		if rec.Replayed {
			if err := json.Unmarshal(rec.Result, &pages); err != nil {
				return err
			}
		}
	}
	_, err = e.write(ctx, func() (*store.Batch, error) {
		result = DocumentImport{ID: body.ID, Name: ep.Parts[0].Name, Status: "ingested", Pages: len(pages), Memories: []string{}}
		if media {
			result.Kind, result.Model, result.Unreviewed = "media", pages[0].Model, true
			if result.Model == "" {
				return nil, jobs.Permanent(fmt.Errorf("media interpretation must report its actual model"))
			}
		}
		b := &store.Batch{Tenant: job.Tenant, OperationKey: "document.complete:" + body.ID, OperationHash: body.ID}
		var changes []pipeline.Change
		lastSection, lastHeading := "", ""
		for i, page := range pages {
			for _, chunk := range documentChunks(page.Text) {
				if chunk.heading == "" {
					chunk.section, chunk.heading = lastSection, lastHeading
					if chunk.start == 0 && lastHeading != "" {
						chunk.continuation = true
					}
				} else {
					lastSection, lastHeading = chunk.section, chunk.heading
				}
				text := page.Text[chunk.start:chunk.end]
				evidenceID := fmt.Sprintf("%s-%d-%d", body.ID, i, chunk.end)
				b.Episodes = append(b.Episodes, memory.Episode{ID: evidenceID, Scope: ep.Scope, Role: "event",
					Status: memory.Final, OccurredAt: ep.OccurredAt, ClientMsgID: evidenceID,
					Metadata: map[string]any{"document_id": body.ID, "page": page.Number},
					Parts:    []memory.Part{{Kind: memory.PartText, Text: text}}})
				m := memory.Memory{ID: "chunk-" + evidenceID, Scope: ep.Scope, Text: text, Title: chunk.heading,
					Visibility: memory.Private, Importance: .5, Confidence: 1,
					Metadata: map[string]any{"document_id": body.ID, "filename": ep.Parts[0].Name, "page": page.Number, "chunk": len(changes) + 1},
					Source: memory.Source{Kind: "document", Trust: memory.TrustNormal,
						Evidence: []memory.Evidence{{Episode: evidenceID, Part: 0, End: len(text)}}}}
				m.Metadata["page_start"], m.Metadata["page_end"] = chunk.start, chunk.end
				m.Metadata["section"], m.Metadata["section_title"] = chunk.section, chunk.heading
				m.Metadata["continuation"], m.Metadata["navigation_only"] = chunk.continuation, chunk.navigation
				m.Entities = chunk.entities(body.ID)
				if media {
					m.Source.Kind, m.Source.Trust, m.Source.DerivedBy = "media_interpreted", memory.TrustLow, result.Model
					m.Confidence = 0 // The interpreter does not provide calibrated confidence.
					m.Metadata["interpretation"] = "unreviewed"
					m.Metadata["original_episode"] = ep.ID
					m.Metadata["original_sha256"] = ep.Parts[0].Blob.SHA256
					b.Episodes[len(b.Episodes)-1].Parts[0].DerivedBy = result.Model
				}
				pipeline.Prepare(&m, e.o.Now())
				if err := m.Validate(e.o.Limits); err != nil {
					return nil, jobs.Permanent(fmt.Errorf("document chunk rejected: %w", err))
				}
				// Source chunks are document-specific evidence, not assertions to
				// merge with potentially differently visible existing memories.
				b.Memories = append(b.Memories, m)
				b.Events = append(b.Events, memory.Event{MemoryID: m.ID, Action: memory.ActionAdd, New: m.Text, Reason: "document import", At: e.o.Now()})
				changes = append(changes, pipeline.Change{Action: memory.ActionAdd, Memory: m})
				result.Memories = append(result.Memories, m.ID)
			}
		}
		if len(changes) == 0 {
			return nil, jobs.Permanent(fmt.Errorf("document contains no readable text"))
		}
		e.followUp(b, changes)
		b.Result = payload(result)
		return b, nil
	})
	return err
}
