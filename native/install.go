package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const appID = "com.neighbourchat.illusion"

type LaunchTicket struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
	Next    string `json:"next,omitempty"`
}

func writeTicket(data string) (LaunchTicket, error) {
	ticket := LaunchTicket{Token: randomToken(32), Expires: time.Now().Add(2 * time.Minute).Unix()}
	b, e := json.Marshal(ticket)
	if e != nil {
		return ticket, e
	}
	e = atomicWrite(filepath.Join(data, "launch-ticket.json"), b, 0600)
	return ticket, e
}
func prepare(root, target, registry string) error {
	root, e := filepath.Abs(root)
	if e != nil {
		return e
	}
	data, e := ensureDataRoot(root)
	if e != nil {
		return e
	}
	if _, e = os.Stat(registry); e != nil {
		return errors.New("Mesquite's application registry is missing; this Kindle/firmware needs checking")
	}
	if e = os.MkdirAll(target, 0755); e != nil {
		return e
	}
	_ = os.Chmod(target, 0755)
	for _, name := range []string{"config.xml", "index.html", "style.css"} {
		b, err := assets.ReadFile("illusion/" + name)
		if err != nil {
			return err
		}
		if err = atomicWrite(filepath.Join(target, name), b, 0644); err != nil {
			return err
		}
	}
	ticket, e := writeTicket(data)
	if e != nil {
		return e
	}
	settings, _ := json.Marshal(map[string]string{"token": ticket.Token, "action": "http://127.0.0.1:8001/_launch"})
	if e = atomicWrite(filepath.Join(target, "bootstrap.js"), append([]byte("var NEIGHBOUR_CHAT="), append(settings, []byte(";\n")...)...), 0644); e != nil {
		return e
	}
	db, e := sql.Open("sqlite", registry)
	if e != nil {
		return e
	}
	defer db.Close()
	if _, e = db.Exec("PRAGMA busy_timeout=10000"); e != nil {
		return e
	}
	backups, e := backupDirectory(root)
	if e != nil {
		return e
	}
	backup := filepath.Join(backups, "appreg-before-v3.sqlite3")
	if _, e = os.Stat(backup); os.IsNotExist(e) {
		if _, e = db.Exec("VACUUM INTO '" + strings.ReplaceAll(backup, "'", "''") + "'"); e != nil {
			return fmt.Errorf("could not back up app registration: %w", e)
		}
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT OR IGNORE INTO interfaces(interface) VALUES('application')"); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT OR IGNORE INTO handlerIds(handlerId) VALUES(?)", appID); e != nil {
		return e
	}
	command := "/usr/bin/mesquite -l " + appID + " -c file://" + filepath.ToSlash(target) + "/"
	for _, p := range [][2]string{{"lipcId", appID}, {"command", command}, {"supportedOrientation", "U"}} {
		if _, e = tx.Exec("INSERT OR REPLACE INTO properties(handlerId,name,value) VALUES(?,?,?)", appID, p[0], p[1]); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return closeOwnMesquite()
}
func closeOwnMesquite() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return e
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 2 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := bytes.Split(raw, []byte{0})
		if len(args) < 3 || filepath.Base(string(args[0])) != "mesquite" {
			continue
		}
		owned := false
		for i := 1; i+1 < len(args); i++ {
			if string(args[i]) == "-l" && string(args[i+1]) == appID {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err = process.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		for n := 0; n < 20; n++ {
			if _, err = os.Stat(filepath.Join("/proc", entry.Name())); os.IsNotExist(err) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}
func stopOwnedProcess(pidFile string, expected []string) error {
	raw := readText(pidFile)
	if raw == "" {
		return nil
	}
	pid, e := strconv.Atoi(raw)
	if e != nil || pid < 2 || pid == os.Getpid() {
		return errors.New("invalid app process identifier; restart the Kindle before uninstalling")
	}
	if runtime.GOOS != "linux" {
		return errors.New("process shutdown is supported on Kindle/Linux only")
	}
	command, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if os.IsNotExist(e) {
		_ = os.Remove(pidFile)
		return nil
	}
	if e != nil {
		return e
	}
	if !ownedCommand(command, expected) {
		return errors.New("the saved PID belongs to another process; it was not stopped")
	}
	process, e := os.FindProcess(pid)
	if e != nil {
		return e
	}
	if e = process.Signal(syscall.SIGTERM); e != nil {
		return e
	}
	for n := 0; n < 150; n++ {
		if _, e = os.Stat(fmt.Sprintf("/proc/%d/cmdline", pid)); os.IsNotExist(e) {
			_ = os.Remove(pidFile)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("the app is still stopping; restart the Kindle and run Uninstall again")
}
func ownedCommand(raw []byte, expected []string) bool {
	args := bytes.Split(raw, []byte{0})
	if len(args) < 2 {
		return false
	}
	for _, path := range expected {
		if strings.HasSuffix(path, ".py") {
			if len(args) > 2 && strings.HasPrefix(filepath.Base(string(args[0])), "python") && string(args[1]) == path {
				return true
			}
		} else if string(args[0]) == path && string(args[1]) == "serve" {
			return true
		}
	}
	return false
}
func uninstall(root, target, registry string) error {
	root, e := filepath.Abs(root)
	if e != nil {
		return e
	}
	for _, process := range []struct {
		pid  string
		args []string
	}{{filepath.Join(root, "data", "server.pid"), []string{filepath.Join(root, "neighbour-chat")}}, {"/tmp/neighbour-chat.pid", []string{filepath.Join(root, "app_kindle.py")}}, {"/tmp/neighbour-chat-illusion.pid", []string{filepath.Join(root, "illusion_agent.py")}}} {
		if e = stopOwnedProcess(process.pid, process.args); e != nil {
			return e
		}
	}
	if _, e = os.Stat(registry); e == nil {
		db, err := sql.Open("sqlite", registry)
		if err != nil {
			return err
		}
		defer db.Close()
		_, _ = db.Exec("PRAGMA busy_timeout=10000")
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM properties WHERE handlerId=?", appID); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM handlerIds WHERE handlerId=?", appID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	for _, name := range []string{"index.html", "config.xml", "style.css", "bootstrap.js"} {
		if e = os.Remove(filepath.Join(target, name)); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	_ = os.Remove(target)
	_ = os.Remove(filepath.Join(root, "data", "launch-ticket.json"))
	if e = closeOwnMesquite(); e != nil {
		return e
	}
	fmt.Println("Neighbour Chat is stopped and unregistered. Your chat data has been kept.")
	return nil
}
