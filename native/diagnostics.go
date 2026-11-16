package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func diagnosticLogPath(root string) string {
	return filepath.Join(root, "data", "NeighbourChat.log")
}

func consolidateLegacyLogs(previous, root string) error {
	paths := []string{filepath.Join(filepath.Dir(root), "NeighbourChat-install.log")}
	if previous != "" {
		for _, name := range []string{"launch.log", "app.log", "uninstall.log"} {
			paths = append(paths, filepath.Join(previous, name))
		}
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(diagnosticLogPath(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			input.Close()
			return err
		}
		_, err = fmt.Fprintf(output, "\n--- Previous %s ---\n", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(output, input)
		}
		input.Close()
		closeErr := output.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
