package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestBindIPKeepsLoopbackAccess 回归测试：
// 设置 bind_ip 指定网卡后，本机必须仍能用 127.0.0.1 / localhost 访问（v1.1.0 的缺陷），
// 同时其它网卡地址不应被监听（bind_ip 的限制作用要保留）。
func TestBindIPKeepsLoopbackAccess(t *testing.T) {
	ips := LocalIPs()
	if len(ips) == 0 {
		t.Skip("本机没有可用的非回环 IP，跳过")
	}
	lan := ips[0]

	dir := t.TempDir()
	cfgPath, cfg := writeTestConfig(t, dir, func(c *Config) {
		c.BindIP = lan
		c.FilePrinters = []string{"EPSON LQ-630K"}
	})
	m := startTestManager(t, cfgPath)

	// 1) 本机回环必须可访问（本次回归的重点）
	if !httpOK(cfg.WebPort, "/healthz") {
		t.Fatalf("bind_ip=%s 时本机 127.0.0.1:%d 必须仍可访问", lan, cfg.WebPort)
	}

	// 2) 指定网卡地址可访问
	if !httpOKAddr(lan, cfg.WebPort, "/healthz") {
		t.Fatalf("绑定网卡 %s:%d 应可访问", lan, cfg.WebPort)
	}

	// 3) 其它本机网卡不监听（有第二块网卡时才测）
	if len(ips) >= 2 {
		other := ips[1]
		if httpOKAddr(other, cfg.WebPort, "/healthz") {
			t.Fatalf("bind_ip=%s 时不应监听其它网卡地址 %s:%d", lan, other, cfg.WebPort)
		}
	}

	// 4) RAW 打印端口同样既监听网卡又监听回环：本机发作业应能落盘
	bs := m.Bindings()
	if len(bs) != 1 {
		t.Fatalf("应有 1 个绑定，实际: %+v", bs)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", bs[0].Port), time.Second)
	if err != nil {
		t.Fatalf("RAW 端口 %d 本机回环应可连接: %v", bs[0].Port, err)
	}
	payload := []byte("\x1b@BIND-IP-LOOPBACK\r\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	_ = conn.Close()
	waitFor(t, 5*time.Second, "回环 RAW 作业落盘", func() bool {
		return len(prnFiles(t, cfg.FileOutputDir)) >= 1
	})
	got := readAllFiles(t, cfg.FileOutputDir)
	if string(got[0]) != string(payload) {
		t.Fatalf("内容不匹配: %q", got[0])
	}
}

// TestBindIPLoopbackOnly 单独绑定回环时不应出现重复监听
func TestBindIPLoopbackOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath, cfg := writeTestConfig(t, dir, func(c *Config) {
		c.BindIP = "127.0.0.1"
		c.FilePrinters = []string{"EPSON LQ-630K"}
	})
	_ = startTestManager(t, cfgPath)
	if !httpOK(cfg.WebPort, "/healthz") {
		t.Fatalf("bind_ip=127.0.0.1 时 %d 应可访问", cfg.WebPort)
	}
}

// TestBindPortPrimaryFailureErrors 主地址监听失败必须报错（供热应用回滚判断）
func TestBindPortPrimaryFailureErrors(t *testing.T) {
	ips := LocalIPs()
	if len(ips) == 0 {
		t.Skip("本机没有可用的非回环 IP，跳过")
	}
	// 用一个本机不存在的 IP 地址（203.0.113.x 是文档保留网段）
	bogus := "203.0.113.7"
	dir := t.TempDir()
	cfgPath, cfg := writeTestConfig(t, dir, func(c *Config) {
		c.BindIP = bogus
		c.FilePrinters = []string{"EPSON LQ-630K"}
	})
	_, m, err := prepareManager(cfgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err == nil {
		m.Stop()
		t.Fatal("主地址（不存在的网卡 IP）监听失败时应返回错误，便于回滚")
	}
	_ = cfg
}
