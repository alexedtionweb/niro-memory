package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
)

func TestMultimodalConfigurationAndAdapter(t *testing.T) {
	cfg := &Config{}
	var opts niromem.Options
	if err := configureMultimodal(cfg, &opts); err != nil || opts.Media != nil {
		t.Fatalf("disabled: %v", err)
	}
	cfg.Multimodal.Use = "ollama"
	if err := configureMultimodal(cfg, &opts); err == nil {
		t.Fatal("accepted missing blob")
	}
	opts.Blob = blob.NewMem()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion", "vision", "audio"}})
			return
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "unknown", 404)
			return
		}
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode chat: %v", err)
		} else if len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Content, "Visual and Audio sections") {
			t.Error("missing joint visual/audio interpretation instruction")
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "lfm3-omni:latest", "done": true, "done_reason": "stop",
			"message": map[string]string{"role": "assistant", "content": "A red square is visible."}})
	}))
	defer server.Close()
	settings, _ := json.Marshal(map[string]any{"model": "lfm3-omni:latest", "base_url": server.URL})
	cfg.Multimodal.Options = map[string]json.RawMessage{"ollama": settings}
	if err := configureMultimodal(cfg, &opts); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	pages, err := opts.Media.Extract(context.Background(), "square.png", data.Bytes())
	if err != nil || len(pages) != 1 || pages[0].Model != "ollama/lfm3-omni:latest" || !strings.Contains(pages[0].Text, "red square") {
		t.Fatalf("adapter: %+v %v", pages, err)
	}
}

func TestLFM3ExampleUsesOmniForKnowledgeAndMedia(t *testing.T) {
	cfg, err := loadConfig("../../configs/lfm3-omni.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range []Choice{cfg.Knowledge.Planner, cfg.Multimodal} {
		var opt struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(choice.Options["ollama"], &opt); err != nil {
			t.Fatal(err)
		}
		if opt.Model != "lfm3-omni:latest" {
			t.Fatalf("unexpected model %q", opt.Model)
		}
	}
	if cfg.Knowledge.TimeoutMS != 600000 || cfg.Worker.Concurrency < 1 || !cfg.Knowledge.BackgroundEnrichment {
		t.Fatal("omni config must tolerate cold CPU loads without blocking saves and have an active worker")
	}
	var media struct {
		TimeoutMS int `json:"timeout_ms"`
	}
	if err := json.Unmarshal(cfg.Multimodal.Options["ollama"], &media); err != nil || media.TimeoutMS != 600000 {
		t.Fatalf("media must tolerate cold CPU inference: %+v %v", media, err)
	}
}

// Opt-in local smoke tests send only generated, non-sensitive fixtures.
func TestLocalOmniMedia(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_OMNI") != "1" {
		t.Skip("set NIROMEM_TEST_OMNI=1 for local model smoke tests")
	}
	cfg := &Config{}
	cfg.Multimodal.Use = "ollama"
	cfg.Multimodal.Options = map[string]json.RawMessage{"ollama": json.RawMessage(`{"model":"lfm3-omni:latest","num_predict":192,"num_ctx":8192,"num_thread":4}`)}
	var opts niromem.Options
	opts.Blob = blob.NewMem()
	if err := configureMultimodal(cfg, &opts); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	im := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, im); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string][]byte{"red.png": pngData.Bytes()}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		for name, args := range map[string][]string{
			"tone.wav":     {"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-ac", "1", "-ar", "16000"},
			"red-tone.mp4": {"-f", "lavfi", "-i", "color=c=red:s=128x128:r=2:d=1", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:v", "mpeg4", "-threads", "1", "-c:a", "aac", "-shortest"},
		} {
			path := filepath.Join(dir, name)
			cmd := exec.CommandContext(context.Background(), "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, append(args, path)...)...)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture %s: %s %v", name, output, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fixtures[name] = data
		}
	}
	for _, name := range []string{"red.png", "tone.wav", "red-tone.mp4"} {
		data, ok := fixtures[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			pages, err := opts.Media.Extract(context.Background(), name, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 1 || strings.TrimSpace(pages[0].Text) == "" || pages[0].Model == "" {
				t.Fatalf("empty interpretation: %+v", pages)
			}
			t.Logf("%s: %s", pages[0].Model, pages[0].Text)
		})
	}
}
