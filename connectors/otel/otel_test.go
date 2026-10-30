package otel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/connectors/otel"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var _ niromem.Observer = (*otel.Observer)(nil)

func TestObserver(t *testing.T) {
	reader := sdk.NewManualReader()
	obs, err := otel.New(sdk.NewMeterProvider(sdk.WithReader(reader)).Meter("niromem"))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := niromem.Open(niromem.Options{Store: memstore.New(nil), Observer: obs})
	e.Remember(context.Background(), niromem.Fact{Scope: memory.Scope{Tenant: "t"}, Text: "x"})
	obs.Observe("search", time.Millisecond, errors.New("boom"))
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	if !names["niromem.op.duration"] || !names["niromem.op.errors"] {
		t.Fatalf("instruments missing: %v", names)
	}
}
