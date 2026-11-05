package multimodal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testWAV(seconds float64) []byte {
	samples := int(seconds * 16000)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+samples*2))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	for _, v := range []any{uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(samples*2))
	for i := 0; i < samples; i++ {
		_ = binary.Write(&b, binary.LittleEndian, int16(2000*math.Sin(float64(i)*2*math.Pi*440/16000)))
	}
	return b.Bytes()
}

type capturedRequest struct {
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  chatOptions    `json:"options"`
	Think    *bool          `json:"think"`
	Other    map[string]any `json:"-"`
}

func testServer(t *testing.T, caps []string, response string, check func(capturedRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid request method or content type")
		}
		switch r.URL.Path {
		case "/api/show":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "lfm3-omni:latest" {
				t.Error("invalid show payload", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
		case "/api/chat":
			var req capturedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if check != nil {
				check(req)
			}
			_, _ = io.WriteString(w, response)
		default:
			t.Error("unexpected route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

const successful = `{"model":"lfm3-omni:latest","message":{"role":"assistant","content":"A red image."},"done":true,"done_reason":"stop"}`

func TestImagePayload(t *testing.T) {
	data := testPNG(t)
	var calls atomic.Int32
	server := testServer(t, []string{"completion", "vision"}, successful, func(req capturedRequest) {
		calls.Add(1)
		if req.Model != "lfm3-omni:latest" || req.Stream || req.Think != nil || req.Options != (chatOptions{4, 8192, 256}) {
			t.Errorf("unexpected options: %+v", req)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || !strings.Contains(req.Messages[0].Content, "untrusted") {
			t.Errorf("missing safety system message: %+v", req.Messages)
			return
		}
		user := req.Messages[1]
		if len(user.Images) != 1 || !bytes.Equal(user.Images[0], data) || !strings.Contains(user.Content, "look") || strings.Contains(user.Content, "secret-name") {
			t.Error("invalid image attachment or prompt")
		}
	})
	defer server.Close()
	p, err := New(Options{BaseURL: server.URL, NumPredict: 5000})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Interpret(context.Background(), Input{Name: "secret-name.png", Data: data, Prompt: "look"})
	if err != nil || result.Frames != 1 || result.Audio || !strings.HasPrefix(result.Text, "Unreviewed model output:") || calls.Load() != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls.Load())
	}
}

func TestCapabilityChecksBeforeDecoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		caps []string
		want string
	}{
		{"x.png", []byte("\x89PNG\r\n\x1a\n"), nil, "completion"},
		{"x.png", []byte("\x89PNG\r\n\x1a\n"), []string{"completion"}, "vision"},
		{"x.wav", testWAV(.1), []string{"completion", "vision", "speech"}, "audio"},
	} {
		t.Run(tc.name+tc.want, func(t *testing.T) {
			server := testServer(t, tc.caps, successful, func(capturedRequest) { t.Error("unexpected chat") })
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL})
			_, err := p.Interpret(context.Background(), Input{Name: tc.name, Data: tc.data})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected capability %s, got %v", tc.want, err)
			}
		})
	}
}

func TestResponseFailures(t *testing.T) {
	for _, body := range []string{
		``, `{`, `{"done":true`, `{} {}`,
		`{"error":"model failure"}`,
		`{"done":true,"message":{"content":" "}}`,
		`{"done":false,"message":{"content":"partial"}}`,
		`{"done":true,"done_reason":"length","message":{"content":"partial"}}`,
		`{"done":true,"done_reason":"max_tokens","message":{"content":"partial"}}`,
		`{"done":true,"message":{"content":"` + strings.Repeat("x", 65537) + `"}}`,
		strings.Repeat("x", 2<<20+1),
	} {
		t.Run(fmt.Sprintf("bytes%d-%s", len(body), body[:min(12, len(body))]), func(t *testing.T) {
			server := testServer(t, []string{"completion", "vision"}, body, nil)
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL})
			if _, err := p.Interpret(context.Background(), Input{Name: "x.png", Data: testPNG(t)}); err == nil {
				t.Fatal("expected response error")
			}
		})
	}
}

func TestHTTPFailuresAndTimeout(t *testing.T) {
	for _, mode := range []string{"status", "show-error", "invalid-show", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "status":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "show-error":
					_, _ = io.WriteString(w, `{"error":"missing model"}`)
				case "invalid-show":
					_, _ = io.WriteString(w, `{"capabilities":`)
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-time.After(100 * time.Millisecond):
					}
				}
			}))
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL, Timeout: 30 * time.Millisecond, HTTP: server.Client()})
			if _, err := p.Interpret(context.Background(), Input{Name: "x.png", Data: testPNG(t)}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestOptions(t *testing.T) {
	for _, o := range []Options{
		{BaseURL: "file:///etc/passwd"}, {BaseURL: "http://"},
		{BaseURL: "http://user:pass@localhost"}, {BaseURL: "http://localhost?q=1"},
		{Timeout: -1}, {Timeout: maxTimeout + 1}, {Model: " "},
		{NumThread: -1}, {NumCtx: -1}, {NumPredict: -1},
	} {
		if _, err := New(o); err == nil {
			t.Fatalf("accepted invalid options: %+v", o)
		}
	}
	p, err := New(Options{NumThread: 2, NumCtx: 4096, NumPredict: 100, Timeout: time.Second})
	if err != nil || p.options != (chatOptions{2, 4096, 100}) || p.base != "http://localhost:11434" {
		t.Fatalf("options: %+v %v", p, err)
	}
}

func TestValidation(t *testing.T) {
	pngData := testPNG(t)
	if err := Validate("X.PNG", pngData); err != nil {
		t.Fatal(err)
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate("x.jpeg", jpegData.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []Input{
		{Name: "x.png"}, {Name: "x.gif", Data: pngData},
		{Name: "x.jpg", Data: pngData}, {Name: "x.png", Data: pngData[:30]},
		{Name: "x.wav", Data: []byte("RIFF0000nope")},
		{Name: "x.mp3", Data: []byte("junk")},
		{Name: "x.mp4", Data: []byte("0000ftyp")},
		{Name: "x.ogg", Data: []byte("OggS")},
		{Name: "x.webm", Data: []byte("not webm")},
		{Name: "x.png", Data: append(pngData, make([]byte, maxImageBytes)...)},
		{Name: "x.wav", Data: make([]byte, maxMediaBytes+1)},
	} {
		if err := Validate(tc.Name, tc.Data); err == nil {
			t.Fatalf("accepted invalid media %s (%d bytes)", tc.Name, len(tc.Data))
		}
	}
	// Rewrite IHDR dimensions and its CRC so DecodeConfig succeeds, but no
	// oversized image is allocated or decoded.
	huge := append([]byte(nil), pngData...)
	binary.BigEndian.PutUint32(huge[16:20], 5000)
	binary.BigEndian.PutUint32(huge[20:24], 5000)
	fixPNGCRC(huge)
	if err := Validate("huge.png", huge); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("oversized image accepted: %v", err)
	}
}

func TestContainerSignatures(t *testing.T) {
	webm := []byte("\x1a\x45\xdf\xa3\x87\x42\x82\x84webm")
	if !isWebM(webm) {
		t.Fatal("valid WebM EBML header rejected")
	}
	for _, invalid := range [][]byte{
		[]byte("\x1a\x45\xdf\xa3"),
		[]byte("\x1a\x45\xdf\xa3\x8b\x42\x82\x88matroska"),
		[]byte("\x1a\x45\xdf\xa3\xff"),
		[]byte("\x1a\x45\xdf\xa3\x80"),
	} {
		if isWebM(invalid) {
			t.Fatal("accepted non-WebM or incomplete header")
		}
	}
	if isMP4([]byte("\x00\x00\x00\xffftypisom\x00\x00\x00\x00")) {
		t.Fatal("accepted ftyp box larger than input")
	}
}

func localDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".multimodal-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
}

func fixture(t *testing.T, extension string, soundtrack bool, duration string) []byte {
	t.Helper()
	return sizedFixture(t, extension, soundtrack, duration, "32x24")
}

func sizedFixture(t *testing.T, extension string, soundtrack bool, duration, size string) []byte {
	t.Helper()
	dir := localDir(t)
	path := filepath.Join(dir, "media."+extension)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1",
		"-filter_threads", "1", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=8:duration=" + duration}
	if soundtrack {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration="+duration)
	}
	if extension == "webm" {
		args = append(args, "-c:v", "libvpx", "-deadline", "realtime", "-c:a", "libopus")
	} else {
		args = append(args, "-c:v", "mpeg4", "-c:a", "aac")
	}
	args = append(args, "-threads", "1", "-y", path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture generation: %v: %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRealAudioFormats(t *testing.T) {
	requireFFmpeg(t)
	dir := localDir(t)
	wav := testWAV(.25)
	path := filepath.Join(dir, "source.wav")
	if err := os.WriteFile(path, wav, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"wav", "mp3", "m4a", "ogg"} {
		t.Run(ext, func(t *testing.T) {
			data := wav
			if ext != "wav" {
				dest := filepath.Join(dir, "converted."+ext)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if out, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-threads", "1", "-i", path, "-threads", "1", "-y", dest).CombinedOutput(); err != nil {
					t.Fatalf("convert fixture: %v %s", err, out)
				}
				var err error
				data, err = os.ReadFile(dest)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := Validate("sound."+ext, data); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := testServer(t, []string{"completion", "audio"}, successful, func(req capturedRequest) {
				calls.Add(1)
				images := req.Messages[1].Images
				if len(images) != 1 || len(images[0]) < 44 || string(images[0][:4]) != "RIFF" || string(images[0][8:12]) != "WAVE" {
					t.Error("audio was not attached as WAV in message.images")
					return
				}
				// ffmpeg emits a standard fmt chunk before optional LIST/data.
				if binary.LittleEndian.Uint16(images[0][22:24]) != 1 || binary.LittleEndian.Uint32(images[0][24:28]) != 16000 {
					t.Error("audio not mono 16 kHz")
				}
			})
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL})
			result, err := p.Interpret(context.Background(), Input{Name: "sound." + ext, Data: data})
			if err != nil || !result.Audio || result.Frames != 0 || calls.Load() != 1 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
	if err := Validate("long.wav", testWAV(60.01)); err == nil || !strings.Contains(err.Error(), "60 seconds") {
		t.Fatalf("accepted long audio: %v", err)
	}
}

func TestRealVideoFormats(t *testing.T) {
	requireFFmpeg(t)
	for _, tc := range []struct {
		ext   string
		audio bool
		dur   string
	}{
		{"mp4", true, "1"}, {"mp4", true, "1.25"}, {"webm", true, "0.5"}, {"mov", false, "0.5"},
		{"mp4", false, "9"},
	} {
		t.Run(fmt.Sprintf("%s-%t-%s", tc.ext, tc.audio, tc.dur), func(t *testing.T) {
			data := fixture(t, tc.ext, tc.audio, tc.dur)
			if err := Validate("movie."+tc.ext, data); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := testServer(t, []string{"completion", "vision", "audio"}, successful, func(req capturedRequest) {
				calls.Add(1)
				user := req.Messages[1]
				frames := len(user.Images)
				if tc.audio {
					frames--
					last := user.Images[len(user.Images)-1]
					if !bytes.HasPrefix(last, []byte("RIFF")) {
						t.Error("soundtrack not in same images array as frames")
					}
				} else if !strings.Contains(user.Content, "No soundtrack") {
					t.Error("missing no soundtrack warning")
				}
				if frames < 1 || frames > 8 || !strings.Contains(user.Content, "NOT exhaustive") || !strings.Contains(user.Content, "image 1:") {
					t.Errorf("invalid sampling: frames=%d, prompt=%q", frames, user.Content)
				}
				for _, frame := range user.Images[:frames] {
					if !bytes.HasPrefix(frame, []byte{0xff, 0xd8, 0xff}) {
						t.Error("not a JPEG frame")
					}
					cfg, _, err := image.DecodeConfig(bytes.NewReader(frame))
					if err != nil || cfg.Width > 384 || cfg.Height > 384 || cfg.Width > 32 || cfg.Height > 24 {
						t.Errorf("frame was oversized or upscaled: %+v %v", cfg, err)
					}
				}
				wantFrames := 1
				if tc.dur == "9" {
					wantFrames = 2
				}
				if frames != wantFrames {
					t.Errorf("redundant sampling: got %d frames, want %d", frames, wantFrames)
				}
				if req.Options.NumPredict > 256 {
					t.Error("excessive CPU output limit")
				}
			})
			defer server.Close()
			p, _ := New(Options{BaseURL: server.URL})
			result, err := p.Interpret(context.Background(), Input{Name: "movie." + tc.ext, Data: data})
			if err != nil || result.Audio != tc.audio || result.Frames < 1 || result.Frames > 8 || calls.Load() != 1 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if tc.audio {
				server := testServer(t, []string{"completion", "vision"}, successful, func(capturedRequest) { t.Error("chat with unsupported audio") })
				defer server.Close()
				p, _ := New(Options{BaseURL: server.URL})
				if _, err := p.Interpret(context.Background(), Input{Name: "x." + tc.ext, Data: data}); err == nil || !strings.Contains(err.Error(), "audio") {
					t.Fatalf("missing soundtrack capability error: %v", err)
				}
			}
		})
	}
}

func TestNativeCancellationAndBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquire(ctx); err == nil {
		<-nativeSlots
		t.Fatal("acquired with canceled context")
	}
	nativeSlots <- struct{}{}
	nativeSlots <- struct{}{}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := acquire(ctx)
	<-nativeSlots
	<-nativeSlots
	if err == nil {
		t.Fatal("unbounded native concurrency")
	}
	b := &cappedBuffer{limit: 2}
	if _, err := b.Write([]byte("123")); err == nil || b.Len() != 0 {
		t.Fatal("output cap not enforced")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := native(ctx, "ffmpeg", 10, "-version"); err == nil {
		t.Fatal("native command ignored canceled context")
	}
}

func TestVideoCPUFrameBounds(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := Input{Name: "large.mp4", Data: sizedFixture(t, "mp4", true, "1", "800x600")}
	out, err := prepare(ctx, video, in, []string{"completion", "vision", "audio"})
	if err != nil {
		t.Fatal(err)
	}
	if out.frames != 1 || !out.audio || len(out.media) != 2 {
		t.Fatalf("short clip must have one frame plus full soundtrack: %+v", out)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out.media[0]))
	if err != nil || cfg.Width != 384 || cfg.Height != 288 {
		t.Fatalf("expected 384x288 bounded frame, got %+v: %v", cfg, err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	server := testServer(t, []string{"completion", "vision"}, successful, nil)
	defer server.Close()
	p, _ := New(Options{BaseURL: server.URL})
	data := testPNG(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Interpret(context.Background(), Input{Name: "x.png", Data: data}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
