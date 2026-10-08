@echo off
title NetPrintServer 前台运行（调试）
cd /d "%~dp0"
set "EXE=netprintserver.exe"
if "%PROCESSOR_ARCHITECTURE%"=="x86" if not defined PROCESSOR_ARCHITEW6432 (
  if exist "netprintserver-x86.exe" set "EXE=netprintserver-x86.exe"
)
"%EXE%" run
echo.
pause
