package ghostline

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type chunkWriter struct {
	buf bytes.Buffer
	max int
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n := w.max
	if n > len(p) {
		n = len(p)
	}
	return w.buf.Write(p[:n])
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteFullRetriesShortWrites(t *testing.T) {
	var writer chunkWriter
	writer.max = 3
	data := []byte("hello world")
	if err := writeFull(&writer, data); err != nil {
		t.Fatalf("writeFull: %v", err)
	}
	if !bytes.Equal(writer.buf.Bytes(), data) {
		t.Fatalf("writeFull = %q, want %q", writer.buf.Bytes(), data)
	}
}

func TestWriteFullZeroProgress(t *testing.T) {
	if err := writeFull(zeroWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeFull zero progress = %v, want io.ErrShortWrite", err)
	}
}

func TestWriteFullPropagatesError(t *testing.T) {
	if err := writeFull(failWriter{}, []byte("x")); err == nil || err.Error() != "boom" {
		t.Fatalf("writeFull error = %v, want boom", err)
	}
}
