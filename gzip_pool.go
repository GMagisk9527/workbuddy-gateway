package main

import (
	"compress/gzip"
	"io"
)

// Bound idle retention independently of active requests. Cache misses allocate
// as before; a full cache discards its extra writer without adding a wait queue.
const maxIdleModelGzipWriters = 2

type modelGzipWriterPool struct {
	idle chan *gzip.Writer
}

var modelGzipWriters = modelGzipWriterPool{idle: make(chan *gzip.Writer, maxIdleModelGzipWriters)}

func (p *modelGzipWriterPool) acquire(dst io.Writer) *gzip.Writer {
	select {
	case w := <-p.idle:
		w.Reset(dst)
		return w
	default:
		return gzip.NewWriter(dst)
	}
}

func (p *modelGzipWriterPool) release(w *gzip.Writer) {
	// Reset before publishing the writer: never retain a request/output buffer
	// or reset it after another request has acquired it. Reset also clears errors.
	w.Reset(io.Discard)
	select {
	case p.idle <- w:
	default:
	}
}
