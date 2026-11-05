package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const wordNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"

func TestMain(m *testing.M) {
	if os.Getenv("DOCUMENTS_TEST_HELPER") == "1" {
		fakeUtility()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func workDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".documents-test-")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(absolute) })
	return absolute
}

func docx(t *testing.T, xml string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, err := z.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestText(t *testing.T) {
	for _, name := range []string{"notes.txt", "notes.MD", "notes.markdown"} {
		text := "# Hello\n\n世界\t\n"
		pages, err := (Parser{}).Extract(context.Background(), name, []byte(text))
		if err != nil || !reflect.DeepEqual(pages, []Page{{Number: 1, Text: text}}) {
			t.Fatalf("%s: %v, %v", name, pages, err)
		}
	}
	pages, err := (Parser{}).Extract(context.Background(), "empty.txt", nil)
	if err != nil || len(pages) != 1 || pages[0].Text != "" {
		t.Fatalf("empty text: %v, %v", pages, err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"x.html", []byte("hello"), "unsupported"},
		{"x.txt", []byte{0xff}, "UTF-8"},
		{"x.md", []byte{0xc3}, "UTF-8"},
		{"x.pdf", []byte("not pdf"), "signature"},
		{"x.docx", []byte("not zip"), "signature"},
		{"x.docx", []byte("PK\x03\x04broken"), "archive"},
		{"x.txt", bytes.Repeat([]byte("x"), maxInput+1), "20 MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.want, func(t *testing.T) {
			err := Validate(tt.name, tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	if err := Validate("x.pdf", []byte("%PDF-1.7\n")); err != nil {
		t.Fatal(err)
	}
	if err := Validate("x.txt", make([]byte, maxInput)); err != nil {
		t.Fatalf("exact input limit rejected: %v", err)
	}
}

func TestDOCXParagraphs(t *testing.T) {
	xml := `<w:document xmlns:w="` + wordNS + `"><w:body>
<w:p><w:r><w:t>Hello &amp; </w:t></w:r><w:r><w:t>世界</w:t><w:tab/><w:t>tab</w:t><w:br/><w:t>line</w:t></w:r></w:p>
<w:p><w:r><w:t>Second</w:t></w:r></w:p></w:body></w:document>`
	data := docx(t, xml)
	if err := Validate("x.docx", data); err != nil {
		t.Fatal(err)
	}
	pages, err := (Parser{}).Extract(context.Background(), "x.docx", data)
	if err != nil || !reflect.DeepEqual(pages, []Page{{1, "Hello & 世界\ttab\nline\nSecond"}}) {
		t.Fatalf("got %v, %v", pages, err)
	}
}

func TestDOCXOnlyDocumentXML(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	ignored, err := z.Create("word/ignored.xml")
	if err != nil {
		t.Fatal(err)
	}
	ignored.Write([]byte(strings.Repeat("x", maxXML+1)))
	document, err := z.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	document.Write([]byte(`<w:document xmlns:w="http://purl.oclc.org/ooxml/wordprocessingml/main"><w:p><w:r><w:t>strict</w:t></w:r></w:p></w:document>`))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	pages, err := (Parser{}).Extract(context.Background(), "x.docx", b.Bytes())
	if err != nil || len(pages) != 1 || pages[0].Text != "strict" {
		t.Fatalf("unrelated ZIP entry affected document: %v, %v", pages, err)
	}
}

func TestDOCXInvalidXML(t *testing.T) {
	for _, xml := range []string{
		"", `<w:document`, `<document/>`, `<foo/>`,
		`<!DOCTYPE x><w:document xmlns:w="` + wordNS + `"/>`,
		`<w:document xmlns:w="` + wordNS + `"/><x/>`,
		`<w:document xmlns:w="` + wordNS + `"/>trailing`,
		`<w:document xmlns:w="` + wordNS + `"><w:p></w:document>`,
	} {
		if _, err := (Parser{}).Extract(context.Background(), "x.docx", docx(t, xml)); err == nil {
			t.Fatalf("accepted malformed/unsupported XML %q", xml)
		}
	}
}

func TestDOCXArchiveChecks(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		z.Create("other.xml")
		z.Close()
		if err := Validate("x.docx", b.Bytes()); err == nil {
			t.Fatal("accepted archive missing document")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		z.Create("word/document.xml")
		z.Create("word/document.xml")
		z.Close()
		if err := Validate("x.docx", b.Bytes()); err == nil {
			t.Fatal("accepted duplicate document")
		}
	})
	for _, kind := range []string{"encrypted", "compression", "oversized", "checksum"} {
		t.Run(kind, func(t *testing.T) {
			data := docx(t, `<w:document xmlns:w="`+wordNS+`"/>`)
			central := bytes.Index(data, []byte("PK\x01\x02"))
			switch kind {
			case "encrypted":
				binary.LittleEndian.PutUint16(data[central+8:], 1)
			case "compression":
				binary.LittleEndian.PutUint16(data[central+10:], 99)
			case "oversized":
				binary.LittleEndian.PutUint32(data[central+24:], maxXML+1)
			case "checksum":
				binary.LittleEndian.PutUint32(data[central+16:], 42)
			}
			if _, err := (Parser{}).Extract(context.Background(), "x.docx", data); err == nil {
				t.Fatalf("accepted %s archive", kind)
			}
		})
	}
}

func TestSizeLimits(t *testing.T) {
	for _, n := range []int{maxText, maxText + 1} {
		_, err := (Parser{}).Extract(context.Background(), "x.txt", bytes.Repeat([]byte("a"), n))
		if (err != nil) != (n > maxText) {
			t.Fatalf("text size %d: %v", n, err)
		}
	}
	xml := `<w:document xmlns:w="` + wordNS + `"><w:p><w:r><w:t>` +
		strings.Repeat("x", maxText+1) + `</w:t></w:r></w:p></w:document>`
	if _, err := (Parser{}).Extract(context.Background(), "x.docx", docx(t, xml)); err == nil {
		t.Fatal("accepted excessive DOCX text")
	}
	if err := Validate("x.docx", docx(t, strings.Repeat(" ", maxXML+1))); err == nil {
		t.Fatal("accepted oversized XML")
	}
}

func fakeTools(t *testing.T, names ...string) string {
	t.Helper()
	dir := workDir(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.Symlink(executable, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("DOCUMENTS_TEST_HELPER", "1")
	t.Setenv("DOCUMENTS_TEST_MODE", "")
	t.Setenv("DOCUMENTS_TEST_PAGES", "3")
	t.Setenv("OMP_THREAD_LIMIT", "32")
	// Each fake command re-executes this binary. Avoid the race runtime's
	// default one-second exit sleep per helper, while retaining race detection.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return dir
}

func TestFakeUtilityExitDelay(t *testing.T) {
	t.Setenv("GORACE", "halt_on_error=1 atexit_sleep_ms=1000")
	fakeTools(t, "pdftotext")
	if got := os.Getenv("GORACE"); got != "halt_on_error=1 atexit_sleep_ms=1000 atexit_sleep_ms=0" {
		t.Fatalf("helper race options lost existing settings: %q", got)
	}
	t.Setenv("DOCUMENTS_TEST_MODE", "text")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Three helpers exceed this deadline with the default race exit delay.
	for i := 0; i < 3; i++ {
		if _, err := (Parser{}).Extract(ctx, "x.pdf", []byte("%PDF-1.7")); err != nil {
			t.Fatalf("fake utility did not exit promptly: %v", err)
		}
	}
	assertClean(t)
}

// The test executable doubles as all three utilities, without shells or tools
// installed on the host. It checks the arguments used to bound rasterization.
func fakeUtility() {
	mode := os.Getenv("DOCUMENTS_TEST_MODE")
	name := filepath.Base(os.Args[0])
	if mode == "cancel" || mode == "cancel-ocr" && name == "tesseract" {
		time.Sleep(10 * time.Second)
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "encrypted or corrupt PDF")
		os.Exit(2)
	}
	switch name {
	case "pdftotext":
		if len(os.Args) != 6 || strings.Join(os.Args[1:4], " ") != "-layout -enc UTF-8" || os.Args[5] != "-" {
			os.Exit(20)
		}
		if !filepath.IsAbs(os.Args[4]) || filepath.Dir(filepath.Dir(os.Args[4])) != os.TempDir() {
			os.Exit(26)
		}
		info, err := os.Stat(filepath.Dir(os.Args[4]))
		if err != nil || info.Mode().Perm() != 0700 {
			os.Exit(27)
		}
		switch mode {
		case "overflow":
			fmt.Print(strings.Repeat("x", maxText+1))
		case "stderr-overflow":
			fmt.Fprint(os.Stderr, strings.Repeat("x", 9000))
		case "invalid-utf8":
			os.Stdout.Write([]byte{0xff})
		case "text":
			fmt.Print(" first \n\fsecond\f")
		case "many":
			fmt.Print(strings.Repeat("\f", maxPages+1))
		case "hundred":
			fmt.Print(strings.Repeat("page\f", maxPages))
		case "all-empty", "cancel-ocr", "raster-large", "raster-invalid", "raster-file-large", "ocr-overflow", "aggregate", "workers":
			count, _ := strconv.Atoi(os.Getenv("DOCUMENTS_TEST_PAGES"))
			fmt.Print(strings.Repeat("\f", count))
		default:
			fmt.Print("first\f \n\fthird\f")
		}
	case "pdftoppm":
		args := os.Args[1:]
		if len(args) != 10 || args[0] != "-f" || args[2] != "-l" ||
			args[1] != args[3] || args[4] != "-singlefile" ||
			args[5] != "-scale-to" || args[6] != "2400" || args[7] != "-png" {
			os.Exit(21)
		}
		if !filepath.IsAbs(args[8]) || !filepath.IsAbs(args[9]) {
			os.Exit(26)
		}
		filename := args[9] + ".png"
		f, err := os.Create(filename)
		if err != nil {
			os.Exit(22)
		}
		if mode == "raster-invalid" {
			f.WriteString("not an image")
		} else if mode == "raster-file-large" {
			f.Truncate(32<<20 + 1)
		} else if mode == "raster-large" {
			// A valid PNG header is sufficient for DecodeConfig.
			f.Write(pngHeader(2401, 1))
		} else {
			png.Encode(f, image.NewGray(image.Rect(0, 0, 2, 2)))
		}
		f.Close()
	case "tesseract":
		if os.Getenv("OMP_THREAD_LIMIT") != "1" || len(os.Args) != 3 || os.Args[2] != "stdout" {
			os.Exit(23)
		}
		if !filepath.IsAbs(os.Args[1]) {
			os.Exit(26)
		}
		if mode == "workers" {
			log, err := os.OpenFile(os.Getenv("DOCUMENTS_TEST_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				os.Exit(25)
			}
			fmt.Fprintf(log, "%d 1\n", time.Now().UnixNano())
			time.Sleep(40 * time.Millisecond)
			fmt.Fprintf(log, "%d -1\n", time.Now().UnixNano())
			log.Close()
		}
		if mode == "ocr-overflow" {
			fmt.Print(strings.Repeat("x", maxText+1))
		} else if mode == "aggregate" {
			fmt.Print(strings.Repeat("x", maxText/2+1))
		} else {
			fmt.Printf("OCR %s\n", strings.TrimSuffix(filepath.Base(os.Args[1]), ".png"))
		}
	default:
		os.Exit(24)
	}
}

func pngHeader(width, height uint32) []byte {
	b := make([]byte, 33)
	copy(b, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(b[8:], 13)
	copy(b[12:], "IHDR")
	binary.BigEndian.PutUint32(b[16:], width)
	binary.BigEndian.PutUint32(b[20:], height)
	b[24], b[25] = 8, 0
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

func assertClean(t *testing.T) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), ".niro-documents-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("working directories leaked: %v, %v", matches, err)
	}
}

func TestPDFReadOnlyWorkingDirectory(t *testing.T) {
	temp := fakeTools(t, "pdftotext", "pdftoppm", "tesseract")
	cwd := workDir(t)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cwd, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cwd, 0755)
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	pages, err := (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
	if err != nil || !reflect.DeepEqual(pages, []Page{{1, "first"}, {2, "OCR page-2"}, {3, "third"}}) {
		t.Fatalf("read-only CWD extraction: %v, %v", pages, err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil || len(entries) != 0 {
		t.Fatalf("parser wrote to CWD: %v, %v", entries, err)
	}
	if os.TempDir() != temp {
		t.Fatalf("temporary location changed: %q", os.TempDir())
	}
	assertClean(t)
}

func TestPDFTextAndOCR(t *testing.T) {
	fakeTools(t, "pdftotext", "pdftoppm", "tesseract")
	pages, err := (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
	want := []Page{{1, "first"}, {2, "OCR page-2"}, {3, "third"}}
	if err != nil || !reflect.DeepEqual(pages, want) {
		t.Fatalf("got %v, %v", pages, err)
	}
	assertClean(t)
	t.Setenv("DOCUMENTS_TEST_MODE", "all-empty")
	t.Setenv("DOCUMENTS_TEST_PAGES", "12")
	for _, concurrency := range []int{-1, 1, 3, 100000} {
		pages, err := (Parser{Concurrency: concurrency}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
		if err != nil || len(pages) != 12 {
			t.Fatalf("concurrency %d: %v, %v", concurrency, pages, err)
		}
		for i, page := range pages {
			if page.Number != i+1 || page.Text != fmt.Sprintf("OCR page-%d", i+1) {
				t.Fatalf("out-of-order page: %+v", page)
			}
		}
		assertClean(t)
	}
}

func TestPDFNoOCRNeeded(t *testing.T) {
	fakeTools(t, "pdftotext")
	t.Setenv("DOCUMENTS_TEST_MODE", "text")
	pages, err := (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
	if err != nil || !reflect.DeepEqual(pages, []Page{{1, "first"}, {2, "second"}}) {
		t.Fatalf("%v, %v", pages, err)
	}
	assertClean(t)
	t.Setenv("DOCUMENTS_TEST_MODE", "hundred")
	pages, err = (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
	if err != nil || len(pages) != maxPages {
		t.Fatalf("exact page limit rejected: %d pages, %v", len(pages), err)
	}
	assertClean(t)
}

func TestPDFWorkerBounds(t *testing.T) {
	dir := fakeTools(t, "pdftotext", "pdftoppm", "tesseract")
	t.Setenv("DOCUMENTS_TEST_MODE", "workers")
	t.Setenv("DOCUMENTS_TEST_PAGES", "12")
	for _, tt := range []struct{ concurrency, limit int }{{0, 2}, {1, 1}, {3, 3}, {100, 8}} {
		t.Run(strconv.Itoa(tt.concurrency), func(t *testing.T) {
			logPath := filepath.Join(dir, "events-"+strconv.Itoa(tt.concurrency))
			t.Setenv("DOCUMENTS_TEST_LOG", logPath)
			_, err := (Parser{Concurrency: tt.concurrency}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			type event struct {
				when  int64
				delta int
			}
			var events []event
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var e event
				if _, err := fmt.Sscanf(line, "%d %d", &e.when, &e.delta); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			sort.Slice(events, func(i, j int) bool { return events[i].when < events[j].when })
			active, peak := 0, 0
			for _, e := range events {
				active += e.delta
				if active > peak {
					peak = active
				}
			}
			if len(events) != 24 || active != 0 || peak < 1 || peak > tt.limit {
				t.Fatalf("worker limit %d: events=%d active=%d peak=%d", tt.limit, len(events), active, peak)
			}
			assertClean(t)
		})
	}
}

func TestPDFErrors(t *testing.T) {
	fakeTools(t, "pdftotext", "pdftoppm", "tesseract")
	for _, tt := range []struct{ mode, want string }{
		{"fail", "encrypted"}, {"overflow", "safety limit"},
		{"stderr-overflow", "safety limit"}, {"invalid-utf8", "UTF-8"},
		{"many", "100-page"}, {"raster-large", "dimensions"},
		{"raster-invalid", "PNG"}, {"raster-file-large", "32 MiB"}, {"ocr-overflow", "safety limit"},
		{"aggregate", "2 MiB"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			t.Setenv("DOCUMENTS_TEST_MODE", tt.mode)
			pages, err := (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
			if err == nil || !strings.Contains(err.Error(), tt.want) || pages != nil {
				t.Fatalf("got %d pages, %v; want %q", len(pages), err, tt.want)
			}
			assertClean(t)
		})
	}
}

func TestMissingUtilities(t *testing.T) {
	for _, names := range [][]string{nil, {"pdftotext"}, {"pdftotext", "pdftoppm"}} {
		t.Run(strconv.Itoa(len(names)), func(t *testing.T) {
			fakeTools(t, names...)
			_, err := (Parser{}).Extract(context.Background(), "x.pdf", []byte("%PDF-1.7"))
			missing := []string{"pdftotext", "pdftoppm", "tesseract"}[len(names)]
			if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "install") {
				t.Fatalf("expected actionable error for %s: %v", missing, err)
			}
			assertClean(t)
		})
	}
}

func TestCancellation(t *testing.T) {
	fakeTools(t, "pdftotext", "pdftoppm", "tesseract")
	for _, mode := range []string{"cancel", "cancel-ocr"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("DOCUMENTS_TEST_MODE", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := (Parser{}).Extract(ctx, "x.pdf", []byte("%PDF-1.7"))
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
				t.Fatalf("cancellation took %v: %v", time.Since(start), err)
			}
			assertClean(t)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Parser{}).Extract(ctx, "x.txt", []byte("hello"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled context: %v", err)
	}
}
