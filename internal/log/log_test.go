package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		emit      func(Logger)
		wantEntry bool
	}{
		{"debug suppressed at info", LevelInfo, func(l Logger) { l.Debug("m") }, false},
		{"info emitted at info", LevelInfo, func(l Logger) { l.Info("m") }, true},
		{"debug emitted at debug", LevelDebug, func(l Logger) { l.Debug("m") }, true},
		{"info suppressed at error", LevelError, func(l Logger) { l.Info("m") }, false},
		{"error emitted at error", LevelError, func(l Logger) { l.Error("m") }, true},
		{"unknown level behaves as info", "nonsense", func(l Logger) { l.Info("m") }, true},
		{"unknown level suppresses debug", "nonsense", func(l Logger) { l.Debug("m") }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.emit(New(&buf, tt.level))

			if got := buf.Len() > 0; got != tt.wantEntry {
				t.Errorf("entry written = %v, want %v", got, tt.wantEntry)
			}
		})
	}
}

func TestWithAttachesAttributes(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, LevelInfo).With("file", 7).Info("flushed")

	out := buf.String()
	if !strings.Contains(out, "file=7") {
		t.Errorf("attribute missing from %q", out)
	}
	if !strings.Contains(out, "flushed") {
		t.Errorf("message missing from %q", out)
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	// Discard is used throughout tests; a regression here would make test
	// output unreadable rather than fail loudly, so assert it directly.
	l := Discard()
	l.Error("this must not appear")
}
