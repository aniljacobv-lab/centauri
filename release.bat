@echo off
REM ============================================================
REM  Centauri release builder - cross-compiles every platform
REM  Output: dist\centauri-<version>-<os>-<arch>[.exe]
REM ============================================================
setlocal enabledelayedexpansion
cd /d "%~dp0"

set VERSION=%1
if "%VERSION%"=="" set VERSION=v0.3.0

if exist dist rmdir /s /q dist
mkdir dist

echo Building Centauri %VERSION% for all platforms...
echo.

call :build windows amd64 .exe
call :build windows arm64 .exe
call :build linux   amd64
call :build linux   arm64
call :build darwin  amd64
call :build darwin  arm64

echo.
echo Generating checksums...
powershell -Command "Get-ChildItem dist\centauri-* | ForEach-Object { '{0}  {1}' -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name } | Set-Content dist\SHA256SUMS.txt"

echo.
echo Done. Contents of dist\:
dir /b dist
echo.
echo Upload these to a GitHub release. Users download ONE file and run:
echo    centauri-%VERSION%-windows-amd64.exe serve
echo The dashboard, CeQL book, Genesis - everything is inside it.
goto :eof

:build
set GOOS=%1
set GOARCH=%2
set EXT=%3
echo   %1/%2 ...
go build -trimpath -ldflags "-s -w" -o dist\centauri-%VERSION%-%1-%2%EXT% .\cmd\centauri
if errorlevel 1 (
  echo   *** BUILD FAILED for %1/%2 ***
  exit /b 1
)
goto :eof
