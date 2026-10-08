@echo off
title NetPrintServer 安装为服务
cd /d "%~dp0"
net session >nul 2>&1
if errorlevel 1 (
  echo 正在请求管理员权限（UAC）...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -WorkingDirectory '%~dp0' -Verb RunAs"
  exit /b
)
set "EXE=netprintserver.exe"
if "%PROCESSOR_ARCHITECTURE%"=="x86" if not defined PROCESSOR_ARCHITEW6432 (
  if exist "netprintserver-x86.exe" set "EXE=netprintserver-x86.exe"
)
echo 使用程序: %EXE%
"%EXE%" install
echo.
pause
