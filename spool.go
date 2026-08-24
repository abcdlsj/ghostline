package ghostline

import (
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SpoolWatcher reads an append-only spool from a persisted byte offset,
// draining to EOF whenever the file grows. The byte slice passed to onBytes is
// valid only for the duration of the callback; callers must copy it to retain
// it.
//
// The watcher also detects in-place truncation (spool compaction). After a
// truncate the file size drops below the watcher offset; the watcher re-bases
// to offset zero and calls onRotate so the consumer can invalidate old offsets
// instead of silently skipping bytes.
type SpoolWatcher struct {
	path       string
	file       *os.File
	offset     atomic.Int64
	maxBytes   atomic.Int64
	buffer     []byte
	onBytes    func([]byte)
	onRotate   func()
	onOverflow func()
	notifier   spoolNotifier
	heartbeat  time.Duration
	stat       func() (os.FileInfo, error)

	ping      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	readMu    sync.Mutex
	paused    bool
}

// NewSpoolWatcher returns a watcher positioned at offset in the file at path.
// The callbacks may be nil. Start begins watching.
func NewSpoolWatcher(path string, offset int64, onBytes func([]byte), onRotate func(), onOverflow func()) (*SpoolWatcher, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		closeQuietly(file)
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > info.Size() {
		closeQuietly(file)
		return nil, &spoolOffsetError{Path: path, Offset: offset, Size: info.Size()}
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		closeQuietly(file)
		return nil, err
	}
	notifier := newSpoolNotifier(path, file)
	w := &SpoolWatcher{
		path:       path,
		file:       file,
		onBytes:    onBytes,
		onRotate:   onRotate,
		onOverflow: onOverflow,
		notifier:   notifier,
		heartbeat:  time.Second,
		stat:       file.Stat,
		ping:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	w.maxBytes.Store(64 * 1024 * 1024)
	w.offset.Store(offset)
	return w, nil
}

type spoolOffsetError struct {
	Path   string
	Offset int64
	Size   int64
}

func (e *spoolOffsetError) Error() string {
	return "spool offset " + strconv.FormatInt(e.Offset, 10) + " is beyond file size " + strconv.FormatInt(e.Size, 10) + ": " + e.Path
}

// Offset returns the next byte position the watcher will deliver.
func (w *SpoolWatcher) Offset() int64 {
	return w.offset.Load()
}

// SetMaxBytes configures the spool size cap before Start. When the watcher
// passes the cap it calls onOverflow so the consumer can compact the spool.
func (w *SpoolWatcher) SetMaxBytes(maxBytes int64) {
	if maxBytes > 0 {
		w.maxBytes.Store(maxBytes)
	}
}

// Ping asks the watcher to check for output without waiting for its next poll.
func (w *SpoolWatcher) Ping() {
	select {
	case w.ping <- struct{}{}:
	default:
	}
}

// Start begins watching. Repeated calls are safe and have no effect.
func (w *SpoolWatcher) Start() {
	w.startOnce.Do(func() { go w.loop() })
}

// Close stops the watcher and releases its file descriptor. It is safe to call
// multiple times.
func (w *SpoolWatcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		w.notifier.Close()
		closeQuietly(w.file)
	})
}

// Pause blocks until any in-flight drain finishes, then prevents new drains.
// Use it while preparing a checkpoint replay so live reads cannot interleave.
func (w *SpoolWatcher) Pause() {
	w.readMu.Lock()
	w.paused = true
	w.readMu.Unlock()
}

// Resume re-enables draining after Pause and asks the watcher to check
// immediately.
func (w *SpoolWatcher) Resume() {
	w.readMu.Lock()
	w.paused = false
	w.readMu.Unlock()
	w.Ping()
}

// SkipTo re-bases the watcher to a byte position covered by a snapshot. It
// must be called while paused and the offset must be within the current file;
// any unread bytes below the target were already rendered by the snapshot and
// must not be delivered again.
func (w *SpoolWatcher) SkipTo(offset int64) error {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	if !w.paused {
		return errors.New("skip spool watcher while running")
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
	heartbeat := time.NewTicker(w.heartbeat)
	defer heartbeat.Stop()
	w.drain()
	for {
		select {
		case <-w.done:
			return
		case <-w.ping:
			w.drain()
		case <-w.notifier.Events():
			w.drain()
		case <-heartbeat.C:
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
	info, err := w.stat()
	if err != nil {
		return
	}
	offset := w.offset.Load()
	if info.Size() < offset {
		// In-place compaction rotated the spool. Re-base and notify the
		// consumer so it can invalidate offsets from the previous contents.
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
			if maxBytes := w.maxBytes.Load(); maxBytes > 0 && offset > maxBytes && w.onOverflow != nil {
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
