//go:build darwin

package ghostline

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// probeForegroundFD resolves the PTY's foreground process group with
// TIOCGPGRP, then reads that group's command line with ps. The command column
// carries the full argv, so a running "npm run dev" is reported instead of only
// the executable name.
func probeForegroundFD(ctx context.Context, fd int) (SessionMetadata, error) {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return SessionMetadata{}, err
	}
	// -ww disables the terminal-width truncation ps otherwise applies to the
	// command column.
	output, err := exec.CommandContext(ctx, "ps", "-ww", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		return SessionMetadata{}, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if err := ctx.Err(); err != nil {
			return SessionMetadata{}, err
		}
		pgid, command, ok := parseForegroundPSLine(line)
		if !ok || pgid != pgrp {
			continue
		}
		return SessionMetadata{
			Process:     firstCommandToken(command),
			CommandLine: command,
		}, nil
	}
	return SessionMetadata{}, fmt.Errorf("no process for foreground process group %d", pgrp)
}

// parseForegroundPSLine extracts the process group and command from one
// "pid=,pgid=,command=" row. Only the first two columns are positional; the
// command keeps its internal spacing because ps makes it the final column.
func parseForegroundPSLine(line string) (int, string, bool) {
	line = strings.TrimLeft(line, " \t")
	if line == "" {
		return 0, "", false
	}
	pidEnd := strings.IndexAny(line, " \t")
	if pidEnd < 0 {
		return 0, "", false
	}
	rest := strings.TrimLeft(line[pidEnd:], " \t")
	pgidEnd := strings.IndexAny(rest, " \t")
	if pgidEnd < 0 {
		return 0, "", false
	}
	pgid, err := strconv.Atoi(rest[:pgidEnd])
	if err != nil {
		return 0, "", false
	}
	command := strings.TrimLeft(rest[pgidEnd:], " \t")
	if command == "" {
		return 0, "", false
	}
	return pgid, command, true
}

// firstCommandToken is the executable as invoked, matching the value ps
// reports for the command name column in the common case.
func firstCommandToken(command string) string {
	if index := strings.IndexAny(command, " \t"); index >= 0 {
		return command[:index]
	}
	return command
}
