package ghostline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	benchmarkOutputBytes = 4 << 20
	benchmarkFanout      = 8
)

type benchmarkStore interface {
	Start(context.Context, SessionOptions) (*Session, error)
	List(context.Context) ([]*Session, error)
}

type discardWriter struct{}

func (discardWriter) Write(data []byte) (int, error) { return len(data), nil }

func BenchmarkOutputAppend64KiB(b *testing.B) {
	log, err := createOutputLog(b.TempDir(), "append")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(log.discard)
	data := bytes.Repeat([]byte{'x'}, 64<<10)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := range b.N {
		if err := log.append(data); err != nil {
			b.Fatal(err)
		}
		if (index+1)%1024 == 0 && index+1 < b.N {
			b.StopTimer()
			boundary, err := log.rotate()
			if err == nil {
				err = log.prune(boundary)
			}
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	}
}

func BenchmarkOutputRotateAndPrune(b *testing.B) {
	log, err := createOutputLog(b.TempDir(), "rotate")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(log.discard)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		boundary, err := log.rotate()
		if err == nil {
			err = log.prune(boundary)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetainedOutputRead(b *testing.B) {
	for _, transport := range []string{"local", "daemon"} {
		b.Run(transport, func(b *testing.B) {
			store := newBenchmarkStore(b, transport)
			payload := bytes.Repeat([]byte{'x'}, benchmarkOutputBytes)
			session := benchmarkRetainedOutput(b, store, "read", payload)
			buffer := make([]byte, 64<<10)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				reader, err := session.Output(context.Background(), Cursor{})
				if err != nil {
					b.Fatal(err)
				}
				read, copyErr := io.CopyBuffer(discardWriter{}, reader, buffer)
				closeErr := reader.Close()
				if copyErr != nil || closeErr != nil || read != int64(len(payload)) {
					b.Fatalf("read retained output = (%d, %v, %v), want %d", read, copyErr, closeErr, len(payload))
				}
			}
		})
	}
}

func BenchmarkCheckpoint(b *testing.B) {
	for _, transport := range []string{"local", "daemon"} {
		b.Run(transport, func(b *testing.B) {
			store := newBenchmarkStore(b, transport)
			for _, historyBytes := range []int{4 << 10, 256 << 10, 1 << 20} {
				b.Run(fmt.Sprintf("history_%dKiB", historyBytes>>10), func(b *testing.B) {
					line := []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n")
					payload := bytes.Repeat(line, historyBytes/len(line)+1)[:historyBytes]
					name := fmt.Sprintf("checkpoint-%d-%d", historyBytes, time.Now().UnixNano())
					session := benchmarkRetainedOutput(b, store, name, payload)
					var replayBytes int64
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						checkpoint, err := session.Checkpoint(context.Background())
						if err != nil {
							b.Fatal(err)
						}
						replayBytes += int64(len(checkpoint.Replay))
					}
					b.ReportMetric(float64(replayBytes)/float64(b.N), "replay-B/op")
				})
			}
		})
	}
}

func BenchmarkOutputFanout(b *testing.B) {
	for _, transport := range []string{"local", "daemon"} {
		b.Run(transport, func(b *testing.B) {
			store := newBenchmarkStore(b, transport)
			payload := bytes.Repeat([]byte{'x'}, 1<<20)
			session := benchmarkRetainedOutput(b, store, "fanout", payload)
			b.SetBytes(int64(len(payload) * benchmarkFanout))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				errs := make(chan error, benchmarkFanout)
				var wait sync.WaitGroup
				for range benchmarkFanout {
					wait.Add(1)
					go func() {
						defer wait.Done()
						reader, err := session.Output(context.Background(), Cursor{})
						if err != nil {
							errs <- err
							return
						}
						read, copyErr := io.CopyBuffer(discardWriter{}, reader, make([]byte, 64<<10))
						closeErr := reader.Close()
						if copyErr != nil || closeErr != nil || read != int64(len(payload)) {
							errs <- fmt.Errorf("read = (%d, %v, %v)", read, copyErr, closeErr)
						}
					}()
				}
				wait.Wait()
				close(errs)
				for err := range errs {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkControlPlane(b *testing.B) {
	for _, transport := range []string{"local", "daemon"} {
		b.Run(transport, func(b *testing.B) {
			store := newBenchmarkStore(b, transport)
			session, err := store.Start(context.Background(), SessionOptions{
				Name: "control", Process: ProcessSpec{Path: "sleep", Args: []string{"600"}},
			})
			if err != nil {
				b.Fatal(err)
			}
			operations := []struct {
				name string
				run  func() error
			}{
				{name: "Status", run: func() error { _, err := session.Status(context.Background()); return err }},
				{name: "Size", run: func() error { _, err := session.Size(context.Background()); return err }},
				{name: "OutputCursor", run: func() error { _, err := session.OutputCursor(context.Background()); return err }},
				{name: "SignalSIGCONT", run: func() error { return session.Signal(context.Background(), syscall.SIGCONT) }},
			}
			for _, operation := range operations {
				b.Run(operation.name, func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						if err := operation.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func BenchmarkList100Sessions(b *testing.B) {
	for _, transport := range []string{"local", "daemon"} {
		b.Run(transport, func(b *testing.B) {
			store := newBenchmarkStore(b, transport)
			for index := range 100 {
				session, err := store.Start(context.Background(), SessionOptions{
					Name: fmt.Sprintf("session-%03d", index), Process: ProcessSpec{Path: "true"},
				})
				if err != nil {
					b.Fatal(err)
				}
				if err := session.Wait(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := store.List(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func newBenchmarkStore(b *testing.B, transport string) benchmarkStore {
	b.Helper()
	switch transport {
	case "local":
		hub, err := New(Options{OutputDir: b.TempDir(), VTScrollbackMaxBytes: 4 << 20})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = hub.Close() })
		return hub
	case "daemon":
		socketDirectory, err := os.MkdirTemp("/tmp", "ghostline-benchmark-")
		if err != nil {
			b.Fatal(err)
		}
		server, err := NewServer(Options{OutputDir: b.TempDir(), VTScrollbackMaxBytes: 4 << 20})
		if err != nil {
			b.Fatal(err)
		}
		socket := filepath.Join(socketDirectory, "ghostline.sock")
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = server.Serve(context.Background(), socket)
		}()
		client := NewClient(socket)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := client.Check(context.Background()); err == nil {
				break
			}
			if time.Now().After(deadline) {
				b.Fatal("benchmark server did not become ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		b.Cleanup(func() {
			_ = server.Shutdown(context.Background())
			<-done
			_ = os.RemoveAll(socketDirectory)
		})
		return client
	default:
		b.Fatalf("unknown benchmark transport %q", transport)
		return nil
	}
}

func benchmarkRetainedOutput(b *testing.B, store benchmarkStore, name string, payload []byte) *Session {
	b.Helper()
	path := filepath.Join(b.TempDir(), name+".data")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	session, err := store.Start(context.Background(), SessionOptions{
		Name: name, Process: ProcessSpec{Path: "cat", Args: []string{path}},
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := session.Wait(ctx); err != nil {
		b.Fatal(err)
	}
	return session
}

func BenchmarkParseCursor(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if _, err := ParseCursor("v1:18446744073709551615:9223372036854775807"); err != nil {
			b.Fatal(err)
		}
	}
}
