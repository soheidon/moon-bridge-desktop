package logger

import "log/slog"

// L returns the default slog.Logger.
func L() *slog.Logger {
	return slog.Default()
}

// Debug logs at debug level via slog.Default().
func Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}

// Info logs at info level via slog.Default().
func Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

// Warn logs at warn level via slog.Default().
func Warn(msg string, args ...any) {
	slog.Warn(msg, args...)
}

// Error logs at error level via slog.Default().
func Error(msg string, args ...any) {
	slog.Error(msg, args...)
}
