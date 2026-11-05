// Package jobs runs background work from a store.Queue: claiming with
// leases, heartbeats while a job runs, exponential retry, dead-lettering,
// bounded concurrency and graceful shutdown.
//
// Workers are stateless. Any number of them, in any process, can share a
// queue: leases guarantee that a job (and every job with the same key) runs
// on one worker at a time, and a crashed worker's jobs are reclaimed when
// its lease expires. Handlers must be idempotent; the engine's handlers are,
// because they commit their own progress (run cursors) atomically.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

// Handler runs one job.
type Handler func(ctx context.Context, j store.Job) error

// permanent marks errors that must not be retried.
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent wraps err so the job is dead-lettered without retries.
func Permanent(err error) error { return permanent{err} }

var errYield = errors.New("job yielded")

// Yield requeues a checkpointed job immediately without consuming an attempt.
// Handlers should durably commit progress before yielding.
func Yield() error { return errYield }

// Runner executes jobs. Configure it with fields; zero values are sensible.
type Runner struct {
	Queue    store.Queue
	Handlers map[string]Handler
	// Worker identifies this runner (default: host-pid-random). Each claim
	// appends a unique lease identity, fencing earlier executions on retry.
	Worker      string
	Concurrency int           // parallel jobs (default 4)
	Lease       time.Duration // lease length (default 30s); renewed at 1/3
	MaxAttempts int           // attempts before dead-lettering (default 8)
	BaseBackoff time.Duration // first retry delay (default 1s), doubled per attempt
	MaxBackoff  time.Duration // cap (default 5m)
	Poll        time.Duration // idle poll interval (default 250ms, backs off to 2s)
	Timeout     time.Duration // per-job timeout (default 5m)
	Logger      *slog.Logger
	Now         func() time.Time
	// Observe, if set, is called after each job with its duration and error.
	Observe func(kind string, took time.Duration, err error)
}

func (r *Runner) defaults() {
	if r.Worker == "" {
		host, _ := os.Hostname()
		r.Worker = fmt.Sprintf("%s-%d-%04x", host, os.Getpid(), rand.IntN(1<<16))
	}
	if r.Concurrency <= 0 {
		r.Concurrency = 4
	}
	if r.Lease <= 0 {
		r.Lease = 30 * time.Second
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 8
	}
	if r.BaseBackoff <= 0 {
		r.BaseBackoff = time.Second
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 5 * time.Minute
	}
	if r.Poll <= 0 {
		r.Poll = 250 * time.Millisecond
	}
	if r.Timeout <= 0 {
		r.Timeout = 5 * time.Minute
	}
	if r.Logger == nil {
		r.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.Now == nil {
		r.Now = time.Now
	}
}

// RunOnce claims up to max jobs, runs them, and returns how many ran. It is
// the entry point for serverless workers (one invocation = one batch).
func (r *Runner) RunOnce(ctx context.Context, max int) (int, error) {
	r.defaults()
	if max <= 0 {
		max = r.Concurrency
	}
	js, err := r.Queue.Claim(ctx, r.Worker+"/"+memory.NewID(), max, r.Lease)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	for _, j := range js {
		sem <- struct{}{}
		wg.Add(1)
		go func(j store.Job) {
			defer func() { <-sem; wg.Done() }()
			r.run(ctx, j)
		}(j)
	}
	wg.Wait()
	return len(js), nil
}

// Run processes jobs until ctx is cancelled, then waits for running jobs
// to finish (their leases are released through Finish).
func (r *Runner) Run(ctx context.Context) error {
	r.defaults()
	idle := r.Poll
	for {
		n, err := r.RunOnce(ctx, r.Concurrency)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			r.Logger.Error("claim failed", "err", err)
		}
		if n > 0 {
			idle = r.Poll
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(idle):
		}
		if idle < 2*time.Second {
			idle *= 2
		}
	}
}

func (r *Runner) run(parent context.Context, j store.Job) {
	// Jobs keep running during shutdown; they get their own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.Timeout)
	defer cancel()
	stop := r.heartbeat(ctx, j, cancel)
	start := r.Now()
	err := r.call(ctx, j)
	leaseErr := stop()
	took := r.Now().Sub(start)
	if r.Observe != nil {
		r.Observe(j.Kind, took, err)
	}
	if leaseErr != nil {
		r.logLeaseFailure(parent, j, "heartbeat", leaseErr)
		return
	}
	var out store.Outcome
	var p permanent
	switch {
	case err == nil:
		out.Done = true
	case errors.Is(err, errYield):
		out.Yield, out.RetryAt = true, r.Now()
	case errors.As(err, &p) || j.Attempts >= r.MaxAttempts:
		out.Dead, out.Error = true, err.Error()
	default:
		out.Error, out.RetryAt = err.Error(), r.Now().Add(r.backoff(j.Attempts))
	}
	// Finish even if the caller is shutting down, so the lease is released.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer fcancel()
	if ferr := r.Queue.Finish(fctx, j.ID, j.Worker, out); ferr != nil {
		r.logLeaseFailure(parent, j, "finish", ferr)
		return
	}
	if out.Dead {
		r.Logger.Error("job dead", "kind", j.Kind, "id", j.ID, "attempts", j.Attempts, "err", err)
	} else if out.Error != "" {
		r.Logger.Warn("job failed, will retry", "kind", j.Kind, "id", j.ID, "attempts", j.Attempts, "err", err)
	}
}

func (r *Runner) logLeaseFailure(parent context.Context, j store.Job, stage string, err error) {
	if errors.Is(err, memory.ErrConflict) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer cancel()
		current, lookupErr := r.Queue.Jobs(ctx, store.JobQuery{Tenant: j.Tenant, ID: j.ID, Limit: 1})
		if lookupErr != nil {
			r.Logger.Warn("job ownership lookup failed", "kind", j.Kind, "id", j.ID, "stage", stage, "err", err, "lookup_err", lookupErr)
			return
		}
		if len(current) == 0 || current[0].State == store.JobPaused || current[0].State == store.JobCancelled {
			r.Logger.Info("job ownership revoked; stale result not applied", "kind", j.Kind, "id", j.ID, "stage", stage)
			return
		}
	}
	r.Logger.Warn("job lease operation failed; stale result not applied", "kind", j.Kind, "id", j.ID, "stage", stage, "err", err)
}

func (r *Runner) call(ctx context.Context, j store.Job) (err error) {
	h := r.Handlers[j.Kind]
	if h == nil {
		return Permanent(fmt.Errorf("no handler for job kind %q", j.Kind))
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, j)
}

// heartbeat renews the lease every Lease/3 and cancels the job when the
// lease is lost or administratively revoked. Handlers must honor the context;
// cancellation cannot undo committed effects or forcibly stop ignored contexts.
func (r *Runner) heartbeat(ctx context.Context, j store.Job, cancel context.CancelFunc) func() error {
	done := make(chan struct{})
	var leaseErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.Queue.Heartbeat(ctx, j.ID, j.Worker, r.Lease, nil); err != nil {
					leaseErr = err
					cancel()
					return
				}
			}
		}
	}()
	return func() error { close(done); wg.Wait(); return leaseErr }
}

func (r *Runner) backoff(attempt int) time.Duration {
	d := r.BaseBackoff << min(attempt-1, 20)
	if d <= 0 || d > r.MaxBackoff {
		d = r.MaxBackoff
	}
	// ±20% jitter spreads retries of jobs that failed together.
	return d - d/5 + time.Duration(rand.Int64N(int64(d/5)*2+1))
}
