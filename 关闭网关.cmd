@echo off
setlocal
cd /d "%~dp0"
title wb2api - Stop Gateway

rem 关闭 7863/panel 上的 wb2api 网关（面板本身没有停机按钮，只能从进程侧关）
rem 用法：
rem   关闭网关.cmd                  直接关闭本机 config.json 里那个网关
rem   关闭网关.cmd -WhatIf          干跑，只报告会关掉哪些进程
rem   关闭网关.cmd -All -NoWait     连残留的 wb2api.exe 一起清，不等端口释放

where pwsh >nul 2>&1
if errorlevel 1 (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0stop-gateway.ps1" %*
) else (
  pwsh -NoProfile -ExecutionPolicy Bypass -File "%~dp0stop-gateway.ps1" %*
)
set RC=%ERRORLEVEL%

echo %* | findstr /I /C:"-WhatIf" /C:"-DryRun" >nul 2>&1
if not errorlevel 1 (
  echo.
  echo [DRY-RUN] nothing was stopped. Run without -WhatIf to really stop the gateway.
  pause
  exit /b %RC%
)

echo.
if "%RC%"=="0" (
  echo [OK] Gateway stopped. Restart with the startup cmd in this folder.
) else (
  echo [FAIL] exit code %RC% - see logs\stop-gateway-*.log
)
pause
exit /b %RC%
