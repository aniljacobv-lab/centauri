@echo off
REM ============================================================
REM  Centauri launcher - double-click to run
REM
REM    run-centauri.bat            start the server + open the dashboard
REM    run-centauri.bat seed       fill the demo database with sample data
REM    run-centauri.bat build      rebuild centauri.exe only
REM    run-centauri.bat stop       stop a running Centauri server
REM ============================================================
setlocal
cd /d "%~dp0"

set DATA=centauri.log
set PORT=7771

if /i "%~1"=="seed"  goto seed
if /i "%~1"=="build" goto build
if /i "%~1"=="stop"  goto stop

REM ---------- default: (re)build if Go is available, then serve ----------
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

REM ---------- stop any server already using the port ----------
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":%PORT%" ^| findstr "LISTENING"') do (
  echo Stopping old server (PID %%p^) on port %PORT% ...
  taskkill /PID %%p /F >nul 2>nul
)

REM ---------- open the dashboard once the server is up ----------
start "" cmd /c "timeout /t 2 >nul & start http://localhost:%PORT%"

echo.
echo  Centauri dashboard:  http://localhost:%PORT%
echo  CeQL textbook:       http://localhost:%PORT%/ceql
echo  Press Ctrl+C in this window to stop the server.
echo.
centauri.exe serve -data %DATA% -addr :%PORT%
goto :eof

:seed
if not exist centauri.exe call "%~f0" build
echo Seeding demo data into %DATA% ...
centauri.exe seed -data %DATA%
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
