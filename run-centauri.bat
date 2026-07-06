@echo off
REM ============================================================
REM  Centauri launcher - double-click to run
REM
REM    run-centauri.bat            start Centauri (desktop) + open the app
REM    run-centauri.bat seed       fill the database with sample data
REM    run-centauri.bat build      rebuild centauri.exe only
REM    run-centauri.bat stop       stop a running Centauri server
REM    run-centauri.bat vision     (re)run the local AI / Vision setup by hand
REM
REM  Plain English: double-clicking this file starts your own private AI.
REM  A black window opens (that's the engine - keep it open) and the app
REM  appears in your browser. On the very first run Centauri offers to set
REM  up local AI for you: it installs anything missing (Ollama) and
REM  downloads the models once - that download can take a few minutes.
REM
REM  Your data lives in your Windows profile (%APPDATA%\Centauri), the same
REM  place for both running and seeding. Set DATA below to use a different
REM  folder (e.g. a OneDrive folder) - Centauri will note the single-writer
REM  caveat. Nothing you put in ever leaves this computer.
REM ============================================================
setlocal
cd /d "%~dp0"

set DATA=%APPDATA%\Centauri\centauri.log
set PORT=7771

if /i "%~1"=="seed"   goto seed
if /i "%~1"=="build"  goto build
if /i "%~1"=="stop"   goto stop
if /i "%~1"=="vision" goto vision

REM ---------- default: (re)build if Go is available, then run desktop ----------
REM (The release zip and the installer ship a ready-made centauri.exe, so
REM  most people skip straight past this build step.)
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
  echo Either use the Windows installer / release zip - they include the exe -
  echo or install Go from https://go.dev/dl/ and run this again.
  pause
  exit /b 1
)

REM ---------- stop any server already using the port ----------
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping old server (PID %%p^) on port %PORT% ...
  taskkill /PID %%p /F >nul 2>nul
)

echo.
echo  Starting Centauri - the app will open in your browser.
echo  Your data: %DATA%
echo  Keep this window open while you use Centauri. Press Ctrl+C to stop.
echo.
REM 'desktop' does the rest itself: opens the browser, asks once about local
REM AI setup (installs Ollama + downloads models with your OK), and manages
REM Ollama for you. Run "run-centauri.bat vision" later for PDF/image reading.
centauri.exe desktop -data "%DATA%" -addr :%PORT%
if errorlevel 1 (
  echo.
  echo *** Centauri stopped with an error - read the messages above. ***
  echo *** This window stays open so you can see what happened.      ***
  pause
)
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

:vision
REM Local AI "Vision" lets Centauri read images and PDFs. `centauri desktop`
REM already sets up the chat/search models; this adds/verifies the PDF
REM renderer and vision model. Only missing pieces are installed.
if not exist centauri.exe call "%~f0" build
echo Running local AI / Vision setup (installs only what's missing)...
centauri.exe setup vision -install
pause
goto :eof
