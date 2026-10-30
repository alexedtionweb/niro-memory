package textutil

import (
	"reflect"
	"testing"
)

func TestTerms(t *testing.T) {
	got := Terms("Hello, hello WORLD! São-Paulo 42 x"+string(make([]byte, 0)), 0)
	want := []string{"hello", "world", "são", "paulo", "42", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	if len(Terms("a b c d", 2)) != 2 {
		t.Fatal("max")
	}
}

func TestSentences(t *testing.T) {
	s := "Hi there. I live in Lisbon! v1.2 is out?\nNew line 。末尾"
	var got []string
	for _, sp := range Sentences(s) {
		got = append(got, s[sp.Start:sp.End])
	}
	want := []string{"Hi there.", "I live in Lisbon!", "v1.2 is out?", "New line 。", "末尾"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestNGramsJaccard(t *testing.T) {
	g := NGrams([]string{"new", "york", "city"}, 2)
	if g[0] != "new york" || len(g) != 5 {
		t.Fatalf("%v", g)
	}
	if Jaccard([]string{"a", "b"}, []string{"b", "c"}) != 1.0/3 {
		t.Fatal("jaccard")
	}
}

func FuzzSentences(f *testing.F) {
	f.Add("a. b! c?\nd")
	f.Fuzz(func(t *testing.T, s string) {
		prev := 0
		for _, sp := range Sentences(s) {
			if sp.Start < prev || sp.End > len(s) || sp.Start >= sp.End {
				t.Fatalf("bad span %+v in %q", sp, s)
			}
			prev = sp.End
		}
	})
}
