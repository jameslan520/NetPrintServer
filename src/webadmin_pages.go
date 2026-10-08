package main

// 登录页
const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>登录 - NetPrintServer</title>
<style>
  *{box-sizing:border-box}
  body{font-family:"Microsoft YaHei",system-ui,sans-serif;background:#f4f6f8;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
  .card{background:#fff;border:1px solid #e1e4e8;border-radius:8px;padding:28px 30px;width:360px;box-shadow:0 4px 16px rgba(0,0,0,.06)}
  h1{font-size:17px;margin:0 0 4px}
  .sub{color:#6a737d;font-size:12px;margin-bottom:18px}
  label{display:block;font-size:13px;margin:12px 0 4px;color:#444}
  input{width:100%;padding:8px 10px;border:1px solid #d0d7de;border-radius:4px;font-size:14px}
  input:focus{outline:none;border-color:#1f6feb}
  button{width:100%;margin-top:18px;background:#1f6feb;color:#fff;border:0;border-radius:4px;padding:10px;font-size:14px;cursor:pointer}
  button:hover{background:#0d5bd9}
  .err{background:#fff1f0;border:1px solid #ffccc7;color:#a8071a;font-size:13px;padding:8px 10px;border-radius:4px;margin-top:12px}
  .tip{color:#6a737d;font-size:12px;margin-top:14px;line-height:1.6}
</style>
</head>
<body>
<div class="card">
  <h1>NetPrintServer 配置管理</h1>
  <div class="sub">打印服务器网页控制台 · 仅授权用户可修改配置</div>
  <form method="post" action="/login">
    <label>用户名</label>
    <input name="user" autocomplete="username" value="{{.Admin}}" required>
    <label>口令</label>
    <input name="pass" type="password" autocomplete="current-password" required autofocus>
    <button type="submit">登 录</button>
  </form>
  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  <div class="tip">口令忘了？在服务器上运行 <code>netprintserver.exe password 新口令</code> 重置，然后重启服务。</div>
</div>
</body>
</html>`

// 配置页
const configHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>配置管理 - NetPrintServer</title>
<style>
  *{box-sizing:border-box}
  body{font-family:"Microsoft YaHei",system-ui,sans-serif;margin:0;background:#f4f6f8;color:#24292f}
  header{background:#1f6feb;color:#fff;padding:12px 22px;display:flex;align-items:center;gap:14px}
  header h1{font-size:17px;margin:0;flex:1}
  header a{color:#fff;text-decoration:none;font-size:13px;opacity:.95;border:1px solid rgba(255,255,255,.6);padding:4px 10px;border-radius:4px}
  header a:hover{background:rgba(255,255,255,.15)}
  main{padding:16px 22px 40px;max-width:1080px;margin:0 auto}
  .box{background:#fff;border:1px solid #e1e4e8;border-radius:6px;padding:14px 16px;margin-bottom:14px}
  .box h2{margin:0 0 10px;font-size:15px;border-left:4px solid #1f6feb;padding-left:8px}
  .box h3{margin:14px 0 6px;font-size:13px;color:#444}
  table{border-collapse:collapse;width:100%;font-size:13px}
  th,td{border:1px solid #e1e4e8;padding:6px 8px;text-align:left;vertical-align:top}
  th{background:#f0f3f6;white-space:nowrap}
  tr:nth-child(even) td{background:#fafbfc}
  .muted{color:#6a737d;font-size:12px}
  .tag{display:inline-block;background:#fff1e5;color:#9a5b00;border:1px solid #ffd8a8;border-radius:9px;padding:0 7px;font-size:11px;margin-left:6px}
  .tag.on{background:#e8f5ec;color:#1a7f37;border-color:#b7e1c2}
  .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:12px 18px}
  label.f{display:block;font-size:13px;margin-bottom:4px;color:#444}
  input[type=text],input[type=number],textarea,select{width:100%;padding:7px 9px;border:1px solid #d0d7de;border-radius:4px;font-size:13px;font-family:inherit}
  textarea{min-height:66px;resize:vertical;font-family:Consolas,monospace}
  input:focus,textarea:focus,select:focus{outline:none;border-color:#1f6feb}
  .radios{display:flex;flex-direction:column;gap:6px;font-size:13px}
  .radios label{display:flex;gap:8px;align-items:flex-start;cursor:pointer}
  .radios .desc{color:#6a737d;font-size:12px}
  .actions{display:flex;gap:10px;align-items:center;margin-top:6px}
  button.primary{background:#1f6feb;color:#fff;border:0;border-radius:4px;padding:9px 22px;font-size:14px;cursor:pointer}
  button.primary:hover{background:#0d5bd9}
  button.primary:disabled{background:#8aaef0;cursor:not-allowed}
  button.ghost{background:#fff;border:1px solid #d0d7de;border-radius:4px;padding:8px 14px;font-size:13px;cursor:pointer}
  #msg{display:none;padding:9px 12px;border-radius:4px;font-size:13px;margin-bottom:12px;white-space:pre-wrap}
  #msg.ok{display:block;background:#e8f5ec;border:1px solid #b7e1c2;color:#1a7f37}
  #msg.err{display:block;background:#fff1f0;border:1px solid #ffccc7;color:#a8071a}
  .applybar{position:sticky;bottom:0;background:#fff;border-top:2px solid #1f6feb;padding:10px 0 0;margin-top:14px;display:flex;gap:12px;align-items:center}
  details summary{cursor:pointer;font-size:14px;color:#1f6feb;margin-bottom:6px}
  .foot{color:#6a737d;font-size:12px;margin-top:10px;line-height:1.7}
</style>
</head>
<body>
<header>
  <h1>NetPrintServer 配置管理</h1>
  <span style="font-size:13px">当前用户：{{.User}}</span>
  <a href="/" target="_blank">状态页</a>
  <form method="post" action="/logout" style="display:inline"><button class="ghost" style="background:transparent;color:#fff;border-color:rgba(255,255,255,.6)">退出登录</button></form>
</header>
<main>
<div id="msg"></div>

{{if .LastApply}}
<div class="box" style="border-color:#ffe58f;background:#fffbe6">上次应用：{{.LastApply}}</div>
{{end}}

<div class="box">
  <h2>1. 选择要转换为网络打印的打印机</h2>
  <div class="muted" style="margin-bottom:10px">已选 <b>{{.SelectedCount}}</b> / 共 {{.TotalPrinters}} 台。勾选后记得点最下方「保存并应用」。</div>
  <div class="radios">
    <label><input type="radio" name="mode" value="selected" {{if eq .Mode "selected"}}checked{{end}}>
      <span>只转换<b>勾选</b>的打印机（推荐）<span class="desc"> — 其它打印机完全不暴露</span></span></label>
    <label><input type="radio" name="mode" value="auto" {{if eq .Mode "auto"}}checked{{end}}>
      <span>自动：全部<b>真实</b>打印机<span class="desc"> — 按排除规则跳过 PDF/XPS/传真等虚拟打印机</span></span></label>
    <label><input type="radio" name="mode" value="all" {{if eq .Mode "all"}}checked{{end}}>
      <span>全部打印机（含虚拟）<span class="desc"> — 不做任何过滤</span></span></label>
  </div>
  <h3>本机打印机列表</h3>
  <table>
    <tr><th>转换为网络打印</th><th>打印机</th><th>驱动</th><th>端口</th><th>状态</th><th>当前分配</th></tr>
    {{range .Printers}}
    <tr>
      <td><input type="checkbox" name="sel" value="{{.Name}}" {{if .Selected}}checked{{end}}></td>
      <td>{{.Name}}{{if .Skipped}}<span class="tag">{{.Reason}}</span>{{else}}{{if .Selected}}<span class="tag on">已转换</span>{{end}}{{end}}</td>
      <td class="muted">{{.Driver}}</td>
      <td class="muted">{{.Port}}</td>
      <td>{{if eq .State "就绪"}}<span style="color:#1a7f37">{{.State}}</span>{{else}}<span style="color:#9a5b00">{{.State}}</span>{{end}}</td>
      <td class="muted">{{if .BoundPort}}端口 {{.BoundPort}}{{if .BoundQ}} / 队列 {{.BoundQ}}{{end}}{{else}}—{{end}}</td>
    </tr>
    {{end}}
  </table>
</div>

<div class="box">
  <h2>2. 网络与端口</h2>
  <div class="grid">
    <div><label class="f">服务器名称（状态页标题）</label><input type="text" id="server_name" value="{{.ServerName}}"></div>
    <div><label class="f">绑定网卡 IP（留空 = 全部网卡；指定后本机仍可用 127.0.0.1/localhost 访问）</label><input type="text" id="bind_ip" value="{{.BindIP}}" placeholder="如 192.168.0.19"></div>
    <div><label class="f">RAW 起始端口（客户端连这个）</label><input type="number" id="start_port" value="{{.StartPort}}" min="1" max="65535"></div>
    <div><label class="f">状态页/配置页端口</label><input type="number" id="web_port" value="{{.WebPort}}" min="1" max="65535"></div>
    <div>
      <label class="f"><input type="checkbox" id="lpd_enabled" {{if .LPDEnabled}}checked{{end}}> 启用 LPD/LPR（备选协议）</label>
      <label class="f" style="margin-top:6px">LPD 端口</label>
      <input type="number" id="lpd_port" value="{{.LPDPort}}" min="1" max="65535">
    </div>
    <div><label class="f"><input type="checkbox" id="firewall" {{if .Firewall}}checked{{end}}> 自动更新 Windows 防火墙规则</label></div>
  </div>
</div>

<div class="box">
  <details {{if or .IncludeKW .KeepVirtual}}open{{end}}>
    <summary>3. 高级设置（一般不用动）</summary>
    <div class="grid">
      <div><label class="f">包含关键字（只转换名称/驱动含这些词的打印机，留空=不限）</label>
        <textarea id="include_kw" placeholder="LQ-630K, SHARP">{{.IncludeKW}}</textarea></div>
      <div><label class="f">排除关键字（命中即跳过，逗号分隔；留空=默认规则）</label>
        <textarea id="exclude_kw">{{.ExcludeKW}}</textarea></div>
      <div>
        <label class="f"><input type="checkbox" id="keep_virtual" {{if .KeepVirtual}}checked{{end}}> 不过滤虚拟打印机（等同于上面选“全部打印机”）</label>
        <label class="f" style="margin-top:8px">单个作业上限（MB）</label>
        <input type="number" id="max_job_mb" value="{{.MaxJobMB}}" min="1" max="4096">
        <label class="f" style="margin-top:8px">客户端空闲超时（秒）</label>
        <input type="number" id="idle_timeout" value="{{.IdleTimeout}}" min="5" max="600">
      </div>
      <div><label class="f">手动绑定 bindings（JSON，可固定某台打印机的端口；一般留原样）</label>
        <textarea id="bindings_json" style="min-height:110px">{{.BindingsJSON}}</textarea></div>
    </div>
  </details>
</div>

<div class="box">
  <h2>4. 安全</h2>
  <div class="grid">
    <div><label class="f">管理员用户名（登录用）</label><input type="text" id="admin_name" value="{{.Admin}}" readonly style="background:#f6f8fa"></div>
    <div><label class="f">修改口令（至少 6 位，留空不改）</label>
      <div style="display:flex;gap:8px">
        <input type="text" id="new_pass" placeholder="输入新口令" style="flex:1">
        <button class="ghost" type="button" onclick="changePass()">修改</button>
      </div>
      <div class="muted" style="margin-top:6px">口令只保存在本机 config.json（加盐哈希），连续输错 5 次会锁定 60 秒。</div>
    </div>
  </div>
</div>

<div class="applybar">
  <button class="primary" id="saveBtn" onclick="save()">保存并应用</button>
  <span class="muted">保存后服务会立即热重启监听（无需手动重启服务）；若改了端口/网卡，页面可能短暂断开，断开后用新地址访问即可。</span>
</div>

<div class="foot">
  本机地址：{{.PrimaryIP}} · 状态页端口 {{.WebPort}} · RAW 起始端口 {{.StartPort}}<br>
  配置保存在服务器上的 config.json；程序版本 NetPrintServer
</div>
</main>

<script>
function el(id){ return document.getElementById(id); }
function show(kind, text){ var m = el('msg'); m.className = kind; m.textContent = text; window.scrollTo({top:0,behavior:'smooth'}); }
function splitKw(s){ return s.split(/[,，;；\n]/).map(function(x){return x.trim();}).filter(function(x){return x.length>0;}); }

function collect(){
  var sel = [];
  document.querySelectorAll('input[name=sel]:checked').forEach(function(c){ sel.push(c.value); });
  var mode = document.querySelector('input[name=mode]:checked');
  return {
    server_name: el('server_name').value.trim(),
    bind_ip: el('bind_ip').value.trim(),
    start_port: parseInt(el('start_port').value, 10),
    lpd_enabled: el('lpd_enabled').checked,
    lpd_port: parseInt(el('lpd_port').value, 10),
    web_enabled: true,
    web_port: parseInt(el('web_port').value, 10),
    firewall: el('firewall').checked,
    max_job_mb: parseInt(el('max_job_mb').value, 10),
    idle_timeout_sec: parseInt(el('idle_timeout').value, 10),
    selection_mode: mode ? mode.value : 'auto',
    selected_printers: sel,
    include_keywords: splitKw(el('include_kw').value),
    exclude_keywords: splitKw(el('exclude_kw').value),
    keep_virtual_printers: el('keep_virtual').checked,
    bindings_json: el('bindings_json').value
  };
}

function save(){
  var data = collect();
  if (data.selection_mode === 'selected' && data.selected_printers.length === 0) {
    show('err', '已选择「只转换勾选的打印机」，但一台都没勾。请勾选至少一台，或切换到其它模式。');
    return;
  }
  var btn = el('saveBtn'); btn.disabled = true; btn.textContent = '保存中…';
  fetch('/api/config', {
    method: 'POST',
    headers: {'Content-Type':'application/json', 'X-NPS-Auth':'1'},
    body: JSON.stringify(data)
  }).then(function(r){ return r.json(); }).then(function(d){
    btn.disabled = false; btn.textContent = '保存并应用';
    if (d.ok) { show('ok', d.note || '已保存'); }
    else { show('err', '保存失败：' + (d.error || '未知错误')); }
  }).catch(function(e){
    btn.disabled = false; btn.textContent = '保存并应用';
    show('err', '请求失败：' + e + '（如果刚改过端口，请用新地址访问 /config）');
  });
}

function changePass(){
  var pw = el('new_pass').value;
  if (pw.length < 6) { show('err', '新口令至少 6 位'); return; }
  fetch('/api/password', {
    method: 'POST',
    headers: {'Content-Type':'application/json', 'X-NPS-Auth':'1'},
    body: JSON.stringify({password: pw})
  }).then(function(r){ return r.json(); }).then(function(d){
    if (d.ok) { el('new_pass').value=''; show('ok', d.note || '口令已修改'); }
    else { show('err', '修改失败：' + (d.error || '')); }
  }).catch(function(e){ show('err', '请求失败：' + e); });
}
</script>
</body>
</html>`
