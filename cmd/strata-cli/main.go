// Command strata-cli inspects Strata data directories from the outside.
//
// The subcommands here are the tools used to debug the engine for the rest of
// the project: dumping a table, replaying the manifest as a version history,
// printing the level layout, and checking invariants on a data directory.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// errNotImplemented marks a subcommand whose plumbing exists but whose engine
// support arrives in a later phase. Callers report it as a clean error rather
// than a panic or a silent success.
var errNotImplemented = errors.New("not implemented yet")

type command struct {
	name    string
	summary string
	// phase names the plan.md task that fills this command in.
	phase string
	run   func(stdout io.Writer, args []string) error
}

func commands() []command {
	return []command{
		{
			name:    "dump",
			summary: "print the contents of an SSTable in readable form",
			phase:   "T3.4",
			run:     runDump,
		},
		{
			name:    "manifest",
			summary: "replay the manifest as a version history",
			phase:   "T4.1",
			run:     runManifest,
		},
		{
			name:    "levels",
			summary: "print the current level layout of a data directory",
			phase:   "T4.1",
			run:     runLevels,
		},
		{
			name:    "validate",
			summary: "check all invariants on a data directory",
			phase:   "T6.5",
			run:     runValidate,
		},
	}
}

func main() {
	if err := dispatch(os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "strata-cli: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(stdout io.Writer, args []string) error {
	if len(args) == 0 {
		if err := usage(stdout); err != nil {
			return err
		}
		return errors.New("no subcommand given")
	}

	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		return usage(stdout)
	}

	for _, c := range commands() {
		if c.name == name {
			if err := c.run(stdout, args[1:]); err != nil {
				if errors.Is(err, errNotImplemented) {
					return fmt.Errorf("%s: %w (arrives in plan.md %s)", c.name, err, c.phase)
				}
				return fmt.Errorf("%s: %w", c.name, err)
			}
			return nil
		}
	}

	if err := usage(stdout); err != nil {
		return err
	}
	return fmt.Errorf("unknown subcommand %q", name)
}

func usage(w io.Writer) error {
	var sb strings.Builder
	sb.WriteString("usage: strata-cli <command> [arguments]\n\ncommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(&sb, "  %-10s %s\n", c.name, c.summary)
	}

	_, err := io.WriteString(w, sb.String())
	return err
}

// requireExistingPath resolves a single path argument, so every subcommand
// reports a missing file the same way rather than each inventing its own error.
func requireExistingPath(args []string, what string) (string, error) {
	fs := flag.NewFlagSet(what, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return "", err
	}

	if fs.NArg() != 1 {
		return "", fmt.Errorf("want exactly one %s path, got %d arguments", what, fs.NArg())
	}

	path := fs.Arg(0)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	switch what {
	case "data directory":
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", path)
		}
	default:
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory, want a file", path)
		}
	}
	return path, nil
}

func runDump(stdout io.Writer, args []string) error {
	path, err := requireExistingPath(args, "sstable")
	if err != nil {
		return err
	}
	if ext := filepath.Ext(path); ext != ".sst" {
		return fmt.Errorf("%s has extension %q, want .sst", path, ext)
	}
	return errNotImplemented
}

func runManifest(stdout io.Writer, args []string) error {
	if _, err := requireExistingPath(args, "manifest"); err != nil {
		return err
	}
	return errNotImplemented
}

func runLevels(stdout io.Writer, args []string) error {
	dir, err := requireExistingPath(args, "data directory")
	if err != nil {
		return err
	}

	vs, err := manifest.Recover(nil, dir)
	if err != nil {
		return err
	}
	v := vs.Current()

	levels := compaction.Summarise(v)
	if len(levels) == 0 {
		_, err := fmt.Fprintln(stdout, "the tree is empty")
		return err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%-6s %6s %12s\n", "level", "files", "bytes")
	for _, l := range levels {
		fmt.Fprintf(&sb, "L%-5d %6d %12d\n", l.Level, l.Files, l.Bytes)
	}
	fmt.Fprintf(&sb, "\nlast sequence: %d\n", v.LastSequence())

	_, err = io.WriteString(stdout, sb.String())
	return err
}

// runValidate is the outside-the-process half of T6.5. The same checker runs
// after every compaction in verifying builds; this is how an operator asks
// the question of a directory nothing is currently running against.
func runValidate(stdout io.Writer, args []string) error {
	dir, err := requireExistingPath(args, "data directory")
	if err != nil {
		return err
	}

	report, err := compaction.CheckDir(dir)
	if err != nil {
		return err
	}

	if report.OK() {
		_, err := fmt.Fprintf(stdout, "ok: %d files, %d entries, every invariant holds\n",
			report.FilesChecked, report.EntriesChecked)
		return err
	}

	// Every violation is printed, not just the first. They are often
	// symptoms of one cause, and seeing the set is what identifies it.
	var sb strings.Builder
	fmt.Fprintf(&sb, "FAILED: %d violations across %d files\n", len(report.Violations), report.FilesChecked)
	for _, v := range report.Violations {
		fmt.Fprintf(&sb, "  %s\n", v)
	}
	if _, err := io.WriteString(stdout, sb.String()); err != nil {
		return err
	}
	return fmt.Errorf("%d invariant violations", len(report.Violations))
}
