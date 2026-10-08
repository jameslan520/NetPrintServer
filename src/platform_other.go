//go:build !windows

package main

import (
	"fmt"
)

// 非 Windows 平台的桩实现（用于开发与端到端测试）

func isWindowsService() bool { return false }

func isElevated() bool { return false }

func RunWindowsService() {}

func installService(cfgPath string) error {
	return fmt.Errorf("仅 Windows 支持安装服务")
}

func uninstallService() error { return fmt.Errorf("仅 Windows 支持安装服务") }

func startService() error { return fmt.Errorf("仅 Windows 支持安装服务") }

func stopService() error { return fmt.Errorf("仅 Windows 支持安装服务") }

func serviceStatusText() string { return "（非 Windows 平台）" }

func configureFirewall(ports []int, log *Logger) {}

// NewWinspoolBackend 非 Windows 平台不会走到（NewBackend 先判断系统）
func NewWinspoolBackend(cfg *Config, tracker *JobTracker, log *Logger) Backend {
	return NewFileBackend(cfg, cfg.FilePrinters, log)
}

// diagPrinters 非 Windows 平台无 winspool 自检
func diagPrinters() error {
	fmt.Println("[自检] 非 Windows 平台：无 winspool，跳过（当前使用 file 后端）")
	return nil
}
