package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"
)

// PrinterStateView 绑定 + 实时状态
type PrinterStateView struct {
	RawBinding
	State    string `json:"state"`
	Driver   string `json:"driver"`
	PortDesc string `json:"port_desc"`
	Detail   string `json:"detail,omitempty"`
	Jobs     int    `json:"jobs"`
}

// StatusView 状态页 / JSON API 的统一数据
type StatusView struct {
	ServerName  string             `json:"server_name"`
	Version     string             `json:"version"`
	Backend     string             `json:"backend"`
	Uptime      string             `json:"uptime"`
	StartedAt   string             `json:"started_at"`
	LANIPs      []string           `json:"lan_ips"`
	LPDEnabled  bool               `json:"lpd_enabled"`
	LPDPort     int                `json:"lpd_port"`
	WebPort     int                `json:"web_port"`
	Printers    []PrinterStateView `json:"printers"`
	Skipped     []Printer          `json:"skipped_printers"`
	Jobs        []JobRecord        `json:"jobs"`
	Token       string             `json:"-"`
	RefreshedAt string             `json:"refreshed_at"`
}

type WebServer struct {
	mgr *Manager
	log *Logger
	tpl *template.Template
}

func NewWebServer(m *Manager) *WebServer {
	w := &WebServer{mgr: m, log: m.log}
	w.tpl = template.Must(template.New("status").Funcs(template.FuncMap{
		"ftime": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Format("01-02 15:04:05")
		},
		"bsize": byteSize,
		"first": func(list []string) string {
			if len(list) == 0 {
				return "本机IP"
			}
			return list[0]
		},
	}).Parse(pageHTML))
	return w
}

func (w *WebServer) view() StatusView {
	m := w.mgr
	m.mu.RLock()
	started := m.startedAt
	m.mu.RUnlock()
	uptime := time.Duration(0)
	if !started.IsZero() {
		uptime = time.Since(started)
	}
	return StatusView{
		ServerName:  m.cfg.ServerName,
		Version:     Version,
		Backend:     m.backend.Kind(),
		Uptime:      humanDur(uptime),
		StartedAt:   started.Format("2006-01-02 15:04:05"),
		LANIPs:      LocalIPs(),
		LPDEnabled:  m.cfg.LPDEnabled,
		LPDPort:     m.cfg.LPDPort,
		WebPort:     m.cfg.WebPort,
		Printers:    m.PrinterStates(),
		Skipped:     m.SkippedPrinters(),
		Jobs:        m.tracker.List(50),
		Token:       m.cfg.WebToken,
		RefreshedAt: time.Now().Format("15:04:05"),
	}
}

func humanDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

func (w *WebServer) Serve(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	// 管理端（登录页/配置页/配置 API）——会话挂在 Manager 上，热重启后仍有效
	w.mgr.Admin().register(mux)
	mux.HandleFunc("/", w.handlePage)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/test", w.handleTest)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte("ok"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		w.log.Error("状态页服务退出: %v", err)
	}
}

func (w *WebServer) handlePage(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := w.tpl.Execute(rw, w.view()); err != nil {
		w.log.Warn("状态页渲染失败: %v", err)
	}
}

func (w *WebServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(rw).Encode(w.view())
}

func (w *WebServer) handleTest(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = rw.Write([]byte(`{"ok":false,"error":"仅支持 POST"}`))
		return
	}
	_ = r.ParseForm()
	token := r.FormValue("token")
	printer := strings.TrimSpace(r.FormValue("printer"))
	if token != w.mgr.cfg.WebToken {
		rw.WriteHeader(http.StatusForbidden)
		_, _ = rw.Write([]byte(`{"ok":false,"error":"令牌错误"}`))
		return
	}
	if err := w.mgr.SendTestPage(printer); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		out, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
		_, _ = rw.Write(out)
		return
	}
	_, _ = rw.Write([]byte(`{"ok":true}`))
}

const pageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.ServerName}}</title>
<style>
  *{box-sizing:border-box}
  body{font-family:"Microsoft YaHei","PingFang SC",system-ui,sans-serif;margin:0;background:#f4f6f8;color:#24292f}
  header{background:#1f6feb;color:#fff;padding:14px 22px;display:flex;align-items:center;gap:16px}
  header .info{flex:1}
  header h1{margin:0;font-size:19px}
  header .sub{font-size:12px;opacity:.9;margin-top:4px}
  header a.cfg{color:#fff;text-decoration:none;border:1px solid rgba(255,255,255,.7);padding:7px 14px;border-radius:4px;font-size:13px;white-space:nowrap}
  header a.cfg:hover{background:rgba(255,255,255,.15)}
  main{padding:14px 22px 30px}
  .box{background:#fff;border:1px solid #e1e4e8;border-radius:6px;padding:12px 16px;margin-bottom:14px}
  .box h2{margin:0 0 8px;font-size:15px;border-left:4px solid #1f6feb;padding-left:8px}
  ol,ul{margin:6px 0;padding-left:22px;font-size:13px;line-height:1.8}
  code{background:#eef1f4;padding:1px 5px;border-radius:3px;font-family:Consolas,monospace}
  table{border-collapse:collapse;width:100%;background:#fff;font-size:13px}
  th,td{border:1px solid #e1e4e8;padding:6px 9px;text-align:left;vertical-align:top}
  th{background:#f0f3f6;white-space:nowrap}
  tr:nth-child(even) td{background:#fafbfc}
  .ok{color:#1a7f37;font-weight:600}
  .off{color:#c1121f;font-weight:600}
  .warn{color:#9a6700;font-weight:600}
  button{background:#1f6feb;color:#fff;border:0;border-radius:4px;padding:4px 10px;font-size:12px;cursor:pointer}
  button:hover{background:#0d5bd9}
  .muted{color:#6a737d;font-size:12px}
  .tag{display:inline-block;background:#eef1f4;border-radius:10px;padding:1px 8px;font-size:11px;margin-right:4px}
  h3{font-size:14px;margin:12px 0 4px}
</style>
</head>
<body>
<header>
  <div class="info">
    <h1>{{.ServerName}}</h1>
    <div class="sub">版本 {{.Version}} · 后端 {{.Backend}} · 运行 {{.Uptime}} · 刷新于 {{.RefreshedAt}} · 状态页端口 {{.WebPort}}</div>
  </div>
  <a class="cfg" href="/config">⚙ 配置管理</a>
</header>
<main>

<div class="box">
  <h2>客户端如何连接</h2>
  <div class="muted">本机地址：{{range $i, $ip := .LANIPs}}<code>{{$ip}}</code> {{end}}（以下用 <code>{{first .LANIPs}}</code> 举例）</div>
  <h3>方式一：TCP/IP 端口（推荐，Windows 7 / 10 / 11 通用）</h3>
  <ol>
    <li>客户端打开「设备和打印机 → 添加打印机 → 我需要的打印机不在列表中 → 使用 TCP/IP 地址或主机名」</li>
    <li>主机名填 <code>{{first .LANIPs}}</code>，端口按下面对应表填（协议选 <b>RAW</b>，关闭 SNMP）</li>
    <li>驱动选择打印机真实型号（如 EPSON LQ-630K）</li>
    <li>一键脚本（管理员运行）：Win10/11 → <code>client\install-win10.ps1</code>；Win7 → <code>client\install-win7.bat</code></li>
  </ol>
  <h3>方式二：LPR 端口（备选）</h3>
  <ol>
    <li>客户端安装「LPR 端口监视器」功能，地址 <code>{{first .LANIPs}}</code>，队列名用下表 LPD 队列</li>
  </ol>
</div>

<div class="box">
  <h2>打印端口与打印机</h2>
  <table>
    <tr><th>RAW 端口</th><th>LPD 队列</th><th>本机打印机</th><th>状态</th><th>打印机端口 / 驱动</th><th>队列作业</th><th>操作</th></tr>
    {{range .Printers}}
    <tr>
      <td><code>{{first $.LANIPs}}:{{.Port}}</code></td>
      <td>{{if .Queue}}<code>{{.Queue}}</code>{{else}}<span class="muted">未启用</span>{{end}}</td>
      <td>{{.Printer}}</td>
      <td>
        {{if eq .State "就绪"}}<span class="ok">● {{.State}}</span>
        {{else if eq .State "离线/未连接"}}<span class="off">● {{.State}}</span>
        {{else}}<span class="warn">● {{.State}}</span>{{end}}
        {{if .Detail}}<div class="muted">{{.Detail}}</div>{{end}}
      </td>
      <td class="muted">{{.PortDesc}}<br>{{.Driver}}</td>
      <td>{{.Jobs}}</td>
      <td><button class="test" data-printer="{{.Printer}}" data-token="{{$.Token}}">打印测试页</button></td>
    </tr>
    {{end}}
  </table>
  <div class="muted" style="margin-top:6px">端口分配持久化于 state 文件，重启后不变；新插的打印机会自动追加端口。</div>
  {{if .Skipped}}
  <h3>已自动跳过的打印机（PDF / XPS / 传真 / 虚拟打印类）</h3>
  <table>
    <tr><th>打印机</th><th>驱动</th><th>端口</th></tr>
    {{range .Skipped}}
    <tr><td>{{.Name}}</td><td class="muted">{{.Driver}}</td><td class="muted">{{.Port}}</td></tr>
    {{end}}
  </table>
  <div class="muted" style="margin-top:6px">这类打印机是软件虚拟打印机（打印到文件），不适合当网络打印服务器。<br>
    确实需要暴露：在 config.json 里设 <code>"keep_virtual_printers": true</code>，或用 <code>include_keywords</code> 指定名称，或直接写在 <code>bindings</code> 里。</div>
  {{end}}
</div>

<div class="box">
  <h2>最近作业</h2>
  {{if .Jobs}}
  <table>
    <tr><th>时间</th><th>协议</th><th>来源</th><th>打印机</th><th>文档</th><th>大小</th><th>状态</th><th>说明</th></tr>
    {{range .Jobs}}
    <tr>
      <td>{{ftime .Started}}</td>
      <td>{{.Protocol}}</td>
      <td>{{.Source}}</td>
      <td>{{.Printer}}</td>
      <td>{{.Doc}}</td>
      <td>{{bsize .Bytes}}</td>
      <td>{{if eq .State "失败"}}<span class="off">{{.State}}</span>{{else if eq .State "已完成"}}<span class="ok">{{.State}}</span>{{else}}<span class="warn">{{.State}}</span>{{end}}</td>
      <td class="muted">{{.Detail}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <div class="muted">暂无作业。可在上表点「打印测试页」验证链路。</div>
  {{end}}
</div>

</main>
<script>
document.querySelectorAll('button.test').forEach(function(btn){
  btn.addEventListener('click', function(){
    btn.disabled = true; btn.textContent = '发送中…';
    fetch('/api/test', {
      method: 'POST',
      headers: {'Content-Type': 'application/x-www-form-urlencoded'},
      body: 'printer=' + encodeURIComponent(btn.dataset.printer) + '&token=' + encodeURIComponent(btn.dataset.token)
    }).then(function(r){ return r.json(); }).then(function(d){
      btn.disabled = false; btn.textContent = '打印测试页';
      alert(d.ok ? ('已向「' + btn.dataset.printer + '」发送测试页，请查看打印机出纸。')
                 : ('发送失败：' + (d.error || '未知错误')));
    }).catch(function(e){
      btn.disabled = false; btn.textContent = '打印测试页';
      alert('请求失败：' + e);
    });
  });
});
</script>
</body>
</html>
`
