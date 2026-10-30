package embed

import (
	"context"
	"testing"

	"github.com/alexedtionweb/niro-memory/vector"
)

func TestHashEmbedder(t *testing.T) {
	h := Hash{Dims: 64}
	vs, u, err := h.Embed(context.Background(), Document, []Input{{Text: "red bicycle"}, {Text: "a red bicycle!"}, {Text: "tax report"}, {Text: ""}})
	if err != nil || u.Inputs != 4 || len(vs[0]) != 64 {
		t.Fatal(err)
	}
	if vector.Dot(vs[0], vs[1]) <= vector.Dot(vs[0], vs[2]) {
		t.Fatal("overlapping texts should be closer")
	}
	again, _, _ := h.Embed(context.Background(), Query, []Input{{Text: "red bicycle"}})
	if vector.Dot(vs[0], again[0]) < 0.9999 {
		t.Fatal("not deterministic")
	}
}
