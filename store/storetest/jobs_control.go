package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func jobController(t *testing.T, s store.Store) store.JobController {
	t.Helper()
	c, ok := s.(store.JobController)
	if !ok {
		t.Skip("optional JobController not implemented")
	}
	return c
}

func testJobControls(t *testing.T, s store.Store) {
	c := jobController(t, s)
	ctx := context.Background()
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "a", Kind: "work", Key: "k"}}})
	control := func(action store.JobAction) store.Job {
		t.Helper()
		j, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "a", Action: action})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return j
	}
	checkErr := func(tenant, id string, action store.JobAction, want error) {
		t.Helper()
		_, err := c.JobControl(ctx, store.JobControl{Tenant: tenant, ID: id, Action: action})
		if !errors.Is(err, want) {
			t.Fatalf("%s %s/%s: got %v, want %v", action, tenant, id, err, want)
		}
	}
	checkErr("", "a", store.JobPause, memory.ErrInvalid)
	checkErr("t", "", store.JobPause, memory.ErrInvalid)
	checkErr("other", "a", store.JobPause, memory.ErrNotFound)
	checkErr("t", "missing", store.JobPause, memory.ErrNotFound)
	checkErr("t", "a", "unknown", memory.ErrInvalid)
	checkErr("t", "a", store.JobRetry, memory.ErrConflict)
	checkErr("t", "a", store.JobResume, memory.ErrConflict)
	if j := control(store.JobPause); j.State != store.JobPaused {
		t.Fatalf("pause: %+v", j)
	}
	if js, err := s.Claim(ctx, "w", 1, time.Minute); err != nil || len(js) != 0 {
		t.Fatalf("claimed paused job: %+v, %v", js, err)
	}
	checkErr("t", "a", store.JobPause, memory.ErrConflict)
	control(store.JobResume)
	js, err := s.Claim(ctx, "w", 1, time.Minute)
	if err != nil || len(js) != 1 {
		t.Fatalf("claim resumed: %+v, %v", js, err)
	}
	checkErr("t", "a", store.JobDelete, memory.ErrConflict)
	if err := s.Heartbeat(ctx, "a", "w", time.Minute, json.RawMessage(`{"step":1}`)); err != nil {
		t.Fatal(err)
	}
	if j := control(store.JobPause); j.Attempts != 1 || string(j.Checkpoint) != `{"step":1}` || j.Worker != "" || !j.LeaseUntil.IsZero() {
		t.Fatalf("pause running: %+v", j)
	}
	if err := s.Heartbeat(ctx, "a", "w", time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("revoked heartbeat: %v", err)
	}
	if err := s.Finish(ctx, "a", "w", store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("revoked finish: %v", err)
	}
	if j := control(store.JobResume); j.Attempts != 1 || len(j.Checkpoint) == 0 {
		t.Fatalf("resume reset progress: %+v", j)
	}
	js, err = s.Claim(ctx, "w2", 1, time.Minute)
	if err != nil || len(js) != 1 || js[0].Attempts != 2 {
		t.Fatalf("reclaim: %+v, %v", js, err)
	}
	if err := s.Finish(ctx, "a", "w2", store.Outcome{Dead: true, Error: "poison"}); err != nil {
		t.Fatal(err)
	}
	checkErr("t", "a", store.JobResume, memory.ErrConflict)
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "b", Kind: "work", Key: "k"}}})
	checkErr("t", "a", store.JobRetry, memory.ErrConflict)
	dead, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "a"})
	if err != nil || len(dead) != 1 || dead[0].State != store.JobDead || dead[0].Attempts != 2 {
		t.Fatalf("conflicting retry mutated job: %+v, %v", dead, err)
	}
	if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "b", Action: store.JobDelete}); err != nil {
		t.Fatal(err)
	}
	if j := control(store.JobRetry); j.Attempts != 0 || j.LastError != "" || len(j.Checkpoint) != 0 || j.State != store.JobPending {
		t.Fatalf("retry: %+v", j)
	}
	control(store.JobPause)
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "b", Kind: "work", Key: "k"}}})
	checkErr("t", "a", store.JobResume, memory.ErrConflict)
	control(store.JobCancel)
	checkErr("t", "a", store.JobCancel, memory.ErrConflict)
	checkErr("t", "a", store.JobRetry, memory.ErrConflict)
	if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "b", Action: store.JobDelete}); err != nil {
		t.Fatal(err)
	}
	control(store.JobRetry)
	if _, err := s.Claim(ctx, "w3", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if j := control(store.JobCancel); j.State != store.JobCancelled {
		t.Fatalf("cancel: %+v", j)
	}
	for _, out := range []store.Outcome{{Done: true}, {Dead: true}, {RetryAt: time.Now()}} {
		if err := s.Finish(ctx, "a", "w3", out); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("cancelled finish %+v: %v", out, err)
		}
	}
	if err := s.Heartbeat(ctx, "a", "w3", time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("cancelled heartbeat: %v", err)
	}
	retained, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "a", State: store.JobCancelled})
	if err != nil || len(retained) != 1 {
		t.Fatalf("cancelled job not retained: %+v, %v", retained, err)
	}
	control(store.JobDelete)
	checkErr("t", "a", store.JobDelete, memory.ErrNotFound)
	scheduled := time.Now().Add(time.Hour)
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "scheduled", Kind: "work", NotBefore: scheduled}}})
	for _, action := range []store.JobAction{store.JobPause, store.JobResume} {
		if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "scheduled", Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	if js, err := s.Claim(ctx, "w", 1, time.Minute); err != nil || len(js) != 0 {
		t.Fatalf("resume bypassed scheduled delay: %+v, %v", js, err)
	}
}

func testJobPriority(t *testing.T, s store.Store) {
	c := jobController(t, s)
	ctx := context.Background()
	for _, p := range []int{store.MinJobPriority - 1, store.MaxJobPriority + 1} {
		if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: fmt.Sprint(p), Kind: "work", Priority: p}}}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("enqueue priority %d: %v", p, err)
		}
	}
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{
		{ID: "a", Kind: "work", Priority: -100},
		{ID: "b", Kind: "work"},
		{ID: "c", Kind: "work", Priority: 100},
	}})
	write(t, s, &store.Batch{Tenant: "u", Jobs: []store.Job{{ID: "d", Kind: "work", Priority: -100}}})
	for _, p := range []int{-101, 101} {
		if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "a", Action: store.JobSetPriority, Priority: p}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("control priority %d: %v", p, err)
		}
	}
	if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "b", Action: store.JobSetPriority, Priority: 50}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", AfterID: "a", Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != "b" || page[0].Priority != 50 {
		t.Fatalf("priority persistence/cursor: %+v, %v", page, err)
	}
	hidden, err := s.Jobs(ctx, store.JobQuery{Tenant: "u", ID: "b"})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("exact ID tenant filter: %+v, %v", hidden, err)
	}
	hidden, err = s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "b", AfterID: "b"})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("exclusive cursor: %+v, %v", hidden, err)
	}
	claimed, err := s.Claim(ctx, "w", 3, time.Minute)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim: %+v, %v", claimed, err)
	}
	if claimed[0].ID != "c" || claimed[1].ID != "d" || claimed[2].ID != "b" {
		t.Fatalf("priority within tenant fairness: %+v", claimed)
	}
}

func testJobControlRace(t *testing.T, s store.Store) {
	c := jobController(t, s)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("job-%02d", i)
		write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: id, Kind: "work"}}})
		start := make(chan struct{})
		var wg sync.WaitGroup
		var claimErr, cancelErr error
		var claimed []store.Job
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			claimed, claimErr = s.Claim(ctx, "w", 1, time.Minute)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = c.JobControl(ctx, store.JobControl{Tenant: "t", ID: id, Action: store.JobCancel})
		}()
		close(start)
		wg.Wait()
		if claimErr != nil || cancelErr != nil {
			t.Fatalf("claim/cancel race: claim %v, cancel %v", claimErr, cancelErr)
		}

		js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: id})
		if err != nil || len(js) != 1 || js[0].State != store.JobCancelled {
			t.Fatalf("race lost cancellation: %+v, %v", js, err)
		}
		if len(claimed) > 0 {
			if err := s.Finish(ctx, id, "w", store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
				t.Fatalf("stale race finish: %v", err)
			}
		}
	}
	// A paused row and its new pending-key owner cannot both become pending.
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "paused", Kind: "keyed", Key: "key"}}})
	if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "paused", Action: store.JobPause}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var resumeErr, writeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, resumeErr = c.JobControl(ctx, store.JobControl{Tenant: "t", ID: "paused", Action: store.JobResume})
	}()
	go func() {
		defer wg.Done()
		_, writeErr = s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "new", Kind: "keyed", Key: "key"}}})
	}()
	wg.Wait()
	if writeErr != nil || (resumeErr != nil && !errors.Is(resumeErr, memory.ErrConflict)) {
		t.Fatalf("resume/enqueue race: resume %v, enqueue %v", resumeErr, writeErr)
	}
	pending, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: "keyed", State: store.JobPending})
	if err != nil || len(pending) != 1 {
		t.Fatalf("duplicate pending key after race: %+v, %v", pending, err)
	}
}

func testJobYield(t *testing.T, s store.Store) {
	ctx := context.Background()
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "yield", Kind: "work"}}})
	for i := 0; i < 3; i++ {
		js, err := s.Claim(ctx, "w", 1, time.Minute)
		if err != nil || len(js) != 1 || js[0].Attempts != 1 {
			t.Fatalf("claim after yield: %+v, %v", js, err)
		}
		if err := s.Finish(ctx, "yield", "w", store.Outcome{Yield: true, RetryAt: time.Now().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
		js, err = s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "yield"})
		if err != nil || len(js) != 1 || js[0].Attempts != 0 || js[0].State != store.JobPending {
			t.Fatalf("yield consumed attempt: %+v, %v", js, err)
		}
	}
}

func testJobPurgeFence(t *testing.T, s store.Store) {
	c := jobController(t, s)
	p, ok := s.(store.TenantPurger)
	if !ok {
		t.Skip("optional TenantPurger not implemented")
	}
	ctx := context.Background()
	write(t, s, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "ordinary", Kind: "work", Priority: 100}}})
	write(t, s, &store.Batch{Tenant: "u", Jobs: []store.Job{{ID: "other", Kind: "work"}}})
	if _, err := p.BeginPurge(ctx, "t", "purge"); err != nil {
		t.Fatal(err)
	}
	// Pending ordinary jobs remain intact; the claim fence blocks them.
	pending, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "ordinary"})
	if err != nil || len(pending) != 1 || pending[0].State != store.JobPending {
		t.Fatalf("purge changed pending job: %+v, %v", pending, err)
	}
	purge, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: store.JobTenantPurge})
	if err != nil || len(purge) != 1 {
		t.Fatalf("purge job: %+v, %v", purge, err)
	}
	id := purge[0].ID
	for _, action := range []string{"pause", "cancel", "delete"} {
		if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: id, Action: store.JobAction(action)}); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("purge %s: %v", action, err)
		}
	}
	if _, err := c.JobControl(ctx, store.JobControl{Tenant: "t", ID: id, Action: store.JobSetPriority, Priority: 100}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, "w", 3, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("purge-fenced claim: %+v, %v", claimed, err)
	}
	for _, j := range claimed {
		if j.Tenant == "t" && j.Kind != store.JobTenantPurge {
			t.Fatalf("claimed fenced ordinary job: %+v", j)
		}
	}
}
