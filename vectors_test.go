package niromem_test

import (
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/vector"
)

func TestEffectiveVectorConfiguration(t *testing.T) {
	e := engine(t, func(o *niromem.Options) { o.Embedder = embed.Hash{Dims: 8} })
	config, err := e.VectorConfiguration(ctx, "one")
	if err != nil || len(config.Models) != 1 || config.Active != "hash@1" || !config.Models[0].Active || config.Models[0].Dims != 8 {
		t.Fatalf("implicit embedder: %+v %v", config, err)
	}
	settings := niromem.Settings{VectorModels: []vector.Model{{ID: "external@1", Dims: 4, Active: true}}}
	if err := e.PutSettings(ctx, "one", settings); err != nil {
		t.Fatal(err)
	}
	config, err = e.VectorConfiguration(ctx, "one")
	if err != nil || config.Active != "external@1" || len(config.Models) != 2 || !config.Models[0].Active || config.Models[1].Active {
		t.Fatalf("explicit active model: %+v %v", config, err)
	}
	other, err := e.VectorConfiguration(ctx, "two")
	if err != nil || other.Active != "hash@1" || len(other.Models) != 1 {
		t.Fatalf("tenant settings leaked: %+v %v", other, err)
	}
}

func TestBackfillMemoriesCreatedBeforeEmbedder(t *testing.T) {
	base := engine(t)
	for _, scope := range []memory.Scope{{Tenant: "one", User: "u"}, {Tenant: "two", User: "u"}} {
		if _, err := base.Remember(ctx, niromem.Fact{Scope: scope, Text: "An existing memory without a vector."}); err != nil {
			t.Fatal(err)
		}
	}
	e := engine(t, func(o *niromem.Options) { o.Store = base.Store(); o.Embedder = embed.Hash{Dims: 8} })
	have, total, err := e.Coverage(ctx, "one", "hash@1")
	if err != nil || have != 0 || total != 1 {
		t.Fatalf("existing item must start missing: %d/%d %v", have, total, err)
	}
	if err := e.Backfill(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	have, total, err = e.Coverage(ctx, "one", "hash@1")
	if err != nil || have != 1 || total != 1 {
		t.Fatalf("existing item not embedded: %d/%d %v", have, total, err)
	}
	have, total, err = e.Coverage(ctx, "two", "hash@1")
	if err != nil || have != 0 || total != 1 {
		t.Fatalf("backfill crossed tenant: %d/%d %v", have, total, err)
	}
	if err := e.Backfill(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	have, total, err = e.Coverage(ctx, "one", "hash@1")
	if err != nil || have != 1 || total != 1 {
		t.Fatalf("repeat backfill changed coverage: %d/%d %v", have, total, err)
	}
}
