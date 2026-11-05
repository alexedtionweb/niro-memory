package multimodal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxImageBytes = 20 << 20
	maxMediaBytes = 64 << 20
	maxPixels     = 16_000_000
)

type mediaKind int

const (
	picture mediaKind = iota
	audio
	video
)

var nativeSlots = make(chan struct{}, 2)

func acquire(ctx context.Context) error {
	select {
	case nativeSlots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-nativeSlots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func identify(name string, data []byte) (mediaKind, error) {
	if len(data) == 0 || len(data) > maxMediaBytes {
		return 0, errors.New("multimodal: media is empty or exceeds 64 MiB")
	}
	ext := strings.ToLower(filepath.Ext(name))
	kind, valid := picture, false
	switch ext {
	case ".png":
		valid = bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case ".jpg", ".jpeg":
		valid = bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	case ".wav":
		kind = audio
		valid = len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	case ".mp3":
		kind = audio
		valid = bytes.HasPrefix(data, []byte("ID3")) || len(data) >= 4 && data[0] == 0xff && data[1]&0xe0 == 0xe0 && data[1]&0x18 != 0x08 && data[1]&0x06 != 0 && data[2]&0xf0 != 0xf0 && data[2]&0x0c != 0x0c
	case ".ogg":
		kind, valid = audio, bytes.HasPrefix(data, []byte("OggS\x00"))
	case ".m4a":
		kind, valid = audio, isMP4(data)
	case ".mp4", ".mov":
		kind, valid = video, isMP4(data)
	case ".webm":
		kind, valid = video, isWebM(data)
	default:
		return 0, fmt.Errorf("multimodal: unsupported extension %q", ext)
	}
	if !valid {
		return 0, fmt.Errorf("multimodal: invalid %s signature/container", ext)
	}
	if kind == picture && len(data) > maxImageBytes {
		return 0, errors.New("multimodal: image exceeds 20 MiB")
	}
	return kind, nil
}

func isMP4(data []byte) bool {
	if len(data) < 16 || string(data[4:8]) != "ftyp" {
		return false
	}
	size := int64(binary.BigEndian.Uint32(data[:4]))
	return size >= 16 && size <= int64(len(data))
}

func isWebM(data []byte) bool {
	if !bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return false
	}
	size, n := ebmlValue(data[4:], false)
	if n == 0 || size > 4096 || size > uint64(len(data)-4-n) {
		return false
	}
	header := data[4+n : 4+n+int(size)]
	for len(header) > 0 {
		id, n := ebmlValue(header, true)
		if n == 0 {
			return false
		}
		header = header[n:]
		size, n := ebmlValue(header, false)
		if n == 0 || size > uint64(len(header)-n) {
			return false
		}
		header = header[n:]
		if id == 0x4282 {
			return string(header[:int(size)]) == "webm"
		}
		header = header[int(size):]
	}
	return false
}

func ebmlValue(data []byte, identifier bool) (uint64, int) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0
	}
	width, mask := 1, byte(0x80)
	for data[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > len(data) || identifier && width > 4 {
		return 0, 0
	}
	v := uint64(data[0])
	if !identifier {
		v &= uint64(mask - 1)
	}
	for _, b := range data[1:width] {
		v = v<<8 | uint64(b)
	}
	return v, width
}

func checkImage(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("multimodal: invalid image header: %w", err)
	}
	if !validDimensions(cfg.Width, cfg.Height) {
		return errors.New("multimodal: image dimensions exceed 16 million pixels")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("multimodal: invalid image: %w", err)
	}
	return nil
}

func validDimensions(w, h int) bool {
	return w > 0 && h > 0 && w <= maxPixels && h <= maxPixels && int64(w)*int64(h) <= maxPixels
}

// Validate checks extension, signature, size, decoded image integrity, and probed
// media streams/duration. Audio/video are limited to 60 seconds and 64 MiB;
// images to 20 MiB and 16 million pixels. It does not contact Ollama.
func Validate(name string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), maxTimeout)
	defer cancel()
	kind, err := identify(name, data)
	if err != nil {
		return err
	}
	if err := acquire(ctx); err != nil {
		return err
	}
	defer func() { <-nativeSlots }()
	if kind == picture {
		return checkImage(data)
	}
	path, cleanup, err := scratch(name, data)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = probe(ctx, path, kind)
	return err
}

func scratch(name string, data []byte) (string, func(), error) {
	dir, err := os.MkdirTemp("", ".niro-multimodal-")
	if err != nil {
		return "", nil, fmt.Errorf("multimodal: create private media directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	abs, err := filepath.Abs(dir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	path := filepath.Join(abs, "input"+strings.ToLower(filepath.Ext(name)))
	if err := os.WriteFile(path, data, 0600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("multimodal: write media: %w", err)
	}
	return path, cleanup, nil
}

type cappedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *cappedBuffer) Len() int       { return b.buffer.Len() }
func (b *cappedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *cappedBuffer) String() string { return b.buffer.String() }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("native output exceeds limit")
	}
	return b.buffer.Write(p)
}

func native(ctx context.Context, program string, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("multimodal: %s: %w", program, ctx.Err())
		}
		return nil, fmt.Errorf("multimodal: %s failed: %w: %.2048s", program, err, stderr.String())
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return stdout.Bytes(), nil
}

type probeResult struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Duration    string `json:"duration"`
		Channels    int    `json:"channels"`
		SampleRate  string `json:"sample_rate"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
	duration float64
	hasAudio bool
}

func probe(ctx context.Context, path string, kind mediaKind) (probeResult, error) {
	var result probeResult
	data, err := native(ctx, "ffprobe", 256<<10,
		"-v", "error", "-max_alloc", "67108864", "-protocol_whitelist", "file,pipe",
		"-probesize", "8388608", "-analyzeduration", "5000000", "-threads", "1",
		"-show_entries", "format=format_name,duration:stream=codec_type,width,height,duration,channels,sample_rate:stream_disposition=attached_pic",
		"-of", "json", path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("multimodal: invalid ffprobe output: %w", err)
	}
	expected := map[string]string{
		".wav": "wav", ".mp3": "mp3", ".ogg": "ogg", ".m4a": "mov,mp4,m4a,3gp,3g2,mj2",
		".mp4": "mov,mp4,m4a,3gp,3g2,mj2", ".mov": "mov,mp4,m4a,3gp,3g2,mj2", ".webm": "matroska,webm",
	}[filepath.Ext(path)]
	if result.Format.Name != expected {
		return result, fmt.Errorf("multimodal: probed container %q does not match extension", result.Format.Name)
	}
	durations := []string{result.Format.Duration}
	hasVideo := false
	if len(result.Streams) == 0 || len(result.Streams) > 32 {
		return result, errors.New("multimodal: missing or excessive media streams")
	}
	for _, stream := range result.Streams {
		durations = append(durations, stream.Duration)
		switch stream.CodecType {
		case "video":
			if !validDimensions(stream.Width, stream.Height) {
				return result, errors.New("multimodal: video dimensions exceed 16 million pixels")
			}
			hasVideo = hasVideo || stream.Disposition.AttachedPic != 1
		case "audio":
			rate, err := strconv.Atoi(stream.SampleRate)
			if err != nil || rate <= 0 || rate > 384000 || stream.Channels <= 0 || stream.Channels > 32 {
				return result, errors.New("multimodal: invalid or excessive audio sample rate/channels")
			}
			result.hasAudio = true
		}
	}
	for _, value := range durations {
		if value == "" || value == "N/A" {
			continue
		}
		d, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 || d > 60 {
			return result, errors.New("multimodal: invalid duration or media exceeds 60 seconds")
		}
		result.duration = max(result.duration, d)
	}
	if result.duration == 0 {
		return result, errors.New("multimodal: media duration unavailable; cannot enforce 60-second limit")
	}
	if kind == video && !hasVideo || kind == audio && !result.hasAudio {
		return result, errors.New("multimodal: required media stream missing")
	}
	if kind == audio && hasVideo {
		return result, errors.New("multimodal: audio input contains a video stream")
	}
	return result, nil
}

type preparedMedia struct {
	media       [][]byte
	description string
	frames      int
	audio       bool
}

func prepare(ctx context.Context, kind mediaKind, in Input, caps []string) (preparedMedia, error) {
	var out preparedMedia
	if err := acquire(ctx); err != nil {
		return out, err
	}
	defer func() { <-nativeSlots }()
	if kind == picture {
		if err := checkImage(in.Data); err != nil {
			return out, err
		}
		return preparedMedia{media: [][]byte{in.Data}, frames: 1, description: "One still image; no audio was provided."}, nil
	}
	path, cleanup, err := scratch(in.Name, in.Data)
	if err != nil {
		return out, err
	}
	defer cleanup()
	info, err := probe(ctx, path, kind)
	if err != nil {
		return out, err
	}
	if info.hasAudio {
		if err := require(caps, "audio"); err != nil {
			return out, err
		}
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-max_alloc", "67108864", "-protocol_whitelist", "file,pipe", "-threads", "1",
		"-filter_threads", "1", "-filter_complex_threads", "1"}
	if kind == video {
		// Sparse midpoint sampling avoids redundant visual prefill for short
		// clips, including containers whose audio padding crosses one second.
		n := min(8, int(math.Ceil(info.duration/8)))
		var labels []string
		for i := 0; i < n; i++ {
			stamp := info.duration * (float64(i) + 0.5) / float64(n)
			a := append(append([]string{}, args...), "-ss", fmt.Sprintf("%.6f", stamp), "-i", path,
				"-map", "0:V:0", "-frames:v", "1", "-vf", "scale=w='min(384,iw)':h='min(384,ih)':force_original_aspect_ratio=decrease",
				"-threads", "1", "-q:v", "3", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1")
			frame, err := native(ctx, "ffmpeg", 4<<20, a...)
			if err != nil {
				return out, err
			}
			if err := checkImage(frame); err != nil {
				return out, fmt.Errorf("multimodal: sampled frame %.3fs: %w", stamp, err)
			}
			out.media = append(out.media, frame)
			labels = append(labels, fmt.Sprintf("image %d: %.3fs", i+1, stamp))
		}
		out.frames = n
		out.description = fmt.Sprintf("Video duration %.3fs. %d sampled frames, NOT exhaustive coverage: %s. Do not infer unsampled events.", info.duration, n, strings.Join(labels, "; "))
	}
	if info.hasAudio {
		a := append(append([]string{}, args...), "-i", path, "-map", "0:a:0", "-vn",
			"-threads", "1", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", "pipe:1")
		wav, err := native(ctx, "ffmpeg", 2<<20, a...)
		if err != nil {
			return out, err
		}
		if len(wav) <= 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
			return out, errors.New("multimodal: ffmpeg produced empty or invalid WAV")
		}
		if err := checkConvertedWAV(wav); err != nil {
			return out, err
		}
		out.media = append(out.media, wav)
		out.audio = true
		out.description += fmt.Sprintf(" Attachment %d is the full bounded soundtrack/audio, mono 16 kHz WAV (%.3fs). Describe audible evidence and transcribe any speech.", len(out.media), info.duration)
	} else {
		out.description += " No soundtrack exists in this input. Do not invent audio or speech."
	}
	return out, nil
}

func checkConvertedWAV(wav []byte) error {
	for offset := 12; offset+8 <= len(wav); {
		size := uint64(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		if string(wav[offset:offset+4]) == "data" {
			// A pipe's data chunk has an unknown (0xffffffff) length. Its
			// actual PCM byte count must independently obey the duration cap.
			n := len(wav) - offset - 8
			if size != math.MaxUint32 && size != uint64(n) {
				return errors.New("multimodal: truncated converted WAV data")
			}
			if n <= 0 || n%2 != 0 || n > 60*16000*2 {
				return errors.New("multimodal: decoded audio empty or exceeds 60 seconds")
			}
			return nil
		}
		if size > uint64(len(wav)-offset-8) {
			break
		}
		offset += 8 + int(size) + int(size%2)
	}
	return errors.New("multimodal: converted WAV is missing PCM data")
}
