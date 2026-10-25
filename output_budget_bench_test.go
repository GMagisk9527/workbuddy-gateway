package main

import (
	"strings"
	"testing"
)

var outputJoinSink string

// Isolates the former aggregate concatenation and its replacement; excludes
// JSON, network and budget accounting, and is not an end-to-end throughput test.
func BenchmarkAggregateTextJoin(b *testing.B) {
	chunk := strings.Repeat("x", 4096)
	for _, builder := range []bool{false, true} {
		b.Run(map[bool]string{false: "previous_concat", true: "builder"}[builder], func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(128 * int64(len(chunk)))
			for range b.N {
				if builder {
					var value strings.Builder
					for range 128 {
						value.WriteString(chunk)
					}
					outputJoinSink = value.String()
				} else {
					value := ""
					for range 128 {
						value += chunk
					}
					outputJoinSink = value
				}
			}
		})
	}
}
