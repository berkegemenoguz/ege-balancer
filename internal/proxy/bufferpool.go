package proxy

import "sync"

// copyBufferSize matches the buffer the reverse proxy allocates by default.
const copyBufferSize = 32 * 1024

// bufferPool lends out the buffers used to copy response bodies from the
// backends to the clients. Without it the reverse proxy allocates a fresh
// 32 KB buffer for every request, which under load is by far the largest
// source of garbage in the process.
type bufferPool struct {
	pool sync.Pool
}

// newBufferPool returns a pool of copy buffers.
func newBufferPool() *bufferPool {
	return &bufferPool{pool: sync.Pool{
		New: func() any {
			buffer := make([]byte, copyBufferSize)
			return &buffer
		},
	}}
}

// Get returns a buffer to copy a response body with.
func (b *bufferPool) Get() []byte {
	return *b.pool.Get().(*[]byte)
}

// Put returns a buffer once the body has been copied.
func (b *bufferPool) Put(buffer []byte) {
	b.pool.Put(&buffer)
}
