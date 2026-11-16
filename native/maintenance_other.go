//go:build !linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func lockMaintenance(root string) (func(), error) {
	sum := sha256.Sum256([]byte(root))
	path := filepath.Join(os.TempDir(), fmt.Sprintf("neighbour-chat-maintenance-%x.lock", sum[:8]))
	file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	return func() { _ = file.Close(); _ = os.Remove(path) }, nil
}
