package ghostline

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func closeQuietly(closer io.Closer) {
	if closer == nil {
		return
	}
	_ = closer.Close()
}

func closeFileQuietly(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}

func removeQuietly(path string) {
	_ = os.Remove(path)
}

// defaultOutputDirectory is the per-session output root when the
// caller does not configure one.
func defaultOutputDirectory() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ghostline", "output")
}

func validateEnvironment(environment []string) error {
	for _, entry := range environment {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 || strings.IndexByte(entry, 0) >= 0 {
			return fmt.Errorf("invalid environment entry %q", entry)
		}
	}
	return nil
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.IndexByte(name, 0) >= 0 {
		return fmt.Errorf("%w: %q", ErrInvalidSessionName, name)
	}
	return nil
}
