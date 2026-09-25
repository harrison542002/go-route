//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package sink

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("sink: open spool lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrSpoolLocked, path)
		}
		return nil, fmt.Errorf("sink: lock spool: %w", err)
	}
	return f, nil
}

func unlockDir(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
