package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/cache"
	"github.com/alexedtionweb/niro-memory/connectors/inference"
	knowledgeprovider "github.com/alexedtionweb/niro-memory/connectors/knowledge"
	"github.com/alexedtionweb/niro-memory/connectors/ollama"
	"github.com/alexedtionweb/niro-memory/connectors/openai"
	"github.com/alexedtionweb/niro-memory/connectors/postgres"
	"github.com/alexedtionweb/niro-memory/connectors/qdrant"
	"github.com/alexedtionweb/niro-memory/connectors/redis"
	"github.com/alexedtionweb/niro-memory/connectors/s3"
	"github.com/alexedtionweb/niro-memory/connectors/sqlite"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/rules"
)

// connectors builds the configured connectors. This switch is the only
// place that knows connector names: to add your own (OpenSearch, S3
// Vectors, …), copy this command and add a case.
func connectors(ctx context.Context, cfg *Config, o *niromem.Options) (closers []io.Closer, err error) {
	if err := cfg.validateKnowledge(); err != nil {
		return nil, err
	}
	switch cfg.Store.Use {
	case "sqlite":
		var opt struct {
			Path    string `json:"path"`
			Readers int    `json:"readers"`
		}
		opt.Path = "niromem.db"
		if err := cfg.Store.Decode(&opt); err != nil {
			return nil, err
		}
		s, err := sqlite.Open(ctx, sqlite.Options{Path: opt.Path, Readers: opt.Readers})
		if err != nil {
			return nil, err
		}
		o.Store, closers = s, append(closers, s)
	case "postgres":
		var opt struct {
			DSN      string `json:"dsn"`
			Schema   string `json:"schema"`
			MaxConns int32  `json:"max_conns"`
			RLS      bool   `json:"rls"`
			// HNSW indexes to ensure at startup (pgvector).
			VectorIndexes []struct {
				Model string `json:"model"`
				Dims  int    `json:"dims"`
			} `json:"vector_indexes"`
		}
		if err := cfg.Store.Decode(&opt); err != nil {
			return nil, err
		}
		s, err := postgres.Open(ctx, postgres.Options{DSN: opt.DSN, Schema: opt.Schema, MaxConns: opt.MaxConns, RLS: opt.RLS})
		if err != nil {
			return nil, err
		}
		o.Store, closers = s, append(closers, s)
		for _, vi := range opt.VectorIndexes {
			if err := s.EnsureVectorIndex(ctx, vi.Model, vi.Dims); err != nil {
				return closers, fmt.Errorf("vector index %s: %w", vi.Model, err)
			}
		}
	default:
		return nil, fmt.Errorf("unknown store %q (sqlite, postgres)", cfg.Store.Use)
	}

	switch cfg.Vector.Use {
	case "", "store":
		// The store's own index (flat scan in SQLite, pgvector in Postgres).
	case "qdrant":
		var opt qdrant.Options
		if err := cfg.Vector.Decode(&struct {
			URL    *string `json:"url"`
			APIKey *string `json:"api_key"`
			Prefix *string `json:"prefix"`
		}{&opt.URL, &opt.APIKey, &opt.Prefix}); err != nil {
			return closers, err
		}
		o.Vector = qdrant.New(opt)
	default:
		return closers, fmt.Errorf("unknown vector index %q (store, qdrant)", cfg.Vector.Use)
	}

	switch cfg.Blob.Use {
	case "":
	case "dir":
		var opt struct {
			Path string `json:"path"`
		}
		opt.Path = "blobs"
		if err := cfg.Blob.Decode(&opt); err != nil {
			return closers, err
		}
		o.Blob = blob.Dir{Root: opt.Path}
	case "memory":
		o.Blob = blob.NewMem()
	case "s3":
		var opt s3.Options
		if err := cfg.Blob.Decode(&struct {
			Endpoint     *string `json:"endpoint"`
			Region       *string `json:"region"`
			Bucket       *string `json:"bucket"`
			Prefix       *string `json:"prefix"`
			AccessKey    *string `json:"access_key"`
			SecretKey    *string `json:"secret_key"`
			SessionToken *string `json:"session_token"`
			PathStyle    *bool   `json:"path_style"`
		}{&opt.Endpoint, &opt.Region, &opt.Bucket, &opt.Prefix, &opt.AccessKey, &opt.SecretKey, &opt.SessionToken, &opt.PathStyle}); err != nil {
			return closers, err
		}
		b, err := s3.New(opt)
		if err != nil {
			return closers, err
		}
		o.Blob = b
	default:
		return closers, fmt.Errorf("unknown blob store %q (dir, memory, s3)", cfg.Blob.Use)
	}

	switch cfg.Cache.Use {
	case "", "lru":
		var opt struct {
			Size int `json:"size"`
		}
		opt.Size = 10000
		if err := cfg.Cache.Decode(&opt); err != nil {
			return closers, err
		}
		o.Cache = cache.NewLRU(opt.Size)
	case "redis":
		var opt redis.Options
		if err := cfg.Cache.Decode(&struct {
			URL    *string `json:"url"`
			Prefix *string `json:"prefix"`
		}{&opt.URL, &opt.Prefix}); err != nil {
			return closers, err
		}
		c, err := redis.New(opt)
		if err != nil {
			return closers, err
		}
		o.Cache, closers = c, append(closers, c)
	default:
		return closers, fmt.Errorf("unknown cache %q (lru, redis)", cfg.Cache.Use)
	}

	switch cfg.Embedder.Use {
	case "":
	case "hash":
		var opt struct {
			Dims int `json:"dims"`
		}
		opt.Dims = 256
		if err := cfg.Embedder.Decode(&opt); err != nil {
			return closers, err
		}
		o.Embedder = embed.Hash{Dims: opt.Dims}
	case "openai":
		var opt openai.Options
		if err := cfg.Embedder.Decode(&struct {
			BaseURL        *string      `json:"base_url"`
			APIKey         *string      `json:"api_key"`
			Model          *string      `json:"model"`
			Dims           *int         `json:"dims"`
			ID             *string      `json:"id"`
			SendDimensions *bool        `json:"send_dimensions"`
			QueryPrefix    *string      `json:"query_prefix"`
			DocumentPrefix *string      `json:"document_prefix"`
			BatchSize      *int         `json:"batch_size"`
			MaxRetries     *int         `json:"max_retries"`
			Headers        *http.Header `json:"headers"`
		}{&opt.BaseURL, &opt.APIKey, &opt.Model, &opt.Dims, &opt.ID, &opt.SendDimensions, &opt.QueryPrefix, &opt.DocumentPrefix, &opt.BatchSize, &opt.MaxRetries, &opt.Header}); err != nil {
			return closers, err
		}
		em, err := openai.New(opt)
		if err != nil {
			return closers, err
		}
		o.Embedder = em
	case "ollama":
		var opt ollama.Options
		if err := cfg.Embedder.Decode(&struct {
			BaseURL        *string `json:"base_url"`
			APIKey         *string `json:"api_key"`
			Model          *string `json:"model"`
			Dims           *int    `json:"dims"`
			ID             *string `json:"id"`
			SendDimensions *bool   `json:"send_dimensions"`
			QueryPrefix    *string `json:"query_prefix"`
			DocumentPrefix *string `json:"document_prefix"`
			BatchSize      *int    `json:"batch_size"`
			KeepAlive      *string `json:"keep_alive"`
			Truncate       *bool   `json:"truncate"`
		}{&opt.BaseURL, &opt.APIKey, &opt.Model, &opt.Dims, &opt.ID, &opt.SendDimensions, &opt.QueryPrefix, &opt.DocumentPrefix, &opt.BatchSize, &opt.KeepAlive, &opt.Truncate}); err != nil {
			return closers, err
		}
		em, err := ollama.New(opt)
		if err != nil {
			return closers, err
		}
		o.Embedder = em
	default:
		return closers, fmt.Errorf("unknown embedder %q (hash, openai, ollama)", cfg.Embedder.Use)
	}

	switch cfg.Relationships.Use {
	case "":
	case "openai", "ollama":
		opt := inference.Options{Protocol: cfg.Relationships.Use}
		if err := cfg.Relationships.Decode(&opt); err != nil {
			return closers, err
		}
		provider, err := inference.New(opt)
		if err != nil {
			return closers, err
		}
		o.Relationships = provider
	default:
		return closers, fmt.Errorf("unknown relationship provider %q (openai, ollama; Go integrations can supply any relations.Inferer)", cfg.Relationships.Use)
	}
	if cfg.Rules != "" {
		rs, err := rules.LoadFile(cfg.Rules)
		if err != nil {
			return closers, fmt.Errorf("rules: %w", err)
		}
		o.Rules = rs
	}
	o.Knowledge = niromem.KnowledgeOptions{
		Mode: cfg.Knowledge.Mode, MaxCandidates: cfg.Knowledge.MaxCandidates,
		MaxGraphHops: cfg.Knowledge.MaxGraphHops,
		Timeout:      time.Duration(cfg.Knowledge.TimeoutMS) * time.Millisecond,
		Fallback:     cfg.Knowledge.Fallback, Consolidation: cfg.Knowledge.Consolidation,
		BackgroundEnrichment: cfg.Knowledge.BackgroundEnrichment,
	}
	if cfg.Knowledge.Admission.Enabled {
		o.Knowledge.Admission = knowledge.AdmissionDeterministic{
			MaxTextBytes: cfg.Knowledge.Admission.MaxTextBytes,
		}
	}
	if cfg.Knowledge.Mode != "disabled" || cfg.knowledgeRuntime {
		planner, err := knowledgePlanner(cfg.Knowledge.Planner)
		if err != nil {
			return closers, err
		}
		o.Knowledge.Planner = planner
	}
	if cfg.knowledgeRuntime {
		o.KnowledgePlanners = map[string]knowledge.Planner{
			"deterministic": knowledge.Deterministic{},
			"configured":    o.Knowledge.Planner,
		}
		for name, choice := range cfg.Knowledge.Profiles {
			if name == "" || name == "configured" || name == "deterministic" {
				return closers, fmt.Errorf("knowledge: reserved or empty profile name %q", name)
			}
			planner, err := knowledgePlanner(choice)
			if err != nil {
				return closers, fmt.Errorf("knowledge profile %q: %w", name, err)
			}
			o.KnowledgePlanners[name] = planner
		}
	}

	if cfg.DebounceMS != 0 {
		o.Debounce = time.Duration(cfg.DebounceMS) * time.Millisecond
	}
	return closers, nil
}

func knowledgePlanner(choice Choice) (knowledge.Planner, error) {
	switch choice.Use {
	case "deterministic":
		return knowledge.Deterministic{}, nil
	case "openai", "ollama":
		var settings struct {
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
			Model   string `json:"model"`
			Think   *bool  `json:"think"`
		}
		if err := choice.Decode(&settings); err != nil {
			return nil, err
		}
		protocol := choice.Use
		if protocol == "openai" {
			protocol = knowledgeprovider.ProtocolOpenAI
		}
		return knowledgeprovider.New(knowledgeprovider.Options{
			Protocol: protocol, BaseURL: settings.BaseURL, APIKey: settings.APIKey, Model: settings.Model, Think: settings.Think,
		})
	default:
		return nil, fmt.Errorf("unknown knowledge planner %q", choice.Use)
	}
}
