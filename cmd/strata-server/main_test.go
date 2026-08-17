package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil, os.Stderr)
	if err != nil {
		t.Fatalf("parseFlags(nil) = %v, want nil", err)
	}

	// Port 6380 keeps a real Redis on 6379 usable during development; a
	// regression here would silently collide with it.
	if cfg.addr != ":6380" {
		t.Errorf("addr = %q, want \":6380\"", cfg.addr)
	}
	if cfg.syncPolicy != "interval" {
		t.Errorf("syncPolicy = %q, want \"interval\"", cfg.syncPolicy)
	}
	if cfg.memtableMB != 4 {
		t.Errorf("memtableMB = %d, want 4", cfg.memtableMB)
	}
	if cfg.cacheMB != 64 {
		t.Errorf("cacheMB = %d, want 64", cfg.cacheMB)
	}
	if cfg.pprofAddr != "" {
		t.Errorf("pprofAddr = %q, want it disabled by default", cfg.pprofAddr)
	}
}

func TestParseFlagsRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown sync policy", []string{"-sync", "sometimes"}, "invalid -sync"},
		{"zero memtable", []string{"-memtable-mb", "0"}, "invalid -memtable-mb"},
		{"negative memtable", []string{"-memtable-mb", "-1"}, "invalid -memtable-mb"},
		{"negative cache", []string{"-cache-mb", "-1"}, "invalid -cache-mb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFlags(tt.args, os.Stderr)
			if err == nil {
				t.Fatalf("parseFlags(%q) succeeded, want error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseFlagsAcceptsEverySyncPolicy(t *testing.T) {
	for _, policy := range []string{"always", "interval", "never"} {
		cfg, err := parseFlags([]string{"-sync", policy}, os.Stderr)
		if err != nil {
			t.Errorf("parseFlags(-sync %s) = %v, want nil", policy, err)
			continue
		}
		if cfg.syncPolicy != policy {
			t.Errorf("syncPolicy = %q, want %q", cfg.syncPolicy, policy)
		}
	}
}

// A zero cache is a legitimate configuration for measuring uncached read cost
// in Phase 5, so it must not be rejected alongside negative values.
func TestParseFlagsAllowsZeroCache(t *testing.T) {
	if _, err := parseFlags([]string{"-cache-mb", "0"}, os.Stderr); err != nil {
		t.Errorf("parseFlags(-cache-mb 0) = %v, want nil", err)
	}
}
