// Package linediff counts the lines added and removed between two versions
// of a text file, as a line-based diff reports them.
//
// Lines keep their "\n" terminator, so "a" and "a\n" are different lines,
// matching the JavaScript diff library the viewer uses. The edit distance is
// computed with Myers' algorithm under a work budget: a file rewritten top
// to bottom costs O(N·D), and a 2 MB file cannot be allowed to pin a CPU on
// every save. Past the budget the counts fall back to comparing the two
// files' lines as multisets, which is exact whenever no lines moved.
package linediff

import "strings"

// Budget is the most Myers steps one comparison may take before falling
// back to the multiset estimate.
const Budget = 20_000_000

// Lines returns how many lines s has.
func Lines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// Count returns the number of lines added and removed going from before to
// after.
func Count(before, after string) (added, removed int) {
	if before == after {
		return 0, 0
	}
	a, b := intern(split(before), split(after))

	// Common prefix and suffix are free and usually most of the file.
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) == 0 || len(b) == 0 {
		return len(b), len(a)
	}

	lcs, ok := myersLCS(a, b, Budget)
	if !ok {
		lcs = multisetCommon(a, b)
	}
	return len(b) - lcs, len(a) - lcs
}

// split breaks s into lines, each keeping its terminator.
func split(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// intern maps each distinct line to a small integer so comparisons are
// cheap.
func intern(a, b []string) (ia, ib []int) {
	ids := make(map[string]int, len(a))
	conv := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	return conv(a), conv(b)
}

// myersLCS returns the length of the longest common subsequence of a and b
// using the forward greedy O(ND) algorithm, or false once more than budget
// steps have been spent.
func myersLCS(a, b []int, budget int) (int, bool) {
	n, m := len(a), len(b)
	maxD := n + m
	offset := maxD + 1
	v := make([]int, 2*maxD+3)
	steps := 0
	for d := 0; d <= maxD; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return (n + m - d) / 2, true
			}
			if steps++; steps > budget {
				return 0, false
			}
		}
	}
	return 0, false
}

// multisetCommon counts the lines a and b share, ignoring order: an upper
// bound on the LCS that equals it when no line moved.
func multisetCommon(a, b []int) int {
	counts := make(map[int]int, len(a))
	for _, id := range a {
		counts[id]++
	}
	common := 0
	for _, id := range b {
		if counts[id] > 0 {
			counts[id]--
			common++
		}
	}
	return common
}
