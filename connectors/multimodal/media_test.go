package multimodal

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixPNGCRC(data []byte) {
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
}

// The test executable doubles as ffprobe/ffmpeg so rejection paths can be
// exercised without a shell or decoding hostile or huge actual media.
func TestMain(m *testing.M) {
	if os.Getenv("NIRO_MULTIMODAL_HELPER") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "ffprobe":
			data := os.Getenv("NIRO_MULTIMODAL_PROBE")
			if data == "oversized" {
				data = strings.Repeat("x", 256<<10+1)
			}
			_, _ = os.Stdout.WriteString(data)
			os.Exit(0)
		case "ffmpeg":
			_, _ = os.Stderr.WriteString("conversion must not run for rejected metadata")
			os.Exit(9)
		}
	}
	os.Exit(m.Run())
}

func TestProbeRejectsUnsafeMetadata(t *testing.T) {
	dir := localDir(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ffprobe", "ffmpeg"} {
		if err := os.Symlink(executable, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("NIRO_MULTIMODAL_HELPER", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for _, tc := range []struct {
		name, probe, want string
	}{
		{"duration", `{"format":{"format_name":"wav","duration":"61"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "60 seconds"},
		{"missing duration", `{"format":{"format_name":"wav"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "duration unavailable"},
		{"nan", `{"format":{"format_name":"wav","duration":"NaN"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "invalid duration"},
		{"inf", `{"format":{"format_name":"wav","duration":"+Inf"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "invalid duration"},
		{"bad duration", `{"format":{"format_name":"wav","duration":"oops"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "invalid duration"},
		{"stream duration", `{"format":{"format_name":"wav","duration":"1"},"streams":[{"codec_type":"audio","duration":"61","channels":1,"sample_rate":"16000"}]}`, "60 seconds"},
		{"sample rate", `{"format":{"format_name":"wav","duration":"1"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"9999999"}]}`, "sample rate"},
		{"channels", `{"format":{"format_name":"wav","duration":"1"},"streams":[{"codec_type":"audio","channels":1000,"sample_rate":"16000"}]}`, "channels"},
		{"container", `{"format":{"format_name":"mp3","duration":"1"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"16000"}]}`, "container"},
		{"dimensions", `{"format":{"format_name":"wav","duration":"1"},"streams":[{"codec_type":"video","width":5000,"height":5000}]}`, "dimensions"},
		{"no streams", `{"format":{"format_name":"wav","duration":"1"},"streams":[]}`, "streams"},
		{"no audio", `{"format":{"format_name":"wav","duration":"1"},"streams":[{"codec_type":"subtitle"}]}`, "stream missing"},
		{"malformed", `{`, "invalid ffprobe"},
		{"capped", "oversized", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NIRO_MULTIMODAL_PROBE", tc.probe)
			err := Validate("x.wav", testWAV(.1))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
	matches, err := filepath.Glob(".niro-multimodal-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("scratch directories leaked: %v %v", matches, err)
	}
}

func TestConvertedWAVBounds(t *testing.T) {
	if err := checkConvertedWAV(testWAV(.1)); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		testWAV(60.01), testWAV(0), testWAV(.1)[:45],
		[]byte("RIFF0000WAVE"),
	} {
		if err := checkConvertedWAV(data); err == nil {
			t.Fatal("accepted empty, truncated, or oversized PCM")
		}
	}
}

func TestMissingProbeExplicitAndCleaned(t *testing.T) {
	t.Setenv("PATH", localDir(t))
	if err := Validate("x.wav", testWAV(.1)); err == nil || !strings.Contains(err.Error(), "ffprobe") {
		t.Fatalf("missing dependency error: %v", err)
	}
	matches, err := filepath.Glob(".niro-multimodal-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("scratch directories leaked: %v %v", matches, err)
	}
}
