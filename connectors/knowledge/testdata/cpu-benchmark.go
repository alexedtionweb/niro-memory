package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	provider "github.com/alexedtionweb/niro-memory/connectors/knowledge"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	repeats := flag.Int("repeats", 5, "repetitions per case (1..100)")
	flag.Parse()
	if *repeats < 1 || *repeats > 100 {
		fmt.Fprintln(os.Stderr, "repeats must be 1..100")
		os.Exit(2)
	}
	type sample struct {
		Text       string
		Candidates []memory.Memory
		Want       string
	}
	cases := []sample{
		{"Atlas uses PostgreSQL.", nil, "postgresql"},
		{"We decided to use Redis for caching because reads must be fast.", nil, "decision"},
		{"Hello! How are you?", nil, "empty"},
		{"Atlas does not use MySQL.", []memory.Memory{{Text: "Atlas uses MySQL.", Subject: "Atlas", Predicate: "uses", Object: "MySQL", Metadata: map[string]any{"assertion": memory.Assertion{
			Subject: "Atlas", Predicate: "uses", Object: "MySQL", EpistemicType: memory.EpistemicFact, Confidence: 1, Conditions: map[string]string{"polarity": "positive"},
		}}}}, "contradicts"},
		{"Atlas uses PostgreSQL!", []memory.Memory{{Text: "Atlas uses PostgreSQL.", Subject: "Atlas", Predicate: "uses", Object: "PostgreSQL", Metadata: map[string]any{"assertion": memory.Assertion{
			Subject: "Atlas", Predicate: "uses", Object: "PostgreSQL", EpistemicType: memory.EpistemicFact, Confidence: .5, Conditions: map[string]string{"polarity": "positive"},
		}}}}, "postgresql"},
		{"Ana trabalha na Acme em Lisboa.", nil, "acme"},
	}
	models := flag.Args()
	if len(models) == 0 {
		models = []string{"qwen3:0.6b", "qwen3:1.7b"}
	}
	results := []map[string]any{}
	for _, model := range models {
		c, err := provider.New(provider.Options{Protocol: "ollama", BaseURL: "http://localhost:11434", Model: model})
		if err != nil {
			panic(err)
		}
		// Warm-up is reported separately from the warm-request distribution.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		start := time.Now()
		_, warmErr := c.Enrich(ctx, knowledge.EnrichmentInput{Text: cases[0].Text})
		warm := time.Since(start)
		cancel()
		durations := []float64{}
		valid, quality, entityQuality := 0, 0, 0
		outputs := []map[string]any{}
		for repeat := 0; repeat < *repeats; repeat++ {
			for _, tc := range cases {
				ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
				start := time.Now()
				out, err := c.Enrich(ctx, knowledge.EnrichmentInput{Text: tc.Text, Candidates: tc.Candidates})
				ms := float64(time.Since(start).Microseconds()) / 1000
				cancel()
				durations = append(durations, ms)
				ok := false
				if err == nil {
					valid++
					switch tc.Want {
					case "empty":
						ok = len(out.Facts) == 0
					case "decision":
						for _, f := range out.Facts {
							ok = ok || f.Type == memory.EpistemicDecision
						}
					case "contradicts":
						for _, r := range out.Relations {
							ok = ok || r.Relation == "contradicts"
						}
					default:
						for _, f := range out.Facts {
							ok = ok || strings.Contains(strings.ToLower(f.Object), tc.Want)
						}
					}
				}
				if ok {
					quality++
				}
				entityOK := err == nil
				names := []string{}
				switch tc.Want {
				case "empty":
					entityOK = entityOK && len(out.Entities) == 0
				case "decision":
					names = []string{"Redis"}
				case "contradicts":
					names = []string{"Atlas", "MySQL"}
				case "acme":
					names = []string{"Ana", "Acme"}
				default:
					names = []string{"Atlas", "PostgreSQL"}
				}
				for _, name := range names {
					found := false
					for _, entity := range out.Entities {
						found = found || strings.EqualFold(entity.Name, name) && entity.Type != ""
					}
					entityOK = entityOK && found
				}
				if entityOK {
					entityQuality++
				}
				item := map[string]any{"text": tc.Text, "ms": ms, "passed": ok, "entities_passed": entityOK, "result": out}
				if err != nil {
					item["error"] = err.Error()
				}
				outputs = append(outputs, item)
			}
		}
		sort.Float64s(durations)
		row := map[string]any{"model": model, "samples": len(durations), "valid": valid, "quality_passes": quality, "entity_passes": entityQuality, "p50_ms": durations[(len(durations)-1)/2], "p95_ms": durations[(95*len(durations)+99)/100-1], "warmup_ms": warm.Milliseconds(), "outputs": outputs}
		if warmErr != nil {
			row["warmup_error"] = warmErr.Error()
		}
		results = append(results, row)
		fmt.Fprintf(os.Stderr, "%s valid=%d/%d quality=%d/%d entities=%d/%d p50=%.0fms p95=%.0fms\n", model, valid, len(durations), quality, len(durations), entityQuality, len(durations), row["p50_ms"], row["p95_ms"])
	}
	raw, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(raw))
}
