@echo off
REM ============================================================
REM  stop-centauri.bat - stop a running Centauri engine.
REM ============================================================
setlocal
set PORT=7771
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping Centauri (PID %%p^) ...
  taskkill /PID %%p /F
)
echo Done.
pause
