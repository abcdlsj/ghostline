package ghostline

// DefaultVTScrollbackMaxBytes is the default logical scrollback budget for
// each embedded VT terminal. libghostty stores history in page-sized units,
// so the physical allocation can be somewhat larger than this value.
const DefaultVTScrollbackMaxBytes uint64 = 2 << 20

type vtTerminalOptions struct {
	// ScrollbackMaxBytes is the maximum logical scrollback allocation. Zero
	// uses DefaultVTScrollbackMaxBytes.
	ScrollbackMaxBytes uint64
}

func (o vtTerminalOptions) resolvedScrollbackMaxBytes() uint64 {
	if o.ScrollbackMaxBytes == 0 {
		return DefaultVTScrollbackMaxBytes
	}
	return o.ScrollbackMaxBytes
}
