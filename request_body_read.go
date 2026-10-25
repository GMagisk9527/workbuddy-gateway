package main

import (
	"bytes"
	"io"
	"net/http"
)

// Only the decompression middleware constructs this body, after checking both
// the decoded size and the shared input budget.
type decodedRequestBody struct {
	data   []byte
	reader *bytes.Reader
}

func newDecodedRequestBody(data []byte) *decodedRequestBody {
	return &decodedRequestBody{data: data, reader: bytes.NewReader(data)}
}

func (b *decodedRequestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (*decodedRequestBody) Close() error                 { return nil }

func readModelRequestBody(r *http.Request) (data []byte, readErr error) {
	started := performanceStarted(r)
	defer func() { recordPerformance(r, phaseRead, started, readErr != nil) }()
	if body, ok := r.Body.(*decodedRequestBody); ok {
		remaining := body.data[len(body.data)-body.reader.Len():]
		_, err := body.reader.Seek(0, io.SeekEnd)
		return remaining, err
	}
	// A known, bounded length needs one allocation. The extra byte lets an
	// exact-length body report EOF without growing its buffer just to probe.
	// Every read still goes through Body, including MaxBytesReader and leases.
	if r.ContentLength > 0 && r.ContentLength <= requestDecodedLimit {
		buf := make([]byte, int(r.ContentLength)+1)
		used := 0
		for used < len(buf) {
			n, err := r.Body.Read(buf[used:])
			used += n
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return buf[:used], err
			}
		}
		// Preserve ReadAll behavior if a custom body exceeds its declared size.
		tail, err := io.ReadAll(r.Body)
		return append(buf, tail...), err
	}
	return io.ReadAll(r.Body)
}
