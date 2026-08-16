package ghostline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultOutputDirectory is the per-session output spool directory when the
// caller does not configure one.
func defaultOutputDirectory() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ghostline", "output")
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func mustGlob(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	return matches
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
