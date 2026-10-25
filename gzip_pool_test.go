package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOfficialGzipWarmAllocationBudget(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"synthetic small request"}]}`)
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := encodeModelRequest(raw, requestProfileOfficial, &profileCN); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 10 {
		t.Fatalf("warm gzip encoding allocates %.0f times per request; budget is 10", allocs)
	}
}

func TestModelGzipPoolBoundsIdleWritersWithoutWaiting(t *testing.T) {
	p := modelGzipWriterPool{idle: make(chan *gzip.Writer, maxIdleModelGzipWriters)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var writers []*gzip.Writer
		for i := 0; i < maxIdleModelGzipWriters+3; i++ {
			writers = append(writers, p.acquire(io.Discard))
		}
		for _, w := range writers {
			p.release(w)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("empty/full compression cache introduced a wait queue")
	}
	if len(p.idle) != maxIdleModelGzipWriters {
		t.Fatal("idle writer retention does not match its bound")
	}
}

func TestOfficialGzipConcurrentBodiesStayIndependent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				raw := []byte(fmt.Sprintf(`{"worker":%d,"iteration":%d,"body":%q}`, i, j, strings.Repeat("synthetic", 128)))
				got, err := encodeModelRequest(raw, requestProfileOfficial, &profileCN)
				if err != nil {
					t.Error(err)
					return
				}
				r, err := gzip.NewReader(bytes.NewReader(got))
				if err != nil {
					t.Error(err)
					return
				}
				decoded, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil || !bytes.Equal(decoded, raw) {
					t.Errorf("concurrent request body changed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestOfficialGzipReusePreservesOutputAndEarlierBodies(t *testing.T) {
	var previous, saved []byte
	for _, raw := range [][]byte{
		[]byte(`{"message":"first <&> 中文"}`),
		[]byte(strings.Repeat("synthetic line\n", 1<<16)),
		{},
		[]byte(`{"id":9007199254740993,"message":"last"}`),
	} {
		var expected bytes.Buffer
		w := gzip.NewWriter(&expected)
		_, _ = w.Write(raw)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := encodeModelRequest(raw, requestProfileOfficial, &profileCN)
		if err != nil || !bytes.Equal(got, expected.Bytes()) {
			t.Fatalf("gzip wire bytes changed: %v", err)
		}
		if !bytes.Equal(previous, saved) {
			t.Fatal("later compression overwrote an earlier request body")
		}
		r, err := gzip.NewReader(bytes.NewReader(got))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(decoded, raw) {
			t.Fatalf("request body changed after round-trip: %v", err)
		}
		previous, saved = got, bytes.Clone(got)
	}
	for _, tc := range []struct {
		profile string
		site    *upstreamProfile
	}{{requestProfileLegacy, &profileCN}, {requestProfileOfficial, profileForEdition("intl")}} {
		raw := []byte(`{"unchanged":true}`)
		got, err := encodeModelRequest(raw, tc.profile, tc.site)
		if err != nil || !bytes.Equal(got, raw) || &got[0] != &raw[0] {
			t.Fatal("uncompressed profiles must still use the original body")
		}
	}
}
