package ghostline

import (
	"bufio"
	"bytes"
	"testing"
)

func FuzzReadLine(f *testing.F) {
	f.Add([]byte("hello\n"), 1024)
	f.Add([]byte(""), 1024)
	f.Add(bytes.Repeat([]byte("a"), 2048), 1024)
	f.Fuzz(func(t *testing.T, data []byte, limit int) {
		if limit <= 0 {
			limit = 1
		}
		line, err := readLine(bufio.NewReader(bytes.NewReader(data)), limit)
		if err == nil && len(line) > limit {
			t.Fatalf("line length %d exceeds limit %d", len(line), limit)
		}
	})
}
