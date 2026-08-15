package ghostline

import (
	"fmt"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// SpoolRecoverer reads a contiguous byte range from a session's append-only
// spool. PTY implements it so Host can recover from the spool even after the
// in-memory ring evicted the client's anchor, avoiding a full screen reset
// and replay.
type SpoolRecoverer interface {
	Recover(context.Context, string, int64, int64) ([]byte, error)
}

// SpoolWatcher reads an append-only spool from a persisted byte offset,
// draining to EOF whenever the file grows. It is the Go counterpart of the
// Swift OutputSpoolWatcher: capture-pane is no longer used for live output.
//
// The watcher also detects in-place truncation (spool compaction). After a
// truncate the file size drops below the watcher offset; the watcher re-bases
// to offset zero and calls onRotate so Host can bump the epoch and reanchor
// every client instead of silently skipping bytes.
type SpoolWatcher struct {
	path       string
	file       *os.File
	offset     atomic.Int64
	maxBytes   int64
	interval   time.Duration
	buffer     []byte
	onBytes    func([]byte)
	onRotate   func()
	onOverflow func()

	ping      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	readMu    sync.Mutex
	paused    bool
}

func NewSpoolWatcher(path string, offset int64, onBytes func([]byte), onRotate func(), onOverflow func()) (*SpoolWatcher, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > info.Size() {
		_ = file.Close()
		return nil, &spoolOffsetError{Path: path, Offset: offset, Size: info.Size()}
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	w := &SpoolWatcher{
		path:       path,
		file:       file,
		maxBytes:   64 * 1024 * 1024,
		interval:   10 * time.Millisecond,
		onBytes:    onBytes,
		onRotate:   onRotate,
		onOverflow: onOverflow,
		ping:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	w.offset.Store(offset)
	return w, nil
}

type spoolOffsetError struct {
	Path   string
	Offset int64
	Size   int64
}

func (e *spoolOffsetError) Error() string {
	return "spool offset " + itoa(e.Offset) + " is beyond file size " + itoa(e.Size) + ": " + e.Path
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}

func (w *SpoolWatcher) Offset() int64 {
	return w.offset.Load()
}

// SetMaxBytes configures the spool size cap before Start. When the watcher
// passes the cap it calls onOverflow; Host archives, truncates, bumps the
// epoch, and reanchors clients.
func (w *SpoolWatcher) SetMaxBytes(maxBytes int64) {
	if maxBytes > 0 {
		w.maxBytes = maxBytes
	}
}

// Ping nudges the watcher after input, matching the old outputWake behavior
// without making it the delivery mechanism.
func (w *SpoolWatcher) Ping() {
	select {
	case w.ping <- struct{}{}:
	default:
	}
}

func (w *SpoolWatcher) Start() {
	w.startOnce.Do(func() { go w.loop() })
}

func (w *SpoolWatcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		_ = w.file.Close()
	})
}

// Pause blocks until any in-flight drain finishes, then prevents new drains.
// Host uses it while preparing a reanchor so the snapshot replay can never
// race with live reads.
func (w *SpoolWatcher) Pause() {
	w.readMu.Lock()
	w.paused = true
	w.readMu.Unlock()
}

func (w *SpoolWatcher) Resume() {
	w.readMu.Lock()
	w.paused = false
	w.readMu.Unlock()
	w.Ping()
}

// SkipTo re-bases the watcher to a byte position covered by a snapshot. It
// must be called while paused; any unread bytes below the target were already
// rendered by the snapshot and must not be delivered again.
func (w *SpoolWatcher) SkipTo(offset int64) error {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	if !w.paused {
		return fmt.Errorf("skip spool watcher while running")
	}
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if offset < 0 || offset > info.Size() {
		return &spoolOffsetError{Path: w.path, Offset: offset, Size: info.Size()}
	}
	if _, err := w.file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	w.offset.Store(offset)
	return nil
}

func (w *SpoolWatcher) loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.drain()
	for {
		select {
		case <-w.done:
			return
		case <-w.ping:
			w.drain()
		case <-ticker.C:
			w.drain()
		}
	}
}

func (w *SpoolWatcher) drain() {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	if w.paused {
		return
	}
	info, err := w.file.Stat()
	if err != nil {
		return
	}
	offset := w.offset.Load()
	if info.Size() < offset {
		// In-place compaction rotated the spool. Re-base and tell Host to
		// bump the epoch; bytes before the truncate are intentionally
		// replaced by a tmux snapshot reanchor.
		if _, err := w.file.Seek(0, io.SeekStart); err != nil {
			return
		}
		offset = 0
		w.offset.Store(0)
		if w.onRotate != nil {
			w.onRotate()
		}
	}
	if info.Size() == offset {
		return
	}
	if w.buffer == nil {
		w.buffer = make([]byte, 64*1024)
	}
	for {
		read, readErr := w.file.Read(w.buffer)
		if read > 0 {
			offset += int64(read)
			w.offset.Store(offset)
			if w.onBytes != nil {
				w.onBytes(w.buffer[:read])
			}
			if w.maxBytes > 0 && offset > w.maxBytes && w.onOverflow != nil {
				w.onOverflow()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return
			}
			return
		}
		if read == 0 {
			return
		}
	}
}
