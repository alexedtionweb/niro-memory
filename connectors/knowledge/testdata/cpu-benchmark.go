package main

import (
	"context"
	"encoding/json"
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
	}
	models := os.Args[1:]
	if len(models) == 0 {
		models = []string{"qwen3:0.6b", "qwen3:1.7b"}
	}
	repeats := 5
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
		valid, quality := 0, 0
		outputs := []map[string]any{}
		for repeat := 0; repeat < repeats; repeat++ {
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
				item := map[string]any{"text": tc.Text, "ms": ms, "passed": ok, "result": out}
				if err != nil {
					item["error"] = err.Error()
				}
				outputs = append(outputs, item)
			}
		}
		sort.Float64s(durations)
		row := map[string]any{"model": model, "samples": len(durations), "valid": valid, "quality_passes": quality, "p50_ms": durations[(len(durations)-1)/2], "p95_ms": durations[(95*len(durations)+99)/100-1], "warmup_ms": warm.Milliseconds(), "outputs": outputs}
		if warmErr != nil {
			row["warmup_error"] = warmErr.Error()
		}
		results = append(results, row)
		fmt.Fprintf(os.Stderr, "%s valid=%d/%d quality=%d/%d p50=%.0fms p95=%.0fms\n", model, valid, len(durations), quality, len(durations), row["p50_ms"], row["p95_ms"])
	}
	raw, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(raw))
}
