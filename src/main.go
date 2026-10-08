package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const Version = "1.1.1"
const winServiceName = "NetPrintServer"

func defaultConfigPath() string {
	if p := os.Getenv("NETPRINT_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(ExeDir(), "config.json")
}

// prepareManager 加载配置、日志、后端与管理器（不启动监听）
func prepareManager(cfgPath string, console bool) (*Config, *Manager, error) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	resolvePaths(cfgPath, cfg)
	log := NewLogger(cfg.LogFile, console)
	// 首次运行：生成网页配置页管理员口令（明文只在这里打印一次，config.json 里只存哈希）
	if cfg.AdminPassword == "" {
		pw := randomPassword(10)
		cfg.AdminPassword = hashPassword(pw)
		if serr := SaveConfig(cfgPath, cfg); serr != nil {
			log.Warn("保存初始口令失败: %v", serr)
		}
		log.Warn("已生成网页配置页登录账号：用户名 %s，口令 %s（忘记可在服务器运行 netprintserver.exe password 重置）", cfg.Admin, pw)
	}
	tracker := NewJobTracker(300)
	backend := NewBackend(cfg, tracker, log)
	m := NewManager(cfgPath, cfg, backend, tracker, log)
	m.log = log
	return cfg, m, nil
}

// startManager 启动全部监听并配置防火墙
func startManager(m *Manager) error {
	if err := m.Start(context.Background()); err != nil {
		return err
	}
	if m.cfg.Firewall {
		configureFirewall(m.requiredPorts(), m.log)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	os.Exit(1)
}

func runCmd(cfgPath string) {
	if isWindowsService() {
		RunWindowsService()
		return
	}
	_, m, err := prepareManager(cfgPath, true)
	if err != nil {
		fatal(err)
	}
	if err := startManager(m); err != nil {
		fatal(err)
	}
	printConnectInfo(m, cfgPath)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	fmt.Println("\n收到退出信号，正在停止…")
	m.Stop()
}

func printConnectInfo(m *Manager, cfgPath string) {
	ips := LocalIPs()
	primary := "<本机IP>"
	if len(ips) > 0 {
		primary = ips[0]
	}
	fmt.Println()
	fmt.Println("──────── 客户端连接信息 ────────")
	fmt.Println("配置文件  :", cfgPath)
	fmt.Println("本机地址  :", strings.Join(ips, ", "))
	bindings := m.Bindings()
	if len(bindings) == 0 {
		fmt.Println("⚠ 没有可用于自动分配的打印机（虚拟/PDF/传真类已自动跳过）。")
		fmt.Println("  请确认本机已安装打印机驱动、能正常打印；如需强制暴露某台打印机，")
		fmt.Println("  在 config.json 的 bindings 里手动指定，或调整 exclude_keywords / include_keywords。")
	}
	for _, b := range bindings {
		fmt.Printf("  打印机「%s」→ 客户端端口 %s:%d", b.Printer, primary, b.Port)
		if b.Queue != "" && m.cfg.LPDEnabled {
			fmt.Printf("  / LPR 队列 %s（端口 %d）", b.Queue, m.cfg.LPDPort)
		}
		fmt.Println()
	}
	if sk := m.SkippedPrinters(); len(sk) > 0 {
		var names []string
		for _, p := range sk {
			names = append(names, p.Name)
		}
		fmt.Printf("  已跳过 %d 台虚拟/软件打印机：%s\n", len(sk), strings.Join(names, "、"))
		fmt.Println("  （确实需要暴露它们：config.json 里设 \"keep_virtual_printers\": true，或用 include_keywords 指定）")
	}
	if m.cfg.WebEnabled {
		fmt.Printf("  状态页: http://%s:%d/\n", primary, m.cfg.WebPort)
		if m.cfg.BindIP != "" {
			fmt.Printf("  本机访问: http://127.0.0.1:%d/（已自动监听回环，localhost 也可）\n", m.cfg.WebPort)
		}
	}
	if !isElevated() {
		fmt.Println("提示: 以管理员身份运行可自动配置 Windows 防火墙入站规则。")
	}
	fmt.Println("──────────────────────────────")
}

func installCmd(cfgPath string) {
	_, m, err := prepareManager(cfgPath, true)
	if err != nil {
		fatal(err)
	}
	bindings, err := m.resolveBindings()
	if err != nil {
		fatal(err)
	}
	if len(bindings) == 0 {
		fmt.Println("⚠ 未发现本机打印机：请先在服务器上安装打印机驱动并确认本机能正常打印，再重新运行本命令。")
	}
	if err := installService(cfgPath); err != nil {
		fatal(err)
	}
	ports := make([]int, 0, len(bindings)+2)
	for _, b := range bindings {
		ports = append(ports, b.Port)
	}
	if m.cfg.LPDEnabled {
		ports = append(ports, m.cfg.LPDPort)
	}
	if m.cfg.WebEnabled {
		ports = append(ports, m.cfg.WebPort)
	}
	configureFirewall(dedupePorts(ports), m.log)

	fmt.Printf("服务已安装：Windows 服务名 %s（开机自启动）\n", winServiceName)
	if err := startService(); err != nil {
		fmt.Println("⚠ 启动服务失败:", err)
		fmt.Println("  可手动执行: net start", winServiceName, "（查看原因看 netprintserver.log）")
	} else {
		time.Sleep(900 * time.Millisecond)
		fmt.Println("服务状态:", serviceStatusText())
	}
	printConnectInfo(m, cfgPath)
}

func uninstallCmd() {
	if err := uninstallService(); err != nil {
		fatal(err)
	}
	fmt.Println("服务已停止并删除。配置文件与日志保留在原位置。")
}

func statusCmd(cfgPath string) {
	fmt.Println("服务状态:", serviceStatusText())
	fmt.Println("配置文件:", cfgPath)
	_, m, err := prepareManager(cfgPath, true)
	if err != nil {
		fmt.Println("配置加载失败:", err)
		return
	}
	bindings, err := m.resolveBindings()
	if err != nil {
		fmt.Println("打印机枚举失败:", err)
		return
	}
	ips := LocalIPs()
	ip := "本机IP"
	if len(ips) > 0 {
		ip = ips[0]
	}
	for _, b := range bindings {
		state := "离线"
		if m.backend.HasPrinter(b.Printer) {
			state = "可用"
		}
		fmt.Printf("  %s:%d → %s（%s），LPR 队列 %s\n", ip, b.Port, b.Printer, state, orDash(b.Queue))
	}
}

func printersCmd(cfgPath string) {
	_, m, err := prepareManager(cfgPath, true)
	if err != nil {
		fatal(err)
	}
	list, err := m.backend.ListPrinters()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("后端: %s，共 %d 台打印机\n", m.backend.Kind(), len(list))
	for _, p := range list {
		fmt.Printf("  %-40s 状态=%-10s 端口=%-16s 驱动=%s\n", p.Name, p.State, p.Port, p.Driver)
	}
}

func dedupePorts(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range in {
		if p <= 0 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func helpText() string {
	return strings.TrimSpace(`
NetPrintServer 网络打印服务器 v` + Version + `

用法:
  netprintserver [run] [-config 路径]   前台运行（默认）
  netprintserver install [-config 路径] 安装为 Windows 服务（开机自启，需管理员）
  netprintserver uninstall              卸载服务（需管理员）
  netprintserver start | stop           启动/停止服务（需管理员）
  netprintserver status                 查看服务与端口状态
  netprintserver diag                   自检（打印 winspool 调用与打印机枚举结果，排障用）
  netprintserver printers               列出本机打印机
  netprintserver password [新口令]      设置网页配置页登录口令（不给参数则随机生成并打印）
  netprintserver version                版本信息

网页配置（推荐，改配置不用手动编辑文件）:
  http://<服务器IP>:8080/config  → 用管理员账号登录后：
    · 勾选要把哪几台打印机转换为网络打印（其它打印机不暴露）
    · 修改端口、协议、防火墙、超时等
    · 修改登录口令
  保存后立即生效（热重启监听，端口配错会自动回滚）

客户端接入（Windows 7 / 10 / 11）:
  控制面板 → 设备和打印机 → 添加打印机 → 我需要的打印机不在列表中
  → 使用 TCP/IP 地址或主机名 → 输入服务器 IP 与 RAW 端口（默认 9100）
  → 驱动选择真实型号（如 EPSON LQ-630K）
  也可以直接运行随附脚本 client\install-win10.ps1 / client\install-win7.bat

状态页: http://<服务器IP>:8080/
`)
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "内部错误（已捕获，进程未崩溃）: %v\n", r)
			fmt.Fprintln(os.Stderr, "请把以上输出连同执行的命令发给开发者。")
			os.Exit(70)
		}
	}()

	args := os.Args[1:]
	cfgPath := defaultConfigPath()
	cmd := "run"
	cmdSet := false
	var extra []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config":
			if i+1 >= len(args) {
				fatal(fmt.Errorf("%s 需要一个路径参数", a))
			}
			i++
			cfgPath = args[i]
		case a == "-h" || a == "--help":
			cmd, cmdSet = "help", true
		case a == "-V" || a == "--version":
			cmd, cmdSet = "version", true
		case strings.HasPrefix(a, "-"):
			fatal(fmt.Errorf("未知选项 %s（-h 查看帮助）", a))
		default:
			if !cmdSet {
				if !isKnownCmd(a) {
					fatal(fmt.Errorf("未知命令 %s（-h 查看帮助）", a))
				}
				cmd, cmdSet = a, true
			} else {
				extra = append(extra, a)
			}
		}
	}

	switch cmd {
	case "help":
		fmt.Println(helpText())
	case "version":
		fmt.Println("NetPrintServer", Version)
	case "run":
		runCmd(cfgPath)
	case "install":
		installCmd(cfgPath)
	case "uninstall":
		uninstallCmd()
	case "start":
		if err := startService(); err != nil {
			fatal(err)
		}
		fmt.Println("已请求启动服务。")
	case "stop":
		if err := stopService(); err != nil {
			fatal(err)
		}
		fmt.Println("已请求停止服务。")
	case "status":
		statusCmd(cfgPath)
	case "diag":
		diagCmd(cfgPath)
	case "printers":
		printersCmd(cfgPath)
	case "password":
		passwordCmd(cfgPath, extra)
	default:
		fmt.Println(helpText())
	}
}

func isKnownCmd(a string) bool {
	switch a {
	case "run", "install", "uninstall", "start", "stop", "status", "diag", "printers", "password", "help", "version":
		return true
	}
	return false
}

// passwordCmd 重置/设置网页配置页管理员口令
func passwordCmd(cfgPath string, args []string) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fatal(err)
	}
	resolvePaths(cfgPath, cfg)
	var pw string
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		pw = strings.TrimSpace(args[0])
		if len(pw) < 6 {
			fatal(fmt.Errorf("口令至少 6 位"))
		}
	} else {
		pw = randomPassword(12)
	}
	cfg.AdminPassword = hashPassword(pw)
	if err := SaveConfig(cfgPath, cfg); err != nil {
		fatal(err)
	}
	fmt.Printf("网页配置页登录口令已设置：\n  用户名 %s\n  口令   %s\n", cfg.Admin, pw)
	if isWindowsOS() {
		fmt.Printf("服务正在运行时需要重启才生效：\n  net stop %s && net start %s\n", winServiceName, winServiceName)
	} else {
		fmt.Println("重启程序后生效。")
	}
}

// diagCmd 自检：环境、服务状态、winspool 调用、端口分配与过滤结果全部打印，便于远程排障
func diagCmd(cfgPath string) {
	fmt.Printf("NetPrintServer 自检 v%s\n", Version)
	fmt.Println("可执行文件:", os.Args[0])
	fmt.Println("配置文件  :", cfgPath)
	fmt.Println("本机 IP   :", strings.Join(LocalIPs(), ", "))
	fmt.Println("Windows 服务状态:", serviceStatusText())

	_, m, err := prepareManager(cfgPath, true)
	if err != nil {
		fmt.Println("配置加载失败:", err)
	} else {
		fmt.Println("打印后端  :", m.backend.Kind())
		fmt.Println("对外的端口分配：")
		if _, err := m.resolveBindings(); err != nil {
			fmt.Println("  计算失败:", err)
		}
		for _, b := range m.Bindings() {
			fmt.Printf("  RAW %d  ←  「%s」  队列 %s\n", b.Port, b.Printer, orDash(b.Queue))
		}
		if sk := m.SkippedPrinters(); len(sk) > 0 {
			fmt.Printf("已按规则跳过 %d 台虚拟/软件打印机：\n", len(sk))
			for _, p := range sk {
				fmt.Printf("  - %s（驱动 %s；端口 %s）\n", p.Name, p.Driver, p.Port)
			}
		}
	}

	if err := diagPrinters(); err != nil {
		fmt.Println("打印机自检失败:", err)
	}
	fmt.Println("自检结束")
}
