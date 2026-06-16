@echo off
REM ============================================================
REM  ship.bat - test, commit, push in one go
REM
REM    ship.bat "your commit message"
REM    ship.bat                         (uses a default message)
REM
REM  Refuses to push if tests fail - same gate as CI, but local.
REM ============================================================
setlocal
cd /d "%~dp0"

set MSG=%~1
if "%MSG%"=="" set MSG=update

REM Single source of truth for the assistant KB: docs\kb.json is canonical;
REM keep the copy embedded into the binary in sync before building.
if exist docs\kb.json copy /Y docs\kb.json internal\assistant\kb.json >nul

echo === 1/4  go vet ^& go test =====================================
go vet ./...
if errorlevel 1 goto :failed
go test ./...
if errorlevel 1 goto :failed

echo === 2/4  python SDK tests =====================================
where python >nul 2>nul
if %errorlevel%==0 (
  pushd sdk\python
  python -m unittest discover -s tests
  if errorlevel 1 ( popd & goto :failed )
  popd
) else (
  echo (python not found - skipping SDK tests; CI will run them)
)

echo === 3/4  commit ===============================================
git add -A
git commit -m "%MSG%"
if errorlevel 1 (
  echo Nothing to commit - working tree clean.
)

echo === 4/4  push =================================================
git push
if errorlevel 1 goto :failed

echo.
echo  Shipped: "%MSG%"
goto :eof

:failed
echo.
echo  *** NOT pushed - fix the errors above first. ***
exit /b 1
