package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------- 测试辅助 ----------

type testClient struct {
	t      *testing.T
	base   string
	client *http.Client
	cookie string
}

func newTestClient(t *testing.T, port int) *testClient {
	return &testClient{
		t:    t,
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		client: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *testClient) req(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	c.t.Helper()
	r, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatalf("构造请求失败: %v", err)
	}
	if c.cookie != "" {
		r.Header.Set("Cookie", c.cookie)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := c.client.Do(r)
	if err != nil {
		c.t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	return resp
}

func (c *testClient) body(resp *http.Response) string {
	c.t.Helper()
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b)
}

func (c *testClient) postJSON(path string, payload interface{}, csrf bool) *http.Response {
	c.t.Helper()
	b, _ := json.Marshal(payload)
	hdr := map[string]string{"Content-Type": "application/json"}
	if csrf {
		hdr[authHeader] = "1"
	}
	return c.req(http.MethodPost, path, bytes.NewReader(b), hdr)
}

func (c *testClient) login(user, pass string) *http.Response {
	c.t.Helper()
	form := strings.NewReader("user=" + user + "&pass=" + pass)
	resp := c.req(http.MethodPost, "/login", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.Value != "" {
			c.cookie = ck.Name + "=" + ck.Value
		}
	}
	_ = resp.Body.Close()
	return resp
}

// startAdminManager 启动一个带已知管理员口令的实例
func startAdminManager(t *testing.T, dir string, password string, mutate func(*Config)) (*Manager, *Config, string) {
	t.Helper()
	cfgPath, cfg := writeTestConfig(t, dir, func(c *Config) {
		c.Admin = "admin"
		c.AdminPassword = hashPassword(password)
		if mutate != nil {
			mutate(c)
		}
	})
	m := startTestManager(t, cfgPath)
	return m, cfg, cfgPath
}

func portAlive(t *testing.T, port int) bool {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// httpOK 轮询用：连不上/非 200 都返回 false，不会让测试直接失败
func httpOK(port int, path string) bool {
	return httpOKAddr("127.0.0.1", port, path)
}

// httpOKAddr 指定主机访问
func httpOKAddr(host string, port int, path string) bool {
	cl := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := cl.Get(fmt.Sprintf("http://%s:%d%s", host, port, path))
	if err != nil {
		return false
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == 200 && strings.TrimSpace(string(b)) != ""
}

// ---------- 选择模式 ----------

func TestSelectionModeSelectedOnlyExposesChosen(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, func(c *Config) {
		c.Admin = "admin"
		c.AdminPassword = hashPassword("test1234")
		c.FilePrinters = []string{"EPSON LQ-630K", "RICOH MP C6004", "Fax"}
		c.SelectionMode = "selected"
		c.SelectedPrinters = []string{"EPSON LQ-630K", "Fax"} // 明确勾选的虚拟机也应被转换
	})
	m := startTestManager(t, cfgPath)

	bs := m.Bindings()
	got := map[string]int{}
	for _, b := range bs {
		got[b.Printer] = b.Port
	}
	if len(got) != 2 || got["EPSON LQ-630K"] == 0 || got["Fax"] == 0 {
		t.Fatalf("选择模式下应只转换勾选的两台，实际: %+v", bs)
	}
	if _, ok := got["RICOH MP C6004"]; ok {
		t.Fatalf("未勾选的打印机不应被转换: %+v", bs)
	}
	sk := m.SkippedPrinters()
	if len(sk) != 1 || sk[0].Name != "RICOH MP C6004" {
		t.Fatalf("应只有 1 台被跳过，实际: %+v", sk)
	}
	if !strings.Contains(sk[0].Detail, "未勾选") {
		t.Fatalf("跳过原因应说明未勾选，实际: %q", sk[0].Detail)
	}
}

// ---------- 登录与鉴权 ----------

func TestAdminAuthFlow(t *testing.T) {
	dir := t.TempDir()
	_, cfg, _ := startAdminManager(t, dir, "test1234", func(c *Config) {
		c.FilePrinters = []string{"EPSON LQ-630K", "RICOH MP C6004"}
		c.SelectionMode = "selected"
		c.SelectedPrinters = []string{"EPSON LQ-630K"}
	})
	c := newTestClient(t, cfg.WebPort)

	// 未登录访问配置页 → 跳转登录
	resp := c.req(http.MethodGet, "/config", nil, nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("未登录访问 /config 应 302 → /login，实际 %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_ = resp.Body.Close()

	// 登录页可打开
	resp = c.req(http.MethodGet, "/login", nil, nil)
	body := c.body(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "配置管理") {
		t.Fatalf("登录页异常: %d", resp.StatusCode)
	}

	// 错误口令
	resp = c.login("admin", "wrongpass")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d", resp.StatusCode)
	}
	if c.cookie != "" {
		t.Fatal("错误口令不应发放会话")
	}

	// 正确口令
	resp = c.login("admin", "test1234")
	if resp.StatusCode != http.StatusFound || c.cookie == "" {
		t.Fatalf("正确口令应 302 并发放会话，实际 %d cookie=%q", resp.StatusCode, c.cookie)
	}

	// 登录后可访问配置页，并包含打印机
	resp = c.req(http.MethodGet, "/config", nil, nil)
	body = c.body(resp)
	if resp.StatusCode != 200 {
		t.Fatalf("登录后 /config 应 200，实际 %d", resp.StatusCode)
	}
	for _, want := range []string{"EPSON LQ-630K", "RICOH MP C6004", "只转换", "保存并应用"} {
		if !strings.Contains(body, want) {
			t.Fatalf("配置页缺少 %q", want)
		}
	}

	// 未登录调 API
	anon := newTestClient(t, cfg.WebPort)
	resp = anon.postJSON("/api/config", configPayload{SelectionMode: "auto"}, true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录调 /api/config 应 401，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 已登录但缺自定义头（CSRF 防护）
	resp = c.postJSON("/api/config", configPayload{SelectionMode: "auto"}, false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 %s 头应 403，实际 %d", authHeader, resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Origin 跨站
	resp = c.req(http.MethodPost, "/api/config", strings.NewReader("{}"), map[string]string{
		"Content-Type": "application/json", authHeader: "1", "Origin": "http://evil.example.com",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestLoginLockout(t *testing.T) {
	dir := t.TempDir()
	_, cfg, _ := startAdminManager(t, dir, "test1234", nil)
	c := newTestClient(t, cfg.WebPort)
	for i := 0; i < 5; i++ {
		resp := c.login("admin", "bad")
		_ = resp.Body.Close()
	}
	resp := c.login("admin", "test1234")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("连续失败 5 次后应锁定（429），实际 %d", resp.StatusCode)
	}
}

// ---------- 保存配置：只改选择 + 热应用 ----------

func TestSaveConfigAppliesSelection(t *testing.T) {
	dir := t.TempDir()
	m, cfg, cfgPath := startAdminManager(t, dir, "test1234", func(c *Config) {
		c.FilePrinters = []string{"EPSON LQ-630K", "RICOH MP C6004"}
	})
	_ = m
	c := newTestClient(t, cfg.WebPort)
	if resp := c.login("admin", "test1234"); resp.StatusCode != http.StatusFound {
		t.Fatalf("登录失败: %d", resp.StatusCode)
	}

	payload := configPayload{
		ServerName: "车间打印服务", BindIP: "", StartPort: cfg.StartPort,
		LPDEnabled: false, LPDPort: cfg.LPDPort, WebEnabled: true, WebPort: cfg.WebPort,
		Firewall: false, MaxJobMB: 100, IdleTimeoutSec: 30,
		SelectionMode: "selected", SelectedPrinters: []string{"RICOH MP C6004"},
		BindingsJSON: "[]",
	}
	resp := c.postJSON("/api/config", payload, true)
	body := c.body(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("保存失败: %d %s", resp.StatusCode, body)
	}

	// 等待热应用完成：绑定应只剩 RICOH
	waitFor(t, 10*time.Second, "配置热生效", func() bool {
		bs := m.Bindings()
		return len(bs) == 1 && bs[0].Printer == "RICOH MP C6004"
	})
	// 落盘的 config.json 也应更新
	saved, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SelectionMode != "selected" || len(saved.SelectedPrinters) != 1 ||
		saved.SelectedPrinters[0] != "RICOH MP C6004" || saved.ServerName != "车间打印服务" {
		t.Fatalf("配置文件未正确保存: %+v", saved)
	}
	if saved.AdminPassword != cfg.AdminPassword {
		t.Fatal("保存配置不应改动管理员口令哈希")
	}
}

func TestSaveConfigRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	_, cfg, _ := startAdminManager(t, dir, "test1234", nil)
	c := newTestClient(t, cfg.WebPort)
	c.login("admin", "test1234")

	// 选择模式但没勾选
	resp := c.postJSON("/api/config", configPayload{
		StartPort: cfg.StartPort, LPDPort: cfg.LPDPort, WebPort: cfg.WebPort,
		MaxJobMB: 100, IdleTimeoutSec: 30, SelectionMode: "selected", SelectedPrinters: []string{},
		BindingsJSON: "[]",
	}, true)
	body := c.body(resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "至少要勾选") {
		t.Fatalf("应拒绝空选择: %d %s", resp.StatusCode, body)
	}

	// 端口冲突（起始端口 = 状态页端口）
	resp = c.postJSON("/api/config", configPayload{
		StartPort: cfg.WebPort, LPDPort: cfg.LPDPort, WebPort: cfg.WebPort,
		MaxJobMB: 100, IdleTimeoutSec: 30, SelectionMode: "auto", BindingsJSON: "[]",
	}, true)
	body = c.body(resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "冲突") {
		t.Fatalf("应拒绝端口冲突: %d %s", resp.StatusCode, body)
	}

	// bindings JSON 非法
	resp = c.postJSON("/api/config", configPayload{
		StartPort: cfg.StartPort, LPDPort: cfg.LPDPort, WebPort: cfg.WebPort,
		MaxJobMB: 100, IdleTimeoutSec: 30, SelectionMode: "auto", BindingsJSON: "{oops}",
	}, true)
	body = c.body(resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "JSON") {
		t.Fatalf("应拒绝非法 bindings: %d %s", resp.StatusCode, body)
	}
}

// ---------- 热应用：换端口 / 端口冲突自动回滚 ----------

func TestSaveConfigHotSwitchesWebPort(t *testing.T) {
	dir := t.TempDir()
	m, cfg, _ := startAdminManager(t, dir, "test1234", nil)
	oldPort := cfg.WebPort
	newPort := freePort(t)
	if newPort == oldPort {
		newPort = freePort(t)
	}

	c := newTestClient(t, oldPort)
	c.login("admin", "test1234")
	payload := configPayload{
		ServerName: cfg.ServerName, StartPort: cfg.StartPort, LPDPort: cfg.LPDPort,
		WebEnabled: true, WebPort: newPort, Firewall: false,
		MaxJobMB: 100, IdleTimeoutSec: 30, SelectionMode: "auto", BindingsJSON: "[]",
	}
	resp := c.postJSON("/api/config", payload, true)
	body := c.body(resp)
	if resp.StatusCode != 200 {
		t.Fatalf("保存失败: %d %s", resp.StatusCode, body)
	}

	// 新端口应能访问状态页
	waitFor(t, 10*time.Second, "新端口生效", func() bool { return httpOK(newPort, "/healthz") })
	if m.Config().WebPort != newPort {
		t.Fatalf("内存配置未更新: %d", m.Config().WebPort)
	}
	// 旧端口应已关闭
	waitFor(t, 5*time.Second, "旧端口关闭", func() bool { return !portAlive(t, oldPort) })
}

func TestSaveConfigRollsBackOnPortConflict(t *testing.T) {
	dir := t.TempDir()
	m, cfg, cfgPath := startAdminManager(t, dir, "test1234", nil)
	oldPort := cfg.WebPort

	// 占住一个端口，模拟“新端口被占用”
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	busyPort := blocker.Addr().(*net.TCPAddr).Port

	c := newTestClient(t, oldPort)
	c.login("admin", "test1234")
	payload := configPayload{
		ServerName: cfg.ServerName, StartPort: cfg.StartPort, LPDPort: cfg.LPDPort,
		WebEnabled: true, WebPort: busyPort, Firewall: false,
		MaxJobMB: 100, IdleTimeoutSec: 30, SelectionMode: "auto", BindingsJSON: "[]",
	}
	resp := c.postJSON("/api/config", payload, true)
	if resp.StatusCode != 200 {
		t.Fatalf("接口应先接受保存: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 回滚完成后：原端口恢复服务，且配置文件已回退到原端口
	waitFor(t, 20*time.Second, "回滚完成（原端口恢复且配置回退）", func() bool {
		if !httpOK(oldPort, "/healthz") {
			return false
		}
		saved, err := LoadConfig(cfgPath)
		return err == nil && saved.WebPort == oldPort
	})
	if m.Config().WebPort != oldPort {
		t.Fatalf("内存配置应回滚到 %d，实际 %d", oldPort, m.Config().WebPort)
	}
	if !strings.Contains(m.LastApply(), "回滚") {
		t.Fatalf("应记录回滚信息，实际: %q", m.LastApply())
	}
}

// ---------- 修改口令 ----------

func TestChangePassword(t *testing.T) {
	dir := t.TempDir()
	_, cfg, _ := startAdminManager(t, dir, "test1234", nil)
	c := newTestClient(t, cfg.WebPort)
	c.login("admin", "test1234")

	resp := c.postJSON("/api/password", map[string]string{"password": "newSecret9"}, true)
	body := c.body(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("改口令失败: %d %s", resp.StatusCode, body)
	}

	// 旧口令失效、新口令可用
	old := newTestClient(t, cfg.WebPort)
	if r := old.login("admin", "test1234"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧口令应失效，实际 %d", r.StatusCode)
	}
	fresh := newTestClient(t, cfg.WebPort)
	if r := fresh.login("admin", "newSecret9"); r.StatusCode != http.StatusFound {
		t.Fatalf("新口令应可用，实际 %d", r.StatusCode)
	}

	// 太短的口令被拒
	resp = fresh.postJSON("/api/password", map[string]string{"password": "123"}, true)
	body = fresh.body(resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("短口令应被拒绝: %d %s", resp.StatusCode, body)
	}
}

func TestVerifyPasswordHashing(t *testing.T) {
	h := hashPassword("abc123")
	if !verifyPassword(h, "abc123") {
		t.Fatal("正确口令应校验通过")
	}
	if verifyPassword(h, "abc124") || verifyPassword(h, "") || verifyPassword("", "abc123") {
		t.Fatal("错误口令不应通过")
	}
	if strings.Contains(h, "abc123") {
		t.Fatal("哈希里不应包含明文")
	}
	if !strings.Contains(h, "$") {
		t.Fatalf("哈希格式应为 salt$hash，实际 %q", h)
	}
}
