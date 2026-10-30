package sqlite

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/textutil"
)

// Full-text search is split in two steps that keep its cost proportional to
// the requester's scope, not to the whole database:
//
//  1. FTS5 only selects candidates: (scope token) AND (exact terms). No
//     bm25() auxiliary function and no FTS5 prefix iterators, both of which
//     walk global posting lists.
//  2. Go scores candidates with Okapi BM25 (k1=1.2, b=0.75). Document
//     frequencies come from the fts5vocab term dictionary; prefix terms
//     (store.PrefixMin) are expanded there with a range scan.

const (
	maxExpansions = 24   // index terms considered per prefix term
	maxCandidates = 2000 // rows scored per query
)

// expansion maps each query term to the index terms it matches and their
// document frequency.
type expansion struct {
	of map[string]string  // index term -> query term
	df map[string]float64 // query term -> documents containing it (or any expansion)
}

func (s *Store) expand(ctx context.Context, vocab string, terms []string) (*expansion, error) {
	x := &expansion{of: map[string]string{}, df: map[string]float64{}}
	seen := map[string]bool{}
	for _, t := range terms {
		for _, tok := range textutil.Tokens(t) {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			var rows *sql.Rows
			var err error
			if utf8.RuneCountInString(tok) >= store.PrefixMin {
				rows, err = s.r.QueryContext(ctx, `SELECT term, doc FROM `+vocab+` WHERE term >= ? AND term < ? ORDER BY doc DESC LIMIT ?`,
					tok, tok+"\U0010FFFF", maxExpansions)
			} else {
				rows, err = s.r.QueryContext(ctx, `SELECT term, doc FROM `+vocab+` WHERE term = ?`, tok)
			}
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var term string
				var doc float64
				if err := rows.Scan(&term, &doc); err != nil {
					rows.Close()
					return nil, err
				}
				if _, dup := x.of[term]; !dup {
					x.of[term] = tok
				}
				x.df[tok] += doc
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	return x, nil
}

// match returns the FTS5 expression for the expanded terms (exact, quoted).
func (x *expansion) match(token string) string {
	if len(x.of) == 0 {
		return ""
	}
	terms := make([]string, 0, len(x.of))
	for t := range x.of {
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	sort.Strings(terms)
	return scoped(token, strings.Join(terms, " OR "))
}

// doc is a candidate for scoring.
type doc struct {
	key  string
	text string
}

// bm25 scores documents against the expanded query.
func (x *expansion) bm25(docs []doc, n float64) map[string]float64 {
	const k1, b = 1.2, 0.75
	toks := make([][]string, len(docs))
	var total float64
	for i, d := range docs {
		toks[i] = textutil.Tokens(d.text)
		total += float64(len(toks[i]))
	}
	avg := math.Max(total/math.Max(float64(len(docs)), 1), 1)
	out := make(map[string]float64, len(docs))
	for i, d := range docs {
		tf := map[string]float64{}
		for _, t := range toks[i] {
			if q, ok := x.of[t]; ok {
				tf[q]++
			}
		}
		dl := float64(len(toks[i]))
		var sc float64
		for q, f := range tf {
			df := math.Min(x.df[q], n)
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			sc += idf * f * (k1 + 1) / (f + k1*(1-b+b*dl/avg))
		}
		out[d.key] = sc
	}
	return out
}

// corpusSize estimates the number of indexed documents from the rowid range.
func (s *Store) corpusSize(ctx context.Context, table string) float64 {
	var n sql.NullInt64
	s.r.QueryRowContext(ctx, `SELECT max(rowid) FROM `+table).Scan(&n)
	return math.Max(float64(n.Int64), 1)
}
