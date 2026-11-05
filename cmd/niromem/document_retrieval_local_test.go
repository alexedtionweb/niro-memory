package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

type retrievalDocumentParser struct{}

func (retrievalDocumentParser) Validate(string, []byte) error { return nil }
func (retrievalDocumentParser) Extract(_ context.Context, _ string, data []byte) ([]niromem.DocumentPage, error) {
	return []niromem.DocumentPage{{Number: 1, Text: string(data)}}, nil
}

func TestLocalDocumentRetrievalPortugueseToSpanish(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EMBEDDING") != "1" {
		t.Skip("opt-in local embedding test; generated document and isolated database")
	}
	cfg, err := loadConfig("../../configs/lfm2.5-350m.example.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "retrieval.db")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var options niromem.Options
	closers, err := connectors(ctx, &Config{Store: Choice{Use: "sqlite", Options: map[string]json.RawMessage{"sqlite": raw}}, Embedder: cfg.Embedder}, &options)
	for _, closer := range closers {
		defer closer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	options.Documents = retrievalDocumentParser{}
	options.Blob = blob.NewMem()
	e, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	document := `# Manual
https://example.org/sharepoint/manual
5.2.1.1 Validacion de instalacion
Si el cliente confirma que completo la instalacion y desea activar el servicio, primero valide la instalacion del decodificador. Confirme que este enchufado y encendido, que los cables esten bien ajustados y que el nivel de senal sea igual o mayor al 70%. Luego continue con la activacion.
5.2.2 Compra del kit
Para comprar un kit prepago, indique los enlaces de compra correspondientes al pais del cliente. El kit incluye el decodificador y la tarjeta.
5.2.7 Planes y seguros
Ofrezca los planes de suscripcion y seguros disponibles segun la oferta comercial.
5.3 Facturacion
Para consultar una factura vencida, valide el saldo y la fecha de vencimiento antes de informar los medios de pago.
5.4 Datos del IVR
Registre los datos de contacto capturados en el sistema telefonico y confirme el numero de solicitud.
`
	_, err = e.ImportDocument(ctx, memory.Scope{Tenant: "fixture", User: "u"}, "manual.txt", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := options.Store.Jobs(ctx, store.JobQuery{Tenant: "fixture", Kind: niromem.JobDocumentImport})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("import jobs: %v %v", jobs, err)
	}
	if err := e.Worker().Handlers[niromem.JobDocumentImport](ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.Worker().Handlers[niromem.JobEmbed](ctx, store.Job{Tenant: "fixture"}); err != nil {
		t.Fatal(err)
	}
	have, total, err := e.Coverage(ctx, "fixture", options.Embedder.Model().ID)
	if err != nil || have != total || total != 6 {
		t.Fatalf("coverage: %d/%d %v", have, total, err)
	}
	for _, tc := range []struct{ query, section string }{
		{"o cliente terminou de instalar o decodificador e quer ativar o servico", "5.2.1.1"},
		{"onde comprar o kit pre-pago", "5.2.2"},
		{"como confirmar se os cabos e o sinal estao corretos", "5.2.1.1"},
	} {
		r, err := e.Search(ctx, niromem.SearchRequest{Tenant: "fixture", As: memory.Principal{User: "u"}, Query: tc.query, Explain: true, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		rank := 0
		for i, hit := range r.Hits {
			if hit.Memory.Metadata["section"] == tc.section {
				rank = i + 1
				break
			}
		}
		vectorRan := false
		for _, step := range r.Trace {
			if step.Strategy == "vector" && step.Ran && step.Candidates > 0 {
				vectorRan = true
			}
		}
		t.Logf("%q: target section %s rank=%d vector=%v", tc.query, tc.section, rank, vectorRan)
		if rank < 1 || rank > 3 || !vectorRan {
			var sections []string
			for _, hit := range r.Hits {
				sections = append(sections, hit.Memory.Title)
			}
			t.Fatalf("expected section in top 3 with real vectors: %s; trace=%+v", strings.Join(sections, ", "), r.Trace)
		}
	}
}
