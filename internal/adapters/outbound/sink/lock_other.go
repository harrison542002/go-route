//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package sink

import "os"

// lockDir is a no-op where there is no advisory file lock to take. The spool
// still works; it just cannot refuse a second process pointed at the same
// directory.
func lockDir(string) (*os.File, error) { return nil, nil }

func unlockDir(*os.File) error { return nil }
