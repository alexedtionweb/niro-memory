package store

import (
	"context"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

// JobAction identifies an administrative lifecycle operation.
type JobAction string

const (
	JobPause       JobAction = "pause"
	JobResume      JobAction = "resume"
	JobRetry       JobAction = "retry"
	JobCancel      JobAction = "cancel"
	JobDelete      JobAction = "delete"
	JobSetPriority JobAction = "priority"
	MinJobPriority           = -100
	MaxJobPriority           = 100
)

// JobControl targets exactly one job within a required tenant.
type JobControl struct {
	Tenant   string    `json:"tenant"`
	ID       string    `json:"id"`
	Action   JobAction `json:"action"`
	Priority int       `json:"priority,omitempty"`
}

// Validate checks the request independently of the current job state.
func (c JobControl) Validate() error {
	if c.Tenant == "" || c.ID == "" {
		return fmt.Errorf("%w: job tenant and id are required", memory.ErrInvalid)
	}
	switch c.Action {
	case JobPause, JobResume, JobRetry, JobCancel, JobDelete:
		return nil
	case JobSetPriority:
		return ValidateJobPriority(c.Priority)
	default:
		return fmt.Errorf("%w: unknown job action %q", memory.ErrInvalid, c.Action)
	}
}

// JobController is optional; existing Queue implementations remain compatible.
// Pause accepts pending/leased jobs; resume accepts paused jobs and preserves
// attempts, checkpoints and schedule. Retry accepts dead/cancelled jobs, resetting attempts,
// checkpoints and errors. Resume/retry fail with ErrConflict if a pending job
// already owns the same key. Cancel accepts pending/paused/leased jobs and retains
// them for inspection. Delete accepts only inactive (non-leased) jobs. Priority
// accepts non-leased jobs. Irreversible tenant.purge jobs cannot be paused,
// cancelled or deleted.
//
// Pause/cancel immediately revoke the lease. A runner observes revocation on its
// next heartbeat and cancels the handler's context. Cancellation is cooperative:
// it cannot roll back effects or stop a handler that ignores its context.
// Finish from the revoked worker cannot overwrite the administrative state.
type JobController interface {
	JobControl(context.Context, JobControl) (Job, error)
}

// ValidateJobPriority checks the inclusive persisted priority range.
func ValidateJobPriority(priority int) error {
	if priority < MinJobPriority || priority > MaxJobPriority {
		return fmt.Errorf("%w: job priority must be between %d and %d", memory.ErrInvalid, MinJobPriority, MaxJobPriority)
	}
	return nil
}

// ApplyJobControl validates a request and computes its result without persistence.
// Stores must apply it atomically, enforce pending-key uniqueness, and delete the
// row for JobDelete. The returned job is a snapshot, including after deletion.
func ApplyJobControl(j Job, c JobControl, now time.Time) (Job, error) {
	if err := c.Validate(); err != nil {
		return Job{}, err
	}
	if j.ID != c.ID || j.Tenant != c.Tenant {
		return Job{}, memory.ErrNotFound
	}
	if j.Kind == "tenant.purge" && (c.Action == JobPause || c.Action == JobCancel || c.Action == JobDelete) {
		return Job{}, fmt.Errorf("%w: tenant purge cannot be interrupted", memory.ErrConflict)
	}
	invalidState := func() (Job, error) {
		return Job{}, fmt.Errorf("%w: cannot %s job in state %s", memory.ErrConflict, c.Action, j.State)
	}
	switch c.Action {
	case JobPause:
		if j.State != JobPending && j.State != JobLeased {
			return invalidState()
		}
		j.State = JobPaused
	case JobResume:
		if j.State != JobPaused {
			return invalidState()
		}
		j.State = JobPending
	case JobRetry:
		if j.State != JobDead && j.State != JobCancelled {
			return invalidState()
		}
		j.State, j.NotBefore, j.Attempts, j.LastError, j.Checkpoint = JobPending, now, 0, "", nil
	case JobCancel:
		if j.State != JobPending && j.State != JobLeased && j.State != JobPaused {
			return invalidState()
		}
		j.State = JobCancelled
	case JobDelete:
		if j.State == JobLeased {
			return invalidState()
		}
	case JobSetPriority:
		if j.State == JobLeased {
			return invalidState()
		}
		if err := ValidateJobPriority(c.Priority); err != nil {
			return Job{}, err
		}
		j.Priority = c.Priority
	default:
		return Job{}, fmt.Errorf("%w: unknown job action %q", memory.ErrInvalid, c.Action)
	}
	if c.Action != JobSetPriority {
		j.Worker, j.LeaseUntil = "", time.Time{}
	}
	j.UpdatedAt = now
	return j, nil
}
