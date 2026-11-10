package linediff

import (
	"strings"
	"testing"
	"time"
)

func TestLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
	} {
		if got := Lines(tc.in); got != tc.want {
			t.Errorf("Lines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		before, after  string
		added, removed int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"from empty", "", "a\nb\n", 2, 0},
		{"to empty", "a\nb\n", "", 0, 2},
		{"append", "a\n", "a\nb\n", 1, 0},
		{"edit middle", "a\nb\nc\n", "a\nx\nc\n", 1, 1},
		{"missing final newline counts as a change", "a", "a\n", 1, 1},
		{"insert and delete", "a\nb\nc\nd\n", "a\nc\nd\ne\n", 1, 1},
		{"swap", "a\nb\n", "b\na\n", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			added, removed := Count(tc.before, tc.after)
			if added != tc.added || removed != tc.removed {
				t.Errorf("Count = +%d -%d, want +%d -%d", added, removed, tc.added, tc.removed)
			}
		})
	}
}

func TestCountIsBoundedOnFullRewrites(t *testing.T) {
	t.Parallel()
	var before, after strings.Builder
	for i := range 100_000 {
		before.WriteString("old line ")
		before.WriteString(strings.Repeat("x", i%7))
		before.WriteString("\n")
		after.WriteString("new line ")
		after.WriteString(strings.Repeat("y", i%5))
		after.WriteString("\n")
	}
	start := time.Now()
	added, removed := Count(before.String(), after.String())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Count took %v on a full rewrite", elapsed)
	}
	if added != 100_000 || removed != 100_000 {
		t.Errorf("Count = +%d -%d, want +100000 -100000", added, removed)
	}
}

func FuzzCount(f *testing.F) {
	f.Add("a\nb\n", "b\nc\n")
	f.Fuzz(func(t *testing.T, before, after string) {
		added, removed := Count(before, after)
		if added < 0 || removed < 0 || added > Lines(after)+1 || removed > Lines(before)+1 {
			t.Fatalf("Count(%q, %q) = +%d -%d", before, after, added, removed)
		}
	})
}
