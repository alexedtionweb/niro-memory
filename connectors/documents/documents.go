// Package documents extracts text locally from TXT, Markdown, DOCX, and PDF files.
// PDF extraction requires Poppler; scanned pages additionally require Tesseract.
package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxInput = 20 << 20
	maxText  = 2 << 20
	maxXML   = 8 << 20
	maxPages = 100
	timeout  = 2 * time.Minute
)

// Page is a one-based document page. TXT, Markdown, and DOCX produce one page;
// DOCX paragraph boundaries are retained, but its rendered pagination is unknown.
type Page struct {
	Number int
	Text   string
}

// Parser uses at most Concurrency OCR workers (default 2, maximum 8).
// PDF processing uses a private, automatically removed directory in the system
// temporary location (respecting TMPDIR); the current directory may be read-only.
// No network services are used. OCR uses Tesseract's default English language;
// this API does not expose a language setting.
type Parser struct {
	Concurrency int
}

// Validate performs inexpensive extension, signature, UTF-8, and size checks.
// It inspects DOCX ZIP metadata, not XML content. It does not fully parse PDFs.
func Validate(filename string, data []byte) error {
	if len(data) > maxInput {
		return errors.New("document exceeds 20 MiB input limit")
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt", ".md", ".markdown":
		if !utf8.Valid(data) {
			return errors.New("text document must be valid UTF-8")
		}
	case ".pdf":
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return errors.New("invalid PDF signature")
		}
	case ".docx":
		_, err := documentXML(data)
		return err
	default:
		return errors.New("unsupported document format: use PDF, TXT, Markdown, or DOCX")
	}
	return nil
}

// Extract returns document pages in order. Extraction is canceled with ctx and
// has a two-minute total deadline, a 100-page PDF limit, and a 2 MiB text limit.
func (p Parser) Extract(ctx context.Context, filename string, data []byte) ([]Page, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Validate(filename, data); err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		return p.pdf(ctx, data)
	case ".docx":
		text, err := extractDOCX(ctx, data)
		if err != nil {
			return nil, err
		}
		return []Page{{Number: 1, Text: text}}, nil
	default:
		if len(data) > maxText {
			return nil, errors.New("document exceeds 2 MiB text limit")
		}
		return []Page{{Number: 1, Text: string(data)}}, nil
	}
}

func documentXML(data []byte) (*zip.File, error) {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return nil, errors.New("invalid DOCX ZIP signature")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid DOCX archive: %w", err)
	}
	var document *zip.File
	for _, f := range z.File {
		if f.Flags&1 != 0 {
			return nil, errors.New("encrypted DOCX archives are unsupported")
		}
		if f.Name != "word/document.xml" {
			continue
		}
		if document != nil {
			return nil, errors.New("DOCX contains duplicate word/document.xml entries")
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, errors.New("unsupported DOCX document compression")
		}
		if f.UncompressedSize64 > maxXML {
			return nil, errors.New("DOCX XML exceeds 8 MiB decompression limit")
		}
		document = f
	}
	if document == nil {
		return nil, errors.New("DOCX is missing word/document.xml")
	}
	return document, nil
}

func extractDOCX(ctx context.Context, data []byte) (string, error) {
	f, err := documentXML(data)
	if err != nil {
		return "", err
	}
	r, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("open DOCX XML: %w", err)
	}
	defer r.Close()
	// Read through EOF to verify ZIP checksum, even when XML ends early.
	raw, err := io.ReadAll(io.LimitReader(r, maxXML+1))
	if err != nil {
		return "", fmt.Errorf("read DOCX XML: %w", err)
	}
	if len(raw) > maxXML {
		return "", errors.New("DOCX XML exceeds 8 MiB decompression limit")
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	var out strings.Builder
	var namespace string
	depth, textDepth := 0, 0
	rootSeen := false
	appendText := func(s string) error {
		if out.Len()+len(s) > maxText {
			return errors.New("document exceeds 2 MiB text limit")
		}
		out.WriteString(s)
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid DOCX XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if rootSeen || t.Name.Local != "document" ||
					(t.Name.Space != "http://schemas.openxmlformats.org/wordprocessingml/2006/main" &&
						t.Name.Space != "http://purl.oclc.org/ooxml/wordprocessingml/main") {
					return "", errors.New("unsupported DOCX XML document root")
				}
				rootSeen, namespace = true, t.Name.Space
			}
			depth++
			if t.Name.Space == namespace {
				switch t.Name.Local {
				case "t":
					textDepth = depth
				case "tab":
					err = appendText("\t")
				case "br", "cr":
					err = appendText("\n")
				}
			}
		case xml.EndElement:
			if t.Name.Space == namespace && t.Name.Local == "p" {
				err = appendText("\n")
			}
			if depth == textDepth {
				textDepth = 0
			}
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return "", errors.New("invalid text outside DOCX XML root")
			}
			if textDepth > 0 {
				err = appendText(string(t))
			}
		case xml.Directive:
			return "", errors.New("DOCX XML directives are unsupported")
		}
		if err != nil {
			return "", err
		}
	}
	if !rootSeen {
		return "", errors.New("DOCX XML document is empty")
	}
	return strings.TrimSpace(out.String()), nil
}
