@echo off
REM ============================================================
REM  Centauri release - ONE command does everything:
REM    1. cross-compiles all 6 platforms
REM    2. builds the Windows installer (.exe)
REM    3. packs the Windows portable zip (exe + launcher + guides)
REM    4. generates SHA-256 checksums
REM    5. creates the GitHub release and uploads every file
REM
REM    release.bat            (uses v0.3.0)
REM    release.bat v0.4.0     (any tag)
REM
REM  Needs on your machine: Go, Inno Setup 6, GitHub CLI (gh).
REM  If Inno Setup or gh are missing, the build still runs and
REM  leaves everything in dist\ with instructions.
REM ============================================================
setlocal enabledelayedexpansion
cd /d "%~dp0"

set VERSION=%1
if "%VERSION%"=="" set VERSION=v0.3.0
set VERNUM=%VERSION:v=%

if exist dist rmdir /s /q dist
mkdir dist

echo === 1/5  build all platforms ==================================
call :build windows amd64 .exe
call :build windows arm64 .exe
call :build linux   amd64
call :build linux   arm64
call :build darwin  amd64
call :build darwin  arm64

echo.
echo === 2/5  Windows installer ====================================
set "ISCC="
if exist "%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe" set "ISCC=%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe"
if not defined ISCC if exist "%ProgramFiles%\Inno Setup 6\ISCC.exe" set "ISCC=%ProgramFiles%\Inno Setup 6\ISCC.exe"
if defined ISCC (
  "%ISCC%" /DMyAppVersion=%VERNUM% installer\centauri.iss
  if errorlevel 1 goto :failed
  echo   built dist\centauri-windows-setup.exe
) else (
  echo   *** Inno Setup not found - skipping installer.
  echo       Install from https://jrsoftware.org/isdl.php to include it.
)

echo.
echo === 3/5  Windows portable zip =================================
REM Everything a non-technical user needs in one folder: the exe (named
REM centauri.exe so run-centauri.bat finds it), the launcher, the README,
REM and the plain-English quickstart.
mkdir dist\zip
copy /Y dist\centauri-windows-amd64.exe dist\zip\centauri.exe >nul
copy /Y run-centauri.bat dist\zip\ >nul
copy /Y README.md dist\zip\ >nul
copy /Y docs\quickstart.md dist\zip\QUICKSTART.md >nul
powershell -Command "Compress-Archive -Path dist\zip\* -DestinationPath dist\centauri-windows-amd64.zip -Force"
if errorlevel 1 goto :failed
rmdir /s /q dist\zip
echo   built dist\centauri-windows-amd64.zip

echo.
echo === 4/5  checksums ============================================
powershell -Command "Get-ChildItem dist\centauri-* | ForEach-Object { '{0}  {1}' -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name } | Set-Content dist\SHA256SUMS.txt"

echo.
echo === 5/5  publish GitHub release ===============================
where gh >nul 2>nul
if errorlevel 1 (
  echo   *** GitHub CLI ^(gh^) not found - release NOT published.
  echo       Install from https://cli.github.com/ and run:
  echo         gh release create %VERSION% dist\* --title "Centauri %VERSION%" --generate-notes
  echo       ...or upload dist\ manually at github.com/aniljacobv-lab/centauri/releases/new
  echo.
  echo   Files are ready in dist\ :
  dir /b dist
  goto :eof
)
gh release create %VERSION% dist\* --title "Centauri %VERSION%" --generate-notes
if errorlevel 1 goto :failed

echo.
echo  Released %VERSION% - download links on the site are now live.
goto :eof

:build
set GOOS=%1
set GOARCH=%2
set EXT=%3
echo   %1/%2 ...
go build -trimpath -ldflags "-s -w" -o dist\centauri-%1-%2%EXT% .\cmd\centauri
if errorlevel 1 (
  echo   *** BUILD FAILED for %1/%2 ***
  exit /b 1
)
goto :eof

:failed
echo.
echo  *** Release aborted - fix the errors above. ***
exit /b 1
