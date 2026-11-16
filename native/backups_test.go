package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupsConsolidateWithoutChangingContents(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "lan_chat")
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chat-before-v3.sqlite3", "chat-before-v3.sqlite3.nc3e", "appreg-before-v3.sqlite3"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("original "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	legacyName := "NeighbourChat-data-backup-20261109-120000-kept"
	legacy := filepath.Join(parent, legacyName)
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "chat.sqlite3"), []byte("old complete data"), 0600); err != nil {
		t.Fatal(err)
	}
	// Do not touch other applications or unrelated similarly named folders.
	unknown := filepath.Join(parent, "NeighbourChat-data-backup-unrelated")
	if err := os.Mkdir(unknown, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknown, "keep.txt"), []byte("leave me"), 0600); err != nil {
		t.Fatal(err)
	}
	destination, err := backupDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(destination, filepath.Join(backupRoot(root), "Automatic")+string(filepath.Separator)) {
		t.Fatal(destination)
	}
	for _, name := range []string{"chat-before-v3.sqlite3", "chat-before-v3.sqlite3.nc3e", "appreg-before-v3.sqlite3"} {
		if readText(filepath.Join(destination, name)) != "original "+name {
			t.Fatal("backup changed", name)
		}
		if _, err = os.Stat(filepath.Join(data, name)); !os.IsNotExist(err) {
			t.Fatal("backup left in data", name)
		}
	}
	if readText(filepath.Join(backupRoot(root), "Older", legacyName, "chat.sqlite3")) != "old complete data" {
		t.Fatal("old backup missing")
	}
	if readText(filepath.Join(unknown, "keep.txt")) != "leave me" {
		t.Fatal("unrelated folder changed")
	}
	again, err := backupDirectory(root)
	if err != nil || again != destination {
		t.Fatal("backup location changed on reopen", err)
	}
	other := filepath.Join(parent, "other-install")
	if err = os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	different, err := backupDirectory(other)
	if err != nil || different == destination {
		t.Fatal("installations share automatic backups", err)
	}
}

func TestBackupCollisionsKeepBothCopiesAndRejectUnsafeLocations(t *testing.T) {
	root := t.TempDir()
	destination, err := backupDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	name := "chat-before-v3.sqlite3"
	source := filepath.Join(root, "data", name)
	if err = os.WriteFile(source, []byte("newer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(destination, name), []byte("older"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = backupDirectory(root); err == nil {
		t.Fatal("collision silently overwrote a backup")
	}
	if readText(source) != "newer" || readText(filepath.Join(destination, name)) != "older" {
		t.Fatal("collision lost data")
	}
	if err = os.WriteFile(filepath.Join(root, "data", "backup-id.txt"), []byte("../../outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = backupDirectory(root); err == nil {
		t.Fatal("unsafe backup ID accepted")
	}
	parent := t.TempDir()
	other := filepath.Join(parent, "lan_chat")
	if err = os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(backupRoot(other), []byte("existing unrelated file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = backupDirectory(other); err == nil {
		t.Fatal("non-directory backup location accepted")
	}
	if readText(backupRoot(other)) != "existing unrelated file" {
		t.Fatal("existing file changed")
	}
}

func TestBackupDirectoryRejectsLinks(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "lan_chat")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Symlink(out, backupRoot(root)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := backupDirectory(root); err == nil {
		t.Fatal("linked backup root accepted")
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside backup root", err)
	}
}

func TestReinstalledDataKeepsItsAutomaticBackupAssociation(t *testing.T) {
	root := t.TempDir()
	s := testStore(t)
	if err := s.enableEncryption(vaultTestPassword); err != nil {
		t.Fatal(err)
	}
	dataBefore := s.Data
	backupBefore := s.Backups
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored")
	if err := os.Mkdir(restored, 0700); err != nil {
		t.Fatal(err)
	}
	// Restore to a new app folder on the same volume, with the whole data folder.
	if err := os.Rename(dataBefore, filepath.Join(restored, "data")); err != nil {
		t.Fatal(err)
	}
	// Move its central backup folder as a USB restore would.
	if err := os.Rename(backupRoot(s.Root), backupRoot(restored)); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer again.DB.Close()
	if filepath.Base(again.Backups) != filepath.Base(backupBefore) {
		t.Fatal("restore changed backup identity")
	}
	if err = again.unlockEncryption(vaultTestPassword); err != nil {
		t.Fatal("restored encryption failed", err)
	}
}

func TestEncryptedLegacyBackupMovesAndStillDecrypts(t *testing.T) {
	s := testStore(t)
	_, err := s.DB.Exec("INSERT INTO messages(user_id,body,created_at) SELECT id,'keep this backup message',? FROM users WHERE username=?", timestamp(), adminUsername)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(s.Backups, "chat-before-v3.sqlite3")
	if _, err = s.DB.Exec("VACUUM INTO '" + strings.ReplaceAll(backup, "'", "''") + "'"); err != nil {
		t.Fatal(err)
	}
	if err = s.enableEncryption(vaultTestPassword); err != nil {
		t.Fatal(err)
	}
	// Emulate a v4.1 encrypted migration backup still inside data/.
	legacy := filepath.Join(s.Data, "chat-before-v3.sqlite3.nc3e")
	if err = os.Rename(backup+".nc3e", legacy); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer again.DB.Close()
	if _, err = os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy encrypted backup remains in data")
	}
	if err = again.unlockEncryption(vaultTestPassword); err != nil {
		t.Fatal(err)
	}
	if err = again.disableEncryption(vaultTestPassword); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body string
	if err = db.QueryRow("SELECT body FROM messages LIMIT 1").Scan(&body); err != nil || body != "keep this backup message" {
		t.Fatal("moved encrypted backup no longer restores", err)
	}
}
