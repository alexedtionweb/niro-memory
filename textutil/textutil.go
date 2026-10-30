// Package textutil holds the small, language-neutral text helpers shared by
// retrieval and processing: term extraction, n-grams, sentence segmentation
// with byte offsets, and token-set similarity.
//
// Nothing here knows a specific human language: terms are Unicode letter and
// digit runs, sentences end at ., !, ?, newlines and their CJK equivalents.
package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTermLen drops pathological tokens (hashes, base64) from term lists.
const MaxTermLen = 64

// Terms returns the lower-cased letter/digit runs of s, deduplicated, in order
// of first appearance, at most max terms (0 = unlimited).
func Terms(s string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range Tokens(s) {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if max > 0 && len(out) == max {
			break
		}
	}
	return out
}

// Tokens returns all lower-cased letter/digit runs of s, with repeats.
func Tokens(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		word := unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r)
		if word && start < 0 {
			start = i
		}
		if !word && start >= 0 {
			out = appendTok(out, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = appendTok(out, s[start:])
	}
	return out
}

func appendTok(out []string, t string) []string {
	if len(t) > MaxTermLen {
		return out
	}
	return append(out, strings.ToLower(t))
}

// NGrams returns the joined word n-grams of the tokens for n = 1..max,
// longest first. They are used to look up multi-word entity keys.
func NGrams(tokens []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for n := max; n >= 1; n-- {
		for i := 0; i+n <= len(tokens); i++ {
			g := strings.Join(tokens[i:i+n], " ")
			if !seen[g] {
				seen[g] = true
				out = append(out, g)
			}
		}
	}
	return out
}

// Span is a byte range [Start, End) of a text.
type Span struct{ Start, End int }

// Sentences splits s into sentence spans. A sentence ends after '.', '!',
// '?', '。', '！', '？' or '…' followed by whitespace or end of text, or at a newline.
// Surrounding whitespace is excluded from the spans.
func Sentences(s string) []Span {
	var out []Span
	start := 0
	emit := func(end int) {
		a, b := start, end
		for a < b {
			r, n := utf8.DecodeRuneInString(s[a:])
			if !unicode.IsSpace(r) {
				break
			}
			a += n
		}
		for b > a {
			r, n := utf8.DecodeLastRuneInString(s[:b])
			if !unicode.IsSpace(r) {
				break
			}
			b -= n
		}
		if b > a {
			out = append(out, Span{a, b})
		}
	}
	for i, r := range s {
		size := utf8.RuneLen(r)
		switch r {
		case '\n':
			emit(i)
			start = i + size
		case '.', '!', '?', '。', '！', '？', '…':
			next := i + size
			if next >= len(s) {
				continue
			}
			nr, _ := utf8.DecodeRuneInString(s[next:])
			if unicode.IsSpace(nr) || r == '。' || r == '！' || r == '？' {
				emit(next)
				start = next
			}
		}
	}
	emit(len(s))
	return out
}

// Jaccard returns |a∩b| / |a∪b| of two term sets. Used by MMR when no
// vectors are available.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[t] = struct{}{}
	}
	inter := 0
	for _, t := range b {
		if _, ok := set[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
