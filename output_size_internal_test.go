package ghostline

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type sizedChunk struct {
	data string
	size Size
}

// readSizedUntilEOF collects ReadSized results, folding a size report into
// the chunk that follows it so tests can assert attribution directly.
func readSizedUntilEOF(t *testing.T, reader *OutputReader) (chunks []sizedChunk, reports []Size) {
	t.Helper()
	buffer := make([]byte, 64)
	for {
		n, size, err := reader.ReadSized(buffer)
		if n == 0 && err == nil {
			reports = append(reports, size)
			continue
		}
		if n > 0 {
			if count := len(chunks); count > 0 && chunks[count-1].size == size {
				chunks[count-1].data += string(buffer[:n])
			} else {
				chunks = append(chunks, sizedChunk{data: string(buffer[:n]), size: size})
			}
		}
		if errors.Is(err, io.EOF) {
			return chunks, reports
		}
		if err != nil {
			t.Fatalf("ReadSized: %v", err)
		}
	}
}

func TestReadSizedNeverSpansAResize(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "sized")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	small, large := Size{Columns: 80, Rows: 24}, Size{Columns: 120, Rows: 40}
	log.markSize(small)
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	mustAppend(t, log, "narrow")
	log.markSize(large)
	mustAppend(t, log, "wide")
	log.markSize(small)
	mustAppend(t, log, "back")
	log.close(nil)

	chunks, reports := readSizedUntilEOF(t, reader)
	want := []sizedChunk{{"narrow", small}, {"wide", large}, {"back", small}}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %+v, want %+v", chunks, want)
	}
	for index := range want {
		if chunks[index] != want[index] {
			t.Fatalf("chunk %d = %+v, want %+v", index, chunks[index], want[index])
		}
	}
	if wantReports := []Size{small, large, small}; !equalSizes(reports, wantReports) {
		t.Fatalf("reports = %v, want %v", reports, wantReports)
	}
}

func TestReadSizedReportsAResizeWithNoFollowingOutput(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "silent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	log.markSize(Size{Columns: 80, Rows: 24})
	mustAppend(t, log, "prompt")
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	buffer := make([]byte, 64)
	if _, _, err := reader.ReadSized(buffer); err != nil {
		t.Fatal(err)
	}
	if n, _, err := reader.ReadSized(buffer); err != nil || n == 0 {
		t.Fatalf("ReadSized = %d, %v; want the prompt", n, err)
	}

	// The reader is now waiting at the end of the log. A resize the child
	// never answers must still wake it.
	type result struct {
		n    int
		size Size
		err  error
	}
	done := make(chan result, 1)
	go func() {
		n, size, err := reader.ReadSized(buffer)
		done <- result{n, size, err}
	}()
	time.Sleep(20 * time.Millisecond)
	resized := Size{Columns: 100, Rows: 30}
	log.markSize(resized)
	select {
	case got := <-done:
		if got.n != 0 || got.err != nil || got.size != resized {
			t.Fatalf("ReadSized = %d, %v, %v; want a size report for %v", got.n, got.size, got.err, resized)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadSized did not wake for a resize")
	}
}

func TestMarkSizeReplacesASizeNoOutputUsed(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "replace")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	small := Size{Columns: 80, Rows: 24}
	log.markSize(small)
	mustAppend(t, log, "a")
	log.markSize(Size{Columns: 90, Rows: 24})
	log.markSize(small)
	mustAppend(t, log, "b")
	log.close(nil)
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	chunks, reports := readSizedUntilEOF(t, reader)
	if len(chunks) != 1 || chunks[0] != (sizedChunk{"ab", small}) {
		t.Fatalf("chunks = %+v, want one %v chunk", chunks, small)
	}
	if !equalSizes(reports, []Size{small}) {
		t.Fatalf("reports = %v, want only %v", reports, small)
	}
}

func TestReadSizedCarriesSizeAcrossRotationAndPrune(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "rotate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	size := Size{Columns: 80, Rows: 24}
	log.markSize(size)
	mustAppend(t, log, "old")
	boundary, err := log.rotate()
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, log, "new")
	if err := log.prune(boundary); err != nil {
		t.Fatal(err)
	}
	log.close(nil)
	reader, err := log.reader(context.Background(), boundary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	chunks, _ := readSizedUntilEOF(t, reader)
	if len(chunks) != 1 || chunks[0] != (sizedChunk{"new", size}) {
		t.Fatalf("chunks = %+v, want new at %v", chunks, size)
	}
}

func TestReadSizedReportsUnknownSizeForUnmarkedOutput(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "unknown")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	mustAppend(t, log, "adopted")
	size := Size{Columns: 80, Rows: 24}
	log.markSize(size)
	mustAppend(t, log, "live")
	log.close(nil)
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	chunks, _ := readSizedUntilEOF(t, reader)
	want := []sizedChunk{{"adopted", Size{}}, {"live", size}}
	if len(chunks) != 2 || chunks[0] != want[0] || chunks[1] != want[1] {
		t.Fatalf("chunks = %+v, want %+v", chunks, want)
	}
}

func TestPlainReadIgnoresSizeMarks(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	log.markSize(Size{Columns: 80, Rows: 24})
	mustAppend(t, log, "one")
	log.markSize(Size{Columns: 81, Rows: 24})
	mustAppend(t, log, "two")
	log.close(nil)
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "onetwo" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
}

func mustAppend(t *testing.T, log *outputLog, data string) {
	t.Helper()
	if err := log.append([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

func equalSizes(got, want []Size) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
