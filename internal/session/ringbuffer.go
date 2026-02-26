package session

import "sync"

const ringBufferSize = 50 * 1024 // 50KB

// RingBuffer is a fixed-size circular buffer for storing terminal output.
type RingBuffer struct {
	buf  []byte
	size int
	pos  int
	full bool
	mu   sync.Mutex
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size), size: size}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		copy(r.buf, p[n-r.size:])
		r.pos = 0
		r.full = true
		return n, nil
	}
	if r.pos+n <= r.size {
		copy(r.buf[r.pos:], p)
	} else {
		first := r.size - r.pos
		copy(r.buf[r.pos:], p[:first])
		copy(r.buf, p[first:])
	}
	r.pos = (r.pos + n) % r.size
	if !r.full && r.pos < n {
		r.full = true
	}
	return n, nil
}

func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]byte(nil), r.buf[:r.pos]...)
	}
	out := make([]byte, r.size)
	copy(out, r.buf[r.pos:])
	copy(out[r.size-r.pos:], r.buf[:r.pos])
	return out
}
