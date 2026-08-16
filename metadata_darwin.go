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
// TIOCGPGRP, then maps it to a process with ps and reads its cwd with lsof.
func probeForegroundFD(ctx context.Context, fd int) (SessionMetadata, error) {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return SessionMetadata{}, err
	}
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,comm=").Output()
	if err != nil {
		return SessionMetadata{}, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if err := ctx.Err(); err != nil {
			return SessionMetadata{}, err
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil || pgid != pgrp {
			continue
		}
		directory, err := processCWD(ctx, pid)
		if err != nil {
			return SessionMetadata{}, err
		}
		return SessionMetadata{
			Process:   strings.Join(fields[2:], " "),
			Directory: directory,
		}, nil
	}
	return SessionMetadata{}, fmt.Errorf("no process for foreground process group %d", pgrp)
}

func processCWD(ctx context.Context, pid int) (string, error) {
	output, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n"), nil
		}
	}
	return "", nil
}
