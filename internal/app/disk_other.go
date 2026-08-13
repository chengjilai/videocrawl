//go:build !linux

package app

import "math"

// diskFreeBytes: no portable statfs; report "plenty" so the crawl-loop is
// never blocked on platforms without syscall.Statfs (the lab runs Linux).
func diskFreeBytes(path string) (int64, error) {
	return math.MaxInt64, nil
}
