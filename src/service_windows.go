//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	dllShell32        = syscall.NewLazyDLL("shell32.dll")
	procIsUserAnAdmin = dllShell32.NewProc("IsUserAnAdmin")

	dllKernel32             = syscall.NewLazyDLL("kernel32.dll")
	procMultiByteToWideChar = dllKernel32.NewProc("MultiByteToWideChar")
)

func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

func isElevated() bool {
	r, _, _ := procIsUserAnAdmin.Call()
	return r != 0
}

// RunWindowsService 作为 Windows 服务运行
func RunWindowsService() {
	if err := svc.Run(winServiceName, &printService{}); err != nil {
		fmt.Printf("服务运行失败: %v\n", err)
	}
}

type printService struct{}

func (s *printService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// 服务启动参数形如：run -config "D:\NetPrintServer\config.json"
	cfgPath := defaultConfigPath()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-config" || args[i] == "--config" {
			cfgPath = args[i+1]
		}
	}

	_, m, err := prepareManager(cfgPath, false)
	if err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		status <- svc.Status{State: svc.Stopped}
		return false, 1
	}
	if err := startManager(m); err != nil {
		fmt.Printf("启动失败: %v\n", err)
		status <- svc.Status{State: svc.Stopped}
		return false, 2
	}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				m.Stop()
				return false, 0
			case svc.Interrogate:
				status <- c.CurrentStatus
			}
		}
	}
}

func installService(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败（请用管理员身份运行）: %w", err)
	}
	defer m.Disconnect()

	// 已存在则先卸载，保证配置最新
	if s, err := m.OpenService(winServiceName); err == nil {
		_, _ = s.Control(svc.Stop)
		time.Sleep(time.Second)
		_ = s.Delete()
		_ = s.Close()
	}

	s, err := m.CreateService(winServiceName, exe, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "NetPrintServer 网络打印服务器",
		Description: "把本机打印机转换为网络打印服务器（RAW 9100 / LPD 515）",
	}, "run", "-config", cfgPath)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	_ = s.Close()
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败（请用管理员身份运行）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(winServiceName)
	if err != nil {
		return fmt.Errorf("服务未安装")
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	for i := 0; i < 10; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(time.Second)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("删除服务失败: %w", err)
	}
	return nil
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败（请用管理员身份运行）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(winServiceName)
	if err != nil {
		return fmt.Errorf("服务未安装，请先运行 install")
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("启动服务失败: %w", err)
	}
	return nil
}

func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败（请用管理员身份运行）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(winServiceName)
	if err != nil {
		return fmt.Errorf("服务未安装")
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止服务失败: %w", err)
	}
	return nil
}

func serviceStatusText() string {
	m, err := mgr.Connect()
	if err != nil {
		return "无法访问服务管理器（需要管理员）"
	}
	defer m.Disconnect()
	s, err := m.OpenService(winServiceName)
	if err != nil {
		return "未安装"
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return "查询失败: " + err.Error()
	}
	switch st.State {
	case svc.Stopped:
		return "已停止"
	case svc.StartPending:
		return "启动中"
	case svc.StopPending:
		return "停止中"
	case svc.Running:
		return "运行中"
	case svc.ContinuePending:
		return "继续中"
	case svc.PausePending:
		return "暂停中"
	case svc.Paused:
		return "已暂停"
	default:
		return fmt.Sprintf("状态 %d", st.State)
	}
}

// configureFirewall 幂等更新防火墙入站规则（delete 旧的再 add）
func configureFirewall(ports []int, log *Logger) {
	if len(ports) == 0 {
		return
	}
	if !isElevated() {
		log.Warn("当前非管理员权限，跳过防火墙配置（请以管理员身份运行一次）")
		return
	}
	_ = exec.Command("netsh", firewallDeleteArgs()...).Run()
	out, err := exec.Command("netsh", firewallAddArgs(ports)...).CombinedOutput()
	if err != nil {
		log.Warn("防火墙规则添加失败: %v %s", err, strings.TrimSpace(decodeConsole(out)))
		return
	}
	var ps []string
	for _, p := range ports {
		ps = append(ps, fmt.Sprintf("%d", p))
	}
	log.Info("防火墙入站规则已更新（TCP %s）", strings.Join(ps, ","))
}

// decodeConsole 用系统 OEM 代码页把 netsh 输出转成 UTF-8（中文系统上避免乱码）
func decodeConsole(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const cpOEM = 1 // CP_OEMCP
	r, _, _ := procMultiByteToWideChar.Call(cpOEM, 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if r == 0 {
		return string(b)
	}
	buf := make([]uint16, r)
	r2, _, _ := procMultiByteToWideChar.Call(cpOEM, 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(r))
	if r2 == 0 {
		return string(b)
	}
	return syscall.UTF16ToString(buf[:r2])
}
