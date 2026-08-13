@echo off
REM ============================================================
REM  open-console.bat - open the Centauri Console.
REM  One screen that shows everything your Centauri can do -
REM  and where you mint API keys in the browser.
REM  Starts the full engine first if it isn't running.
REM ============================================================
setlocal
cd /d "%~dp0"
set PORT=7771
netstat -ano | findstr ":%PORT%" | findstr "LISTENING" >nul
if errorlevel 1 (
  echo Engine not running - starting everything first ...
  start "Centauri" cmd /c "%~dp0run-centauri.bat"
  timeout /t 6 /nobreak >nul
)
start http://localhost:%PORT%/console
