<#
.SYNOPSIS
  NetPrintServer 客户端接入脚本（Windows 7 / 10 / 11 通用，自动提权）

.DESCRIPTION
  1. 创建指向打印服务器的 TCP/IP 打印端口（RAW 协议）
  2. 用指定驱动创建本地打印机
  3. 打印一张 Windows 测试页验证链路

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1 -Server 192.168.0.10 -Driver "EPSON LQ-630K"

.EXAMPLE
  .\install.ps1 -Server 192.168.0.10 -Port 9101 -PrinterName "LQ630K车间" -Driver "EPSON LQ-630K"

.EXAMPLE
  .\install.ps1 -Server 192.168.0.10 -Remove          # 卸载本脚本创建的打印机与端口
#>
param(
  [string]$Server = "",
  [int]$Port = 9100,
  [string]$Driver = "",
  [string]$PrinterName = "",
  [switch]$Remove,
  [switch]$NoTest
)

$ErrorActionPreference = "Stop"
$scriptPath = $MyInvocation.MyCommand.Path

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $p = New-Object Security.Principal.WindowsPrincipal($id)
  return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# ---- 自动提权（PowerShell 2.0 兼容） ----
if (-not (Test-Admin)) {
  $argList = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ('"' + $scriptPath + '"'))
  if ($Server)     { $argList += "-Server";     $argList += ('"' + $Server + '"') }
  if ($Port -ne 9100) { $argList += "-Port";   $argList += "$Port" }
  if ($Driver)     { $argList += "-Driver";     $argList += ('"' + $Driver + '"') }
  if ($PrinterName){ $argList += "-PrinterName";$argList += ('"' + $PrinterName + '"') }
  if ($Remove)     { $argList += "-Remove" }
  if ($NoTest)     { $argList += "-NoTest" }
  Write-Host "需要管理员权限，正在请求提升（UAC）..." -ForegroundColor Yellow
  Start-Process -FilePath "powershell.exe" -Verb RunAs -ArgumentList $argList
  exit 0
}

function Find-AdminScript([string]$name) {
  $candidates = @(
    (Join-Path $env:windir "System32\Printing_Admin_Scripts\zh-CN\$name"),
    (Join-Path $env:windir "System32\Printing_Admin_Scripts\en-US\$name"),
    (Join-Path $env:windir "System32\$name")
  )
  foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
  $root = Join-Path $env:windir "System32\Printing_Admin_Scripts"
  if (Test-Path $root) {
    $hit = Get-ChildItem -Path $root -Recurse -Filter $name -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($hit) { return $hit.FullName }
  }
  throw "找不到 $name，请确认系统打印管理脚本完整。"
}

function Test-PortExists([string]$portName) {
  $reg = "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName"
  return (Test-Path $reg)
}

function Ensure-TcpPort([string]$server, [int]$port) {
  $portName = "NPS_$($server)_$($port)"
  if (Test-PortExists $portName) {
    Write-Host "端口已存在: $portName"
    return $portName
  }
  if (Get-Command Add-PrinterPort -ErrorAction SilentlyContinue) {
    # -PortNumber 在个别系统版本上可能不被接受，失败时按默认 9100 重试
    $ok = $false
    try {
      Add-PrinterPort -Name $portName -PrinterHostAddress $server -PortNumber $port -ErrorAction Stop | Out-Null
      $ok = $true
    } catch {
      if ($port -eq 9100) {
        try {
          Add-PrinterPort -Name $portName -PrinterHostAddress $server -ErrorAction Stop | Out-Null
          $ok = $true
        } catch { }
      }
      if (-not $ok) { throw "创建 TCP/IP 端口失败: $($_.Exception.Message)" }
    }
    Write-Host "已创建 TCP/IP 端口: $portName -> ${server}:$port (RAW)"
  } else {
    # Windows 7（PowerShell 2.0）：prnport.vbs
    $script = Find-AdminScript "prnport.vbs"
    & cscript //nologo $script -a -r $portName -h $server -o raw -n $port -m d
    if (-not (Test-PortExists $portName)) {
      throw "创建端口失败，请确认以管理员身份运行，且服务器地址 $server 可达（ping 测试）。"
    }
    Write-Host "已创建 TCP/IP 端口: $portName -> ${server}:$port (RAW, SNMP 关闭)"
  }
  return $portName
}

function Get-InstalledDrivers {
  if (Get-Command Get-PrinterDriver -ErrorAction SilentlyContinue) {
    return @(Get-PrinterDriver | ForEach-Object { $_.Name })
  }
  return @(Get-WmiObject -Class Win32_PrinterDriver | ForEach-Object { $_.Name })
}

function Get-PrinterByName([string]$name) {
  if (Get-Command Get-Printer -ErrorAction SilentlyContinue) {
    return (Get-Printer -Name $name -ErrorAction SilentlyContinue)
  }
  $all = @(Get-WmiObject -Class Win32_Printer)
  foreach ($p in $all) { if ($p.Name -eq $name) { return $p } }
  return $null
}

function Remove-PrinterByName([string]$name) {
  if (Get-Command Remove-Printer -ErrorAction SilentlyContinue) {
    Remove-Printer -Name $name -ErrorAction SilentlyContinue
  } else {
    $p = Get-PrinterByName $name
    if ($p) { [void]$p.Delete() }
  }
}

function New-PrinterWmi([string]$name, [string]$driver, [string]$portName) {
  $cls = [WmiClass]"Win32_Printer"
  $inst = $cls.SpawnInstance_()
  $inst.DriverName = $driver
  $inst.PortName = $portName
  $inst.Network = $false
  $inst.Shared = $false
  $inst.Comment = "NetPrintServer"
  [void]$inst.Put()
}

# ================= 主流程 =================

if (-not $Server) {
  Write-Host ""
  Write-Host "NetPrintServer 客户端接入" -ForegroundColor Cyan
  Write-Host "把打印机接到打印服务器（服务器已装 netprintserver.exe 并启动）"
  $Server = Read-Host "请输入打印服务器 IP（如 192.168.0.10）"
  if (-not $Server) { Write-Host "未输入 IP，退出。" -ForegroundColor Red; exit 1 }
}
$Server = $Server.Trim()
if (-not $PrinterName) { $PrinterName = "网络打印-$($Server)_$Port" }

if ($Remove) {
  Write-Host "卸载打印机: $PrinterName"
  Remove-PrinterByName $PrinterName
  $portName = "NPS_$($Server)_$($Port)"
  if (Test-PortExists $portName) {
    if (Get-Command Remove-PrinterPort -ErrorAction SilentlyContinue) {
      Remove-PrinterPort -Name $portName -ErrorAction SilentlyContinue
    } else {
      $script = Find-AdminScript "prnport.vbs"
      & cscript //nologo $script -d -r $portName
    }
  }
  Write-Host "完成。" -ForegroundColor Green
  exit 0
}

# 连通性测试（PowerShell 2.0 兼容，直接用 .NET Ping）
$reachable = $false
try {
  $ping = New-Object System.Net.NetworkInformation.Ping
  $reply = $ping.Send($Server, 2000)
  $reachable = ($reply.Status -eq [System.Net.NetworkInformation.IPStatus]::Success)
} catch { $reachable = $false }
if (-not $reachable) {
  Write-Host "警告: ping 不通 $Server（有些环境禁 ICMP，可继续尝试）" -ForegroundColor Yellow
}

$portName = Ensure-TcpPort $Server $Port

if (-not $Driver) {
  Write-Host ""
  Write-Host "本机已安装的打印驱动：" -ForegroundColor Cyan
  $drivers = Get-InstalledDrivers
  for ($i = 0; $i -lt $drivers.Count; $i++) { Write-Host ("  [{0}] {1}" -f ($i + 1), $drivers[$i]) }
  Write-Host ""
  if ($drivers.Count -eq 0) {
    Write-Host "本机没有可用驱动，请先安装打印机驱动（如 EPSON LQ-630K 驱动光盘/官网下载）。" -ForegroundColor Red
    exit 2
  }
  $sel = Read-Host "选择驱动编号（回车=第 1 个）"
  if ($sel -eq "") { $sel = 1 }
  $idx = 0
  if (-not [int]::TryParse($sel, [ref]$idx)) { $idx = 1 }
  if ($idx -lt 1 -or $idx -gt $drivers.Count) { $idx = 1 }
  $Driver = $drivers[$idx - 1]
}
Write-Host "使用驱动: $Driver"

if (Get-PrinterByName $PrinterName) {
  Write-Host "打印机「$PrinterName」已存在，删除后重建..."
  Remove-PrinterByName $PrinterName
}

if (Get-Command Add-Printer -ErrorAction SilentlyContinue) {
  Add-Printer -Name $PrinterName -DriverName $Driver -PortName $portName | Out-Null
} else {
  New-PrinterWmi $PrinterName $Driver $portName
  Start-Sleep -Seconds 1
}

if (-not (Get-PrinterByName $PrinterName)) {
  Write-Host "创建打印机失败。常见原因：驱动名与实际不匹配（上面列出的名字要完全一致）。" -ForegroundColor Red
  exit 3
}

Write-Host ""
Write-Host "完成！" -ForegroundColor Green
Write-Host "  打印机 : $PrinterName"
Write-Host "  驱动   : $Driver"
Write-Host "  端口   : $portName  (${Server}:$Port, RAW)"

if (-not $NoTest) {
  $ans = Read-Host "是否打印 Windows 测试页验证？(y/n, 回车=y)"
  if ($ans -ne "n" -and $ans -ne "N") {
    & rundll32 printui.dll,PrintUIEntry /k /n "$PrinterName"
    Write-Host "已发送测试页，请查看打印机出纸。" -ForegroundColor Green
  }
}
