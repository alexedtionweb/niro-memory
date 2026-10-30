package cache

import (
	"context"
	"testing"
	"time"
)

func TestLRU(t *testing.T) {
	ctx := context.Background()
	c := NewLRU(2)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Set(ctx, "a", []byte("1"), time.Minute)
	c.Set(ctx, "b", []byte("2"), time.Minute)
	c.Get(ctx, "a") // a is now most recent
	c.Set(ctx, "c", []byte("3"), time.Minute)
	if _, ok := c.Get(ctx, "b"); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if v, ok := c.Get(ctx, "a"); !ok || string(v) != "1" {
		t.Fatal("recent entry evicted")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get(ctx, "a"); ok {
		t.Fatal("expired entry returned")
	}
}
