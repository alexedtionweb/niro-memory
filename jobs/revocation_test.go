package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type finishCounter struct {
	store.Queue
	finishes     atomic.Int32
	heartbeatErr error
	finishErr    error
}

func (q *finishCounter) Finish(ctx context.Context, id, worker string, out store.Outcome) error {
	q.finishes.Add(1)
	if q.finishErr != nil {
		return q.finishErr
	}
	return q.Queue.Finish(ctx, id, worker, out)
}

func (q *finishCounter) Heartbeat(ctx context.Context, id, worker string, lease time.Duration, checkpoint json.RawMessage) error {
	if q.heartbeatErr != nil {
		return q.heartbeatErr
	}
	return q.Queue.Heartbeat(ctx, id, worker, lease, checkpoint)
}

func TestRealLeaseFailuresRemainVisibleWithoutFalseOutcomeLogs(t *testing.T) {
	for _, stage := range []string{"heartbeat", "finish"} {
		t.Run(stage, func(t *testing.T) {
			s := memstore.New(nil)
			enqueue(t, s, "t", store.Job{Kind: "work"})
			queue := &finishCounter{Queue: s}
			if stage == "heartbeat" {
				queue.heartbeatErr = errors.New("database unavailable")
			} else {
				queue.finishErr = errors.New("database unavailable")
			}
			var logs bytes.Buffer
			r := &jobs.Runner{Queue: queue, Lease: 30 * time.Millisecond, MaxAttempts: 1, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
				Handlers: map[string]jobs.Handler{"work": func(ctx context.Context, _ store.Job) error {
					if stage == "heartbeat" {
						<-ctx.Done()
					}
					return jobs.Permanent(errors.New("handler failure"))
				}},
			}
			if _, err := r.RunOnce(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database unavailable") || strings.Contains(logs.String(), "job dead") {
				t.Fatalf("lost infrastructure failure or claimed unpersisted outcome: %s", &logs)
			}
			rows, err := s.Jobs(context.Background(), store.JobQuery{Tenant: "t"})
			if err != nil || len(rows) != 1 || rows[0].State != store.JobLeased {
				t.Fatalf("unpersisted outcome reported: %+v %v", rows, err)
			}
		})
	}
}

func TestRevokedAttemptDoesNotFinishOrReportFalseDeadFailure(t *testing.T) {
	for _, waitHeartbeat := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish-race", true: "heartbeat-revocation"}[waitHeartbeat], func(t *testing.T) {
			s := memstore.New(nil)
			enqueue(t, s, "t", store.Job{Kind: "cancel-me"})
			queue := &finishCounter{Queue: s}
			var logs bytes.Buffer
			r := &jobs.Runner{Queue: queue, Lease: 30 * time.Millisecond, MaxAttempts: 1, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
				Handlers: map[string]jobs.Handler{"cancel-me": func(ctx context.Context, j store.Job) error {
					if _, err := s.JobControl(ctx, store.JobControl{Tenant: j.Tenant, ID: j.ID, Action: store.JobCancel}); err != nil {
						return err
					}
					if waitHeartbeat {
						<-ctx.Done()
						return ctx.Err()
					}
					return jobs.Permanent(errors.New("late failure after cancellation"))
				}},
			}

			if _, err := r.RunOnce(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if waitHeartbeat && queue.finishes.Load() != 0 {
				t.Fatal("revoked heartbeat attempt still called Finish")
			}
			if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "job dead") {
				t.Fatalf("expected administrative cancellation logged as failure: %s", &logs)
			}
			rows, err := s.Jobs(context.Background(), store.JobQuery{Tenant: "t"})
			if err != nil || len(rows) != 1 || rows[0].State != store.JobCancelled {
				t.Fatalf("revoked state overwritten: %+v %v", rows, err)
			}
		})
	}
}
