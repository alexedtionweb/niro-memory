//go:build devui

package devui

import (
	"context"
	"encoding/json"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

type jobSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type sourceJob struct {
	store.Job
	File *jobSource `json:"file,omitempty"`
}

// jobSources hydrates only this bounded queue page, using exact IDs. Shared
// model-wide embedding work deliberately has no single-file owner.
func (d *dev) jobSources(ctx context.Context, jobs []store.Job) ([]sourceJob, error) {
	out := make([]sourceJob, len(jobs))
	for i, j := range jobs {
		out[i].Job = j
	}
	byTenant := map[string][]int{}
	for i, j := range jobs {
		byTenant[j.Tenant] = append(byTenant[j.Tenant], i)
	}
	for tenant, indexes := range byTenant {
		targets := map[int]string{}
		var ids []string
		for _, i := range indexes {
			j := jobs[i]
			var body struct {
				ID         string `json:"id"`
				DocumentID string `json:"document_id"`
			}
			if (j.Kind == niromem.JobDocumentReprocess || j.Kind == niromem.JobDocumentRepair) && json.Unmarshal(j.Payload, &body) == nil && body.DocumentID != "" {
				targets[i] = body.DocumentID
				continue
			}
			if len(j.Payload) > 0 && json.Unmarshal(j.Payload, &body) == nil && body.ID != "" {
				targets[i] = body.ID
				if j.Kind != niromem.JobDocumentImport && j.Kind != niromem.JobMediaInterpret {
					ids = append(ids, body.ID)
				}
			}
		}
		memories, err := d.o.Engine.Store().Get(ctx, tenant, ids)
		if err != nil {
			return nil, err
		}
		sources := map[string]string{}
		parents := map[string]string{}
		var parentIDs []string
		for _, m := range memories {
			if id, ok := m.Metadata["document_id"].(string); ok {
				sources[m.ID] = id
			} else if enrichment, ok := m.Metadata["enrichment"].(map[string]any); ok {
				if id, ok := enrichment["source_id"].(string); ok && id != "" {
					parents[m.ID] = id
					parentIDs = append(parentIDs, id)
				}
			}
		}
		if len(parentIDs) > 0 {
			parentsFound, err := d.o.Engine.Store().Get(ctx, tenant, parentIDs)
			if err != nil {
				return nil, err
			}
			for _, m := range parentsFound {
				if id, ok := m.Metadata["document_id"].(string); ok {
					sources[m.ID] = id
				}
			}
			for id, parent := range parents {
				sources[id] = sources[parent]
			}
		}
		var sourceIDs []string
		owner := map[int]string{}
		for i, target := range targets {
			source := sources[target]
			if jobs[i].Kind == niromem.JobDocumentImport || jobs[i].Kind == niromem.JobMediaInterpret || jobs[i].Kind == niromem.JobDocumentReprocess || jobs[i].Kind == niromem.JobDocumentRepair {
				source = target
			}
			if source != "" {
				owner[i] = source
				sourceIDs = append(sourceIDs, source)
			}
		}
		if len(sourceIDs) == 0 {
			continue
		}
		episodes, err := d.o.Engine.Store().Episodes(ctx, store.EpisodeQuery{Tenant: tenant, IDs: sourceIDs, Limit: len(sourceIDs)})
		if err != nil {
			return nil, err
		}
		files := map[string]*jobSource{}
		for _, ep := range episodes {
			if ep.Metadata["document"] == true && ep.Status != memory.Deleted && len(ep.Parts) == 1 && ep.Parts[0].Blob != nil {
				files[ep.ID] = &jobSource{ID: ep.ID, Name: ep.Parts[0].Name}
			}
		}
		for i, source := range owner {
			out[i].File = files[source]
		}
	}
	return out, nil
}
