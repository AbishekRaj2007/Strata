package log

import (
	"io"
	"log/slog"
	"strings"
)

// Level names accepted on the command line.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Logger is the single interface the engine logs through. Keeping the rest of
// the codebase off slog directly means the backend can change without touching
// call sites.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

type slogLogger struct {
	l *slog.Logger
}

// New returns a Logger writing structured records to w at the given level.
// An unrecognised level name falls back to info rather than failing startup.
func New(w io.Writer, level string) Logger {
	return &slogLogger{l: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (s *slogLogger) Debug(msg string, args ...any) { s.l.Debug(msg, args...) }
func (s *slogLogger) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogLogger) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogLogger) Error(msg string, args ...any) { s.l.Error(msg, args...) }

func (s *slogLogger) With(args ...any) Logger {
	return &slogLogger{l: s.l.With(args...)}
}

// Discard returns a Logger that drops everything, for use in tests.
func Discard() Logger {
	return New(io.Discard, LevelError)
}
