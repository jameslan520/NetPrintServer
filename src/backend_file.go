package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileBackend 把作业写成 .prn 文件。
// 用于 Linux 下的端到端测试，也用于 Windows 上先“干跑”验证链路。
type FileBackend struct {
	dir      string
	printers []string
	log      *Logger
	mu       sync.Mutex
	seq      uint64
}

func NewFileBackend(cfg *Config, printers []string, log *Logger) *FileBackend {
	if len(printers) == 0 {
		printers = []string{"DEFAULT"}
	}
	return &FileBackend{dir: cfg.FileOutputDir, printers: printers, log: log}
}

func (f *FileBackend) Kind() string { return "file" }

func (f *FileBackend) ListPrinters() ([]Printer, error) {
	out := make([]Printer, 0, len(f.printers))
	for _, p := range f.printers {
		out = append(out, Printer{
			Name:   p,
			Port:   "本地队列",
			Driver: "文件输出（测试）",
			State:  "就绪",
			Detail: "输出目录: " + f.dir,
		})
	}
	return out, nil
}

func (f *FileBackend) HasPrinter(name string) bool {
	for _, p := range f.printers {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

func (f *FileBackend) StatusOf(printer string) string {
	if f.HasPrinter(printer) {
		return "就绪（文件输出）"
	}
	return "打印机不存在"
}

func (f *FileBackend) Refresh() {}

func (f *FileBackend) OpenJob(printer string, meta JobMeta) (JobHandle, error) {
	if !f.HasPrinter(printer) {
		return nil, fmt.Errorf("打印机不存在: %s", printer)
	}
	dir := filepath.Join(f.dir, safeName(printer))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()
	ts := time.Now().Format("20060102_150405")
	final := filepath.Join(dir, fmt.Sprintf("%s_%04d_%s.prn", ts, seq, safeName(meta.Doc)))
	tmp := final + ".part"
	file, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("创建输出文件失败: %w", err)
	}
	return &fileHandle{file: file, tmp: tmp, final: final}, nil
}

type fileHandle struct {
	file  *os.File
	tmp   string
	final string
	done  bool
}

func (h *fileHandle) JobID() uint32 { return 0 }

func (h *fileHandle) Write(p []byte) (int, error) {
	return h.file.Write(p)
}

func (h *fileHandle) Commit() error {
	if h.done {
		return nil
	}
	h.done = true
	if err := h.file.Close(); err != nil {
		return err
	}
	return os.Rename(h.tmp, h.final)
}

func (h *fileHandle) Abort() {
	if h.done {
		return
	}
	h.done = true
	_ = h.file.Close()
	_ = os.Remove(h.tmp)
}
