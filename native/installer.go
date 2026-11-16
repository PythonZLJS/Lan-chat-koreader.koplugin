package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func copyInstallFile(source, destination string, mode os.FileMode) error {
	info, e := os.Lstat(source)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return errors.New("the install package contains an unexpected file type")
	}
	input, e := os.Open(source)
	if e != nil {
		return e
	}
	defer input.Close()
	if e = os.MkdirAll(filepath.Dir(destination), 0755); e != nil {
		return e
	}
	temporary, e := os.CreateTemp(filepath.Dir(destination), ".install-*")
	if e != nil {
		return e
	}
	name := temporary.Name()
	defer os.Remove(name)
	_ = temporary.Chmod(mode)
	_, e = io.Copy(temporary, input)
	if e == nil {
		e = temporary.Sync()
	}
	closeErr := temporary.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, destination)
}
func installBundle(source, root, registry, target string) error {
	root, e := filepath.Abs(root)
	if e != nil {
		return e
	}
	if filepath.Base(root) != "lan_chat" {
		return errors.New("refusing an unexpected application directory")
	}
	release, e := lockMaintenance(root)
	if e != nil {
		return e
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	if info, e := os.Lstat(root); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the application directory must not be a symlink")
	}
	payload := filepath.Join(source, "lan_chat")
	for _, name := range []string{"neighbour-chat", "open-app.sh", "installation.json"} {
		if _, e = os.Stat(filepath.Join(payload, name)); e != nil {
			return fmt.Errorf("incomplete package: %s", name)
		}
	}
	if _, e = os.Stat(root); e == nil {
		known := false
		for _, name := range []string{"neighbour-chat", "app_kindle.py", "installation.json"} {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				known = true
			}
		}
		if !known {
			return errors.New("the existing lan_chat folder is not a recognized app installation")
		}
	}
	if e = stopOwnedProcess(filepath.Join(root, "data", "server.pid"), []string{filepath.Join(root, "neighbour-chat")}); e != nil {
		return e
	}
	for _, old := range []struct{ pid, name string }{{"/tmp/neighbour-chat.pid", "app_kindle.py"}, {"/tmp/neighbour-chat-illusion.pid", "illusion_agent.py"}} {
		if e = stopOwnedProcess(old.pid, []string{filepath.Join(root, old.name)}); e != nil {
			return e
		}
	}
	if e = closeOwnMesquite(); e != nil {
		return e
	}
	upgradeBackups, e := os.MkdirTemp(filepath.Dir(root), ".neighbourchat-upgrade-*")
	if e != nil {
		return e
	}
	_ = os.Chmod(upgradeBackups, 0700)
	defer os.RemoveAll(upgradeBackups)
	previous := filepath.Join(upgradeBackups, time.Now().Format("20060102-150405")+"-"+randomToken(6))
	hadPrevious := false
	dataMoved := false
	complete := false
	if _, e = os.Stat(root); e == nil {
		release()
		released = true
		if e = os.Rename(root, previous); e != nil {
			return e
		}
		hadPrevious = true
	}
	defer func() {
		if !complete {
			if dataMoved {
				_ = os.Rename(filepath.Join(root, "data"), filepath.Join(previous, "data"))
			}
			_ = os.RemoveAll(root)
			if hadPrevious {
				_ = os.Rename(previous, root)
			}
		}
	}()
	if e = os.MkdirAll(root, 0755); e != nil {
		return e
	}
	e = filepath.WalkDir(payload, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(payload, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative == "data" || strings.HasPrefix(relative, "data"+string(filepath.Separator)) {
			return errors.New("installer packages must not contain chat data")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("installer links are not allowed")
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(root, relative), 0755)
		}
		mode := os.FileMode(0644)
		if relative == "neighbour-chat" || relative == "open-app.sh" {
			mode = 0755
		}
		return copyInstallFile(path, filepath.Join(root, relative), mode)
	})
	if e != nil {
		return e
	}
	if hadPrevious {
		if _, e = os.Stat(filepath.Join(previous, "data")); e == nil {
			if e = os.Rename(filepath.Join(previous, "data"), filepath.Join(root, "data")); e != nil {
				return e
			}
			dataMoved = true
		}
	}
	store, e := openStore(root)
	if e != nil {
		return fmt.Errorf("chat data verification failed: %w", e)
	}
	if e = store.DB.Close(); e != nil {
		return e
	}
	documents := filepath.Join(filepath.Dir(root), "documents")
	if e = os.MkdirAll(documents, 0755); e != nil {
		return e
	}
	for _, name := range []string{"Neighbour Chat.sh", "Uninstall Neighbour Chat.sh"} {
		if e = copyInstallFile(filepath.Join(source, "documents", name), filepath.Join(documents, name), 0755); e != nil {
			return e
		}
	}
	for _, name := range []string{"config.xml", "index.html", "style.css"} {
		if e = copyInstallFile(filepath.Join(source, "documents", "NeighbourChat", name), filepath.Join(documents, "NeighbourChat", name), 0644); e != nil {
			return e
		}
	}
	if e = prepare(root, target, registry); e != nil {
		return e
	}
	oldLogRoot := ""
	if hadPrevious {
		oldLogRoot = previous
	}
	if e = consolidateLegacyLogs(oldLogRoot, root); e != nil {
		return fmt.Errorf("could not consolidate previous diagnostic logs: %w", e)
	}
	complete = true
	for _, name := range []string{"Start Chat.sh", "Stop Chat.sh", "Chat Details.sh"} {
		_ = os.Remove(filepath.Join(documents, name))
	}
	if hadPrevious {
		if e = os.RemoveAll(previous); e != nil {
			return errors.New("installed successfully, but old app files could not all be removed")
		}
	}
	fmt.Println("Installed Neighbour Chat. Existing chat data was preserved.")
	return nil
}
func removeInstalledFiles(root string) error {
	root, e := filepath.Abs(root)
	if e != nil {
		return e
	}
	if filepath.Base(root) != "lan_chat" {
		return errors.New("refusing an unexpected application directory")
	}
	var marker struct {
		AppID string `json:"app_id"`
	}
	b, e := os.ReadFile(filepath.Join(root, "installation.json"))
	if e != nil || json.Unmarshal(b, &marker) != nil || marker.AppID != appID {
		return errors.New("installation marker is missing; app data was kept and files were not removed")
	}
	if info, e := os.Lstat(root); e != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove a linked installation")
	}
	if info, err := os.Lstat(filepath.Join(root, "data")); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing to remove linked or non-directory chat data")
		}
		fmt.Println("Your chat data was kept in: " + filepath.Join(root, "data"))
	} else if !os.IsNotExist(err) {
		return err
	}
	documents := filepath.Join(filepath.Dir(root), "documents")
	for _, name := range []string{"Neighbour Chat.sh", "Uninstall Neighbour Chat.sh", "Start Chat.sh", "Stop Chat.sh", "Chat Details.sh"} {
		if e = os.Remove(filepath.Join(documents, name)); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e = os.RemoveAll(filepath.Join(documents, "NeighbourChat")); e != nil {
		return e
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "data" {
			continue
		}
		if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
