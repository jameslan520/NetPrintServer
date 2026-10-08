package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Logger 同时输出到控制台与日志文件。
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	console bool
	prefix  string
}

func NewLogger(path string, console bool) *Logger {
	l := &Logger{console: console}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			l.file = f
		} else if console {
			fmt.Printf("[警告] 无法打开日志文件 %s: %v\n", path, err)
		}
	}
	return l
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

func (l *Logger) output(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s [%s] %s%s\n", time.Now().Format("2006-01-02 15:04:05"), level, l.prefix, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.console {
		os.Stdout.WriteString(line)
	}
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
}

func (l *Logger) Info(format string, args ...interface{})  { l.output("信息", format, args...) }
func (l *Logger) Warn(format string, args ...interface{})  { l.output("警告", format, args...) }
func (l *Logger) Error(format string, args ...interface{}) { l.output("错误", format, args...) }

// Sub 返回带前缀的子日志（用于区分监听器）。
func (l *Logger) Sub(prefix string) *Logger {
	return &Logger{file: l.file, console: l.console, prefix: prefix}
}
