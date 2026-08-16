//go:build linux || darwin

package ghostline

import (
	"golang.org/x/sys/unix"
)

// duplicateMasterFD duplicates the PTY master so the OS probe never races a
// migration or session close that could close the original descriptor.
func duplicateMasterFD(fd int) (int, error) {
	return unix.Dup(fd)
}

func closeMasterFD(fd int) error {
	return unix.Close(fd)
}
