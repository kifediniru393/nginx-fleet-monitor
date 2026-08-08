//go:build !linux

package collectors

// listWorkers requires /proc; on non-Linux platforms the workers collector
// emits no per-pid series.
func listWorkers() []workerStats { return nil }
