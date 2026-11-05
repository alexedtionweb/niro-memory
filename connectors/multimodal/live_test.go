package multimodal

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in; contacts only the existing server/model and never installs,
// pulls, unloads, or changes models. Fixtures are synthetic Go/ffmpeg media.
func TestLiveProtocol(t *testing.T) {
	if os.Getenv("NIRO_MULTIMODAL_LIVE") != "1" {
		t.Skip("set NIRO_MULTIMODAL_LIVE=1 to verify installed localhost Ollama")
	}
	requireFFmpeg(t)
	p, err := New(Options{Timeout: 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := p.post(ctx, "/api/show", map[string]any{"model": p.model}, &show); err != nil {
		t.Fatal(err)
	}
	if err := require(show.Capabilities, "completion", "vision", "audio", "speech"); err != nil {
		t.Fatal(err)
	}
	t.Logf("installed capabilities: %v", show.Capabilities)
	t.Run("protocol", func(t *testing.T) {
		// A bogus audio field is ignored; the same bytes in images must reach
		// the native multimodal loader and fail explicitly.
		for _, field := range []string{"audio", "images"} {
			var response map[string]any
			user := map[string]any{"role": "user", "content": "Reply with OK.", field: [][]byte{[]byte("not media")}}
			err := p.post(ctx, "/api/chat", map[string]any{
				"model": p.model, "stream": false,
				"messages": []any{user},
				"options":  map[string]int{"num_thread": 4, "num_ctx": 8192, "num_predict": 8},
			}, &response)
			if field == "images" {
				b, _ := json.Marshal(response)
				detail := string(b)
				if err != nil {
					detail += err.Error()
				}
				if !strings.Contains(detail, "Failed to load image or audio file") {
					t.Fatalf("images did not reach media loader: %s", detail)
				}
				t.Log("invalid images explicitly rejected by native image/audio loader")
			} else {
				if err != nil || response["error"] != nil || response["done"] != true {
					t.Fatalf("bogus audio field probe failed: %v %+v", err, response)
				}
				t.Log("bogus audio field silently ignored")
			}
		}
	})
	for _, in := range []Input{
		{Name: "red.png", Data: testPNG(t), Prompt: "In one short sentence, describe the image."},
		{Name: "tone.wav", Data: testWAV(.25), Prompt: "In one short sentence, describe the sound; do not invent speech."},
		{Name: "clip.mp4", Data: fixture(t, "mp4", true, "0.5"), Prompt: "In one short sentence, describe the sampled frames and sound."},
	} {
		t.Run(in.Name, func(t *testing.T) {
			result, err := p.Interpret(ctx, in)
			if err != nil {
				t.Fatalf("%s: %v", in.Name, err)
			}
			t.Logf("%s: frames=%d audio=%t %s", in.Name, result.Frames, result.Audio, result.Text)
		})
	}
}
