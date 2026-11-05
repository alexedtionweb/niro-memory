package niromem

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// PurgeTenant begins an irreversible, durable wipe of the entire tenant, not
// merely the current page or project. requestID is retained for safe HTTP retries.
// Authentication and typed-name confirmation belong to the admin API.
func (e *Engine) PurgeTenant(ctx context.Context, tenant, requestID string) (*store.PurgeStatus, error) {
	p, ok := e.o.Store.(store.TenantPurger)
	if !ok {
		return nil, fmt.Errorf("%w: store does not support tenant cleanup", memory.ErrInvalid)
	}
	if e.o.Blob != nil {
		if _, ok := e.o.Blob.(blob.TenantPurger); !ok {
			return nil, fmt.Errorf("%w: blob connector does not support full tenant cleanup", memory.ErrInvalid)
		}
	}
	if e.external {
		if _, ok := e.index.(vector.TenantPurger); !ok {
			return nil, fmt.Errorf("%w: vector connector does not support full tenant cleanup", memory.ErrInvalid)
		}
	}
	return p.BeginPurge(ctx, tenant, requestID)
}

// TenantPurgeStatus returns durable progress, or memory.ErrNotFound.
func (e *Engine) TenantPurgeStatus(ctx context.Context, tenant string) (*store.PurgeStatus, error) {
	p, ok := e.o.Store.(store.TenantPurger)
	if !ok {
		return nil, fmt.Errorf("%w: store does not support tenant cleanup", memory.ErrInvalid)
	}
	return p.PurgeStatus(ctx, tenant)
}

func (e *Engine) handleTenantPurge(ctx context.Context, j store.Job) error {
	p, ok := e.o.Store.(store.TenantPurger)
	if !ok {
		return jobs.Permanent(fmt.Errorf("store does not support tenant cleanup"))
	}
	status, err := p.PurgeStatus(ctx, j.Tenant)
	if err != nil {
		return err
	}
	if status.ID != j.Key || status.State == "complete" {
		return nil
	}
	if e.o.Now().Before(status.ReadyAt) {
		return errors.New("waiting for revoked worker leases to expire")
	}
	if e.o.Blob != nil {
		b, ok := e.o.Blob.(blob.TenantPurger)
		if !ok {
			return jobs.Permanent(errors.New("blob connector does not support full tenant cleanup"))
		}
		for i := 0; i < 100; i++ {
			_, done, err := b.PurgeTenantBatch(ctx, j.Tenant, 256)
			if err != nil {
				return fmt.Errorf("purge original files: %w", err)
			}
			if done {
				break
			}
			if i == 99 {
				return jobs.Yield()
			}
		}
	}
	if e.external {
		v, ok := e.index.(vector.TenantPurger)
		if !ok {
			return jobs.Permanent(errors.New("vector connector does not support full tenant cleanup"))
		}
		if err := v.PurgeTenant(ctx, j.Tenant); err != nil {
			return fmt.Errorf("purge external vectors: %w", err)
		}
	}
	for i := 0; i < 100; i++ {
		_, done, err := p.PurgeBatch(ctx, j.Tenant, 500)
		if err != nil {
			return fmt.Errorf("purge records: %w", err)
		}
		if done {
			if err := p.FinishPurge(ctx, j.Tenant); err != nil {
				return err
			}
			e.o.Cache.Set(ctx, "settings:"+j.Tenant, []byte("{}"), e.o.SettingsTTL)
			e.heatMu.Lock()
			delete(e.heat, j.Tenant)
			e.heatMu.Unlock()
			return nil
		}
	}
	return jobs.Yield()
}
