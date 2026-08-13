@echo off
REM ============================================================
REM  start-gateway.bat - the API GATEWAY (single-port mode).
REM  A reverse proxy on port 8080. The dashboard and the Console
REM  (http://localhost:8080/console) ride on the SAME port: any
REM  path that matches no route serves the admin UI instead.
REM  Routes and API keys live in the control-plane file below -
REM  add routes right in the browser, no restart needed.
REM
REM  Optional:  start-gateway.bat key <name>
REM    mints an API key called <name> (printed ONCE) and exits
REM    instead of starting the gateway. Minting takes the writer
REM    lock, so the gateway must not be running. Keys can ALSO be
REM    minted in the browser Console (/console) while the gateway
REM    runs - that way you never have to stop anything.
REM ============================================================
setlocal
cd /d "%~dp0"
set DATA=%APPDATA%\Centauri\gateway.log
set PORT=8080

if not exist centauri.exe (
  echo *** centauri.exe not found. Run:  run-centauri.bat build
  pause & exit /b 1
)
if /i "%1"=="key" (
  if "%2"=="" (
    echo *** Usage:  start-gateway.bat key ^<name^>
    pause & exit /b 1
  )
  centauri.exe gateway -data "%DATA%" -newkey %2
  if errorlevel 1 pause
  exit /b
)
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping old server (PID %%p^) on port %PORT% ...
  taskkill /PID %%p /F >nul 2>nul
)
echo.
echo  API gateway - one port for everything. Control plane: %DATA%
echo    proxy:      http://localhost:%PORT%          (your routes win)
echo    dashboard:  http://localhost:%PORT%          (when no route matches)
echo    console:    http://localhost:%PORT%/console
echo  Add routes in the browser; mint keys there too, or:  start-gateway.bat key ^<name^>
echo  Keep this window open. Ctrl+C stops the gateway.
echo.
centauri.exe gateway -data "%DATA%" -addr :%PORT% -admin-inline
if errorlevel 1 pause
