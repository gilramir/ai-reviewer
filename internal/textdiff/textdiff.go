// Package textdiff finds what a sequence of words gained.
//
// It answers one question, for one purpose: which of the words now on screen
// were not in the words the reviewer started the session with. Deletions are
// not reported, and cannot be -- what a change removed leaves nothing on the
// page to point at.
package textdiff

// maxEdits bounds the search. Reaching it takes a document rewritten from end
// to end, where the honest answer is that all of it is new, and that is what
// the caller gets.
const maxEdits = 2000

// Changed reports, for each item of after, whether it arrived with the change.
func Changed(before, after []string) []bool {
	changed := make([]bool, len(after))

	// The common head and tail are the bulk of a document between two turns,
	// and taking them off first is what keeps the search below cheap on the
	// one-sentence edits that a review is mostly made of.
	lo := 0
	for lo < len(before) && lo < len(after) && before[lo] == after[lo] {
		lo++
	}
	endBefore, endAfter := len(before), len(after)
	for endBefore > lo && endAfter > lo && before[endBefore-1] == after[endAfter-1] {
		endBefore--
		endAfter--
	}

	a, b := before[lo:endBefore], after[lo:endAfter]
	if len(b) == 0 {
		// Nothing was added; whatever changed, changed by going away.
		return changed
	}
	if len(a) == 0 {
		markAll(changed, lo, endAfter)
		return changed
	}

	inner, ok := myers(a, b)
	if !ok {
		markAll(changed, lo, endAfter)
		return changed
	}
	copy(changed[lo:], inner)
	return changed
}

func markAll(changed []bool, from, to int) {
	for i := from; i < to; i++ {
		changed[i] = true
	}
}

// myers runs the greedy shortest-edit-script search and reports which items of
// b the script had to insert. The false result means the two sequences are
// further apart than maxEdits allows.
//
// The search walks diagonals k = x - y, keeping for each one the furthest x it
// has reached in d edits. A snapshot of that frontier per d is what makes the
// path recoverable afterwards; it costs O(d^2) rather than the O(d*(n+m)) a
// full copy of the frontier would.
func myers(a, b []string) ([]bool, bool) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxEdits {
		return nil, false
	}

	offset := limit
	frontier := make([]int32, 2*limit+1)
	trace := make([][]int32, 0, limit+1)

	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && frontier[offset+k-1] < frontier[offset+k+1]) {
				x = int(frontier[offset+k+1])
			} else {
				x = int(frontier[offset+k-1]) + 1
			}
			y := x - k

			// Follow the diagonal as far as the two agree: matches are free,
			// and taking every one of them is what makes the script shortest.
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			frontier[offset+k] = int32(x)

			if x >= n && y >= m {
				return insertions(trace, n, m), true
			}
		}

		snapshot := make([]int32, 2*d+1)
		copy(snapshot, frontier[offset-d:offset+d+1])
		trace = append(trace, snapshot)
	}
	return nil, false
}

// insertions walks the recorded frontiers backwards from the end of both
// sequences, marking every step that took a word from b without taking one
// from a.
func insertions(trace [][]int32, n, m int) []bool {
	changed := make([]bool, m)

	x, y := n, m
	for d := len(trace); d > 0; d-- {
		previous := trace[d-1]
		k := x - y

		// The same choice the forward pass made on this diagonal, read back.
		var fromK int
		if k == -d || (k != d && endpoint(previous, d-1, k-1) < endpoint(previous, d-1, k+1)) {
			fromK = k + 1
		} else {
			fromK = k - 1
		}
		fromX := int(endpoint(previous, d-1, fromK))
		fromY := fromX - fromK

		// Back down the free diagonal, then over the one edit that started it.
		for x > fromX && y > fromY {
			x--
			y--
		}
		if y > fromY {
			changed[fromY] = true
		}
		x, y = fromX, fromY
	}
	return changed
}

// endpoint reads diagonal k out of a frontier recorded at d edits, where k runs
// from -d to d. A diagonal outside that range was never reached, and reports a
// position no real endpoint can lose to.
func endpoint(frontier []int32, d, k int) int32 {
	if k < -d || k > d {
		return -1
	}
	return frontier[k+d]
}
