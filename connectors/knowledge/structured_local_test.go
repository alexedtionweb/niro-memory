package knowledge

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
)

func TestLocalQwenProceduralDocumentExtraction(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EXTRACTION") != "1" {
		t.Skip("opt-in local Qwen extraction on generated procedural document passages")
	}
	noThinking := false
	p, err := New(Options{Protocol: ProtocolOllama, BaseURL: "http://127.0.0.1:11434", Model: "qwen3.5:2b", Think: &noThinking,
		NumThread: 4, NumCtx: 8192, NumPredict: 1536, HTTP: &http.Client{Timeout: 4 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, text string
		required   []string
	}{
		{"exclusions", "2.2 Exclusiones del servicio La venta del equipo no se gestiona mediante esta funcionalidad. Si el usuario solicita ayuda para comprarlo, debe recibir la lista de distribuidores autorizados y continuar por otro canal. La instalacion presencial no esta incluida en este servicio. Las garantias del equipo se tramitan con el proveedor. El registro de ordenes de trabajo queda fuera de este flujo.", []string{"venta", "instalacion"}},
		{"rules", "4. Reglas generales El cliente debe completar la instalacion antes de solicitar la activacion. La cuenta se crea con los datos obtenidos durante la activacion. Cada pais solicita datos diferentes. Si la sesion permanece inactiva durante 45 minutos, el proceso debe reiniciarse desde el principio. El agente debe informar al usuario cuantos pasos faltan. Ningun paso de activacion puede omitirse.", []string{"instalacion", "45"}},
		{"thresholds", "5. Validacion del equipo El receptor debe estar enchufado y encendido. Los cables deben estar ajustados. El nivel de senal debe ser igual o mayor al 75% para continuar con la activacion. Si la senal es inferior al 75%, el usuario debe revisar la orientacion de la antena antes de continuar. El numero de tarjeta debe contener 12 digitos. La tarjeta debe insertarse con el chip hacia abajo.", []string{"75%"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			start := time.Now()
			input := domain.EnrichmentInput{Text: tc.text}
			out, err := p.Enrich(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if err := domain.ValidateEnrichment(input, out); err != nil {
				t.Fatal(err)
			}
			if len(out.Facts) < 2 || len(out.Excerpts) > 0 {
				t.Fatalf("procedural passage not extracted: %+v", out)
			}
			var assertions strings.Builder
			for _, fact := range out.Facts {
				assertions.WriteString(strings.ToLower(fact.Subject+" "+fact.Predicate+" "+fact.Object) + "\n")
				t.Logf("%s | %s | %s (negated=%t)", fact.Subject, fact.Predicate, fact.Object, fact.Negated)
			}
			for _, term := range tc.required {
				if !strings.Contains(assertions.String(), term) {
					t.Errorf("critical document detail %q missing from assertions: %+v", term, out.Facts)
				}
			}
			t.Logf("%d facts, %d entities, %d repair notes, %s", len(out.Facts), len(out.Entities), len(out.Notes), time.Since(start).Round(time.Millisecond))
		})
	}
}
func TestLocalQwenStructuredExtraction(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EXTRACTION") != "1" {
		t.Skip("opt-in local Qwen structured extraction, generated fixtures only")
	}
	noThinking := false
	p, err := New(Options{Protocol: ProtocolOllama, BaseURL: "http://127.0.0.1:11434", Model: "qwen3.5:2b", Think: &noThinking,
		NumThread: 4, NumCtx: 8192, NumPredict: 1536, HTTP: &http.Client{Timeout: 3 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Atlas uses Redis.", "Bruna trabalha na Contoso em Recife.", "El decodificador requiere una senal del 70% para activar el servicio."} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		start := time.Now()
		out, err := p.Enrich(ctx, domain.EnrichmentInput{Text: text})
		cancel()
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if len(out.Facts) == 0 || len(out.Excerpts) > 0 {
			t.Fatalf("%q: no structured claims: %+v", text, out)
		}
		if err := domain.ValidateEnrichment(domain.EnrichmentInput{Text: text}, out); err != nil {
			t.Fatal(err)
		}
		t.Logf("%q: %d facts, %d entities, %s", text, len(out.Facts), len(out.Entities), time.Since(start).Round(time.Millisecond))
	}
}
