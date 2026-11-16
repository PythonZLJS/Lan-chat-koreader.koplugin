//go:build linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockMaintenance(root string) (func(), error) {
	data, err := ensureDataRoot(root)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(root))
	path := filepath.Join(data, fmt.Sprintf(".maintenance-%x.lock", sum[:8]))
	file, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		file.Close()
		return nil, fmt.Errorf("another install/uninstall is already running")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
