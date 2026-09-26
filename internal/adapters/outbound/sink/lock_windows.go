//go:build windows

package sink

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("sink: open spool lock: %w", err)
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
	return f.Close()
}
