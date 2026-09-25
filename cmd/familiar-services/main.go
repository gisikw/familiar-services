// Command familiar-services provides Familiar's singleton services.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gisikw/familiar-services/internal/api"
	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/continuity"
	"github.com/gisikw/familiar-services/internal/scheduler"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: familiar-services serve --socket PATH --attention-db PATH --state-dir PATH [--default-target ID]")
	fmt.Fprintln(os.Stderr, "       familiar-services migrate --state-dir PATH --default-target ID")
	fmt.Fprintln(os.Stderr, "       familiar-services continuity <import|stats> [options]")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "familiar-services:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "serve" {
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		socket := fs.String("socket", "", "Unix socket path")
		attentionDB := fs.String("attention-db", "", "Attention SQLite path")
		stateDir := fs.String("state-dir", "", "Familiar state directory")
		defaultTarget := fs.String("default-target", "", "default instance target")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *socket == "" || *attentionDB == "" || *stateDir == "" {
			return errors.New("socket, attention-db, and state-dir are required")
		}
		attn, err := attention.Open(*attentionDB)
		if err != nil {
			return err
		}
		defer attn.Close()
		sched, err := scheduler.Open(*stateDir)
		if err != nil {
			return err
		}
		defer sched.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return api.New(*socket, api.Services{Attention: attn, Scheduler: sched, DefaultTarget: *defaultTarget}).Serve(ctx)
	}
	if len(args) > 0 && args[0] == "migrate" {
		fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
		stateDir := fs.String("state-dir", "", "Familiar state directory")
		defaultTarget := fs.String("default-target", "", "instance receiving unaddressed imported events")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *stateDir == "" || *defaultTarget == "" {
			return errors.New("state-dir and default-target are required")
		}
		sched, err := scheduler.Open(*stateDir)
		if err != nil {
			return err
		}
		defer sched.Close()
		n, err := sched.Migrate(*stateDir, *defaultTarget)
		if err == nil {
			fmt.Printf("imported %d events\n", n)
		}
		return err
	}
	if len(args) < 2 || args[0] != "continuity" {
		usage()
		return errors.New("unknown command")
	}
	switch args[1] {
	case "import":
		fs := flag.NewFlagSet("continuity import", flag.ContinueOnError)
		var sessions []string
		fs.Func("sessions", "Pi sessions directory (repeatable)", func(path string) error {
			sessions = append(sessions, path)
			return nil
		})
		handoffs := fs.String("handoffs", "", "handoff Markdown directory")
		dbPath := fs.String("db", "", "SQLite index path")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		return continuity.Import(continuity.ImportOptions{SessionsDirs: sessions, HandoffsDir: *handoffs, DBPath: *dbPath})
	case "stats":
		fs := flag.NewFlagSet("continuity stats", flag.ContinueOnError)
		dbPath := fs.String("db", "", "SQLite index path")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *dbPath == "" {
			return errors.New("db path is required")
		}
		db, err := continuity.Open(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		s, err := continuity.ReadStats(db)
		if err != nil {
			return err
		}
		fmt.Print(continuity.FormatStats(s))
		return nil
	default:
		usage()
		return errors.New("unknown continuity command")
	}
}
