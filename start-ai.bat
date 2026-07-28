@echo off
REM ============================================================
REM  start-ai.bat - switch the LOCAL AI on.
REM  Needs a running engine (start-db.bat or start-all.bat first).
REM  Tells Centauri to set up the private AI: it installs Ollama
REM  if missing and downloads this machine's models once, in the
REM  background. Local AI needs NO API key or token of its own.
REM ============================================================
setlocal
cd /d "%~dp0"
set PORT=7771
set AUTH=
if defined CENTAURI_TOKEN set AUTH=-H "Authorization: Bearer %CENTAURI_TOKEN%"

curl -s -o nul -w "" http://localhost:%PORT%/v1/version 2>nul
if errorlevel 1 (
  echo The engine is not running. Start it first: start-db.bat or start-all.bat
  pause & exit /b 1
)
echo Turning on the private, fully-local AI ...
curl -s -X POST http://localhost:%PORT%/v1/ai/enable %AUTH% -H "Content-Type: application/json" -d "{\"tier\":\"auto\"}"
echo.
echo Models download once in the background - watch progress on the
echo dashboard's AI panel:  http://localhost:%PORT%
echo (PDF/image reading too? run:  run-centauri.bat vision)
pause
