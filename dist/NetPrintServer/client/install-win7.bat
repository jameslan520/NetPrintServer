@echo off
title NetPrintServer 客户端接入
rem Windows 7 双击运行；会通过 install.ps1 自动请求管理员权限(UAC)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
echo.
pause
