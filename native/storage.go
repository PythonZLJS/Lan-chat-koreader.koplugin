package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ensureDataRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("the plugin directory must not be a symlink")
	}
	if err := os.MkdirAll(absolute, 0755); err != nil {
		return "", err
	}
	data := filepath.Join(absolute, "data")
	if info, err := os.Lstat(data); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("data location is not a regular directory: %s", data)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else if err := os.Mkdir(data, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(data, 0700); err != nil {
		return "", err
	}
	return data, nil
}

func migrateLegacyStorage(root, data string) error {
	parent := filepath.Dir(root)
	legacyDirs := []struct {
		name   string
		target string
	}{
		{"NeighbourChat-Backups", filepath.Join(data, "backups", "legacy")},
		{"NeighbourChat-Downloads", filepath.Join(data, "downloads")},
	}
	for _, item := range legacyDirs {
		source := filepath.Join(parent, item.name)
		info, err := os.Lstat(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to migrate linked or non-directory storage: %s", source)
		}
		if _, err := os.Lstat(item.target); err == nil {
			item.target += "-" + randomToken(8)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := privateDirectory(filepath.Dir(item.target)); err != nil {
			return err
		}
		if err := os.Rename(source, item.target); err != nil {
			return fmt.Errorf("migrate %s: %w", item.name, err)
		}
	}
	legacyLog := filepath.Join(parent, "NeighbourChat.log")
	logInfo, err := os.Lstat(legacyLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !logInfo.Mode().IsRegular() {
		return fmt.Errorf("refusing to migrate non-file diagnostic log: %s", legacyLog)
	}
	target := filepath.Join(data, "NeighbourChat.log")
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		if err := os.Rename(legacyLog, target); err != nil {
			return err
		}
		return os.Chmod(target, 0600)
	} else if err != nil {
		return err
	}
	input, err := os.Open(legacyLog)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		input.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	input.Close()
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Remove(legacyLog)
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("storage location is not a regular directory: %s", path)
	}
	return os.Chmod(path, 0700)
}

