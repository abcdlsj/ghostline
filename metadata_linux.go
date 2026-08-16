//go:build linux

package ghostline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// probeForegroundFD resolves the PTY's foreground process group and reads
// the group's command name and working directory from /proc.
func probeForegroundFD(ctx context.Context, fd int) (SessionMetadata, error) {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return SessionMetadata{}, err
	}
	pid, err := processIDForGroup(ctx, pgrp)
	if err != nil {
		return SessionMetadata{}, err
	}
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return SessionMetadata{}, err
	}
	directory, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return SessionMetadata{}, err
	}
	return SessionMetadata{
		Process:   strings.TrimSpace(string(comm)),
		Directory: directory,
	}, nil
}

// processIDForGroup returns a process whose process group matches pgrp. The
// group leader is tried first because it is usually the foreground process;
// the full /proc scan is the fallback for a leader that already exited.
func processIDForGroup(ctx context.Context, pgrp int) (int, error) {
	leader := filepath.Join("/proc", strconv.Itoa(pgrp), "stat")
	if stat, err := os.ReadFile(leader); err == nil && statPgrp(stat) == pgrp {
		return pgrp, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		if statPgrp(stat) != pgrp {
			continue
		}
		return pid, nil
	}
	return 0, fmt.Errorf("no process for foreground process group %d", pgrp)
}

// statPgrp extracts field 5 (process group) from /proc/<pid>/stat. The
// command name can contain spaces and parentheses, so parsing starts after
// the final ')' that closes field 2.
func statPgrp(stat []byte) int {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0
	}
	rest := strings.Fields(string(stat[end+1:]))
	if len(rest) < 3 {
		return 0
	}
	pgrp, _ := strconv.Atoi(rest[2])
	return pgrp
}
