package memstore

import (
	"context"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

// BeginPurge atomically fences writes and revokes work for the tenant.
func (s *Store) BeginPurge(_ context.Context, tenant, id string) (*store.PurgeStatus, error) {
	if tenant == "" || id == "" {
		return nil, fmt.Errorf("%w: tenant and purge id required", memory.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := s.purgeRequests[tenant]
	if requests == nil {
		requests = map[string]string{}
		s.purgeRequests[tenant] = requests
	}
	if prior := requests[id]; prior != "" {
		p := s.purges[tenant]
		if p.ID != prior {
			return &store.PurgeStatus{ID: prior, Tenant: tenant, State: "complete"}, nil
		}
		v := *p
		return &v, nil
	}
	if p := s.purges[tenant]; p != nil && p.State != "complete" {
		requests[id] = p.ID
		v := *p
		return &v, nil
	}
	requests[id] = id
	now := s.now()
	ready := now
	for _, j := range s.jobs {
		if j.Tenant != tenant {
			continue
		}
		if j.State == store.JobLeased && j.LeaseUntil.After(ready) {
			ready = j.LeaseUntil.Add(time.Second)
		}
		if j.State == store.JobLeased {
			j.State = store.JobCancelled
			j.Worker = ""
		}
	}
	p := &store.PurgeStatus{ID: id, Tenant: tenant, State: "purging", ReadyAt: ready}
	s.purges[tenant] = p
	job := &store.Job{ID: memory.NewID(), Tenant: tenant, Kind: store.JobTenantPurge, Key: id, State: store.JobPending, NotBefore: ready, CreatedAt: now, UpdatedAt: now}
	s.jobs[job.ID] = job
	v := *p
	return &v, nil
}

// PurgeStatus returns a copy of the last wipe's progress.
func (s *Store) PurgeStatus(_ context.Context, tenant string) (*store.PurgeStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.purges[tenant]
	if p == nil {
		return nil, memory.ErrNotFound
	}
	v := *p
	return &v, nil
}

// PurgeBatch drops the in-process reference tenant. Persistent connectors use
// bounded SQL transactions; this reference store is not for large datasets.
func (s *Store) PurgeBatch(ctx context.Context, tenant string, limit int) (*store.PurgeStatus, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, fmt.Errorf("%w: purge batch must be 1..1000", memory.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.purges[tenant]
	if p == nil {
		return nil, false, memory.ErrNotFound
	}
	if s.now().Before(p.ReadyAt) {
		return nil, false, conflict("waiting for revoked worker leases to expire")
	}
	if t := s.tenants[tenant]; t != nil {
		p.Deleted += int64(len(t.mems) + len(t.episodes) + len(t.entities) + len(t.runs) + len(t.operations))
		delete(s.tenants, tenant)
	}
	if err := s.vec.PurgeTenant(ctx, tenant); err != nil {
		return nil, false, err
	}
	for id, j := range s.jobs {
		if j.Tenant == tenant && j.Kind != store.JobTenantPurge {
			delete(s.jobs, id)
			p.Deleted++
		}
	}
	p.Phase = 13
	v := *p
	return &v, true, nil
}

// FinishPurge unlocks the empty tenant.
func (s *Store) FinishPurge(_ context.Context, tenant string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.purges[tenant]
	if p == nil {
		return memory.ErrNotFound
	}
	if p.Phase < 13 {
		return conflict("purge has unfinished tables")
	}
	p.State = "complete"
	return nil
}
