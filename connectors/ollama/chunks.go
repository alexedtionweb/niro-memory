package ollama

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/embed"
)

var errContextLength = errors.New("the input length exceeds the context length")

// Embed preserves one vector per original input. Chunking is opt-in because it
// changes the embedding strategy and therefore requires a distinct model ID.
func (e *Embedder) Embed(ctx context.Context, purpose embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	if e.o.ChunkSize == 0 {
		return e.embedRaw(ctx, purpose, in)
	}
	usage := embed.Usage{Inputs: len(in)}
	type chunk struct {
		text          string
		owner, weight int
	}
	var chunks []chunk
	for owner, input := range in {
		if (input.Modality != "" && input.Modality != "text") || len(input.Data) != 0 || !utf8.ValidString(input.Text) {
			return nil, usage, fmt.Errorf("ollama: chunked embedding requires valid UTF-8 text inputs")
		}
		text := input.Text
		for {
			end := min(e.o.ChunkSize, len(text))
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end--
			}
			part := text[:end]
			chunks = append(chunks, chunk{part, owner, max(1, utf8.RuneCountInString(part))})
			if len(chunks) > 4096 {
				return nil, usage, fmt.Errorf("ollama: embedding exceeds 4096 chunks; split the input batch")
			}
			text = text[end:]
			if text == "" {
				break
			}
		}
	}
	// Each successful leaf contributes its source-rune weight, including leaves
	// created after a context rejection. Prefix bytes never affect the weight.
	sums := make([][]float64, len(in))
	counts := make([]int, len(in))
	weights := make([]int, len(in))
	for i := range sums {
		sums[i] = make([]float64, e.o.Dims)
	}
	var process func([]chunk) error
	process = func(batch []chunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		inputs := make([]embed.Input, len(batch))
		for i, c := range batch {
			inputs[i] = embed.Input{Text: c.text}
		}
		vectors, used, err := e.embedRaw(ctx, purpose, inputs)
		usage.Tokens += used.Tokens
		if err != nil {
			if !errors.Is(err, errContextLength) {
				return err
			}
			if len(batch) > 1 {
				mid := len(batch) / 2
				if err := process(batch[:mid]); err != nil {
					return err
				}
				return process(batch[mid:])
			}
			c := batch[0]
			if utf8.RuneCountInString(c.text) < 2 {
				return fmt.Errorf("ollama: context limit exceeded by minimal chunk; check prefix and model context: %w", err)
			}
			mid := len(c.text) / 2
			for mid > 0 && !utf8.RuneStart(c.text[mid]) {
				mid--
			}
			if mid == 0 {
				_, mid = utf8.DecodeRuneInString(c.text)
			}
			left, right := c.text[:mid], c.text[mid:]
			return process([]chunk{{left, c.owner, utf8.RuneCountInString(left)}, {right, c.owner, utf8.RuneCountInString(right)}})
		}
		for i, v := range vectors {
			c := batch[i]
			counts[c.owner]++
			weights[c.owner] += c.weight
			for j, value := range v {
				sums[c.owner][j] += float64(value) * float64(c.weight)
			}
		}
		return nil
	}
	for start := 0; start < len(chunks); start += e.o.BatchSize {
		if err := process(chunks[start:min(start+e.o.BatchSize, len(chunks))]); err != nil {
			return nil, usage, err
		}
	}
	out := make([][]float32, len(in))
	for i, sum := range sums {
		var norm float64
		for _, v := range sum {
			norm += v * v
		}
		if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
			return nil, usage, fmt.Errorf("ollama: combined embedding has invalid norm")
		}
		out[i] = make([]float32, e.o.Dims)
		scale := math.Sqrt(norm)
		if counts[i] == 1 {
			// A single valid chunk should retain the provider's original vector.
			scale = float64(weights[i])
		}
		for j, v := range sum {
			out[i][j] = float32(v / scale)
		}
	}
	return out, usage, nil
}
