package par2

// WithAfterChunk calls fn after every chunk of a repair has been written.
func WithAfterChunk(fn func(index int)) RepairOption {
	return func(c *repairConfig) { c.afterChunk = fn }
}
