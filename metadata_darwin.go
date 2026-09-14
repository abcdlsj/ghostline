//go:build darwin

package ghostline

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// probeForegroundFD resolves the PTY's foreground process group with
// TIOCGPGRP, then reads that group leader's name and full argv. The native
// sysctl path costs microseconds per probe, so callers can refresh foreground
// metadata on a short interval without spawning ps or lsof.
func probeForegroundFD(ctx context.Context, fd int) (SessionMetadata, error) {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return SessionMetadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return SessionMetadata{}, err
	}
	pid, name, ok := foregroundProcess(pgrp)
	if !ok {
		return SessionMetadata{}, fmt.Errorf("no process for foreground process group %d", pgrp)
	}
	return SessionMetadata{
		Process:     name,
		CommandLine: processCommandLine(pid, name),
	}, nil
}

// foregroundProcess returns the pid and command name of one process in the
// group. The leader is tried first because its pid equals the group id; the
// full process table is scanned only when the leader already exited.
func foregroundProcess(pgrp int) (int, string, bool) {
	if info, err := unix.SysctlKinfoProc("kern.proc.pid", pgrp); err == nil {
		if int(info.Eproc.Pgid) == pgrp {
			if name := commString(info.Proc.P_comm[:]); name != "" {
				return int(info.Proc.P_pid), name, true
			}
		}
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, "", false
	}
	for _, info := range processes {
		if int(info.Eproc.Pgid) != pgrp {
			continue
		}
		if name := commString(info.Proc.P_comm[:]); name != "" {
			return int(info.Proc.P_pid), name, true
		}
	}
	return 0, "", false
}

// commString trims the fixed-width process name at its first NUL. The kernel
// copies the full array without clearing the bytes after the terminator.
func commString(raw []byte) string {
	if index := bytes.IndexByte(raw, 0); index >= 0 {
		raw = raw[:index]
	}
	return strings.TrimSpace(string(raw))
}

// processCommandLine joins the process argv into one command line. It falls
// back to the process name when argv cannot be read, so callers always have a
// usable label.
func processCommandLine(pid int, fallback string) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return fallback
	}
	if argv := parseProcArgs(raw); len(argv) > 0 {
		return strings.Join(argv, " ")
	}
	return fallback
}

// parseProcArgs decodes a KERN_PROCARGS2 buffer into argv. The layout is
// [argc int32][exec_path NUL][padding NULs][argv0 NUL][argv1 NUL]...[env].
func parseProcArgs(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := int(int32(binary.NativeEndian.Uint32(raw[:4])))
	if argc < 1 {
		return nil
	}
	rest := raw[4:]
	execEnd := bytes.IndexByte(rest, 0)
	if execEnd < 0 {
		return nil
	}
	// Skip the exec path terminator and any alignment padding.
	position := execEnd
	for position < len(rest) && rest[position] == 0 {
		position++
	}
	argv := make([]string, 0, argc)
	for index := 0; index < argc; index++ {
		if position >= len(rest) {
			break
		}
		end := bytes.IndexByte(rest[position:], 0)
		if end < 0 {
			argv = append(argv, string(rest[position:]))
			break
		}
		argv = append(argv, string(rest[position:position+end]))
		position += end + 1
	}
	return argv
}
