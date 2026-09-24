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
	"github.com/gisikw/familiar-services/internal/wakes"
	"github.com/gisikw/familiar-services/internal/worklist"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: familiar-services serve --socket PATH --attention-db PATH --state-dir PATH")
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
		work, err := worklist.Open(*stateDir)
		if err != nil {
			return err
		}
		wake, err := wakes.Open(*stateDir, work)
		if err != nil {
			return err
		}
		defer wake.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return api.New(*socket, api.Services{Attention: attn, Worklist: work, Wakes: wake}).Serve(ctx)
	}
	if len(args) < 2 || args[0] != "continuity" {
		usage()
		return errors.New("unknown command")
	}
	switch args[1] {
	case "import":
		fs := flag.NewFlagSet("continuity import", flag.ContinueOnError)
		sessions := fs.String("sessions", "", "Pi sessions directory")
		handoffs := fs.String("handoffs", "", "handoff Markdown directory")
		dbPath := fs.String("db", "", "SQLite index path")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		return continuity.Import(continuity.ImportOptions{SessionsDir: *sessions, HandoffsDir: *handoffs, DBPath: *dbPath})
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
