package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyStorageMigratesIntoPluginData(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "neighbourchat.koplugin")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	legacyBackup := filepath.Join(parent, "NeighbourChat-Backups")
	if err := os.Mkdir(legacyBackup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyBackup, "keep.txt"), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyDownloads := filepath.Join(parent, "NeighbourChat-Downloads")
	if err := os.Mkdir(legacyDownloads, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDownloads, "keep.txt"), []byte("download"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "NeighbourChat.log"), []byte("old log"), 0600); err != nil {
		t.Fatal(err)
	}

	data, err := ensureDataRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateLegacyStorage(root, data); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(legacyBackup); !os.IsNotExist(err) {
		t.Fatal("legacy backup directory was not moved")
	}
	if _, err = os.Stat(legacyDownloads); !os.IsNotExist(err) {
		t.Fatal("legacy download directory was not moved")
	}
	if readText(filepath.Join(data, "backups", "legacy", "keep.txt")) != "backup" {
		t.Fatal("backup was not migrated into plugin data")
	}
	if readText(filepath.Join(data, "downloads", "keep.txt")) != "download" {
		t.Fatal("download was not migrated into plugin data")
	}
	if readText(filepath.Join(data, "NeighbourChat.log")) != "old log" {
		t.Fatal("log was not migrated into plugin data")
	}
}

func TestEnsureDataRootRejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "neighbourchat.koplugin")
	target := filepath.Join(parent, "outside")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, root); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := ensureDataRoot(root); err == nil {
		t.Fatal("symlinked plugin root was accepted")
	}
}
