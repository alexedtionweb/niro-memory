package documents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxRasterDimension = 2400

func utility(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		pkg := "poppler-utils (Poppler)"
		if name == "tesseract" {
			pkg = "tesseract-ocr (Tesseract, including a local language model)"
		}
		return "", fmt.Errorf("required local utility %q is unavailable; install %s and put %s on PATH: %w", name, pkg, name, err)
	}
	return path, nil
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if n > remaining {
		b.buffer.Write(p[:remaining])
		b.exceeded = true
		b.cancel()
	} else {
		b.buffer.Write(p)
	}
	return n, nil
}

func run(ctx context.Context, program string, args ...string) ([]byte, error) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(child, program, args...)
	cmd.WaitDelay = time.Second // Bound waits for inherited subprocess pipes after cancellation.
	stdout := &boundedOutput{limit: maxText, cancel: cancel}
	stderr := &boundedOutput{limit: 8 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "OMP_THREAD_LIMIT=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "OMP_THREAD_LIMIT=1")
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("%s output exceeds safety limit", filepath.Base(program))
	}
	if err != nil {
		return nil, fmt.Errorf("%s failed (corrupt, encrypted, or unsupported PDF, or local utility error): %w: %s",
			filepath.Base(program), err, strings.TrimSpace(stderr.buffer.String()))
	}
	if !utf8.Valid(stdout.buffer.Bytes()) {
		return nil, fmt.Errorf("%s returned invalid UTF-8 text", filepath.Base(program))
	}
	return stdout.buffer.Bytes(), nil
}

func (p Parser) pdf(ctx context.Context, data []byte) ([]Page, error) {
	textTool, err := utility("pdftotext")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", ".niro-documents-")
	if err != nil {
		return nil, fmt.Errorf("create private document working directory: %w", err)
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	input := filepath.Join(dir, "document.pdf")
	if err := os.WriteFile(input, data, 0600); err != nil {
		return nil, err
	}
	raw, err := run(ctx, textTool, "-layout", "-enc", "UTF-8", input, "-")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(raw), "\f")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > maxPages {
		return nil, errors.New("PDF exceeds 100-page limit")
	}
	pages := make([]Page, len(parts))
	var empty []int
	total := 0
	for i, text := range parts {
		pages[i] = Page{Number: i + 1, Text: strings.TrimSpace(text)}
		total += len(pages[i].Text)
		if pages[i].Text == "" {
			empty = append(empty, i)
		}
	}
	if len(empty) == 0 {
		return pages, nil
	}
	rasterTool, err := utility("pdftoppm")
	if err != nil {
		return nil, err
	}
	ocrTool, err := utility("tesseract")
	if err != nil {
		return nil, err
	}
	workers := p.Concurrency
	if workers <= 0 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	if workers > len(empty) {
		workers = len(empty)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(empty))
	for _, i := range empty {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if workCtx.Err() != nil {
					return
				}
				text, err := ocrPage(workCtx, rasterTool, ocrTool, input, dir, i+1)
				mu.Lock()
				if err == nil && total+len(text) > maxText {
					err = errors.New("document exceeds 2 MiB text limit")
				}
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					total += len(text)
					pages[i].Text = text
				}
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return pages, nil
}

func ocrPage(ctx context.Context, rasterTool, ocrTool, input, dir string, page int) (string, error) {
	prefix := filepath.Join(dir, "page-"+strconv.Itoa(page))
	n := strconv.Itoa(page)
	if _, err := run(ctx, rasterTool, "-f", n, "-l", n, "-singlefile", "-scale-to",
		strconv.Itoa(maxRasterDimension), "-png", input, prefix); err != nil {
		return "", fmt.Errorf("rasterize PDF page %d: %w", page, err)
	}
	filename := prefix + ".png"
	defer os.Remove(filename)
	if err := checkRaster(filename); err != nil {
		return "", fmt.Errorf("PDF page %d: %w", page, err)
	}
	raw, err := run(ctx, ocrTool, filename, "stdout")
	if err != nil {
		return "", fmt.Errorf("OCR PDF page %d: %w", page, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func checkRaster(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open raster: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return errors.New("raster exceeds 32 MiB safety limit or is not a regular file")
	}
	config, err := png.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("invalid PNG raster: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxRasterDimension || config.Height > maxRasterDimension {
		return errors.New("raster dimensions exceed 2400-pixel safety limit")
	}
	return nil
}
