package main

import (
	"encoding/json"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
)

func TestLFMTextExampleUsesEmbeddingGemmaAndDisablesMedia(t *testing.T) {
	cfg, err := loadConfig("../../configs/lfm2.5-350m.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var planner struct {
		Model            string `json:"model"`
		StagedExtraction bool   `json:"staged_extraction"`
		ExtractiveOnly   bool   `json:"extractive_only"`
		Think            *bool  `json:"think"`
		TimeoutMS        int    `json:"timeout_ms"`
		NumPredict       int    `json:"num_predict"`
	}
	if err := json.Unmarshal(cfg.Knowledge.Planner.Options["ollama"], &planner); err != nil {
		t.Fatal(err)
	}
	if cfg.Knowledge.Planner.Use != "ollama" || planner.Model != "qwen3.5:2b" || planner.StagedExtraction || planner.ExtractiveOnly || len(cfg.Knowledge.Profiles) != 0 ||
		planner.Think == nil || *planner.Think || planner.TimeoutMS != 1260000 || planner.NumPredict != 1536 {
		t.Fatalf("expected one text generation model: %+v", planner)
	}
	configured, err := knowledgePlanner(cfg.Knowledge.Planner)
	if err != nil {
		t.Fatalf("extractive planner configuration: %v", err)
	}
	if d, ok := configured.(interface{ KnowledgeExtractionMode() string }); !ok || d.KnowledgeExtractionMode() != "structured" {
		t.Fatal("structured extraction did not reach provider")
	}
	var opts niromem.Options
	if cfg.Multimodal.Use != "" {
		t.Fatal("text-only configuration must not enable media")
	}
	if err := configureMultimodal(cfg, &opts); err != nil || opts.Media != nil {
		t.Fatalf("media must remain disabled: %v", err)
	}
	var embedding struct {
		Model          string `json:"model"`
		ID             string `json:"id"`
		Dims           int    `json:"dims"`
		QueryPrefix    string `json:"query_prefix"`
		DocumentPrefix string `json:"document_prefix"`
		TimeoutMS      int    `json:"timeout_ms"`
		Truncate       bool   `json:"truncate"`
		ChunkSize      int    `json:"chunk_size"`
	}
	if err := json.Unmarshal(cfg.Embedder.Options["ollama"], &embedding); err != nil {
		t.Fatal(err)
	}
	if cfg.Embedder.Use != "ollama" || embedding.Model != "embeddinggemma-2:440m" || embedding.ID != "ollama/embeddinggemma-2:440m-chunk1024-mean@1" || embedding.Dims != 768 || embedding.ChunkSize != 1024 ||
		embedding.QueryPrefix != "task: search result | query: " || embedding.DocumentPrefix != "title: none | text: " || embedding.TimeoutMS != 600000 || embedding.Truncate {
		t.Fatalf("expected separate EmbeddingGemma vector space, retrieval prefixes and explicit no-truncation: %+v", embedding)
	}
	if !cfg.Documents.Enabled || !cfg.Knowledge.BackgroundEnrichment || cfg.Knowledge.TimeoutMS != 1200000 {
		t.Fatal("documents and asynchronous knowledge extraction must remain enabled")
	}
}
