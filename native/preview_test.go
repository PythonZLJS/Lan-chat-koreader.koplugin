package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewScreens(t *testing.T) {
	directory := os.Getenv("NC_PREVIEW_DIR")
	if directory == "" {
		t.Skip("set NC_PREVIEW_DIR to render current interface previews")
	}
	if e := os.MkdirAll(directory, 0755); e != nil {
		t.Fatal(e)
	}
	store := testStore(t)
	app, e := newApp(store)
	if e != nil {
		t.Fatal(e)
	}
	pages := []Page{
		{Title: "Neighbour Chat", View: "home", CSRF: "preview-only", Local: true, Saved: []SavedHost{{Origin: "https://192.168.1.42:8000"}}},
		{Title: "Neighbour Chat", View: "chat", CSRF: "preview-only", Local: true, Authenticated: true, Admin: true, Session: SessionInfo{Display: "Alex"}, Address: "https://192.168.1.42:8000", SendID: "preview-only", Users: []User{{ID: 2, Display: "Jamie", Username: "jamie"}}, Messages: []Message{{ID: 1, Display: "Jamie", Body: "Hi Alex! The Kindle chat is working.", Created: timestamp()}, {ID: 2, Display: "Alex", Body: "Great. We can share messages and files over Wi-Fi.", Created: timestamp()}}},
		{Title: "Neighbour Chat", View: "setup", CSRF: "preview-only", Local: true},
		{Title: "Neighbour Chat", View: "details", CSRF: "preview-only", Local: true, Hosting: true, Code: "A7C2-195E-20F8-93B1", Pin: strings.Repeat("a7c2", 16), JoinCode: "NEARBY-CHAT", Addresses: []string{"https://192.168.1.42:8000"}},
	}
	// Exercise real reading/navigation states as well as empty forms.
	hosting := pages[0]
	hosting.Hosting = true
	pages = append(pages, hosting)
	older := pages[1]
	older.Older, older.More, older.Before = true, true, 13
	older.Messages = []Message{{ID: 24, Display: "Jamie", Body: "An earlier private message. The next page should remain easy to read and navigate.", Created: timestamp()}}
	pages = append(pages, older)
	for _, locked := range []bool{false, true} {
		pages = append(pages, Page{Title: "Neighbour Chat", View: "privacy", Local: true, VaultEnabled: true, VaultLocked: locked, CSRF: "preview-only"})
	}
	for _, view := range []string{"join", "confirm", "login", "register", "files", "people", "search", "privacy", "uninstall", "uninstalling", "offline"} {
		pages = append(pages, Page{Title: "Neighbour Chat", View: view, CSRF: "preview-only", Local: true, Authenticated: view == "files" || view == "people" || view == "search", Admin: true, Address: "https://192.168.1.42:8000", UploadLimit: 25, Code: "A7C2-195E-20F8-93B1", Pin: strings.Repeat("a7c2", 16), Users: []User{{ID: 2, Display: strings.Repeat("LongName", 5), Username: "jamie"}}})
	}
	for _, tab := range []string{"requests", "accounts", "security", "storage", "history"} {
		pages = append(pages, Page{Title: "Neighbour Chat", View: "admin", AdminTab: tab, CSRF: "preview-only", Local: true, Authenticated: true, Admin: true, Counts: map[string]int64{"files": 0, "messages": 0}, UploadLimit: 25, Users: []User{{ID: 2, Display: strings.Repeat("LongName", 5), Username: "jamie", Status: "approved"}}, Pending: []User{{ID: 3, Display: "New neighbour", Username: "new-user", Status: "pending"}}})
	}
	for _, p := range pages {
		var output bytes.Buffer
		if e = app.templates.ExecuteTemplate(&output, "layout", p); e != nil {
			t.Fatal(e)
		}
		html := strings.ReplaceAll(output.String(), "href=\"/app.css\"", "href=\"app.css\"")
		html = strings.ReplaceAll(html, "src=\"/app.js\"", "src=\"app.js\"")
		name := p.View
		if p.View == "home" && p.Hosting {
			name += "-hosting"
		}
		if p.Older {
			name += "-older"
		}
		if p.View == "privacy" && p.VaultEnabled {
			name += "-enabled"
			if p.VaultLocked {
				name += "-locked"
			}
		}
		if p.AdminTab != "" {
			name += "-" + p.AdminTab
		}
		if e = os.WriteFile(filepath.Join(directory, name+".html"), []byte(html), 0644); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"app.css", "app.js"} {
		b, _ := assets.ReadFile("ui/" + name)
		if e = os.WriteFile(filepath.Join(directory, name), b, 0644); e != nil {
			t.Fatal(e)
		}
	}
}
