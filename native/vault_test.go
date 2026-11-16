package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vaultTestPassword = "local-encryption-test-password"

func TestOptionalEncryptionToggleProtectsExistingAndNewContent(t *testing.T) {
	s, _, server := testAPI(t)
	client := adminRemote(t, s, server)
	if e := client.call("POST", "/api/send", map[string]string{"body": "secret stored message"}, nil); e != nil {
		t.Fatal(e)
	}
	if e := client.upload("note.txt", "", strings.NewReader("secret file bytes"), nil); e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(s.Backups, "chat-before-v3.sqlite3")
	backup6 := filepath.Join(s.Backups, "chat-before-v6.sqlite3")
	if _, e := s.DB.Exec("VACUUM INTO '" + strings.ReplaceAll(backup6, "'", "''") + "'"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec("VACUUM INTO '" + strings.ReplaceAll(backup, "'", "''") + "'"); e != nil {
		t.Fatal(e)
	}
	if e := s.enableEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(backup6); !os.IsNotExist(e) {
		t.Fatal("plaintext v6 snapshot left behind")
	}
	if _, e := os.Stat(backup6 + ".nc3e"); e != nil {
		t.Fatal("encrypted v6 snapshot missing", e)
	}
	var encoded, stored string
	var encrypted int
	if e := s.DB.QueryRow("SELECT body,body_encrypted FROM messages LIMIT 1").Scan(&encoded, &encrypted); e != nil || encrypted != 1 || strings.Contains(encoded, "secret") {
		t.Fatal("message not encrypted", e)
	}
	_ = s.DB.QueryRow("SELECT stored_name FROM files LIMIT 1").Scan(&stored)
	raw, e := os.ReadFile(filepath.Join(s.Data, "uploads", stored+".nc3e"))
	if e != nil || bytes.Contains(raw, []byte("secret file bytes")) {
		t.Fatal("file not encrypted", e)
	}
	if _, e = os.Stat(backup); !os.IsNotExist(e) {
		t.Fatal("plaintext migration backup left behind")
	}
	if e = client.call("POST", "/api/send", map[string]string{"body": "another protected message"}, nil); e != nil {
		t.Fatal(e)
	}
	var data ChatData
	if e = client.call("GET", "/api/chat-data", nil, &data); e != nil || len(data.Messages) != 2 || data.Messages[0].Body != "secret stored message" {
		t.Fatal("encrypted chat read failed", e)
	}
	var found struct{ Results []Message }
	if e = client.call("GET", "/api/search?q=protected", nil, &found); e != nil || len(found.Results) != 1 {
		t.Fatal("encrypted search failed", e)
	}
	response, e := client.request("GET", "/api/download/1", nil, "")
	if e != nil {
		t.Fatal(e)
	}
	plain, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(plain) != "secret file bytes" {
		t.Fatal("encrypted download failed")
	}
	s.Vault.lock()
	if _, e = client.info(); e == nil {
		t.Fatal("locked content was accessible")
	}
	if e = s.unlockEncryption("wrong-password"); e == nil {
		t.Fatal("wrong unlock password accepted")
	}
	if e = s.unlockEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	if e = s.disableEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	enabled, locked := s.Vault.status()
	if enabled || locked {
		t.Fatal("encryption did not switch off")
	}
	if readText(filepath.Join(s.Data, "uploads", stored)) != "secret file bytes" {
		t.Fatal("file not restored after disabling")
	}
	if e = s.DB.QueryRow("SELECT body FROM messages ORDER BY id LIMIT 1").Scan(&encoded); e != nil || encoded != "secret stored message" {
		t.Fatal("message not restored", e)
	}
	if _, e = os.Stat(backup); e != nil {
		t.Fatal("backup was not restored", e)
	}
}
func TestEncryptedFileBoundariesTamperAndTruncation(t *testing.T) {
	s := testStore(t)
	if e := s.enableEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	for _, size := range []int{0, 1, 65535, 65536, 65537, 131073} {
		data := bytes.Repeat([]byte("k"), size)
		var encrypted, plain bytes.Buffer
		if _, e := s.Vault.encryptFile(&encrypted, bytes.NewReader(data), "test-file", 1<<20); e != nil {
			t.Fatal(e)
		}
		if _, e := s.Vault.decryptFile(&plain, bytes.NewReader(encrypted.Bytes()), "test-file"); e != nil || !bytes.Equal(data, plain.Bytes()) {
			t.Fatal("chunk boundary failed", size, e)
		}
		bad := append([]byte{}, encrypted.Bytes()...)
		bad[len(bad)-1] ^= 1
		if _, e := s.Vault.decryptFile(io.Discard, bytes.NewReader(bad), "test-file"); e == nil {
			t.Fatal("tampered file accepted")
		}
		if _, e := s.Vault.decryptFile(io.Discard, bytes.NewReader(encrypted.Bytes()[:encrypted.Len()-1]), "test-file"); e == nil {
			t.Fatal("truncated file accepted")
		}
	}
}
func TestEncryptionSettingsCannotBeAlteredWithoutPassword(t *testing.T) {
	s := testStore(t)
	if e := s.enableEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	s.Vault.lock()
	s.Vault.config.Enabled = false
	if e := s.unlockEncryption(vaultTestPassword); e == nil {
		t.Fatal("modified encryption policy accepted")
	}
}
func TestClickInstallerUpgradeAndFullRemovalKeepDataAndOtherApps(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "lan_chat")
	if e := os.MkdirAll(filepath.Join(root, "data"), 0700); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(root, "app_kindle.py"), []byte("old app"), 0600)
	for _, name := range []string{"app.log", "launch.log", "uninstall.log"} {
		_ = os.WriteFile(filepath.Join(root, name), []byte("previous-"+name), 0600)
	}
	_ = os.WriteFile(filepath.Join(parent, "NeighbourChat-install.log"), []byte("previous-install"), 0600)
	_ = os.WriteFile(filepath.Join(root, "data", "keep.txt"), []byte("preserve"), 0600)
	registry := filepath.Join(parent, "registry.db")
	db, _ := sql.Open("sqlite", registry)
	_, e := db.Exec("CREATE TABLE interfaces(interface TEXT PRIMARY KEY);CREATE TABLE handlerIds(handlerId TEXT PRIMARY KEY);CREATE TABLE properties(handlerId TEXT,name TEXT,value TEXT,PRIMARY KEY(handlerId,name));INSERT INTO handlerIds VALUES('other.app');")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(parent, "package")
	for _, dir := range []string{"lan_chat", "documents/NeighbourChat"} {
		_ = os.MkdirAll(filepath.Join(source, dir), 0755)
	}
	for _, name := range []string{"neighbour-chat", "open-app.sh"} {
		_ = os.WriteFile(filepath.Join(source, "lan_chat", name), []byte("release"), 0755)
	}
	marker, _ := json.Marshal(map[string]string{"app_id": appID})
	_ = os.WriteFile(filepath.Join(source, "lan_chat", "installation.json"), marker, 0600)
	for _, name := range []string{"Neighbour Chat.sh", "Uninstall Neighbour Chat.sh"} {
		_ = os.WriteFile(filepath.Join(source, "documents", name), []byte("launcher"), 0600)
	}
	for _, name := range []string{"config.xml", "index.html", "style.css"} {
		b, _ := assets.ReadFile("illusion/" + name)
		_ = os.WriteFile(filepath.Join(source, "documents", "NeighbourChat", name), b, 0600)
	}
	documents := filepath.Join(parent, "documents")
	_ = os.MkdirAll(documents, 0755)
	book := filepath.Join(documents, "my-book.epub")
	_ = os.WriteFile(book, []byte("book"), 0600)
	if e = installBundle(source, root, registry, filepath.Join(parent, "mesquite")); e != nil {
		t.Fatal(e)
	}
	if readText(filepath.Join(root, "data", "keep.txt")) != "preserve" {
		t.Fatal("upgrade lost user data")
	}
	if _, e = os.Stat(filepath.Join(root, "app_kindle.py")); !os.IsNotExist(e) {
		t.Fatal("old Python code remains")
	}
	combined := readText(diagnosticLogPath(root))
	for _, value := range []string{"previous-app.log", "previous-launch.log", "previous-uninstall.log", "previous-install"} {
		if !strings.Contains(combined, value) {
			t.Fatal("previous diagnostic log was lost", value)
		}
	}
	if _, e = os.Stat(filepath.Join(parent, "NeighbourChat-install.log")); !os.IsNotExist(e) {
		t.Fatal("separate installer log remains")
	}
	if e = removeInstalledFiles(root); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("app files remain")
	}
	backups, _ := filepath.Glob(filepath.Join(parent, "NeighbourChat-Backups", "Uninstalled", "*", "keep.txt"))
	if len(backups) != 1 || readText(backups[0]) != "preserve" {
		t.Fatal("uninstall lost data")
	}
	if readText(book) != "book" {
		t.Fatal("uninstall changed a book")
	}
	if readText(diagnosticLogPath(root)) != combined {
		t.Fatal("uninstall removed diagnostics")
	}
}
func TestEncryptionRebootRequiresUnlockAndKeepsTheSameContent(t *testing.T) {
	s := testStore(t)
	root := s.Root
	_, e := s.DB.Exec("INSERT INTO messages(user_id,body,created_at) SELECT id,'persisted private text',? FROM users WHERE username=?", timestamp(), adminUsername)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.enableEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	again, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	defer again.DB.Close()
	enabled, locked := again.Vault.status()
	if !enabled || !locked {
		t.Fatal("protected data was not locked after restart")
	}
	if e = again.unlockEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	var id int64
	var body string
	var encrypted int
	if e = again.DB.QueryRow("SELECT id,body,body_encrypted FROM messages LIMIT 1").Scan(&id, &body, &encrypted); e != nil {
		t.Fatal(e)
	}
	plain, e := again.Vault.decodeMessage(id, body, encrypted)
	if e != nil || plain != "persisted private text" {
		t.Fatal("content changed across restart", e)
	}
}
func TestInterruptedEncryptionConversionResumesWithoutLosingMessages(t *testing.T) {
	s := testStore(t)
	_, e := s.DB.Exec("INSERT INTO messages(user_id,body,created_at) SELECT id,'keep this secret',? FROM users WHERE username=?", timestamp(), adminUsername)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec("INSERT INTO files(user_id,stored_name,original_name,size,created_at) SELECT id,'missing-file','lost.txt',8,? FROM users WHERE username=?", timestamp(), adminUsername)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.enableEncryption(vaultTestPassword); e == nil {
		t.Fatal("missing file should stop the conversion")
	}
	if !s.Vault.pending() {
		t.Fatal("incomplete conversion was reported as complete")
	}
	_ = os.WriteFile(filepath.Join(s.Data, "uploads", "missing-file"), []byte("restored"), 0600)
	s.Vault.lock()
	if e = s.unlockEncryption(vaultTestPassword); e != nil {
		t.Fatal(e)
	}
	if s.Vault.pending() {
		t.Fatal("conversion did not resume")
	}
	var id int64
	var body string
	var flag int
	_ = s.DB.QueryRow("SELECT id,body,body_encrypted FROM messages LIMIT 1").Scan(&id, &body, &flag)
	plain, e := s.Vault.decodeMessage(id, body, flag)
	if e != nil || plain != "keep this secret" {
		t.Fatal("resume lost message content", e)
	}
}
