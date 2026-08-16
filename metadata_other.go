//go:build !linux && !darwin

package ghostline

import (
	"context"
	"errors"
)

func duplicateMasterFD(int) (int, error) {
	return -1, errors.New("foreground metadata is unsupported on this platform")
}

func closeMasterFD(int) error { return nil }

func probeForegroundFD(context.Context, int) (SessionMetadata, error) {
	return SessionMetadata{}, nil
}
