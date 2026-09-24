// Command familiar-services provides the M0 read-only continuity mirror.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/gisikw/familiar-services/internal/continuity"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: familiar-services continuity <import|stats> [options]")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "familiar-services:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
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
