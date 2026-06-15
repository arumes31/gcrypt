package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Log entry
// ---------------------------------------------------------------------------

// LogEntry represents a single structured log record.
type LogEntry struct {
	Time    time.Time              `json:"time"`
	Level   string                 `json:"level"`
	Message string                 `json:"message"`
	Fields  map[string]interface{} `json:"fields,omitempty"`
}

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

// Default rotation settings.
const (
	defaultMaxSize    = 10 * 1024 * 1024 // 10 MiB
	defaultMaxBackups = 3
)

// Logger provides file-based logging with automatic rotation. Every log line
// is also mirrored to os.Stdout for console visibility.
type Logger struct {
	file       *os.File
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
}

// NewLogger creates or opens the log file at the given path. Parent
// directories are created automatically if they do not exist.
func NewLogger(path string) (*Logger, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("service: create log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("service: open log file: %w", err)
	}

	return &Logger{
		file:       f,
		path:       path,
		maxSize:    defaultMaxSize,
		maxBackups: defaultMaxBackups,
	}, nil
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return fmt.Errorf("service: close log file: %w", err)
		}
		l.file = nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// Public level helpers
// ---------------------------------------------------------------------------

// Debug logs a message at DEBUG level.
func (l *Logger) Debug(msg string, fields ...map[string]interface{}) {
	l.log("DEBUG", msg, fields...)
}

// Info logs a message at INFO level.
func (l *Logger) Info(msg string, fields ...map[string]interface{}) {
	l.log("INFO", msg, fields...)
}

// Warn logs a message at WARN level.
func (l *Logger) Warn(msg string, fields ...map[string]interface{}) {
	l.log("WARN", msg, fields...)
}

// Error logs a message at ERROR level.
func (l *Logger) Error(msg string, fields ...map[string]interface{}) {
	l.log("ERROR", msg, fields...)
}

// ---------------------------------------------------------------------------
// Core log function
// ---------------------------------------------------------------------------

// log writes a formatted log line to both the log file and stdout. The file
// is rotated beforehand if it exceeds maxSize.
func (l *Logger) log(level string, msg string, fields ...map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Rotate if the file has grown beyond the size limit.
	if l.file != nil {
		if info, err := l.file.Stat(); err == nil && info.Size() >= l.maxSize {
			_ = l.rotate()
		}
	}

	entry := LogEntry{
		Time:    time.Now().UTC(),
		Level:   level,
		Message: msg,
	}

	if len(fields) > 0 && fields[0] != nil {
		entry.Fields = fields[0]
	}

	// Human-readable line format:
	// [2024-01-15T10:30:00Z] [INFO] message {"key":"value"}
	var fieldsJSON string
	if entry.Fields != nil {
		b, err := json.Marshal(entry.Fields)
		if err != nil {
			fieldsJSON = fmt.Sprintf(`{"error":"marshal failed: %v"}`, err)
		} else {
			fieldsJSON = string(b)
		}
	}

	var line string
	if fieldsJSON != "" {
		line = fmt.Sprintf("[%s] [%s] %s %s\n",
			entry.Time.Format(time.RFC3339),
			entry.Level,
			entry.Message,
			fieldsJSON,
		)
	} else {
		line = fmt.Sprintf("[%s] [%s] %s\n",
			entry.Time.Format(time.RFC3339),
			entry.Level,
			entry.Message,
		)
	}

	// Write to file.
	if l.file != nil {
		if _, err := l.file.WriteString(line); err != nil {
			// Best-effort — don't block on file write errors.
			fmt.Fprintf(os.Stderr, "service: log write: %v\n", err)
		}
	}

	// Mirror to stdout.
	fmt.Print(line)
}

// ---------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------

// rotate closes the current log file, shifts existing backup files by one
// (path.1 → path.2, path → path.1), removes the oldest backup if it exceeds
// maxBackups, and opens a fresh log file.
//
// Must be called with l.mu held.
func (l *Logger) rotate() error {
	// Close current file.
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return fmt.Errorf("service: close log for rotation: %w", err)
		}
		l.file = nil
	}

	// Shift backup files: path.N → path.(N+1), starting from the highest.
	for i := l.maxBackups; i >= 1; i-- {
		older := fmt.Sprintf("%s.%d", l.path, i)
		newer := l.path
		if i > 1 {
			newer = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		// Remove the oldest backup if it would exceed maxBackups.
		if i == l.maxBackups {
			_ = os.Remove(older)
			continue
		}
		// Rename newer → older (shift up).
		_ = os.Rename(newer, older)
	}

	// Rename current log → .1
	_ = os.Rename(l.path, fmt.Sprintf("%s.1", l.path))

	// Open a fresh log file.
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("service: open new log file after rotation: %w", err)
	}
	l.file = f

	return nil
}

// ---------------------------------------------------------------------------
// Configuration helpers
// ---------------------------------------------------------------------------

// SetMaxSize sets the maximum file size in bytes before rotation occurs.
func (l *Logger) SetMaxSize(size int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxSize = size
}

// SetMaxBackups sets the maximum number of rotated backup files to keep.
func (l *Logger) SetMaxBackups(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxBackups = n
}

// Path returns the log file path. Useful for opening the file in an editor.
func (l *Logger) Path() string {
	return l.path
}

// Writer returns an io.Writer that writes to the log file at INFO level.
// This is useful for redirecting standard library loggers.
func (l *Logger) Writer() io.Writer {
	return &loggerWriter{l: l}
}

// loggerWriter adapts a Logger to the io.Writer interface at INFO level.
type loggerWriter struct {
	l *Logger
}

func (w *loggerWriter) Write(p []byte) (int, error) {
	w.l.Info(string(p))
	return len(p), nil
}
