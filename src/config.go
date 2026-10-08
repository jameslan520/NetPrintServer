package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// RawBinding 客户端打印端口与本机打印机的绑定关系
type RawBinding struct {
	Port    int    `json:"port"`              // RAW 监听端口（客户端 TCP/IP 端口指向该端口）
	Queue   string `json:"queue"`             // LPD 队列名（\1 队列名 打印）
	Printer string `json:"printer"`           // 本机 Windows 打印机队列名
	Comment string `json:"comment,omitempty"` // 备注（如端口/驱动信息）
}

type Config struct {
	ServerName     string       `json:"server_name"`
	BindIP         string       `json:"bind_ip"`         // 留空 = 监听所有网卡
	Backend        string       `json:"backend"`         // auto | windows | file
	FileOutputDir  string       `json:"file_output_dir"` // file 后端输出目录
	FilePrinters   []string     `json:"file_printers"`   // file 后端模拟的打印机名
	AutoAssign     bool         `json:"auto_assign"`     // 自动为本机打印机分配端口
	StartPort      int          `json:"start_port"`      // 自动分配起始端口（默认 9100）
	Bindings       []RawBinding `json:"bindings"`        // 手动绑定（优先于自动分配）
	LPDEnabled     bool         `json:"lpd_enabled"`
	LPDPort        int          `json:"lpd_port"`
	WebEnabled     bool         `json:"web_enabled"`
	WebPort        int          `json:"web_port"`
	WebToken       string       `json:"web_token"` // 状态页操作令牌（测试页等）
	Firewall       bool         `json:"firewall"`  // 自动添加 Windows 防火墙入站规则
	MaxJobBytes    int64        `json:"max_job_bytes"`
	IdleTimeoutSec int          `json:"idle_timeout_sec"`
	LogFile        string       `json:"log_file"`
	Version        string       `json:"version"`

	// 打印机筛选：默认跳过 PDF/传真/虚拟打印类，只暴露真实打印机
	IncludeKeywords     []string `json:"include_keywords"`      // 非空时，只有名称/驱动命中这些关键字的打印机才会自动分配端口
	ExcludeKeywords     []string `json:"exclude_keywords"`      // 名称或驱动命中即跳过
	ExcludePortPrefixes []string `json:"exclude_port_prefixes"` // 端口以此开头即跳过（虚拟端口）
	KeepVirtualPrinters bool     `json:"keep_virtual_printers"` // true = 不过滤，虚拟打印机也分配端口

	// 打印机选择：只转换指定的打印机
	// selection_mode: "selected" = 只用 SelectedPrinters（网页配置页勾选）
	//                 "auto"     = 自动：全部真实打印机（按上面的排除规则过滤虚拟打印机）
	//                 "all"      = 全部打印机（含虚拟打印机）
	SelectionMode    string   `json:"selection_mode"`
	SelectedPrinters []string `json:"selected_printers"`

	// 网页配置页登录凭据
	Admin         string `json:"admin_user"`          // 管理员用户名，默认 admin
	AdminPassword string `json:"admin_password_hash"` // salt$sha256(salt+password)，空则首次运行时生成随机口令
}

// Assign 持久化的端口分配（保证重启后端口不漂移，客户端配置不失效）
type Assign struct {
	Port  int    `json:"port"`
	Queue string `json:"queue"`
}

type State struct {
	Assign map[string]Assign `json:"assign"`
}

var (
	stateMu sync.Mutex
)

func defaultConfig() *Config {
	return &Config{
		ServerName:     "NetPrintServer 网络打印服务器",
		BindIP:         "",
		Backend:        "auto",
		FileOutputDir:  "spool",
		FilePrinters:   []string{"LQ630K"},
		AutoAssign:     true,
		StartPort:      9100,
		Bindings:       []RawBinding{},
		LPDEnabled:     true,
		LPDPort:        515,
		WebEnabled:     true,
		WebPort:        8080,
		WebToken:       newToken(),
		Firewall:       true,
		MaxJobBytes:    100 * 1024 * 1024,
		IdleTimeoutSec: 60,
		LogFile:        "netprintserver.log",
		Version:        Version,
		// 默认跳过 PDF/XPS/传真/虚拟绘图仪等软件打印机，只暴露真实打印机
		ExcludeKeywords: []string{
			"PDF", "XPS", "Fax", "传真", "OneNote", "Virtual", "虚拟",
			"导出", "WPS", "pdfFactory", "Software Printer",
		},
		ExcludePortPrefixes: []string{"PORTPROMPT:", "FILE:", "SHRFAX:", "FPP", "NUL:", "RDP", "TSC"},
		SelectionMode:       "auto",
		Admin:               "admin",
	}
}

func newToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "netprint"
	}
	return hex.EncodeToString(b)
}

// LoadConfig 加载配置；不存在时生成默认配置并写盘。
func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if werr := SaveConfig(path, cfg); werr != nil {
				return cfg, fmt.Errorf("无法创建默认配置: %w", werr)
			}
			return cfg, nil
		}
		return cfg, fmt.Errorf("读取配置失败: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return cfg, fmt.Errorf("解析配置失败 %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	d := defaultConfig()
	if c.ServerName == "" {
		c.ServerName = d.ServerName
	}
	if c.Backend == "" {
		c.Backend = d.Backend
	}
	if c.FileOutputDir == "" {
		c.FileOutputDir = d.FileOutputDir
	}
	if len(c.FilePrinters) == 0 {
		c.FilePrinters = d.FilePrinters
	}
	if c.StartPort <= 0 || c.StartPort > 65535 {
		c.StartPort = d.StartPort
	}
	if c.LPDPort <= 0 {
		c.LPDPort = d.LPDPort
	}
	if c.WebPort <= 0 {
		c.WebPort = d.WebPort
	}
	if c.WebToken == "" {
		c.WebToken = newToken()
	}
	if c.MaxJobBytes <= 0 {
		c.MaxJobBytes = d.MaxJobBytes
	}
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = d.IdleTimeoutSec
	}
	if c.LogFile == "" {
		c.LogFile = d.LogFile
	}
	// 打印机筛选规则：配置文件里没写就用默认规则（默认跳过 PDF/传真/虚拟打印机）
	if len(c.ExcludeKeywords) == 0 {
		c.ExcludeKeywords = d.ExcludeKeywords
	}
	if len(c.ExcludePortPrefixes) == 0 {
		c.ExcludePortPrefixes = d.ExcludePortPrefixes
	}
	if c.SelectionMode == "" {
		c.SelectionMode = "auto"
	}
	if c.Admin == "" {
		c.Admin = d.Admin
	}
}

// validateConfig 保存/应用前的校验，返回中文错误信息
func validateConfig(c *Config) error {
	seen := map[int]string{}
	mark := func(name string, p int) error {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s 必须在 1~65535 之间（当前 %d）", name, p)
		}
		if old, ok := seen[p]; ok {
			return fmt.Errorf("端口 %d 冲突：%s 与 %s", p, old, name)
		}
		seen[p] = name
		return nil
	}
	if err := mark("起始端口", c.StartPort); err != nil {
		return err
	}
	if c.LPDEnabled {
		if err := mark("LPD 端口", c.LPDPort); err != nil {
			return err
		}
	}
	if c.WebEnabled {
		if err := mark("状态页端口", c.WebPort); err != nil {
			return err
		}
	}
	switch c.SelectionMode {
	case "auto", "selected", "all":
	default:
		return fmt.Errorf("selection_mode 只能是 auto / selected / all（当前 %q）", c.SelectionMode)
	}
	if c.SelectionMode == "selected" && len(c.SelectedPrinters) == 0 {
		return fmt.Errorf("选择“只转换勾选的打印机”时，至少要勾选一台打印机")
	}
	if c.MaxJobBytes < 1<<20 || c.MaxJobBytes > 4096<<20 {
		return fmt.Errorf("单个作业上限必须在 1MB ~ 4096MB 之间（当前 %d MB）", c.MaxJobBytes>>20)
	}
	if c.IdleTimeoutSec < 5 || c.IdleTimeoutSec > 600 {
		return fmt.Errorf("空闲超时必须在 5~600 秒之间（当前 %d）", c.IdleTimeoutSec)
	}
	if c.BindIP != "" && net.ParseIP(c.BindIP) == nil {
		return fmt.Errorf("绑定 IP 格式不正确：%q（留空表示监听所有网卡）", c.BindIP)
	}
	bp := map[int]string{}
	for _, b := range c.Bindings {
		if b.Printer == "" {
			return fmt.Errorf("手动绑定里有打印机名为空的项")
		}
		if b.Port < 1 || b.Port > 65535 {
			return fmt.Errorf("手动绑定「%s」的端口 %d 不在 1~65535 之间", b.Printer, b.Port)
		}
		if old, ok := bp[b.Port]; ok {
			return fmt.Errorf("手动绑定端口 %d 冲突：%s 与 %s", b.Port, old, b.Printer)
		}
		bp[b.Port] = b.Printer
	}
	return nil
}

func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadState 读取端口分配状态。
func LoadState(path string) *State {
	stateMu.Lock()
	defer stateMu.Unlock()
	st := &State{Assign: map[string]Assign{}}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, st)
		if st.Assign == nil {
			st.Assign = map[string]Assign{}
		}
	}
	return st
}

func SaveState(path string, st *State) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolvePaths 把配置里的相对路径统一到配置文件所在目录（服务模式下工作目录不可控）。
func resolvePaths(configPath string, cfg *Config) {
	dir := filepath.Dir(configPath)
	if !filepath.IsAbs(cfg.LogFile) {
		cfg.LogFile = filepath.Join(dir, cfg.LogFile)
	}
	if cfg.Backend == "file" && !filepath.IsAbs(cfg.FileOutputDir) {
		cfg.FileOutputDir = filepath.Join(dir, cfg.FileOutputDir)
	}
}
