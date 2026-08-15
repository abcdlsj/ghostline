package ghostline

import (
	"os"
	"path/filepath"
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
