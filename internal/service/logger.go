package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// levelSeverity maps a log level name to its numeric severity. Messages below
// the logger's configured threshold are dropped. Unknown levels are treated as
// the most verbose so they are never accidentally suppressed.
var levelSeverity = map[string]int{
	"DEBUG": 0,
	"INFO":  1,
	"WARN":  2,
	"ERROR": 3,
}

// severityOf returns the numeric severity for a level name (case-insensitive),
// defaulting to DEBUG (0) for unknown names.
func severityOf(level string) int {
	if s, ok := levelSeverity[strings.ToUpper(level)]; ok {
		return s
	}
	return 0
}

// Logger provides file-based logging with automatic rotation. Every log line
// is also mirrored to os.Stdout for console visibility.
type Logger struct {
	file       *os.File
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	minLevel   int // messages below this severity are dropped
}

// NewLogger creates or opens the log file at the given path. Parent
// directories are created automatically if they do not exist.
func NewLogger(path string) (*Logger, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("service: create log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) // #nosec G304 -- path is the app's configured log file location
	if err != nil {
		return nil, fmt.Errorf("service: open log file: %w", err)
	}

	return &Logger{
		file:       f,
		path:       path,
		maxSize:    defaultMaxSize,
		maxBackups: defaultMaxBackups,
		minLevel:   severityOf("INFO"),
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

	// Drop messages below the configured minimum level.
	if severityOf(level) < l.minLevel {
		return
	}

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

	// Mirror to stderr (diagnostic output belongs on stderr, not stdout). The
	// "%s" format keeps any '%' in the message from being interpreted.
	fmt.Fprintf(os.Stderr, "%s", line)
}

// ---------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------

// rotate closes the current log file, drops the oldest backup, shifts the
// remaining backups up by one (path.(N-1) → path.N, …, path.1 → path.2,
// path → path.1), and opens a fresh log file.
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

	if l.maxBackups < 1 {
		// No backups kept — discard the current log entirely.
		_ = os.Remove(l.path)
	} else {
		// Drop the oldest backup, then shift every remaining backup up by one
		// (highest first so nothing is overwritten before it has moved), and
		// finally move the current log to path.1.
		_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.maxBackups))
		for i := l.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(
				fmt.Sprintf("%s.%d", l.path, i),
				fmt.Sprintf("%s.%d", l.path, i+1),
			)
		}
		_ = os.Rename(l.path, fmt.Sprintf("%s.1", l.path))
	}

	// Open a fresh log file.
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
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

// SetLevel sets the minimum log level (one of "debug", "info", "warn",
// "error"; case-insensitive). Messages below this level are dropped. An
// unrecognized level enables the most verbose output.
func (l *Logger) SetLevel(level string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.minLevel = severityOf(level)
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
