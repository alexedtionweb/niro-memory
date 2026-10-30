package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

var ctx = context.Background()

func enqueue(t *testing.T, s store.Store, tenant string, js ...store.Job) {
	t.Helper()
	if _, err := s.Write(ctx, &store.Batch{Tenant: tenant, Jobs: js}); err != nil {
		t.Fatal(err)
	}
}

func TestRunOnceRetryAndDead(t *testing.T) {
	s := memstore.New(nil)
	enqueue(t, s, "t", store.Job{Kind: "ok", Key: "a"}, store.Job{Kind: "flaky", Key: "b"}, store.Job{Kind: "bad", Key: "c"}, store.Job{Kind: "unknown", Key: "d"})
	var flaky atomic.Int32
	r := &jobs.Runner{Queue: s, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxAttempts: 3, Handlers: map[string]jobs.Handler{
		"ok": func(context.Context, store.Job) error { return nil },
		"flaky": func(context.Context, store.Job) error {
			if flaky.Add(1) < 2 {
				return errors.New("transient")
			}
			return nil
		},
		"bad": func(context.Context, store.Job) error { return jobs.Permanent(errors.New("poison")) },
	}}
	for i := 0; i < 5; i++ {
		if _, err := r.RunOnce(ctx, 10); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	left, _ := s.Jobs(ctx, store.JobQuery{})
	if len(left) != 2 {
		t.Fatalf("want 2 dead jobs left, got %+v", left)
	}
	for _, j := range left {
		if j.State != store.JobDead || (j.Kind != "bad" && j.Kind != "unknown") {
			t.Fatalf("%+v", j)
		}
	}
	if flaky.Load() != 2 {
		t.Fatalf("flaky ran %d times", flaky.Load())
	}
}

func TestPanicIsRetried(t *testing.T) {
	s := memstore.New(nil)
	enqueue(t, s, "t", store.Job{Kind: "p", Key: "x"})
	r := &jobs.Runner{Queue: s, BaseBackoff: time.Hour, Handlers: map[string]jobs.Handler{
		"p": func(context.Context, store.Job) error { panic("boom") },
	}}
	r.RunOnce(ctx, 1)
	js, _ := s.Jobs(ctx, store.JobQuery{})
	if len(js) != 1 || js[0].State != store.JobPending || js[0].LastError == "" {
		t.Fatalf("%+v", js)
	}
}

func TestHeartbeatKeepsLeaseAndRunStops(t *testing.T) {
	s := memstore.New(nil)
	enqueue(t, s, "t", store.Job{Kind: "slow", Key: "x"})
	var runs atomic.Int32
	slow := func(ctx context.Context, _ store.Job) error {
		runs.Add(1)
		time.Sleep(150 * time.Millisecond) // > lease: only heartbeats keep it
		return nil
	}
	a := &jobs.Runner{Queue: s, Worker: "a", Lease: 60 * time.Millisecond, Handlers: map[string]jobs.Handler{"slow": slow}}
	b := &jobs.Runner{Queue: s, Worker: "b", Lease: 60 * time.Millisecond, Handlers: map[string]jobs.Handler{"slow": slow}}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { a.Run(cctx); done <- struct{}{} }()
	time.Sleep(20 * time.Millisecond)
	go func() { b.Run(cctx); done <- struct{}{} }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	<-done
	if runs.Load() != 1 {
		t.Fatalf("job ran %d times; heartbeat failed to hold the lease", runs.Load())
	}
	if js, _ := s.Jobs(ctx, store.JobQuery{}); len(js) != 0 {
		t.Fatalf("job not finished: %+v", js)
	}
}
