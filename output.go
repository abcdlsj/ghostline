package ghostline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Cursor identifies a position in a session's output log. Its representation
// is intentionally opaque; cursors may be compared, stored as text, and
// passed back to Output, but their fields are not independently meaningful.
// The zero Cursor asks Output to start at the earliest retained byte.
type Cursor struct {
	generation uint64
	offset     uint64
}

// ParseCursor parses the stable text representation produced by Cursor.String.
func ParseCursor(value string) (Cursor, error) {
	if value == "" {
		return Cursor{}, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return Cursor{}, fmt.Errorf("%w: %q", ErrInvalidCursor, value)
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || generation == 0 {
		return Cursor{}, fmt.Errorf("%w: %q", ErrInvalidCursor, value)
	}
	offset, err := strconv.ParseUint(parts[2], 10, 63)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %q", ErrInvalidCursor, value)
	}
	return Cursor{generation: generation, offset: offset}, nil
}

// String returns the stable text form of c. The zero Cursor is encoded as an
// empty string.
func (c Cursor) String() string {
	if c == (Cursor{}) {
		return ""
	}
	return "v1:" + strconv.FormatUint(c.generation, 10) + ":" + strconv.FormatUint(c.offset, 10)
}

// MarshalText implements encoding.TextMarshaler.
func (c Cursor) MarshalText() ([]byte, error) {
	if c.generation == 0 && c.offset != 0 {
		return nil, ErrInvalidCursor
	}
	return []byte(c.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *Cursor) UnmarshalText(text []byte) error {
	if c == nil {
		return errors.New("ghostline: unmarshal cursor into nil receiver")
	}
	parsed, err := ParseCursor(string(text))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// Distance reports how many bytes separate c from to.
//
// It reports false when the distance is not defined: either cursor is zero,
// they belong to different output generations, or to precedes c. Callers must
// treat false as "unknown" rather than as zero. A reader that cannot bound its
// next read has to fall back to reading a single byte at a time, because
// consuming past a known target cursor duplicates output the live stream is
// still going to deliver.
func (c Cursor) Distance(to Cursor) (uint64, bool) {
	if c.generation == 0 || to.generation == 0 || c.generation != to.generation {
		return 0, false
	}
	if to.offset < c.offset {
		return 0, false
	}
	return to.offset - c.offset, true
}

// OutputReader streams raw PTY output from a cursor. Read applies natural
// backpressure: it returns at most len(p) bytes and does not buffer the rest in
// memory. Close unblocks a pending Read. Cursor returns the next unread byte.
type OutputReader struct {
	source outputSource
}

func newOutputReader(source outputSource) *OutputReader {
	return &OutputReader{source: source}
}

// Read implements io.Reader.
func (r *OutputReader) Read(p []byte) (int, error) { return r.source.Read(p) }

// Close implements io.Closer. It is safe to call more than once.
func (r *OutputReader) Close() error { return r.source.Close() }

// Cursor returns the next raw output position Read will deliver.
func (r *OutputReader) Cursor() Cursor { return r.source.Cursor() }

// ReadSized reads like Read and also reports the terminal grid size the
// returned bytes were produced for. A chunk never spans a resize: when the
// size changes, ReadSized first returns zero bytes with the new size, then
// bytes produced for it. That zero-length result is not an error and is
// delivered even if the child writes nothing after the resize.
//
// A zero Size means the size is unknown, which happens for output recorded
// before the serving process adopted the session or when the server predates
// size reporting. Do not mix Read and ReadSized on one reader.
func (r *OutputReader) ReadSized(p []byte) (int, Size, error) { return r.source.ReadSized(p) }

type outputSource interface {
	io.ReadCloser
	Cursor() Cursor
	ReadSized([]byte) (int, Size, error)
}

type outputSegment struct {
	path string
	size uint64
}

// outputLog owns one active segment and zero or more immutable completed
// segments. generation only changes while mu is held, before the new active
// segment becomes visible to readers.
type outputLog struct {
	mu         sync.Mutex
	directory  string
	generation uint64
	segments   map[uint64]outputSegment
	active     *os.File
	changed    chan struct{}
	closed     bool
	err        error
	// sizes records where the terminal grid changed, in cursor order. Every
	// byte at or after a mark's cursor was produced for that mark's size, up
	// to the next mark. The marks are held in memory only: output recorded
	// before this process adopted the log has no known size.
	sizes []sizeMark
}

// sizeMark is one grid change at an exact output position.
type sizeMark struct {
	cursor Cursor
	size   Size
}

func createOutputLog(root, name string) (*outputLog, error) {
	directory, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		return nil, fmt.Errorf("create output log: %w", err)
	}
	log, err := openOutputLog(directory, 1, true)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	return log, nil
}

func adoptOutputLog(directory string, generation uint64) (*outputLog, error) {
	if generation == 0 {
		return nil, ErrInvalidCursor
	}
	return openOutputLog(directory, generation, false)
}

func openOutputLog(directory string, generation uint64, create bool) (*outputLog, error) {
	path := outputSegmentPath(directory, generation)
	flags := os.O_APPEND | os.O_WRONLY
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	active, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open output segment: %w", err)
	}
	info, err := active.Stat()
	if err != nil {
		closeQuietly(active)
		return nil, fmt.Errorf("stat output segment: %w", err)
	}
	segments := make(map[uint64]outputSegment)
	entries, err := os.ReadDir(directory)
	if err != nil {
		closeQuietly(active)
		return nil, fmt.Errorf("read output log: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".out") {
			continue
		}
		number := strings.TrimSuffix(entry.Name(), ".out")
		current, parseErr := strconv.ParseUint(number, 10, 64)
		if parseErr != nil || current == 0 || current > generation {
			continue
		}
		segmentInfo, infoErr := entry.Info()
		if infoErr != nil {
			closeQuietly(active)
			return nil, fmt.Errorf("stat output segment %d: %w", current, infoErr)
		}
		segments[current] = outputSegment{
			path: filepath.Join(directory, entry.Name()), size: uint64(segmentInfo.Size()),
		}
	}
	if _, ok := segments[generation]; !ok {
		closeQuietly(active)
		return nil, fmt.Errorf("active output generation %d is missing", generation)
	}
	segments[generation] = outputSegment{path: path, size: uint64(info.Size())}
	return &outputLog{
		directory:  directory,
		generation: generation,
		segments:   segments,
		active:     active,
		changed:    make(chan struct{}),
	}, nil
}

func (l *outputLog) metadata() (string, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.directory, l.generation
}

func (l *outputLog) terminalError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func outputSegmentPath(directory string, generation uint64) string {
	return filepath.Join(directory, fmt.Sprintf("%020d.out", generation))
}

func (l *outputLog) append(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		if l.err != nil {
			return l.err
		}
		return ErrSessionClosed
	}
	if err := writeFull(l.active, data); err != nil {
		l.failLocked(fmt.Errorf("append output: %w", err))
		return l.err
	}
	segment := l.segments[l.generation]
	segment.size += uint64(len(data))
	l.segments[l.generation] = segment
	l.signalLocked()
	return nil
}

func (l *outputLog) cursor() (Cursor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return Cursor{}, l.err
	}
	segment := l.segments[l.generation]
	return Cursor{generation: l.generation, offset: segment.size}, nil
}

func (l *outputLog) reader(ctx context.Context, from Cursor) (*OutputReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.err != nil {
		err := l.err
		l.mu.Unlock()
		return nil, err
	}
	from, err := l.resolveCursorLocked(from)
	segment := l.segments[from.generation]
	complete := from.generation < l.generation
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	source := newLocalOutputSource(ctx, l, from)
	if err := source.pin(segment.path, segment.size, complete); err != nil {
		return nil, err
	}
	return newOutputReader(source), nil
}

func (l *outputLog) resolveCursorLocked(cursor Cursor) (Cursor, error) {
	if cursor == (Cursor{}) {
		generation := l.generation
		for candidate := range l.segments {
			if candidate < generation {
				generation = candidate
			}
		}
		return Cursor{generation: generation}, nil
	}
	if cursor.generation == 0 || cursor.offset > math.MaxInt64 {
		return Cursor{}, ErrInvalidCursor
	}
	segment, ok := l.segments[cursor.generation]
	if !ok {
		if cursor.generation < l.generation {
			return Cursor{}, ErrCursorExpired
		}
		return Cursor{}, ErrInvalidCursor
	}
	if cursor.offset > segment.size {
		return Cursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

func (l *outputLog) rotate() (Cursor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		if l.err != nil {
			return Cursor{}, l.err
		}
		return Cursor{}, ErrSessionClosed
	}
	next := l.generation + 1
	if next == 0 {
		return Cursor{}, errors.New("ghostline: output generation exhausted")
	}
	path := outputSegmentPath(l.directory, next)
	active, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Cursor{}, fmt.Errorf("create output segment: %w", err)
	}
	previous := l.active
	// Readers cannot observe this intermediate state: generation changes
	// first, then the new active segment and descriptor are published under mu.
	l.generation = next
	l.segments[next] = outputSegment{path: path}
	l.active = active
	// Start every generation with its size so a reader that begins there
	// never depends on a mark in a generation prune may remove.
	if count := len(l.sizes); count > 0 {
		l.sizes = append(l.sizes, sizeMark{cursor: Cursor{generation: next}, size: l.sizes[count-1].size})
	}
	l.signalLocked()
	closeQuietly(previous)
	return Cursor{generation: next}, nil
}

func (l *outputLog) prune(before Cursor) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.closed {
		return ErrSessionClosed
	}
	if before.generation == 0 || before.offset != 0 || before.generation > l.generation {
		return ErrInvalidCursor
	}
	for generation, segment := range l.segments {
		if generation >= before.generation || generation == l.generation {
			continue
		}
		if err := os.Remove(segment.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune output generation %d: %w", generation, err)
		}
		// New readers must treat this cursor as expired, but retaining a
		// tombstone for every removed generation would make the in-memory index
		// grow forever under a rotate/prune policy. Existing readers pin the file
		// they are currently draining and therefore keep their own valid path and
		// size after this entry is removed.
		delete(l.segments, generation)
	}
	retained := l.sizes[:0]
	for _, mark := range l.sizes {
		if _, ok := l.segments[mark.cursor.generation]; ok {
			retained = append(retained, mark)
		}
	}
	l.sizes = retained
	l.signalLocked()
	return nil
}

// markSize records that output appended from now on is produced for size.
// The caller holds the session output lock, so no byte can be appended
// between the grid change and this mark.
func (l *outputLog) markSize(size Size) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	cursor := Cursor{generation: l.generation, offset: l.segments[l.generation].size}
	if count := len(l.sizes); count > 0 {
		last := &l.sizes[count-1]
		if last.size == size {
			return
		}
		if last.cursor == cursor {
			// No byte was produced for the previous size; it never applied.
			last.size = size
			l.collapseSizesLocked()
			l.signalLocked()
			return
		}
	}
	l.sizes = append(l.sizes, sizeMark{cursor: cursor, size: size})
	l.signalLocked()
}

// collapseSizesLocked drops a trailing mark that repeats its predecessor's
// size, which a replaced zero-length mark can leave behind.
func (l *outputLog) collapseSizesLocked() {
	count := len(l.sizes)
	if count >= 2 && l.sizes[count-1].size == l.sizes[count-2].size &&
		l.sizes[count-1].cursor.generation == l.sizes[count-2].cursor.generation {
		l.sizes = l.sizes[:count-1]
	}
}

// sizeSpanLocked reports the size in effect at cursor and how many bytes
// from cursor that size covers before the next mark. A zero Size means the
// size is unknown; a false bounded means no later mark exists in cursor's
// generation yet.
func (l *outputLog) sizeSpanLocked(cursor Cursor) (size Size, span uint64, bounded bool) {
	for _, mark := range l.sizes {
		if mark.cursor.generation != cursor.generation {
			if mark.cursor.generation > cursor.generation {
				break
			}
			continue
		}
		if mark.cursor.offset <= cursor.offset {
			size = mark.size
			continue
		}
		return size, mark.cursor.offset - cursor.offset, true
	}
	return size, 0, false
}

func (l *outputLog) close(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.err = err
	closeQuietly(l.active)
	l.active = nil
	l.signalLocked()
}

func (l *outputLog) discard() {
	l.close(nil)
	_ = os.RemoveAll(l.directory)
}

func (l *outputLog) failLocked(err error) {
	if l.err == nil {
		l.err = err
	}
	l.closed = true
	closeQuietly(l.active)
	l.active = nil
	l.signalLocked()
}

func (l *outputLog) signalLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

type localOutputSource struct {
	ctx    context.Context
	log    *outputLog
	mu     sync.Mutex
	cursor Cursor
	// reported is the size last returned by ReadSized.
	reported  Size
	file      *os.File
	fileGen   uint64
	fileLimit uint64
	complete  bool
	done      chan struct{}
	closeOnce sync.Once
}

func newLocalOutputSource(ctx context.Context, log *outputLog, cursor Cursor) *localOutputSource {
	return &localOutputSource{ctx: ctx, log: log, cursor: cursor, done: make(chan struct{})}
}

func (r *localOutputSource) Cursor() Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

func (r *localOutputSource) Read(buffer []byte) (int, error) {
	n, _, err := r.read(buffer, false)
	return n, err
}

func (r *localOutputSource) ReadSized(buffer []byte) (int, Size, error) {
	return r.read(buffer, true)
}

// read serves Read and ReadSized. A sized read never returns bytes that span
// a size mark, and it returns a zero-length chunk to report a size change
// that no byte has followed yet, so the caller learns of a resize even while
// the child is silent.
func (r *localOutputSource) read(buffer []byte, sized bool) (int, Size, error) {
	if len(buffer) == 0 {
		return 0, Size{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		select {
		case <-r.done:
			return 0, Size{}, io.ErrClosedPipe
		case <-r.ctx.Done():
			return 0, Size{}, r.ctx.Err()
		default:
		}

		r.log.mu.Lock()
		segment, exists := r.log.segments[r.cursor.generation]
		if exists {
			r.fileLimit = segment.size
			r.complete = r.cursor.generation < r.log.generation
		} else if r.file != nil && r.fileGen == r.cursor.generation {
			// A reader pins its current file. prune may remove that generation
			// from the shared index after the reader was opened, but the reader
			// may still drain its pinned file and then advance to retained output.
			r.complete = r.cursor.generation < r.log.generation
		}
		if !exists && !(r.file != nil && r.fileGen == r.cursor.generation && r.complete) {
			r.log.mu.Unlock()
			return 0, Size{}, ErrCursorExpired
		}
		var size Size
		end := r.fileLimit
		if sized {
			var span uint64
			var bounded bool
			size, span, bounded = r.log.sizeSpanLocked(r.cursor)
			if size != (Size{}) && size != r.reported {
				r.reported = size
				r.log.mu.Unlock()
				return 0, size, nil
			}
			if bounded && r.cursor.offset+span < end {
				// Stop at the next mark; the following call reports it before
				// any byte produced for the new size.
				end = r.cursor.offset + span
			}
		}
		if r.cursor.offset < end {
			path := segment.path
			if !exists {
				path = r.file.Name()
			}
			limit := end - r.cursor.offset
			r.log.mu.Unlock()
			if err := r.openFile(path); err != nil {
				return 0, Size{}, err
			}
			readBuffer := buffer
			if uint64(len(readBuffer)) > limit {
				readBuffer = readBuffer[:limit]
			}
			n, err := r.file.ReadAt(readBuffer, int64(r.cursor.offset))
			r.cursor.offset += uint64(n)
			if n > 0 && (err == nil || errors.Is(err, io.EOF)) {
				return n, size, nil
			}
			return n, size, err
		}
		if r.complete && r.cursor.offset >= r.fileLimit {
			next := r.cursor.generation + 1
			r.log.mu.Unlock()
			r.closeFile()
			r.cursor = Cursor{generation: next}
			continue
		}
		closed, terminalErr, changed := r.log.closed, r.log.err, r.log.changed
		r.log.mu.Unlock()
		if closed {
			if terminalErr != nil {
				return 0, Size{}, terminalErr
			}
			return 0, Size{}, io.EOF
		}
		select {
		case <-r.done:
			return 0, Size{}, io.ErrClosedPipe
		case <-r.ctx.Done():
			return 0, Size{}, r.ctx.Err()
		case <-changed:
		}
	}
}

func (r *localOutputSource) openFile(path string) error {
	if r.file != nil && r.fileGen == r.cursor.generation {
		return nil
	}
	r.closeFile()
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open output generation %d: %w", r.cursor.generation, err)
	}
	r.file = file
	r.fileGen = r.cursor.generation
	return nil
}

func (r *localOutputSource) pin(path string, limit uint64, complete bool) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open output generation %d: %w", r.cursor.generation, err)
	}
	r.file = file
	r.fileGen = r.cursor.generation
	r.fileLimit = limit
	r.complete = complete
	return nil
}

func (r *localOutputSource) closeFile() {
	if r.file != nil {
		closeQuietly(r.file)
		r.file = nil
	}
}

func (r *localOutputSource) Close() error {
	r.closeOnce.Do(func() { close(r.done) })
	r.mu.Lock()
	r.closeFile()
	r.mu.Unlock()
	return nil
}
