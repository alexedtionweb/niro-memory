package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestAdministrativeCancellation(t *testing.T) {
	for _, action := range []store.JobAction{store.JobPause, store.JobCancel} {
		t.Run(string(action), func(t *testing.T) {
			s := memstore.New(nil)
			enqueue(t, s, "t", store.Job{ID: "job", Kind: "work"})
			started, stopped := make(chan struct{}), make(chan struct{})
			r := &jobs.Runner{
				Queue: s, Worker: "worker", Lease: 30 * time.Millisecond,
				Handlers: map[string]jobs.Handler{"work": func(ctx context.Context, _ store.Job) error {
					close(started)
					<-ctx.Done()
					close(stopped)
					return ctx.Err()
				}},
			}
			done := make(chan error, 1)
			go func() { _, err := r.RunOnce(ctx, 1); done <- err }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not start")
			}
			j, err := s.JobControl(ctx, store.JobControl{Tenant: "t", ID: "job", Action: action})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not observe cooperative cancellation")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			left, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "job"})
			if err != nil || len(left) != 1 || left[0].State != j.State {
				t.Fatalf("Finish overwrote administrative state: %+v, %v", left, err)
			}
		})
	}
}

func TestYieldDoesNotConsumeAttempts(t *testing.T) {
	s := memstore.New(nil)
	enqueue(t, s, "t", store.Job{ID: "job", Kind: "work"})
	r := &jobs.Runner{Queue: s, Worker: "worker", MaxAttempts: 1, Handlers: map[string]jobs.Handler{
		"work": func(context.Context, store.Job) error { return jobs.Yield() },
	}}
	var previousWorker string
	for i := 0; i < 4; i++ {
		var worker string
		r.Handlers["work"] = func(_ context.Context, j store.Job) error {
			worker = j.Worker
			return jobs.Yield()
		}
		if n, err := r.RunOnce(ctx, 1); err != nil || n != 1 {
			t.Fatalf("run: %d, %v", n, err)
		}
		left, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "job"})
		if err != nil || len(left) != 1 || left[0].State != store.JobPending || left[0].Attempts != 0 {
			t.Fatalf("yield consumed attempt: %+v, %v", left, err)
		}
		if worker == previousWorker {
			t.Fatal("claim reused a lease identity")
		}
		if previousWorker != "" {
			if err := s.Finish(ctx, "job", previousWorker, store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
				t.Fatalf("stale worker finish: %v", err)
			}
		}
		previousWorker = worker
	}
}

func TestResumedJobFencesEarlierExecution(t *testing.T) {
	s := memstore.New(nil)
	enqueue(t, s, "t", store.Job{ID: "job", Kind: "work"})
	started, release := make(chan store.Job, 2), make(chan struct{})
	r := &jobs.Runner{Queue: s, Worker: "worker", Concurrency: 1, Lease: time.Second, Handlers: map[string]jobs.Handler{
		"work": func(_ context.Context, j store.Job) error {
			started <- j
			<-release
			return nil
		},
	}}
	firstDone := make(chan struct{})
	go func() { r.RunOnce(ctx, 1); close(firstDone) }()
	first := <-started
	if _, err := s.JobControl(ctx, store.JobControl{Tenant: "t", ID: "job", Action: store.JobPause}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobControl(ctx, store.JobControl{Tenant: "t", ID: "job", Action: store.JobResume}); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, "new-execution", 1, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("reclaim: %+v, %v", second, err)
	}
	close(release)
	<-firstDone
	if err := s.Heartbeat(ctx, "job", first.Worker, time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("old heartbeat: %v", err)
	}
	left, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "job"})
	if err != nil || len(left) != 1 || left[0].Worker != "new-execution" || left[0].State != store.JobLeased {
		t.Fatalf("earlier execution overwrote new lease: %+v, %v", left, err)
	}
}
