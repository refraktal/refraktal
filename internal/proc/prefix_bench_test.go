package proc

import (
	"strconv"
	"strings"
	"testing"
)

func BenchmarkRejectedSessionRecord(b *testing.B) {
	for _, size := range []int{4096, 1 << 20, 8 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			input := strings.Repeat("x", size) + "\n" + recordedSeed
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for range b.N {
				got, err := readSessions(strings.NewReader(input))
				if err != nil || len(got) != 1 {
					b.Fatal("record following oversized line was lost")
				}
			}
		})
	}
}
