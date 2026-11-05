package multimodal

import (
	"testing"
	"time"
)

func TestCPUMediaDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, 10 * time.Minute} {
		p, err := New(Options{Timeout: timeout})
		if err != nil {
			t.Fatal(err)
		}
		if p.timeout != 10*time.Minute {
			t.Fatalf("shortened CPU deadline: %v", p.timeout)
		}
	}
	if _, err := New(Options{Timeout: 10*time.Minute + time.Millisecond}); err == nil {
		t.Fatal("accepted timeout beyond documented bound")
	}
}
