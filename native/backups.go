package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var backupMu sync.Mutex
var validBackupID = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// Backups live beside the application so uninstalling never removes them.
func backupRoot(root string) string {
	return filepath.Join(filepath.Dir(root), "NeighbourChat-Backups")
}

func privateBackupDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup location is not a regular directory: %s", path)
	}
	return nil
}

// Caller holds backupMu. Only recognise names and contents produced by this app.
func prepareBackupRoot(root string) (string, error) {
	base := backupRoot(root)
	if err := privateBackupDirectory(base); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		oldData := strings.HasPrefix(name, "NeighbourChat-data-backup-")
		oldUpgrade := strings.HasPrefix(name, "lan_chat.previous-")
		if !entry.IsDir() || (!oldData && !oldUpgrade) {
			continue
		}
		source := filepath.Join(filepath.Dir(root), name)
		markers := []string{"chat.sqlite3", "admin-password.hash"}
		if oldUpgrade {
			markers = []string{"installation.json", "neighbour-chat", "app_kindle.py"}
		}
		known := false
		for _, marker := range markers {
			if info, e := os.Lstat(filepath.Join(source, marker)); e == nil && info.Mode().IsRegular() {
				known = true
			}
		}
		if !known {
			continue
		}
		older := filepath.Join(base, "Older")
		if err = privateBackupDirectory(older); err != nil {
			return "", err
		}
		destination := filepath.Join(older, name)
		if _, e := os.Lstat(destination); e == nil {
			destination += "-" + randomToken(8)
		} else if !os.IsNotExist(e) {
			return "", e
		}
		if err = os.Rename(source, destination); err != nil {
			return "", fmt.Errorf("could not organise old backup %s: %w", name, err)
		}
	}
	readme := filepath.Join(base, "READ-ME.txt")
	if _, e := os.Lstat(readme); os.IsNotExist(e) {
		text := "NEIGHBOUR CHAT BACKUPS\n\nAutomatic/ contains upgrade safety copies, grouped by installation.\nUninstalled/ contains complete chat-data folders saved during removal.\nOlder/ contains recognised backups made by previous app versions.\nUpgrades/ is temporary rollback space; successful upgrades remove their copy.\n\nTo restore chats: stop/restart the Kindle, connect USB, and keep a PC copy\nof the current lan_chat/data and this complete backup folder first.\nCopy the CONTENTS of the desired Uninstalled/<date-id>/ folder into\nlan_chat/data after installing the app. Disconnect USB and open the app.\nKeep the Automatic folder with its matching data/backup-id.txt so automatic\nbackups and any saved-content encryption remain associated correctly.\n\nOld/uninstalled copies are snapshots: changing encryption in the running app\ndoes not retroactively protect them. Keep backups private.\nThe installer and uninstaller preserve this folder.\n"
		if err = atomicWrite(readme, []byte(text), 0600); err != nil {
			return "", err
		}
	} else if e != nil {
		return "", e
	}
	return base, nil
}

// The ID travels with restored data. Distinct installations cannot accidentally
// share (or encrypt/decrypt) each other's automatic migration backup.
func backupDirectory(root string) (string, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	base, err := prepareBackupRoot(root)
	if err != nil {
		return "", err
	}
	data := filepath.Join(root, "data")
	if err = privateBackupDirectory(data); err != nil {
		return "", err
	}
	idPath := filepath.Join(data, "backup-id.txt")
	raw, err := os.ReadFile(idPath)
	var id string
	if os.IsNotExist(err) {
		id = randomToken(16)
		if err = atomicWrite(idPath, []byte(id), 0600); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else {
		id = strings.TrimSpace(string(raw))
		if !validBackupID.MatchString(id) {
			return "", errors.New("the saved backup ID is damaged; restore data/backup-id.txt from your matching backup")
		}
	}
	automatic := filepath.Join(base, "Automatic")
	if err = privateBackupDirectory(automatic); err != nil {
		return "", err
	}
	destination := filepath.Join(automatic, id)
	if err = privateBackupDirectory(destination); err != nil {
		return "", err
	}
	for _, name := range []string{"chat-before-v3.sqlite3", "chat-before-v3.sqlite3.nc3e", "appreg-before-v3.sqlite3", "chat-before-v6.sqlite3", "chat-before-v6.sqlite3.nc3e"} {
		source := filepath.Join(data, name)
		info, e := os.Lstat(source)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing a linked or non-file backup: %s", source)
		}
		target := filepath.Join(destination, name)
		if _, e = os.Lstat(target); !os.IsNotExist(e) {
			if e != nil {
				return "", e
			}
			return "", fmt.Errorf("two copies of %s exist; both were kept. Move the older Automatic copy to Older before reopening", name)
		}
		if err = os.Rename(source, target); err != nil {
			return "", err
		}
	}
	return destination, nil
}

func backupSection(root, section string) (string, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	base, err := prepareBackupRoot(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, section)
	if err = privateBackupDirectory(path); err != nil {
		return "", err
	}
	return path, nil
}
