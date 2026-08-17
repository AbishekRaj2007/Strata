package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchErrors(t *testing.T) {
	dir := t.TempDir()
	sst := filepath.Join(dir, "000002.sst")
	if err := os.WriteFile(sst, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	notTable := filepath.Join(dir, "000002.log")
	if err := os.WriteFile(notTable, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no arguments", nil, "no subcommand"},
		{"unknown subcommand", []string{"frobnicate"}, "unknown subcommand"},
		{"dump without path", []string{"dump"}, "want exactly one"},
		{"dump too many paths", []string{"dump", sst, sst}, "want exactly one"},
		{"dump missing file", []string{"dump", filepath.Join(dir, "absent.sst")}, "no such file"},
		{"dump rejects directory", []string{"dump", dir}, "is a directory"},
		{"dump rejects wrong extension", []string{"dump", notTable}, "want .sst"},
		{"levels rejects a file", []string{"levels", sst}, "not a directory"},
		{"validate missing directory", []string{"validate", filepath.Join(dir, "absent")}, "no such file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dispatch(io.Discard, tt.args)
			if err == nil {
				t.Fatalf("dispatch(%q) succeeded, want error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A valid invocation of an unfinished subcommand must report cleanly and name
// the task that implements it, never panic. This is the T0.4 "Done when".
func TestUnimplementedCommandsReportCleanly(t *testing.T) {
	dir := t.TempDir()
	sst := filepath.Join(dir, "000002.sst")
	if err := os.WriteFile(sst, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "MANIFEST-000001")
	if err := os.WriteFile(manifest, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{"dump", []string{"dump", sst}},
		{"manifest", []string{"manifest", manifest}},
		{"levels", []string{"levels", dir}},
		{"validate", []string{"validate", dir}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dispatch(io.Discard, tt.args)
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("error = %v, want it to wrap errNotImplemented", err)
			}
			if !strings.Contains(err.Error(), "plan.md T") {
				t.Errorf("error %q does not name the implementing task", err)
			}
		})
	}
}

func TestHelpSucceeds(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var sb strings.Builder
		if err := dispatch(&sb, []string{arg}); err != nil {
			t.Errorf("dispatch(%q) = %v, want nil", arg, err)
		}
		if !strings.Contains(sb.String(), "usage:") {
			t.Errorf("dispatch(%q) printed no usage", arg)
		}
	}
}
