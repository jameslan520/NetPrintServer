package main

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

func isWindowsOS() bool { return runtime.GOOS == "windows" }

// Printer 本机打印机（或模拟打印机）状态
type Printer struct {
	Name   string `json:"name"`
	Port   string `json:"port"`
	Driver string `json:"driver"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Jobs   int    `json:"jobs"`
}

// JobMeta 一次打印作业的元信息
type JobMeta struct {
	Doc    string
	User   string
	Source string
	Queue  string
}

// JobHandle 一次打开的打印作业（流式写入）
type JobHandle interface {
	JobID() uint32
	Write(p []byte) (int, error)
	Commit() error
	Abort()
}

// Backend 打印后端：windows(winspool) / file(输出 .prn 文件)
type Backend interface {
	Kind() string
	ListPrinters() ([]Printer, error)
	HasPrinter(name string) bool
	StatusOf(printer string) string
	OpenJob(printer string, meta JobMeta) (JobHandle, error)
	Refresh() // 周期刷新队列状态（可空实现）
}

// NewBackend 根据配置选择后端。windows 平台的实现见 winspool_windows.go。
func NewBackend(cfg *Config, tracker *JobTracker, log *Logger) Backend {
	kind := cfg.Backend
	switch kind {
	case "", "auto":
		if isWindowsOS() {
			kind = "windows"
		} else {
			kind = "file"
		}
	}
	if kind == "windows" {
		if !isWindowsOS() {
			log.Warn("配置要求 windows 后端，但当前系统不支持，改用 file 后端")
			kind = "file"
		} else {
			return NewWinspoolBackend(cfg, tracker, log)
		}
	}
	return NewFileBackend(cfg, cfg.FilePrinters, log)
}

// finishJob 统一收尾：更新作业记录状态
func finishJob(t *JobTracker, b Backend, rec *JobRecord, n int64, winID uint32, err error) {
	if rec == nil {
		return
	}
	if err != nil {
		t.Finish(rec, n, err)
		return
	}
	if b.Kind() == "file" {
		t.Finish(rec, n, nil)
		t.MarkPrinted(rec, "已保存为 .prn 文件")
		return
	}
	if winID > 0 {
		t.SetWinJob(rec, winID)
	}
	t.Finish(rec, n, nil)
}

var unsafeFileRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// safeName 清理用作目录/文件名的字符串
func safeName(s string) string {
	s = strings.TrimSpace(s)
	s = unsafeFileRe.ReplaceAllString(s, "_")
	s = strings.ReplaceAll(s, " ", "_")
	if s == "" || s == "." || s == ".." {
		s = "printer"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// hostPart 从 "ip:port" 取出 IP
func hostPart(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// ---- 打印机筛选（避免把「打印到 PDF / XPS / 传真 / 虚拟绘图仪」也暴露成网络端口）----

type printerFilter struct {
	includeKeywords     []string
	excludeKeywords     []string
	excludePortPrefixes []string
	disabled            bool // keep_virtual_printers = true 时不过滤
}

func newPrinterFilter(cfg *Config) printerFilter {
	return printerFilter{
		includeKeywords:     cfg.IncludeKeywords,
		excludeKeywords:     cfg.ExcludeKeywords,
		excludePortPrefixes: cfg.ExcludePortPrefixes,
		disabled:            cfg.KeepVirtualPrinters,
	}
}

// matchAny 返回第一个命中的关键字
func matchAny(haystacks []string, keywords []string) (string, bool) {
	for _, h := range haystacks {
		hu := strings.ToUpper(h)
		for _, kw := range keywords {
			k := strings.ToUpper(strings.TrimSpace(kw))
			if k != "" && strings.Contains(hu, k) {
				return kw, true
			}
		}
	}
	return "", false
}

// keep 判定该打印机是否参与自动分配端口
func (f printerFilter) keep(p Printer) bool {
	if f.disabled {
		return true
	}
	if len(f.includeKeywords) > 0 {
		if _, ok := matchAny([]string{p.Name, p.Driver}, f.includeKeywords); !ok {
			return false
		}
	}
	return !f.excluded(p.Name, p.Driver, p.Port)
}

// keepOfflineByName 打印机当前未枚举到（离线）时只能按名字判断
func (f printerFilter) keepOfflineByName(name string) bool {
	if f.disabled {
		return true
	}
	if len(f.includeKeywords) > 0 {
		if _, ok := matchAny([]string{name}, f.includeKeywords); !ok {
			return false
		}
	}
	_, bad := matchAny([]string{name}, f.excludeKeywords)
	return !bad
}

func (f printerFilter) excluded(name, driver, port string) bool {
	pu := strings.ToUpper(strings.TrimSpace(port))
	for _, pref := range f.excludePortPrefixes {
		p := strings.ToUpper(strings.TrimSpace(pref))
		if p != "" && strings.HasPrefix(pu, p) {
			return true
		}
	}
	_, bad := matchAny([]string{name, driver}, f.excludeKeywords)
	return bad
}

// reason 给出“这台打印机为什么被排除”的中文说明（给状态页/配置页显示）
func (f printerFilter) reason(p Printer) string {
	if f.disabled {
		return ""
	}
	pu := strings.ToUpper(strings.TrimSpace(p.Port))
	for _, pref := range f.excludePortPrefixes {
		pre := strings.ToUpper(strings.TrimSpace(pref))
		if pre != "" && strings.HasPrefix(pu, pre) {
			return fmt.Sprintf("端口 %s 属于虚拟端口（%s）", p.Port, pref)
		}
	}
	if kw, ok := matchAny([]string{p.Name}, f.excludeKeywords); ok {
		return fmt.Sprintf("名称命中排除关键字「%s」", kw)
	}
	if kw, ok := matchAny([]string{p.Driver}, f.excludeKeywords); ok {
		return fmt.Sprintf("驱动命中排除关键字「%s」", kw)
	}
	if len(f.includeKeywords) > 0 {
		if _, ok := matchAny([]string{p.Name, p.Driver}, f.includeKeywords); !ok {
			return "不在 include_keywords 白名单内"
		}
	}
	return "被排除规则过滤"
}

// skipReason 按选择模式生成跳过原因
func skipReason(mode string, p Printer, f printerFilter) string {
	switch mode {
	case "selected":
		return "未勾选（网页配置页里勾选即可转换）"
	case "all":
		return "被规则排除"
	default:
		return f.reason(p)
	}
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
