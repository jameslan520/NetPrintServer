package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ================= 会话与登录限流 =================

const (
	sessionCookie = "nps_admin"
	authHeader    = "X-NPS-Auth"
	sessionTTL    = 12 * time.Hour
)

type adminSession struct {
	User     string
	LastSeen time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*adminSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: map[string]*adminSession{}}
}

func (s *sessionStore) create(user string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m {
		if now.Sub(v.LastSeen) > sessionTTL {
			delete(s.m, k)
		}
	}
	tok := randomHex(24)
	s.m[tok] = &adminSession{User: user, LastSeen: now}
	return tok
}

func (s *sessionStore) get(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[tok]
	if !ok {
		return "", false
	}
	if time.Since(sess.LastSeen) > sessionTTL {
		delete(s.m, tok)
		return "", false
	}
	sess.LastSeen = time.Now()
	return sess.User, true
}

func (s *sessionStore) delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tok)
}

// deleteOthers 修改口令后踢掉其它设备的会话
func (s *sessionStore) deleteOthers(keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if k != keep {
			delete(s.m, k)
		}
	}
}

// loginLimiter 连续失败 5 次锁定 60 秒，防止暴力猜口令
type loginLimiter struct {
	mu          sync.Mutex
	fails       int
	lockedUntil time.Time
}

func (l *loginLimiter) allowed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Now().After(l.lockedUntil)
}

func (l *loginLimiter) fail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails++
	if l.fails >= 5 {
		l.fails = 0
		l.lockedUntil = time.Now().Add(60 * time.Second)
		return "连续失败次数过多，已锁定 60 秒"
	}
	return ""
}

func (l *loginLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails = 0
	l.lockedUntil = time.Time{}
}

// ================= 管理端 =================

type webAdmin struct {
	mgr      *Manager
	sessions *sessionStore
	limiter  *loginLimiter
	loginTpl *template.Template
	pageTpl  *template.Template
}

func newWebAdmin(m *Manager) *webAdmin {
	return &webAdmin{
		mgr:      m,
		sessions: newSessionStore(),
		limiter:  &loginLimiter{},
		loginTpl: template.Must(template.New("login").Parse(loginHTML)),
		pageTpl: template.Must(template.New("config").Funcs(template.FuncMap{
			"hasPrefix": strings.HasPrefix,
		}).Parse(configHTML)),
	}
}

func (a *webAdmin) register(mux *http.ServeMux) {
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/config", a.requireAuth(a.handleConfigPage))
	mux.HandleFunc("/api/config", a.requireAuth(a.handleSaveConfig))
	mux.HandleFunc("/api/password", a.requireAuth(a.handleChangePassword))
	mux.HandleFunc("/api/whoami", a.handleWhoami)
}

func (a *webAdmin) sessionFrom(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return a.sessions.get(c.Value)
}

func (a *webAdmin) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if _, ok := a.sessionFrom(r); !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(rw, http.StatusUnauthorized, map[string]interface{}{
					"ok": false, "error": "未登录或会话已过期，请重新登录",
				})
			} else {
				http.Redirect(rw, r, "/login", http.StatusFound)
			}
			return
		}
		next(rw, r)
	}
}

// checkCSRF 写操作要求自定义头 + Origin 同源（跨站页面无法伪造）
func checkCSRF(r *http.Request) bool {
	if r.Header.Get(authHeader) != "1" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if !strings.EqualFold(o, "http://"+r.Host) && !strings.EqualFold(o, "https://"+r.Host) {
			return false
		}
	}
	return true
}

func writeJSON(rw http.ResponseWriter, code int, v interface{}) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func (a *webAdmin) handleWhoami(rw http.ResponseWriter, r *http.Request) {
	user, ok := a.sessionFrom(r)
	if !ok {
		writeJSON(rw, http.StatusOK, map[string]interface{}{"ok": false, "auth": false})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]interface{}{"ok": true, "auth": true, "user": user})
}

func (a *webAdmin) handleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = a.loginTpl.Execute(rw, map[string]interface{}{"Error": "", "Admin": a.mgr.Config().Admin})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(rw, "方法不允许", http.StatusMethodNotAllowed)
		return
	}
	if !a.limiter.allowed() {
		rw.WriteHeader(http.StatusTooManyRequests)
		_ = a.loginTpl.Execute(rw, map[string]interface{}{
			"Error": "尝试次数过多，请 60 秒后再试", "Admin": a.mgr.Config().Admin,
		})
		return
	}
	_ = r.ParseForm()
	user := strings.TrimSpace(r.FormValue("user"))
	pass := r.FormValue("pass")
	cfg := a.mgr.Config()
	if user == cfg.Admin && verifyPassword(cfg.AdminPassword, pass) {
		a.limiter.reset()
		tok := a.sessions.create(user)
		http.SetCookie(rw, &http.Cookie{
			Name:     sessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		http.Redirect(rw, r, "/config", http.StatusFound)
		return
	}
	msg := a.limiter.fail()
	if msg == "" {
		msg = "用户名或口令错误"
	}
	rw.WriteHeader(http.StatusUnauthorized)
	_ = a.loginTpl.Execute(rw, map[string]interface{}{"Error": msg, "Admin": cfg.Admin})
}

func (a *webAdmin) handleLogout(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.delete(c.Value)
	}
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(rw, r, "/login", http.StatusFound)
}

// ================= 配置页 =================

type configPrinterView struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Port      string `json:"port"`
	State     string `json:"state"`
	Selected  bool   `json:"selected"`
	Skipped   bool   `json:"skipped"`
	Reason    string `json:"reason"`
	BoundPort int    `json:"bound_port"`
	BoundQ    string `json:"bound_queue"`
}

type configPageView struct {
	Admin         string
	User          string
	Mode          string
	Printers      []configPrinterView
	ServerName    string
	BindIP        string
	StartPort     int
	LPDEnabled    bool
	LPDPort       int
	WebPort       int
	Firewall      bool
	MaxJobMB      int
	IdleTimeout   int
	IncludeKW     string
	ExcludeKW     string
	KeepVirtual   bool
	BindingsJSON  string
	LastApply     string
	PrimaryIP     string
	SelectedCount int
	TotalPrinters int
}

func joinKw(list []string) string {
	var out []string
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

func (a *webAdmin) buildConfigView(user string) (*configPageView, error) {
	m := a.mgr
	cfg := m.Config()
	printers, err := m.AllPrinters()
	if err != nil {
		return nil, err
	}
	sel := map[string]bool{}
	for _, s := range cfg.SelectedPrinters {
		sel[strings.ToLower(strings.TrimSpace(s))] = true
	}
	skipped := map[string]string{} // name -> reason
	for _, p := range m.SkippedPrinters() {
		skipped[strings.ToLower(p.Name)] = p.Detail
	}
	bound := map[string]RawBinding{}
	for _, b := range m.Bindings() {
		bound[strings.ToLower(b.Printer)] = b
	}

	seen := map[string]bool{}
	var rows []configPrinterView
	addRow := func(p Printer) {
		ln := strings.ToLower(p.Name)
		if seen[ln] {
			return
		}
		seen[ln] = true
		b, hasBound := bound[ln]
		_, isSkipped := skipped[ln]
		isSel := sel[ln] || hasBound
		row := configPrinterView{
			Name: p.Name, Driver: p.Driver, Port: p.Port, State: p.State,
			Selected: isSel, Skipped: isSkipped, Reason: skipped[ln],
		}
		if hasBound {
			row.BoundPort = b.Port
			row.BoundQ = b.Queue
		}
		rows = append(rows, row)
	}
	for _, p := range printers {
		addRow(p)
	}
	// 有历史分配但当前不在线的打印机也列出来（可取消勾选）
	for name, b := range bound {
		if !seen[name] {
			addRow(Printer{Name: b.Printer, State: "离线/未连接", Port: "-", Driver: "-"})
		}
	}

	bj, _ := json.MarshalIndent(cfg.Bindings, "", "  ")
	ips := LocalIPs()
	primary := ""
	if len(ips) > 0 {
		primary = ips[0]
	}
	selCount := 0
	for _, r := range rows {
		if r.Selected {
			selCount++
		}
	}
	return &configPageView{
		Admin: cfg.Admin, User: user, Mode: cfg.SelectionMode,
		Printers: rows, ServerName: cfg.ServerName, BindIP: cfg.BindIP,
		StartPort: cfg.StartPort, LPDEnabled: cfg.LPDEnabled, LPDPort: cfg.LPDPort,
		WebPort: cfg.WebPort, Firewall: cfg.Firewall,
		MaxJobMB:    int(cfg.MaxJobBytes >> 20),
		IdleTimeout: cfg.IdleTimeoutSec,
		IncludeKW:   joinKw(cfg.IncludeKeywords), ExcludeKW: joinKw(cfg.ExcludeKeywords),
		KeepVirtual:  cfg.KeepVirtualPrinters,
		BindingsJSON: string(bj), LastApply: m.LastApply(),
		PrimaryIP: primary, SelectedCount: selCount, TotalPrinters: len(rows),
	}, nil
}

func (a *webAdmin) handleConfigPage(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/config" {
		http.NotFound(rw, r)
		return
	}
	user, _ := a.sessionFrom(r)
	view, err := a.buildConfigView(user)
	if err != nil {
		http.Error(rw, "读取配置失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.pageTpl.Execute(rw, view); err != nil {
		a.mgr.log.Warn("配置页渲染失败: %v", err)
	}
}

// configPayload 网页“保存并应用”提交的配置
type configPayload struct {
	ServerName       string   `json:"server_name"`
	BindIP           string   `json:"bind_ip"`
	StartPort        int      `json:"start_port"`
	LPDEnabled       bool     `json:"lpd_enabled"`
	LPDPort          int      `json:"lpd_port"`
	WebEnabled       bool     `json:"web_enabled"`
	WebPort          int      `json:"web_port"`
	Firewall         bool     `json:"firewall"`
	MaxJobMB         int      `json:"max_job_mb"`
	IdleTimeoutSec   int      `json:"idle_timeout_sec"`
	SelectionMode    string   `json:"selection_mode"`
	SelectedPrinters []string `json:"selected_printers"`
	IncludeKeywords  []string `json:"include_keywords"`
	ExcludeKeywords  []string `json:"exclude_keywords"`
	KeepVirtual      bool     `json:"keep_virtual_printers"`
	BindingsJSON     string   `json:"bindings_json"`
}

func splitKw(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ';' || r == '；'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (a *webAdmin) handleSaveConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "仅支持 POST"})
		return
	}
	if !checkCSRF(r) {
		writeJSON(rw, http.StatusForbidden, map[string]interface{}{"ok": false, "error": "请求校验失败（缺少 X-NPS-Auth 头或来源异常）"})
		return
	}
	var p configPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "提交的数据无法解析: " + err.Error()})
		return
	}

	old := a.mgr.Config()
	next := *old // 浅拷贝，敏感字段（口令哈希、令牌）原样保留
	next.ServerName = strings.TrimSpace(p.ServerName)
	next.BindIP = strings.TrimSpace(p.BindIP)
	next.StartPort = p.StartPort
	next.LPDEnabled = p.LPDEnabled
	next.LPDPort = p.LPDPort
	next.WebPort = p.WebPort
	next.WebEnabled = true // 网页端不允许关闭状态页，避免把自己锁在门外
	next.Firewall = p.Firewall
	next.MaxJobBytes = int64(p.MaxJobMB) << 20
	next.IdleTimeoutSec = p.IdleTimeoutSec
	next.SelectionMode = p.SelectionMode
	next.SelectedPrinters = append([]string{}, p.SelectedPrinters...)
	next.IncludeKeywords = splitKw(strings.Join(p.IncludeKeywords, ","))
	next.ExcludeKeywords = splitKw(strings.Join(p.ExcludeKeywords, ","))
	next.KeepVirtualPrinters = p.KeepVirtual

	var bindings []RawBinding
	if strings.TrimSpace(p.BindingsJSON) != "" {
		if err := json.Unmarshal([]byte(p.BindingsJSON), &bindings); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]interface{}{
				"ok": false, "error": "手动绑定(bindings) 不是合法 JSON 数组: " + err.Error()})
			return
		}
	}
	next.Bindings = bindings

	if err := validateConfig(&next); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	cfgPath := a.mgr.cfgPath
	if err := SaveConfig(cfgPath, &next); err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "写入配置文件失败: " + err.Error()})
		return
	}

	portChanged := next.WebPort != old.WebPort || next.BindIP != old.BindIP
	note := "配置已保存，正在重新应用监听…"
	if portChanged {
		note = fmt.Sprintf("配置已保存，正在切换到新端口 %d；若新地址打不开，程序会自动回滚到原配置（详见 netprintserver.log）", next.WebPort)
	}

	// 先应答（此时旧状态页还活着），再异步热重启
	writeJSON(rw, http.StatusOK, map[string]interface{}{"ok": true, "note": note})
	go func() {
		time.Sleep(400 * time.Millisecond)
		if err := a.mgr.Reload(&next); err != nil {
			a.mgr.SetLastApply("上次应用失败：" + err.Error() + "（已回滚到原配置）")
			return
		}
		a.mgr.SetLastApply("配置已在 " + time.Now().Format("15:04:05") + " 应用")
	}()
}

func (a *webAdmin) handleChangePassword(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "仅支持 POST"})
		return
	}
	if !checkCSRF(r) {
		writeJSON(rw, http.StatusForbidden, map[string]interface{}{"ok": false, "error": "请求校验失败"})
		return
	}
	var p struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "数据无法解析"})
		return
	}
	pw := strings.TrimSpace(p.Password)
	if len(pw) < 6 {
		writeJSON(rw, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "新口令至少 6 位"})
		return
	}
	old := a.mgr.Config()
	next := *old
	next.AdminPassword = hashPassword(pw)
	if err := SaveConfig(a.mgr.cfgPath, &next); err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "写入配置失败: " + err.Error()})
		return
	}
	a.mgr.SetConfig(&next)
	// 踢掉其它设备，保留当前会话
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.deleteOthers(c.Value)
	}
	a.mgr.log.Info("管理员口令已修改（%s）", old.Admin)
	writeJSON(rw, http.StatusOK, map[string]interface{}{"ok": true, "note": "口令已修改，其它设备需要重新登录"})
}
