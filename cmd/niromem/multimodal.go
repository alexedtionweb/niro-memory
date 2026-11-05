package main

import (
	"context"
	"fmt"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/connectors/multimodal"
)

type mediaInterpreter struct {
	provider *multimodal.Provider
	model    string
}

func (m mediaInterpreter) Name() string { return "ollama/" + m.model }
func (mediaInterpreter) Validate(name string, data []byte) error {
	return multimodal.Validate(name, data)
}
func (m mediaInterpreter) Extract(ctx context.Context, name string, data []byte) ([]niromem.DocumentPage, error) {
	result, err := m.provider.Interpret(ctx, multimodal.Input{Name: name, Data: data,
		Prompt: "Summarize the supplied media for memory. For video, describe BOTH visible sampled frames and audible content in separate Visual and Audio sections. For images describe visible content. For audio transcribe speech if present and describe other audible content. State uncertainty; do not infer missing details."})
	if err != nil {
		return nil, err
	}
	return []niromem.DocumentPage{{Number: 1, Text: result.Text, Model: "ollama/" + result.Model}}, nil
}

func configureMultimodal(cfg *Config, o *niromem.Options) error {
	if cfg.Multimodal.Use == "" {
		return nil
	}
	if cfg.Multimodal.Use != "ollama" {
		return fmt.Errorf("unknown multimodal provider %q (ollama)", cfg.Multimodal.Use)
	}
	if o.Blob == nil {
		return fmt.Errorf("multimodal ingestion requires durable blob storage")
	}
	var settings struct {
		BaseURL    string `json:"base_url"`
		Model      string `json:"model"`
		NumThread  int    `json:"num_thread"`
		NumCtx     int    `json:"num_ctx"`
		NumPredict int    `json:"num_predict"`
		TimeoutMS  int    `json:"timeout_ms"`
	}
	if err := cfg.Multimodal.Decode(&settings); err != nil {
		return err
	}
	if settings.Model == "" {
		settings.Model = "lfm3-omni:latest"
	}
	if settings.TimeoutMS < 0 || settings.TimeoutMS > 600000 {
		return fmt.Errorf("multimodal timeout_ms must be 0..600000")
	}
	provider, err := multimodal.New(multimodal.Options{BaseURL: settings.BaseURL, Model: settings.Model,
		NumThread: settings.NumThread, NumCtx: settings.NumCtx, NumPredict: settings.NumPredict,
		Timeout: time.Duration(settings.TimeoutMS) * time.Millisecond})
	if err != nil {
		return err
	}
	o.Media = mediaInterpreter{provider: provider, model: settings.Model}
	return nil
}
