@echo off
REM ============================================================
REM  Centauri launcher - double-click to run
REM
REM    run-centauri.bat            start Centauri (desktop) + open the dashboard
REM    run-centauri.bat seed       fill the database with sample data
REM    run-centauri.bat build      rebuild centauri.exe only
REM    run-centauri.bat stop       stop a running Centauri server
REM
REM  Uses the 'desktop' command: your data lives in your Windows profile
REM  (%APPDATA%\Centauri), the same place for both running and seeding, and
REM  the dashboard opens itself. Set DATA below to use a different folder
REM  (e.g. a OneDrive folder) - Centauri will note the single-writer caveat.
REM
REM  On launch it also offers (with your permission) to set up local AI
REM  "Vision" - installing only the missing pieces (Ollama + a PDF renderer)
REM  and downloading the models. Already-installed tools are skipped.
REM ============================================================
setlocal
cd /d "%~dp0"

set DATA=%APPDATA%\Centauri\centauri.log
set PORT=7771

if /i "%~1"=="seed"  goto seed
if /i "%~1"=="build" goto build
if /i "%~1"=="stop"  goto stop

REM ---------- default: (re)build if Go is available, then run desktop ----------
where go >nul 2>nul
if %errorlevel%==0 (
  echo Building centauri.exe ...
  go build -o centauri.exe ./cmd/centauri
  if errorlevel 1 (
    echo.
    echo *** Build failed - fix the errors above and run again. ***
    pause
    exit /b 1
  )
) else (
  echo Go not found - using the existing centauri.exe
)
if not exist centauri.exe (
  echo.
  echo *** centauri.exe not found and Go is not installed. ***
  echo Install Go from https://go.dev/dl/ and run this again.
  pause
  exit /b 1
)

REM ---------- one-click: check local AI "Vision" prerequisites ----------
REM 'setup vision' (detect mode) exits non-zero if Ollama, the models, or a PDF
REM renderer are missing. It only installs what's absent; installed pieces are
REM skipped. Goto labels (not a parenthesised block) keep set /p working.
centauri.exe setup vision >nul 2>nul
if not errorlevel 1 goto vision_done
echo.
echo  Optional: local AI "Vision" lets Centauri read images and PDFs.
echo  It needs Ollama + a PDF renderer; the models are a one-time ~5 GB download.
echo  (Whatever you already have installed is skipped.)
set /p VYN=  Set this up now? [y/N]:
if /i "%VYN%"=="y" centauri.exe setup vision -install
:vision_done

REM ---------- stop any server already using the port ----------
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping old server (PID %%p^) on port %PORT% ...
  taskkill /PID %%p /F >nul 2>nul
)

echo.
echo  Starting Centauri - the dashboard will open in your browser.
echo  Your data: %DATA%
echo  Press Ctrl+C in this window to stop.
echo.
REM 'desktop' opens the browser itself and prints the URLs + vision setup hint.
centauri.exe desktop -data "%DATA%" -addr :%PORT%
goto :eof

:seed
if not exist centauri.exe call "%~f0" build
echo Seeding demo data into %DATA% ...
centauri.exe seed -data "%DATA%"
pause
goto :eof

:build
where go >nul 2>nul
if not %errorlevel%==0 (
  echo Go is not installed - get it from https://go.dev/dl/
  pause
  exit /b 1
)
go build -o centauri.exe ./cmd/centauri
if errorlevel 1 ( echo Build failed. ) else ( echo Build OK: centauri.exe )
pause
goto :eof

:stop
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping Centauri (PID %%p^) ...
  taskkill /PID %%p /F
)
echo Done.
pause
goto :eof
