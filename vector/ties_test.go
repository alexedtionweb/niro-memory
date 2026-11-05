package vector

import "testing"

func TestTopKEqualScoresDoNotDependOnInsertionOrder(t *testing.T) {
	for _, ids := range [][]string{{"a", "b", "c", "d"}, {"d", "c", "b", "a"}, {"c", "a", "d", "b"}} {
		top := NewTopK(2)
		for _, id := range ids {
			top.Push(Hit{Target: Target{Kind: TargetMemory, ID: id}, Score: .6})
		}
		hits := top.Sorted()
		if len(hits) != 2 || hits[0].Target.ID != "d" || hits[1].Target.ID != "c" {
			t.Fatalf("unstable ties: %+v", hits)
		}
	}
}
