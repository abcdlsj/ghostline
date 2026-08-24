//go:build !darwin && !linux

package ghostline

import (
	"os"
)

func newSpoolNotifier(_ string, _ *os.File) spoolNotifier {
	return newPollingSpoolNotifier()
}
