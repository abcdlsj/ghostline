package ghostline

import "fmt"

const (
	defaultColumns       = 120
	defaultRows          = 36
	maxTerminalDimension = 1<<16 - 1
)

// Size is a terminal grid size in cells.
type Size struct {
	// Columns is the number of character cells per line.
	Columns int
	// Rows is the number of lines in the grid.
	Rows int
}

func (s Size) validate() error {
	if s.Columns <= 0 || s.Rows <= 0 ||
		s.Columns > maxTerminalDimension || s.Rows > maxTerminalDimension {
		return fmt.Errorf("invalid terminal size %dx%d", s.Columns, s.Rows)
	}
	return nil
}

func (s Size) resolve(fallback Size) (Size, error) {
	if s.Columns == 0 && s.Rows == 0 {
		s = fallback
	}
	if err := s.validate(); err != nil {
		return Size{}, err
	}
	return s, nil
}
