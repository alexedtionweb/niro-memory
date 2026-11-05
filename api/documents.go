package api

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func (s *Server) documents(mux *http.ServeMux) {
	s.route(mux, "GET /v1/documents/{id}/graph", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		graph, err := s.Engine.DocumentGraph(r.Context(), id.Tenant, id.Principal, r.PathValue("id"), r.URL.Query().Get("revision_id"), r.URL.Query().Get("after_id"), qInt(r, "limit", 20))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, graph)
	})
	s.route(mux, "GET /v1/documents/{id}/revisions", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		if revision := r.URL.Query().Get("revision_id"); revision != "" {
			result, err := s.Engine.DocumentRevision(r.Context(), id.Tenant, id.Principal, r.PathValue("id"), revision)
			if err != nil {
				return err
			}
			return writeJSON(w, 200, result)
		}
		results, err := s.Engine.DocumentRevisions(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"revisions": results, "limit": 20})
	})
	s.route(mux, "POST /v1/documents/{id}/reprocess", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		if !id.Principal.Admin {
			return fmt.Errorf("%w: document reprocessing requires admin approval", errForbidden)
		}
		var body niromem.DocumentReprocessRequest
		if err := decode(r, &body); err != nil {
			return err
		}
		result, err := s.Engine.ReprocessDocument(r.Context(), id.Tenant, id.Principal, r.PathValue("id"), body)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusAccepted, result)
	})
	s.route(mux, "GET /v1/memories/{id}/document", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		result, err := s.Engine.MemoryDocument(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
	s.route(mux, "GET /v1/documents/{id}/preview", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		data, name, err := s.Engine.DocumentFile(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		contentType := http.DetectContentType(data)
		switch contentType {
		case "image/png", "image/jpeg", "application/pdf", "audio/wave", "audio/x-wav", "audio/mpeg", "audio/ogg", "video/mp4", "video/webm":
		default:
			switch strings.ToLower(filepath.Ext(name)) {
			case ".txt", ".md", ".markdown":
				contentType = "text/plain; charset=utf-8"
			case ".m4a":
				contentType = "audio/mp4"
			case ".mov":
				contentType = "video/quicktime"
			default:
				return fmt.Errorf("%w: inline preview unavailable for this format; download the original", memory.ErrInvalid)
			}
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		_, err = w.Write(data)
		return err
	})
	s.route(mux, "GET /v1/documents/{id}/file", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		data, name, err := s.Engine.DocumentFile(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, err = w.Write(data)
		return err
	})
	upload := func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Name  string `json:"name"`
			Bytes []byte `json:"bytes"`
			User  string `json:"user_id,omitempty"`
			Agent string `json:"agent_id,omitempty"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		id := identity(r)
		u, a, err := scope(id, body.User, body.Agent)
		if err != nil {
			return err
		}
		importSource := s.Engine.ImportDocument
		if r.URL.Path == "/v1/media" {
			importSource = s.Engine.ImportMedia
		}
		result, err := importSource(r.Context(), memory.Scope{Tenant: id.Tenant, User: u, Agent: a}, body.Name, body.Bytes)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusAccepted, result)
	}
	s.route(mux, "POST /v1/documents", upload)
	s.route(mux, "POST /v1/media", upload)
	s.route(mux, "GET /v1/documents/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		result, err := s.Engine.DocumentStatus(r.Context(), id.Tenant, id.Principal, r.PathValue("id"))
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	})
	s.route(mux, "GET /v1/documents", func(w http.ResponseWriter, r *http.Request) error {
		id := identity(r)
		u, a, err := scope(id, r.URL.Query().Get("user_id"), r.URL.Query().Get("agent_id"))
		if err != nil {
			return err
		}
		if !id.Principal.Admin && u == "" && a == "" {
			return fmt.Errorf("%w: document listing needs a bound identity", errForbidden)
		}
		eps, err := s.Engine.Store().Episodes(r.Context(), store.EpisodeQuery{Tenant: id.Tenant, User: u, Agent: a,
			Roles: []string{"user"}, Limit: 200, Desc: true})
		if err != nil {
			return err
		}
		results := []niromem.DocumentImport{}
		for _, ep := range eps {
			if ep.Metadata["document"] != true {
				continue
			}
			result, err := s.Engine.DocumentStatus(r.Context(), id.Tenant, id.Principal, ep.ID)
			if err != nil {
				return err
			}
			results = append(results, *result)
			if len(results) == 20 {
				break
			}
		}
		return writeJSON(w, 200, map[string]any{"documents": results})
	})
}
