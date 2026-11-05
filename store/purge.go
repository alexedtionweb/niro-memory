package store

import (
	"context"
	"time"
)

// PurgeStatus is durable progress for an irreversible tenant wipe. A completed
// receipt is retained so a lost HTTP response cannot accidentally start a new wipe.
type PurgeStatus struct {
	ID      string    `json:"id"`
	Tenant  string    `json:"tenant"`
	State   string    `json:"state"`
	Phase   int       `json:"phase"`
	Deleted int64     `json:"deleted"`
	ReadyAt time.Time `json:"ready_at"`
}

// TenantPurger deletes tenant data in bounded transactions. Begin fences writes
// and revokes ordinary job leases atomically. PurgeBatch is restart-safe; Finish
// removes the fence only after all tables have been exhausted.
type TenantPurger interface {
	BeginPurge(context.Context, string, string) (*PurgeStatus, error)
	PurgeStatus(context.Context, string) (*PurgeStatus, error)
	PurgeBatch(context.Context, string, int) (*PurgeStatus, bool, error)
	FinishPurge(context.Context, string) error
}

// JobTenantPurge is the only job allowed to run while a tenant is fenced.
const JobTenantPurge = "tenant.purge"
