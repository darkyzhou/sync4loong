package logger

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-logfmt/logfmt"
)

type Logger struct {
	encoder *logfmt.Encoder
	output  io.Writer
	mu      sync.Mutex
}

func New(output io.Writer) *Logger {
	if output == nil {
		output = os.Stdout
	}
	return &Logger{
		encoder: logfmt.NewEncoder(output),
		output:  output,
	}
}

func NewDefault() *Logger {
	return New(os.Stdout)
}

func (l *Logger) log(level string, msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	_ = l.encoder.EncodeKeyval("time", time.Now().Format(time.RFC3339))
	_ = l.encoder.EncodeKeyval("level", level)
	_ = l.encoder.EncodeKeyval("msg", msg)

	for k, v := range fields {
		_ = l.encoder.EncodeKeyval(k, v)
	}

	_ = l.encoder.EndRecord()
}

func (l *Logger) Info(msg string, fields map[string]any) {
	l.log("info", msg, fields)
}

func (l *Logger) Error(msg string, err error, fields map[string]any) {
	if fields == nil {
		fields = make(map[string]any)
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	l.log("error", msg, fields)
}

func (l *Logger) Warn(msg string, fields map[string]any) {
	l.log("warn", msg, fields)
}

func (l *Logger) Fatal(msg string, fields map[string]any) {
	l.log("fatal", msg, fields)
	os.Exit(1)
}

var defaultLogger = NewDefault()

func Info(msg string, fields map[string]any) {
	defaultLogger.Info(msg, fields)
}

func Error(msg string, err error, fields map[string]any) {
	defaultLogger.Error(msg, err, fields)
}

func Warn(msg string, fields map[string]any) {
	defaultLogger.Warn(msg, fields)
}

func Fatal(msg string, fields map[string]any) {
	defaultLogger.Fatal(msg, fields)
}

func Printf(format string, args ...any) {
	defaultLogger.Info(fmt.Sprintf(format, args...), nil)
}

func Fatalf(format string, args ...any) {
	defaultLogger.Fatal(fmt.Sprintf(format, args...), nil)
}
