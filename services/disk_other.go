//go:build !linux

package services

import "math"

// FreeDiskSpace is a stub for non-Linux platforms (dev only).
// Returns max uint64 so space checks never block during local testing.
func FreeDiskSpace(path string) (uint64, error) {
	return math.MaxUint64, nil
}
