package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
)

//go:embed ui/* illusion/*
var assets embed.FS

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "Neighbour Chat:", e)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("use serve, prepare, health, uninstall, or version")
	}
	if args[0] == "version" {
		fmt.Println("Neighbour Chat 7.2 · KOReader service · Linux ARMv7")
		return nil
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	root := flags.String("root", "/mnt/us/koreader/plugins/neighbourchat.koplugin", "KOReader plugin folder")
	port := flags.String("local-port", "8001", "local controller port")
	screen := flags.String("screen", "home", "home or uninstall")
	delay := flags.Int("delay", 0, "uninstall delay in seconds")
	source := flags.String("source", "", "extracted installer package")
	removeFiles := flags.Bool("remove-files", false, "remove app files while preserving its plugin-local data")
	if e := flags.Parse(args[1:]); e != nil {
		return e
	}
	switch args[0] {
	case "install":
		if *source == "" {
			return errors.New("installer source is required")
		}
		return installBundle(*source, *root, "/var/local/appreg.db", "/var/local/mesquite/neighbour-chat")
	case "prepare":
		if *screen != "home" && *screen != "uninstall" {
			return errors.New("invalid app screen")
		}
		if e := prepare(*root, "/var/local/mesquite/neighbour-chat", "/var/local/appreg.db"); e != nil {
			return e
		}
		if *screen == "uninstall" {
			path := filepath.Join(*root, "data", "launch-ticket.json")
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			var ticket LaunchTicket
			if e = json.Unmarshal(b, &ticket); e != nil {
				return e
			}
			ticket.Next = "/uninstall"
			b, _ = json.Marshal(ticket)
			return atomicWrite(path, b, 0600)
		}
		return nil
	case "uninstall":
		release, e := lockMaintenance(*root)
		if e != nil {
			return e
		}
		defer release()
		if *delay < 0 || *delay > 5 {
			return errors.New("invalid uninstall delay")
		}
		time.Sleep(time.Duration(*delay) * time.Second)
		if e := uninstall(*root, "/var/local/mesquite/neighbour-chat", "/var/local/appreg.db"); e != nil {
			return e
		}
		if *removeFiles {
			return removeInstalledFiles(*root)
		}
		return nil
	case "health":
		client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, e := client.Get("http://127.0.0.1:" + *port + "/_health")
		if e != nil {
			return errors.New("controller is not running")
		}
		defer response.Body.Close()
		var v struct {
			Service string `json:"service"`
		}
		if json.NewDecoder(response.Body).Decode(&v) != nil || v.Service != "neighbour-chat-v3" {
			return errors.New("another service is using the local port")
		}
		return nil
	case "serve":
		debug.SetMemoryLimit(96 << 20)
		store, e := openStore(*root)
		if e != nil {
			return e
		}
		defer store.DB.Close()
		app, e := newApp(store)
		if e != nil {
			return e
		}
		defer app.stopHost()
		app.localPort = *port
		app.shutdown = make(chan struct{})
		app.reader, e = newReaderBridge(app)
		if e != nil {
			return e
		}
		defer app.reader.close()
		listener, e := net.Listen("tcp", "127.0.0.1:"+*port)
		if e != nil {
			return errors.New("the local app port is busy; stop the old app or restart the Kindle before upgrading")
		}
		pidPath := filepath.Join(store.Data, "server.pid")
		if e = atomicWrite(pidPath, []byte(fmt.Sprint(os.Getpid())), 0600); e != nil {
			listener.Close()
			return e
		}
		defer os.Remove(pidPath)
		listener = limitConnections(listener, 16)
		server := serverFor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.web(w, r, true) }))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		finished := make(chan error, 1)
		go func() { finished <- server.Serve(listener) }()
		select {
		case e = <-finished:
			if errors.Is(e, http.ErrServerClosed) {
				return nil
			}
			return e
		case <-ctx.Done():
		case <-app.shutdown:
		}
		app.reader.cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		app.stopHost()
		return nil
	default:
		return errors.New("unknown command")
	}
}
