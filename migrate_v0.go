package ghostline

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// adoptV0State creates a new v1 output log and fills it from the v0 source
// archives and live spool. The v0 byte offset is intentionally not reused as
// a v1 cursor: the destination starts at generation one and owns its cursor
// namespace from the first rebuilt byte.
func adoptV0State(ctx context.Context, outputRoot, name string, master *os.File, size Size, spoolPath string, createdAt time.Time, pid int, exit *ExitError, scrollbackMaxBytes uint64) (*sessionState, error) {
	output, err := createOutputLog(outputRoot, name)
	if err != nil {
		closeFileQuietly(master)
		return nil, err
	}
	vt, err := newVTTerminalWithOptions(size.Columns, size.Rows, vtTerminalOptions{
		ScrollbackMaxBytes: scrollbackMaxBytes,
	})
	if err != nil {
		output.discard()
		closeFileQuietly(master)
		return nil, fmt.Errorf("create v0 handoff vt: %w", err)
	}
	if err := replayV0Spool(ctx, vt, output, spoolPath); err != nil {
		output.discard()
		vt.Close()
		closeFileQuietly(master)
		return nil, err
	}
	state, err := adoptStateWithTerminal(name, master, vt, output, size, createdAt, pid, exit, scrollbackMaxBytes)
	if err != nil {
		output.discard()
	}
	return state, err
}

func replayV0Spool(ctx context.Context, vt *vtTerminal, output *outputLog, spoolPath string) error {
	archives, err := filepath.Glob(spoolPath + ".*.gz")
	if err != nil {
		return fmt.Errorf("find v0 spool archives: %w", err)
	}
	sort.Strings(archives)
	paths := make([]struct {
		path       string
		compressed bool
	}, 0, len(archives)+1)
	for _, archive := range archives {
		paths = append(paths, struct {
			path       string
			compressed bool
		}{path: archive, compressed: true})
	}
	if _, err := os.Stat(spoolPath); err == nil {
		paths = append(paths, struct {
			path       string
			compressed bool
		}{path: spoolPath})
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat v0 spool: %w", err)
	} else if len(paths) == 0 {
		return fmt.Errorf("stat v0 spool: %w", err)
	}

	for _, item := range paths {
		if err := replayV0SpoolFile(ctx, vt, output, item.path, item.compressed); err != nil {
			return err
		}
	}
	return nil
}

func replayV0SpoolFile(ctx context.Context, vt *vtTerminal, output *outputLog, path string, compressed bool) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open v0 spool replay %s: %w", path, err)
	}
	defer closeQuietly(file)

	var reader io.Reader = file
	var compressedReader *gzip.Reader
	if compressed {
		compressedReader, err = gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("open compressed v0 spool replay %s: %w", path, err)
		}
		defer compressedReader.Close()
		reader = compressedReader
	}

	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			vt.Feed(buffer[:read])
			if err := output.append(buffer[:read]); err != nil {
				return fmt.Errorf("append v0 spool replay %s: %w", path, err)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read v0 spool replay %s: %w", path, readErr)
		}
	}
}
