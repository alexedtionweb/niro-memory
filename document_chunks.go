package niromem

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/memory"
)

const documentChunkTarget = 1800
const documentChunkMax = 3500

var documentHeading = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}[ \t]+[^\n]+|[0-9]+(?:\.[0-9]+)+[.)]?[ \t]+[^\n]+)`)
var documentSectionNumber = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+`)
var documentURL = regexp.MustCompile(`https?://\S+`)

type documentChunk struct {
	start, end       int
	section, heading string
	continuation     bool
	navigation       bool
}

func (c documentChunk) entities(documentID string) []memory.EntityRef {
	var out []memory.EntityRef
	if c.heading != "" {
		name := strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(c.heading, c.section), ".)"))
		if name != "" {
			out = append(out, memory.EntityRef{Name: name, Type: "document_section"})
		}
	}
	if c.section != "" && c.section != c.heading {
		out = append(out, memory.EntityRef{Name: c.section, Type: "document_section"})
	}
	return append(out, memory.EntityRef{Name: documentID, Type: "document"})
}

// Offsets always refer to the unmodified parser output, including whitespace.
func documentChunks(text string) []documentChunk {
	headings := documentHeading.FindAllStringIndex(text, -1)
	var out []documentChunk
	start, section, heading := 0, "", ""
	appendSection := func(end int) {
		for start < end {
			stop := documentChunkEnd(text, start, end)
			body := text[start:stop]
			if strings.TrimSpace(body) != "" {
				prose := documentHeading.ReplaceAllStringFunc(body, func(line string) string {
					if len(line) <= 240 {
						return ""
					}
					return line
				})
				prose = documentURL.ReplaceAllString(prose, "")
				out = append(out, documentChunk{start: start, end: stop, section: section, heading: heading,
					continuation: start > 0 && !strings.ContainsAny(text[start-1:start], "\n.!?"),
					navigation:   strings.Trim(prose, " \t\r\n[]()<>-*#") == ""})
			}
			start = stop
		}
	}
	for _, h := range headings {
		if h[1]-h[0] > 240 {
			continue
		}
		appendSection(h[0])
		heading = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text[h[0]:h[1]]), "#"))
		section = documentSectionNumber.FindString(heading)
	}
	appendSection(len(text))
	return out
}

func documentChunkEnd(text string, start, end int) int {
	if end-start <= documentChunkTarget {
		return end
	}
	limit := min(start+documentChunkMax, end)
	for limit < end && !utf8.RuneStart(text[limit]) {
		limit--
	}
	last := 0
	for pos := start; pos < limit; {
		r, n := utf8.DecodeRuneInString(text[pos:limit])
		next := pos + n
		boundary := r == '\n' && next < end && text[next] == '\n'
		if strings.ContainsRune(".!?", r) && next < end {
			following, _ := utf8.DecodeRuneInString(text[next:])
			boundary = unicode.IsSpace(following)
		}
		if boundary {
			last = next
			if next-start >= documentChunkTarget {
				return next
			}
		}
		pos = next
	}
	if limit == end {
		return end
	}
	if last > start {
		return last
	}
	// A single oversized sentence still needs a bounded, UTF-8-safe chunk.
	if space := strings.LastIndexAny(text[start:limit], " \t"); space > 0 {
		return start + space + 1
	}
	return limit
}
