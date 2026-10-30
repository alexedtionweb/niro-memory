// Package otel reports engine measurements to OpenTelemetry. Observer
// implements niromem.Observer with two instruments:
//
//	niromem.op.duration  histogram (seconds), attribute op
//	niromem.op.errors    counter,              attribute op
//
// Wire it with niromem.Options{Observer: obs}; exporters are configured by
// the application through the MeterProvider, as usual in OpenTelemetry.
package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Observer records operation latency and errors.
type Observer struct {
	duration metric.Float64Histogram
	errors   metric.Int64Counter
}

// New creates the instruments on meter.
func New(meter metric.Meter) (*Observer, error) {
	d, err := meter.Float64Histogram("niromem.op.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of niromem operations"),
		metric.WithExplicitBucketBoundaries(0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, err
	}
	e, err := meter.Int64Counter("niromem.op.errors", metric.WithDescription("Failed niromem operations"))
	if err != nil {
		return nil, err
	}
	return &Observer{duration: d, errors: e}, nil
}

// Observe records one operation.
func (o *Observer) Observe(name string, took time.Duration, err error) {
	attrs := metric.WithAttributes(attribute.String("op", name))
	o.duration.Record(context.Background(), took.Seconds(), attrs)
	if err != nil {
		o.errors.Add(context.Background(), 1, attrs)
	}
}
