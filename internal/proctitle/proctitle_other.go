//go:build !linux

package proctitle

// Set is a no-op on non-Linux platforms.
func Set(name string) error { return nil }
