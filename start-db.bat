@echo off
REM ============================================================
REM  start-db.bat - the DATABASE ONLY (no AI).
REM  Starts the Centauri engine with the dashboard, Studio, the
REM  CeQL book and the full API - but skips all AI setup.
REM  Use start-ai.bat later to switch the local AI on.
REM ============================================================
setlocal
cd /d "%~dp0"
set DATA=%APPDATA%\Centauri\centauri.log
set PORT=7771

if not exist centauri.exe (
  echo *** centauri.exe not found. Run:  run-centauri.bat build
  pause & exit /b 1
)
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping old server (PID %%p^) on port %PORT% ...
  taskkill /PID %%p /F >nul 2>nul
)
echo.
echo  Database only - no AI. Your data: %DATA%
echo    dashboard:  http://localhost:%PORT%
echo    studio:     http://localhost:%PORT%/studio
echo    simple app: http://localhost:%PORT%/app
echo    CeQL book:  http://localhost:%PORT%/ceql
echo  Keep this window open. Ctrl+C stops the engine.
echo.
centauri.exe serve -ai off -data "%DATA%" -addr :%PORT%
if errorlevel 1 pause
