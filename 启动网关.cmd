@echo off
setlocal
cd /d "%~dp0"
title wb2api - WorkBuddy2API Panel

rem ---- idempotent: if 7863 is already listening, only open the panel ----
netstat -ano | findstr /C:"127.0.0.1:7863" | findstr /C:"LISTENING" >nul 2>&1
if not errorlevel 1 goto already

if exist "wb2api.exe" goto start

:build
echo [1/2] wb2api.exe not found -> building (Go 1.24+; go.mod wants 1.26.10, toolchain auto-download)
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
if errorlevel 1 (
  echo Build FAILED. Check your Go installation.
  pause
  exit /b 1
)

:start
if not exist "data" mkdir data
if not exist "logs" mkdir logs
echo [2/2] Gateway: http://127.0.0.1:7863/panel/
echo   API key  : see "api_key" in config.json (editable in panel, hot reload)
echo   Stop     : close this window
echo.
"%~dp0wb2api.exe" -config config.json
echo.
echo Gateway exited, code %errorlevel%
pause
exit /b 0

:already
echo Gateway is ALREADY running - opening panel instead of starting a 2nd instance.
echo   http://127.0.0.1:7863/panel/
start "" http://127.0.0.1:7863/panel/
ping -n 3 127.0.0.1 >nul
exit /b 0