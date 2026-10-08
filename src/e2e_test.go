package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 测试工具 ----------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func writeTestConfig(t *testing.T, dir string, mutate func(*Config)) (cfgPath string, cfg *Config) {
	t.Helper()
	cfgPath = filepath.Join(dir, "config.json")
	cfg = defaultConfig()
	cfg.Backend = "file"
	cfg.FilePrinters = []string{"EPSON LQ-630K"}
	cfg.FileOutputDir = filepath.Join(dir, "spool")
	cfg.StartPort = freePort(t)
	cfg.LPDPort = freePort(t)
	cfg.WebPort = freePort(t)
	cfg.IdleTimeoutSec = 2
	cfg.Firewall = false
	cfg.LogFile = filepath.Join(dir, "test.log")
	if mutate != nil {
		mutate(cfg)
	}
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return cfgPath, cfg
}

func startTestManager(t *testing.T, cfgPath string) *Manager {
	t.Helper()
	_, m, err := prepareManager(cfgPath, false)
	if err != nil {
		t.Fatalf("prepareManager 失败: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(m.Stop)
	return m
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func prnFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".prn") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func readAllFiles(t *testing.T, dir string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, f := range prnFiles(t, dir) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		out = append(out, b)
	}
	return out
}

func bindingOf(t *testing.T, m *Manager, printer string) RawBinding {
	t.Helper()
	for _, b := range m.Bindings() {
		if b.Printer == printer {
			return b
		}
	}
	t.Fatalf("找不到打印机 %s 的绑定: %+v", printer, m.Bindings())
	return RawBinding{}
}

// ---------- RAW 9100 ----------

func TestRAWPrint(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")

	payload := []byte("\x1b@INVOICE 2026-001\r\nQTY: 10\r\n\x0c")
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		t.Fatalf("连接 RAW 端口失败: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	_ = conn.Close()

	spool := filepath.Join(dir, "spool")
	waitFor(t, 5*time.Second, "RAW 作业落盘", func() bool { return len(prnFiles(t, spool)) >= 1 })

	contents := readAllFiles(t, spool)
	if len(contents) != 1 {
		t.Fatalf("期望 1 个文件，实际 %d", len(contents))
	}
	if !bytes.Equal(contents[0], payload) {
		t.Fatalf("文件内容不匹配:\n got=%q\nwant=%q", contents[0], payload)
	}

	// 作业记录应已完成
	jobs := m.tracker.List(10)
	if len(jobs) != 1 {
		t.Fatalf("期望 1 条作业记录，实际 %d", len(jobs))
	}
	if jobs[0].State != jobStateDone {
		t.Fatalf("作业状态应为 %s，实际 %s（%s）", jobStateDone, jobs[0].State, jobs[0].Detail)
	}
	if jobs[0].Protocol != "RAW" || jobs[0].Bytes != int64(len(payload)) {
		t.Fatalf("作业记录异常: %+v", jobs[0])
	}
}

func TestRAWIdleTimeoutCommitsPartial(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	payload := []byte("PARTIAL-DATA")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 不关闭连接，等待空闲超时（配置 2 秒）后提交
	spool := filepath.Join(dir, "spool")
	waitFor(t, 8*time.Second, "空闲超时提交", func() bool { return len(prnFiles(t, spool)) >= 1 })
	got := readAllFiles(t, spool)
	if !bytes.Equal(got[0], payload) {
		t.Fatalf("部分数据不匹配: got=%q", got[0])
	}
}

func TestRAWEmptryConnectionNoFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	_ = conn.Close()
	time.Sleep(600 * time.Millisecond)
	if files := prnFiles(t, filepath.Join(dir, "spool")); len(files) != 0 {
		t.Fatalf("空连接不应产生文件: %v", files)
	}
}

func TestRAWUnknownPrinterRejected(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfgPath, _ := writeTestConfig(t, dir, func(c *Config) {
		c.FilePrinters = []string{"REAL"}
		// 手动绑定一台不存在的打印机（端口仍会监听，但作业应被拒绝）
		c.Bindings = []RawBinding{{Port: port, Printer: "GHOST", Queue: "QG"}}
	})
	m := startTestManager(t, cfgPath)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	_, _ = conn.Write([]byte("SHOULD-NOT-BE-SAVED"))
	_ = conn.Close()
	time.Sleep(600 * time.Millisecond)
	if files := prnFiles(t, filepath.Join(dir, "spool")); len(files) != 0 {
		t.Fatalf("不可用打印机不应产生文件: %v", files)
	}
	_ = m
}

// ---------- LPD 515 ----------

func lpdRecv(t *testing.T, conn net.Conn, queue, doc, data string) {
	t.Helper()
	mustWrite := func(b []byte) {
		if _, err := conn.Write(b); err != nil {
			t.Fatalf("LPD 发送失败: %v", err)
		}
	}
	mustRead := func(want string) {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("LPD 读取响应失败(期望 %q): %v", want, err)
		}
		if string(got) != want {
			t.Fatalf("LPD 响应不匹配: got=%q want=%q", got, want)
		}
	}
	mustWrite([]byte{0x02})
	mustWrite([]byte(queue + "\n"))
	mustRead("0\n")
	ctrl := fmt.Sprintf("Hhost01\nPjames\nJ%s\n", doc)
	mustWrite([]byte(ctrl))
	mustWrite([]byte{0x02})
	mustRead("0\n")
	mustWrite([]byte(data))
	mustWrite([]byte{0x02})
	mustRead("0\n")
}

func TestLPDPrint(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")
	if b.Queue == "" {
		t.Fatal("自动分配的 LPD 队列为空")
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", m.cfg.LPDPort))
	if err != nil {
		t.Fatalf("连接 LPD 失败: %v", err)
	}
	defer conn.Close()

	payload := "\x1b@LPD JOB\r\nline2\r\n"
	lpdRecv(t, conn, b.Queue, "invoice-888", payload)
	_ = conn.Close()

	spool := filepath.Join(dir, "spool")
	waitFor(t, 5*time.Second, "LPD 作业落盘", func() bool { return len(prnFiles(t, spool)) >= 1 })
	got := readAllFiles(t, spool)
	if string(got[0]) != payload {
		t.Fatalf("LPD 数据不匹配: got=%q", got[0])
	}

	jobs := m.tracker.List(10)
	found := false
	for _, j := range jobs {
		if j.Protocol == "LPD" && j.Doc == "invoice-888" {
			found = true
		}
	}
	if !found {
		t.Fatalf("LPD 作业记录缺失: %+v", jobs)
	}
}

func TestLPDWithoutSTXTerminator(t *testing.T) {
	// 模拟不发送 \2、发完直接关闭连接的 LPR 客户端
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", m.cfg.LPDPort))
	if err != nil {
		t.Fatalf("连接 LPD 失败: %v", err)
	}
	mustRead := func(want string) {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("读取响应失败: %v", err)
		}
		if string(got) != want {
			t.Fatalf("响应不匹配 got=%q want=%q", got, want)
		}
	}
	_, _ = conn.Write([]byte{0x01})
	_, _ = conn.Write([]byte(b.Queue + "\n"))
	mustRead("0\n")
	_, _ = conn.Write([]byte("Hx\nPbob\nJdoc\n"))
	_, _ = conn.Write([]byte{0x02})
	mustRead("0\n")
	_, _ = conn.Write([]byte("RAW-DATA-NO-TERMINATOR"))
	_ = conn.Close()

	spool := filepath.Join(dir, "spool")
	waitFor(t, 5*time.Second, "LPD(无终止符)作业落盘", func() bool { return len(prnFiles(t, spool)) >= 1 })
	got := readAllFiles(t, spool)
	if string(got[0]) != "RAW-DATA-NO-TERMINATOR" {
		t.Fatalf("数据不匹配: %q", got[0])
	}
}

func TestLPDUnknownQueue(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", m.cfg.LPDPort))
	if err != nil {
		t.Fatalf("连接 LPD 失败: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte{0x02})
	_, _ = conn.Write([]byte("NO-SUCH-QUEUE\n"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 2)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != "2\n" {
		t.Fatalf("未知队列应返回 2，实际 %q", got)
	}
}

func TestLPDStatusCommands(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)

	// \4 队列状态
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", m.cfg.LPDPort))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte{0x04})
	_, _ = conn.Write([]byte("PRN1\n"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	body, _ := io.ReadAll(conn)
	_ = conn.Close()
	if !strings.Contains(string(body), "Printer:") {
		t.Fatalf("队列状态响应异常: %q", body)
	}

	// \5 服务器状态
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", m.cfg.LPDPort))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn2.Write([]byte{0x05})
	_, _ = conn2.Write([]byte("server\n"))
	_ = conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	body2, _ := io.ReadAll(conn2)
	_ = conn2.Close()
	if !strings.Contains(string(body2), "Server:") {
		t.Fatalf("服务器状态响应异常: %q", body2)
	}
}

// ---------- 状态页 / API ----------

func TestWebStatusAndTestPage(t *testing.T) {
	dir := t.TempDir()
	cfgPath, cfg := writeTestConfig(t, dir, nil)
	m := startTestManager(t, cfgPath)
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.WebPort)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("访问状态页失败: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态页状态码 %d", resp.StatusCode)
	}
	if !strings.Contains(string(page), "EPSON LQ-630K") {
		t.Fatalf("状态页未包含打印机名:\n%s", page)
	}

	resp2, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("访问 API 失败: %v", err)
	}
	var st StatusView
	if err := json.NewDecoder(resp2.Body).Decode(&st); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	_ = resp2.Body.Close()
	if len(st.Printers) != 1 || st.Printers[0].Printer != "EPSON LQ-630K" {
		t.Fatalf("API 打印机列表异常: %+v", st.Printers)
	}
	if st.Backend != "file" {
		t.Fatalf("后端应为 file，实际 %s", st.Backend)
	}

	// 错误令牌 → 403
	resp3, err := http.PostForm(base+"/api/test", map[string][]string{
		"printer": {"EPSON LQ-630K"},
		"token":   {"wrong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("错误令牌应返回 403，实际 %d", resp3.StatusCode)
	}

	// 正确令牌 → 测试页落盘
	resp4, err := http.PostForm(base+"/api/test", map[string][]string{
		"printer": {"EPSON LQ-630K"},
		"token":   {cfg.WebToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	_ = resp4.Body.Close()
	var res map[string]interface{}
	if err := json.Unmarshal(body4, &res); err != nil {
		t.Fatalf("测试页响应 JSON 无效: %s", body4)
	}
	if res["ok"] != true {
		t.Fatalf("测试页发送失败: %s", body4)
	}
	spool := filepath.Join(dir, "spool")
	waitFor(t, 5*time.Second, "测试页落盘", func() bool { return len(prnFiles(t, spool)) >= 1 })
	found := false
	for _, b := range readAllFiles(t, spool) {
		if bytes.Contains(b, []byte("NetPrintServer Network Test Page")) {
			found = true
		}
	}
	if !found {
		t.Fatal("测试页内容缺失")
	}
	_ = m
}

func TestHealthz(t *testing.T) {
	dir := t.TempDir()
	cfgPath, cfg := writeTestConfig(t, dir, nil)
	_ = startTestManager(t, cfgPath)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.WebPort))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "ok" {
		t.Fatalf("healthz 返回 %q", b)
	}
}

// ---------- 绑定稳定性 ----------

func TestBindingStickyAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, nil)
	m1 := startTestManager(t, cfgPath)
	b1 := bindingOf(t, m1, "EPSON LQ-630K")
	m1.Stop()

	// 重新启动（模拟服务重启）
	m2 := startTestManager(t, cfgPath)
	b2 := bindingOf(t, m2, "EPSON LQ-630K")
	if b1.Port != b2.Port || b1.Queue != b2.Queue {
		t.Fatalf("重启后端口漂移: before=%+v after=%+v", b1, b2)
	}
}

func TestManualBindingWins(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfgPath, _ := writeTestConfig(t, dir, func(c *Config) {
		c.Bindings = []RawBinding{{Port: port, Queue: "MYQ", Printer: "EPSON LQ-630K"}}
	})
	m := startTestManager(t, cfgPath)
	b := bindingOf(t, m, "EPSON LQ-630K")
	if b.Port != port || b.Queue != "MYQ" {
		t.Fatalf("手动绑定未生效: %+v", b)
	}
}

// ---------- 配置与状态 ----------

func TestConfigAutoCreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("默认配置未写盘")
	}
	if cfg.WebToken == "" || cfg.StartPort != 9100 || cfg.LPDPort != 515 {
		t.Fatalf("默认配置异常: %+v", cfg)
	}
	// 再次加载应保留自定义值
	cfg.WebPort = 9999
	cfg.ServerName = "测试服务器"
	if err := SaveConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.WebPort != 9999 || cfg2.ServerName != "测试服务器" {
		t.Fatalf("配置未保留: %+v", cfg2)
	}
}

func TestStateSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := &State{Assign: map[string]Assign{"打印机A": {Port: 9100, Queue: "PRN1"}}}
	if err := SaveState(p, st); err != nil {
		t.Fatal(err)
	}
	st2 := LoadState(p)
	if a := st2.Assign["打印机A"]; a.Port != 9100 || a.Queue != "PRN1" {
		t.Fatalf("状态读取异常: %+v", st2.Assign)
	}
}
