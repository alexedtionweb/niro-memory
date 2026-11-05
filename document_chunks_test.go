package niromem

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentChunksPreserveSectionsSentencesAndOffsets(t *testing.T) {
	first := "5.2.1.1 Validacion de instalacion\n" + strings.Repeat("Antes de activar, confirme que el cliente termino la instalacion. ", 75)
	second := "5.2.1.2 Smartcard\nIngrese el numero de smartcard.\n"
	text := first + "\n" + second
	chunks := documentChunks(text)
	var reconstructed strings.Builder
	for i, c := range chunks {
		body := text[c.start:c.end]
		reconstructed.WriteString(body)
		if !utf8.ValidString(body) || len(body) > documentChunkMax {
			t.Fatalf("invalid UTF-8/size: %+v", c)
		}
		if c.start < len(first) && (c.section != "5.2.1.1" || c.end > len(first)+1) {
			t.Fatalf("section mixed or lost: %+v", c)
		}
		if i+1 < len(chunks) && !strings.HasSuffix(strings.TrimSpace(body), ".") {
			t.Fatalf("sentence split: %q", body)
		}
	}
	if reconstructed.String() != text || len(chunks) < 3 || chunks[len(chunks)-1].section != "5.2.1.2" {
		t.Fatal("source lost or section not detected")
	}
}

func TestDocumentChunkPathologicalInputs(t *testing.T) {
	for _, text := range []string{"", " \n ", strings.Repeat("a", 9000), strings.Repeat("\u00e7", 5000), strings.Repeat("sentence. ", 1000), "# Heading\nhttps://example.org/manual"} {
		prev := 0
		for _, c := range documentChunks(text) {
			if c.start < prev || c.end <= c.start || c.end-c.start > documentChunkMax || !utf8.ValidString(text[c.start:c.end]) {
				t.Fatalf("invalid bounds for %d bytes: %+v", len(text), c)
			}
			prev = c.end
		}
		if strings.TrimSpace(text) != "" && prev != len(text) {
			t.Fatalf("missing source tail: %d != %d", prev, len(text))
		}
	}
	nav := documentChunks("# Manual\nhttps://example.org/manual")
	rule := documentChunks("5.1 Activacion\nConecte el equipo y espere la luz verde.")
	if len(nav) != 1 || !nav[0].navigation || len(rule) != 1 || rule[0].navigation {
		t.Fatal("navigation classification")
	}
}
