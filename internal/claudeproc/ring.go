package claudeproc

// ring is a fixed-size buffer keeping only the most recent bytes written to it.
// The CLI's stderr is only ever read to explain a failure, so unbounded
// buffering would be a leak in exchange for output nobody reads.
type ring struct {
	buf    []byte
	next   int
	filled bool
}

func newRing(size int) *ring {
	return &ring{buf: make([]byte, size)}
}

func (r *ring) Write(p []byte) (int, error) {
	n := len(p)
	if n >= len(r.buf) {
		copy(r.buf, p[n-len(r.buf):])
		r.next = 0
		r.filled = true
		return n, nil
	}
	written := copy(r.buf[r.next:], p)
	if written < n {
		copy(r.buf, p[written:])
		r.filled = true
	}
	r.next = (r.next + n) % len(r.buf)
	if r.next == 0 {
		r.filled = true
	}
	return n, nil
}

// String returns the retained bytes in write order.
func (r *ring) String() string {
	if !r.filled {
		return string(r.buf[:r.next])
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return string(out)
}
