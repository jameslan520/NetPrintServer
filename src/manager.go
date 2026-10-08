package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Manager 装配所有监听器、绑定关系与状态刷新
type Manager struct {
	cfgPath   string
	cfg       *Config
	statePath string
	backend   Backend
	tracker   *JobTracker
	log       *Logger

	mu        sync.RWMutex
	bindings  []RawBinding
	skipped   []Printer
	rawSvrs   map[int]*RAWListener
	startedAt time.Time
	baseCtx   context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	web       *WebServer
	lastApply string
	adminOnce sync.Once
	admin     *webAdmin
}

func NewManager(cfgPath string, cfg *Config, backend Backend, tracker *JobTracker, log *Logger) *Manager {
	return &Manager{
		cfgPath:   cfgPath,
		cfg:       cfg,
		statePath: strings.TrimSuffix(cfgPath, filepath.Ext(cfgPath)) + ".state.json",
		backend:   backend,
		tracker:   tracker,
		log:       log,
		rawSvrs:   map[int]*RAWListener{},
	}
}

// resolveBindings 计算打印机 → 端口/队列 的稳定映射（持久化到 state 文件）。
// 优先级：手动绑定 > 历史分配（粘性）> 自动分配；虚拟/软件打印机默认不参与自动分配。
func (m *Manager) resolveBindings() ([]RawBinding, error) {
	cfg := m.cfg
	state := LoadState(m.statePath)

	avail, err := m.backend.ListPrinters()
	if err != nil {
		return nil, fmt.Errorf("枚举打印机失败: %w", err)
	}
	filter := newPrinterFilter(cfg)

	// 手动绑定（即使是 PDF/传真类也照样保留，用户说了算）
	explicit := map[string]RawBinding{}
	for _, b := range cfg.Bindings {
		if b.Port <= 0 || b.Printer == "" {
			continue
		}
		explicit[strings.ToLower(b.Printer)] = b
	}
	isExplicit := func(name string) bool {
		_, ok := explicit[strings.ToLower(name)]
		return ok
	}

	// 按选择模式决定“哪些打印机参与转换”
	selected := map[string]bool{}
	for _, s := range cfg.SelectedPrinters {
		selected[strings.ToLower(strings.TrimSpace(s))] = true
	}
	mode := cfg.SelectionMode
	want := func(p Printer) bool {
		if isExplicit(p.Name) {
			return true
		}
		switch mode {
		case "selected": // 只转换勾选的打印机
			return selected[strings.ToLower(p.Name)]
		case "all": // 全部（含虚拟打印机）
			return true
		default: // auto：排除规则过滤虚拟打印机
			return filter.keep(p)
		}
	}
	wantOffline := func(name string) bool {
		if isExplicit(name) {
			return true
		}
		switch mode {
		case "selected":
			return selected[strings.ToLower(name)]
		case "all":
			return true
		default:
			return filter.keepOfflineByName(name)
		}
	}

	// 当前枚举到的打印机按规则分流
	var availNames []string
	var skipped []Printer
	enumerated := map[string]bool{}
	for _, p := range avail {
		enumerated[strings.ToLower(p.Name)] = true
		if want(p) {
			availNames = append(availNames, p.Name)
		} else {
			p.Detail = skipReason(mode, p, filter)
			skipped = append(skipped, p)
		}
	}
	m.setSkipped(skipped)

	// 状态清理：被跳过的、以及离线且名字不合规则的，从历史分配里移除，避免残留占端口
	for _, p := range skipped {
		if !isExplicit(p.Name) {
			delete(state.Assign, p.Name)
		}
	}
	for name := range state.Assign {
		if enumerated[strings.ToLower(name)] || isExplicit(name) {
			continue
		}
		if !wantOffline(name) {
			delete(state.Assign, name)
		}
	}

	takenPorts := map[int]bool{}
	takenQueues := map[string]bool{}
	if cfg.LPDEnabled {
		takenPorts[cfg.LPDPort] = true
	}
	if cfg.WebEnabled {
		takenPorts[cfg.WebPort] = true
	}

	var result []RawBinding
	included := map[string]bool{}
	add := func(b RawBinding) {
		result = append(result, b)
		takenPorts[b.Port] = true
		if b.Queue != "" {
			takenQueues[strings.ToUpper(b.Queue)] = true
		}
		included[strings.ToLower(b.Printer)] = true
	}

	// 1) 手动绑定优先
	for _, b := range cfg.Bindings {
		if b.Port <= 0 || b.Printer == "" {
			continue
		}
		add(b)
	}

	// 2) 当前可用的打印机：沿用历史分配，其次自动分配
	var needAssign []string
	for _, n := range availNames {
		ln := strings.ToLower(n)
		if included[ln] {
			continue
		}
		if a, ok := state.Assign[n]; ok && a.Port > 0 && !takenPorts[a.Port] {
			add(RawBinding{Printer: n, Port: a.Port, Queue: a.Queue})
			continue
		}
		needAssign = append(needAssign, n)
	}
	if !cfg.AutoAssign {
		needAssign = nil
	}
	nextPort := cfg.StartPort
	nextQueue := 1
	for _, n := range needAssign {
		for takenPorts[nextPort] {
			nextPort++
		}
		var q string
		for {
			q = fmt.Sprintf("PRN%d", nextQueue)
			nextQueue++
			if !takenQueues[strings.ToUpper(q)] {
				break
			}
		}
		b := RawBinding{Printer: n, Port: nextPort, Queue: q}
		state.Assign[n] = Assign{Port: b.Port, Queue: b.Queue}
		add(b)
	}

	// 3) 保留历史上分配过、当前不可用的打印机（端口保持监听，状态页显示离线）
	var missing []string
	for name := range state.Assign {
		if !included[strings.ToLower(name)] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		ln := strings.ToLower(name)
		if included[ln] {
			continue
		}
		// 手动绑定里已有的跳过
		if _, isExplicit := explicit[ln]; isExplicit {
			continue
		}
		a := state.Assign[name]
		if a.Port <= 0 || takenPorts[a.Port] {
			continue
		}
		add(RawBinding{Printer: name, Port: a.Port, Queue: a.Queue})
	}

	if err := SaveState(m.statePath, state); err != nil {
		m.log.Warn("保存端口分配状态失败: %v", err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Port < result[j].Port })

	m.mu.Lock()
	m.bindings = result
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) setSkipped(list []Printer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skipped = list
}

// SkippedPrinters 返回被筛选规则跳过的打印机（虚拟/PDF/传真类）
func (m *Manager) SkippedPrinters() []Printer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Printer, len(m.skipped))
	copy(out, m.skipped)
	return out
}

// bindPort 绑定一个端口的所有监听地址：
//   - bind_ip 为空 → 监听所有网卡（:port）
//   - bind_ip 指定 → 监听该地址，同时【额外监听 127.0.0.1】，保证本机 localhost/127.0.0.1 也能访问
//
// 主地址（bind_ip）监听失败视为错误；回环监听失败只警告（端口被本机其它程序占用等情况）。
func (m *Manager) bindPort(port int) ([]net.Listener, error) {
	var addrs []string
	if m.cfg.BindIP != "" {
		addrs = append(addrs, fmt.Sprintf("%s:%d", m.cfg.BindIP, port))
	} else {
		addrs = append(addrs, fmt.Sprintf(":%d", port))
	}
	// 指定网卡时，补一个回环监听（bind_ip 本身就是回环则跳过）
	if m.cfg.BindIP != "" {
		if ip := net.ParseIP(m.cfg.BindIP); ip == nil || !ip.IsLoopback() {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))
		}
	}

	var lns []net.Listener
	var errs []string
	for i, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a, err))
			// 主地址（第一个）失败 = 整体失败；回环失败只警告
			if i == 0 {
				for _, done := range lns {
					_ = done.Close()
				}
				return nil, fmt.Errorf("端口 %d 监听失败: %s", port, strings.Join(errs, "；"))
			}
			continue
		}
		lns = append(lns, ln)
	}
	if len(errs) > 0 {
		m.log.Warn("端口 %d 附加地址未监听（不影响主地址）: %s", port, strings.Join(errs, "；"))
	}
	return lns, nil
}

// bindNote 指定了网卡时的日志备注（此时会额外监听回环）
func (m *Manager) bindNote() string {
	if m.cfg.BindIP == "" {
		return ""
	}
	if ip := net.ParseIP(m.cfg.BindIP); ip != nil && ip.IsLoopback() {
		return ""
	}
	return "（另监听 127.0.0.1）"
}

// webNote 指定了网卡时，提示本机仍可用回环地址访问
func (m *Manager) webNote() string {
	if m.cfg.BindIP == "" {
		return ""
	}
	if ip := net.ParseIP(m.cfg.BindIP); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("，本机亦可访问 http://127.0.0.1:%d/", m.cfg.WebPort)
}

func (m *Manager) startRawListener(b RawBinding) error {
	m.mu.Lock()
	if _, ok := m.rawSvrs[b.Port]; ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	lns, err := m.bindPort(b.Port)
	if err != nil {
		return err
	}
	srv := &RAWListener{
		Port:     b.Port,
		Printer:  b.Printer,
		backend:  m.backend,
		tracker:  m.tracker,
		log:      m.log,
		maxBytes: m.cfg.MaxJobBytes,
		idle:     time.Duration(m.cfg.IdleTimeoutSec) * time.Second,
	}
	m.mu.Lock()
	m.rawSvrs[b.Port] = srv
	m.mu.Unlock()
	for _, ln := range lns {
		m.wg.Add(1)
		go func(ln net.Listener) {
			defer m.wg.Done()
			srv.Serve(m.ctx(), ln)
		}(ln)
	}
	m.log.Info("RAW 打印端口已监听 :%d%s → 打印机「%s」", b.Port, m.bindNote(), b.Printer)
	return nil
}

func (m *Manager) ctx() context.Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.baseCtx == nil {
		return context.Background()
	}
	return m.baseCtx
}

// Start 启动全部服务；任一监听端口绑定失败都会返回错误（网页“保存并应用”据此回滚）
func (m *Manager) Start(parent context.Context) error {
	bindings, err := m.resolveBindings()
	if err != nil {
		return err
	}
	m.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	m.baseCtx = ctx
	m.cancel = cancel
	m.bindings = bindings
	m.startedAt = time.Now()
	m.mu.Unlock()

	var bindErrs []string
	for _, b := range bindings {
		if err := m.startRawListener(b); err != nil {
			m.log.Error("%v", err)
			bindErrs = append(bindErrs, err.Error())
		}
	}

	if m.cfg.LPDEnabled {
		lns, err := m.bindPort(m.cfg.LPDPort)
		if err != nil {
			m.log.Error("%v", err)
			bindErrs = append(bindErrs, err.Error())
		} else {
			srv := &LPDListener{
				Port:       m.cfg.LPDPort,
				Resolve:    m.ResolveQueue,
				ListQueues: m.Bindings,
				backend:    m.backend,
				tracker:    m.tracker,
				log:        m.log,
				maxBytes:   m.cfg.MaxJobBytes,
				idle:       time.Duration(m.cfg.IdleTimeoutSec) * time.Second,
				serverName: m.cfg.ServerName,
				dataQuiet:  10 * time.Second,
			}
			for _, ln := range lns {
				m.wg.Add(1)
				go func(ln net.Listener) {
					defer m.wg.Done()
					srv.Serve(ctx, ln)
				}(ln)
			}
			m.log.Info("LPD 打印端口已监听 :%d%s", m.cfg.LPDPort, m.bindNote())
		}
	}

	if m.cfg.WebEnabled {
		m.web = NewWebServer(m)
		lns, err := m.bindPort(m.cfg.WebPort)
		if err != nil {
			m.log.Error("%v", err)
			bindErrs = append(bindErrs, err.Error())
		} else {
			for _, ln := range lns {
				m.wg.Add(1)
				go func(ln net.Listener) {
					defer m.wg.Done()
					m.web.Serve(ctx, ln)
				}(ln)
			}
			m.log.Info("状态页已启动 http://<本机IP>:%d/%s", m.cfg.WebPort, m.webNote())
		}
	}

	// 周期任务：刷新打印机/队列状态、动态接入新打印机
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.backend.Refresh()
				m.reconcile()
			}
		}
	}()

	if len(bindErrs) > 0 {
		return fmt.Errorf("%d 个端口监听失败：%s", len(bindErrs), strings.Join(bindErrs, "；"))
	}

	m.log.Info("打印服务器「%s」启动完成，共 %d 个打印端口", m.cfg.ServerName, len(bindings))
	for _, b := range bindings {
		st := "离线"
		if m.backend.HasPrinter(b.Printer) {
			st = "可用"
		}
		m.log.Info("  端口 %d / 队列 %s → %s（%s）", b.Port, orDash(b.Queue), b.Printer, st)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// reconcile 周期对账：新出现的打印机分配端口并开始监听
func (m *Manager) reconcile() {
	bindings, err := m.resolveBindings()
	if err != nil {
		m.log.Warn("打印机重新枚举失败: %v", err)
		return
	}
	m.mu.Lock()
	m.bindings = bindings
	m.mu.Unlock()
	for _, b := range bindings {
		m.mu.Lock()
		_, ok := m.rawSvrs[b.Port]
		m.mu.Unlock()
		if ok {
			continue
		}
		if err := m.startRawListener(b); err != nil {
			m.log.Error("新打印机接入失败: %v", err)
			continue
		}
		m.log.Info("检测到新打印机「%s」，已分配端口 %d", b.Printer, b.Port)
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		m.log.Warn("部分监听器未能在 5 秒内退出")
	}
	m.mu.Lock()
	m.rawSvrs = map[int]*RAWListener{}
	m.web = nil
	m.mu.Unlock()
	m.log.Info("打印服务器已停止")
}

// Reload 用新配置热重启全部监听（含状态页本身）。
// 新配置应用失败时自动回滚到旧配置并恢复原监听，保证不会把管理入口搞丢。
// 注意：m.cfg 的指针替换在锁内完成，读取方可能短暂看到旧配置对象（无害，旧对象在回滚期间保持存活）。
func (m *Manager) Reload(newCfg *Config) error {
	m.mu.Lock()
	oldCfg := m.cfg
	oldBackend := m.backend
	m.mu.Unlock()

	m.Stop()

	m.mu.Lock()
	m.cfg = newCfg
	m.backend = NewBackend(newCfg, m.tracker, m.log)
	m.mu.Unlock()

	if err := m.Start(context.Background()); err != nil {
		m.log.Error("应用新配置失败: %v，开始回滚", err)
		m.Stop()
		m.mu.Lock()
		m.cfg = oldCfg
		m.backend = oldBackend
		m.mu.Unlock()
		// 先把配置文件恢复，再恢复监听，避免回滚过程中读到半套配置
		if serr := SaveConfig(m.cfgPath, oldCfg); serr != nil {
			m.log.Error("回滚配置文件失败: %v", serr)
		} else {
			m.log.Info("配置文件已回滚到原配置")
		}
		if rerr := m.Start(context.Background()); rerr != nil {
			m.log.Error("回滚旧配置后仍然失败: %v", rerr)
			return fmt.Errorf("应用新配置失败（回滚也失败: %v）: %w", rerr, err)
		}
		return fmt.Errorf("应用新配置失败（已回滚）: %w", err)
	}
	if newCfg.Firewall {
		configureFirewall(m.requiredPorts(), m.log)
	}
	m.log.Info("配置已保存并生效")
	return nil
}

func (m *Manager) Bindings() []RawBinding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RawBinding, len(m.bindings))
	copy(out, m.bindings)
	return out
}

// Config 返回当前配置（可能在热应用时被替换，读取方应把它当作快照）
func (m *Manager) Config() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// SetConfig 直接替换配置（只改内存不重启监听；改口令等轻量字段用）
func (m *Manager) SetConfig(c *Config) {
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// Admin 返回管理端（登录会话跨热重启保持有效）
func (m *Manager) Admin() *webAdmin {
	m.adminOnce.Do(func() {
		m.admin = newWebAdmin(m)
	})
	return m.admin
}

// AllPrinters 枚举本机全部打印机
func (m *Manager) AllPrinters() ([]Printer, error) {
	return m.backend.ListPrinters()
}

func (m *Manager) SetLastApply(s string) {
	m.mu.Lock()
	m.lastApply = s
	m.mu.Unlock()
}

func (m *Manager) LastApply() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastApply
}

// ResolveQueue LPD 队列名 → 打印机名
func (m *Manager) ResolveQueue(queue string) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(queue))
	for _, b := range m.Bindings() {
		if b.Queue != "" && strings.ToLower(b.Queue) == q {
			return b.Printer, true
		}
		if strings.ToLower(b.Printer) == q {
			return b.Printer, true
		}
	}
	return "", false
}

// SendTestPage 向指定打印机发送一页测试内容
func (m *Manager) SendTestPage(printer string) error {
	if !m.backend.HasPrinter(printer) {
		return fmt.Errorf("打印机「%s」不可用", printer)
	}
	meta := JobMeta{
		Doc:    "NetPrintServer-测试页",
		Source: "本地状态页",
	}
	h, err := m.backend.OpenJob(printer, meta)
	if err != nil {
		return err
	}
	rec := m.tracker.Begin(printer, meta.Doc, meta.Source, "本地")
	winID := h.JobID()
	if winID > 0 {
		m.tracker.SetWinJob(rec, winID)
	}
	data := buildTestPage(printer, m)
	if _, err := h.Write(data); err != nil {
		h.Abort()
		finishJob(m.tracker, m.backend, rec, 0, winID, err)
		return err
	}
	if err := h.Commit(); err != nil {
		finishJob(m.tracker, m.backend, rec, 0, winID, err)
		return err
	}
	finishJob(m.tracker, m.backend, rec, int64(len(data)), winID, nil)
	m.log.Info("已向「%s」发送测试页（%d 字节）", printer, len(data))
	return nil
}

// buildTestPage 生成 ESC/P 测试页（纯 ASCII，点阵打印机直接可打）
func buildTestPage(printer string, m *Manager) []byte {
	var b []byte
	b = append(b, 0x1b, 0x40) // ESC @ 复位
	b = append(b, []byte("\r\n")...)
	lines := []string{
		"==========================================",
		"   NetPrintServer Network Test Page",
		"==========================================",
		fmt.Sprintf("Printer : %s", printer),
		fmt.Sprintf("Server  : %s", m.cfg.ServerName),
		fmt.Sprintf("Time    : %s", time.Now().Format("2006-01-02 15:04:05")),
		fmt.Sprintf("Version : %s", Version),
		"",
		"If this page prints correctly, the network",
		"print server is working. You may tear off",
		"this page now.",
		"",
	}
	for _, ln := range lines {
		b = append(b, []byte(ln+"\r\n")...)
	}
	b = append(b, 0x0c) // FF 换页
	return b
}

// PrinterStates 合并绑定与打印机实时状态
func (m *Manager) PrinterStates() []PrinterStateView {
	avail, err := m.backend.ListPrinters()
	if err != nil {
		m.log.Warn("枚举打印机失败: %v", err)
		avail = nil
	}
	byName := map[string]Printer{}
	for _, p := range avail {
		byName[strings.ToLower(p.Name)] = p
	}
	var out []PrinterStateView
	for _, b := range m.Bindings() {
		v := PrinterStateView{RawBinding: b}
		if p, ok := byName[strings.ToLower(b.Printer)]; ok {
			v.State = p.State
			v.Driver = p.Driver
			v.PortDesc = p.Port
			v.Jobs = p.Jobs
			v.Detail = p.Detail
		} else {
			v.State = "离线/未连接"
		}
		out = append(out, v)
	}
	return out
}

// LocalIPs 返回本机可用于客户端连接的 IPv4 地址：
// 过滤掉 169.254 链路本地地址，把真实内网地址排在前面，虚拟网卡（VirtualBox/VMware/Hyper-V/Docker 等）排在后面。
func LocalIPs() []string {
	type entry struct {
		ip    string
		score int
	}
	var entries []entry
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			entries = append(entries, entry{ip: ip.String(), score: ipSortKey(ip, iface.Name)})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].score < entries[j].score })
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ip)
	}
	return out
}

// ipSortKey 越小越优先：真实内网 > 其它私网 > 其它；链路本地与虚拟网卡降级
func ipSortKey(ip net.IP, ifName string) int {
	v4 := ip.To4()
	if v4 == nil {
		return 45
	}
	score := 40
	switch {
	case v4[0] == 10:
		score = 10
	case v4[0] == 192 && v4[1] == 168:
		score = 20
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		score = 30
	case v4[0] == 169 && v4[1] == 254:
		score = 60 // 链路本地，客户端连不上
	}
	if isVirtualIface(ifName) {
		score += 5
	}
	return score
}

func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, kw := range []string{
		"virtual", "vmware", "vbox", "virtualbox", "hyper-v", "vethernet", "docker",
		"loopback", "tap", "tun", "tailscale", "zerotier", "hamachi", "wsl", "npcap",
	} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// ExeDir 可执行文件所在目录（服务模式下工作目录不可控，配置以它为基准）
func ExeDir() string {
	p, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(p)
}

// requiredPorts 所有需要放行防火墙的端口
func (m *Manager) requiredPorts() []int {
	var ports []int
	for _, b := range m.Bindings() {
		ports = append(ports, b.Port)
	}
	if m.cfg.LPDEnabled {
		ports = append(ports, m.cfg.LPDPort)
	}
	if m.cfg.WebEnabled {
		ports = append(ports, m.cfg.WebPort)
	}
	return dedupePorts(ports)
}
