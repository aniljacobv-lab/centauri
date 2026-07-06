@echo off
REM ============================================================
REM  deploy.bat - THE one command: verify, benchmark, ship.
REM
REM    deploy.bat                      checks + benchmarks + push ("update")
REM    deploy.bat "your message"       checks + benchmarks + push
REM    deploy.bat "your message" v0.4.0
REM        all of the above, then tags the release - GitHub Actions
REM        builds the 6 binaries + Windows installer + checksums and
REM        publishes the release automatically. Nothing else to do.
REM
REM  What happens AFTER the push, automatically (GitHub Actions):
REM    - CI: vet, tests, race detector, cross-compile, Python SDK,
REM      zero-dependency invariant   (.github/workflows/ci.yml)
REM    - Claude code review on every PR (claude-review.yml)
REM    - Benchmark comparison vs base on every Go PR (perf.yml)
REM    - Book/KB copies kept in sync (sync.yml)
REM    - Website (docs/) republished by GitHub Pages
REM    - Render redeploys if your Render service is connected to the repo
REM
REM  Set CENTAURI_SKIP_BENCH=1 to skip the benchmark step (slow-ish).
REM ============================================================
setlocal
cd /d "%~dp0"

set MSG=%~1
if "%MSG%"=="" set MSG=update
set VERSION=%~2

echo === 1/8  sync embedded copies =================================
if exist docs\kb.json copy /Y docs\kb.json internal\assistant\kb.json >nul
if exist internal\api\ceql.html copy /Y internal\api\ceql.html docs\ceql.html >nul
echo   ok

echo === 2/8  gofmt ================================================
set FMTBAD=
for /f "delims=" %%f in ('gofmt -l cmd internal sdk\go 2^>nul') do (
  echo   *** needs gofmt: %%f
  set FMTBAD=1
)
if defined FMTBAD (
  echo   Run:  gofmt -w cmd internal sdk\go
  goto :failed
)
echo   clean

echo === 3/8  go vet + compile =====================================
go vet ./...
if errorlevel 1 goto :failed
go build -o centauri.exe .\cmd\centauri
if errorlevel 1 goto :failed
echo   built centauri.exe

echo === 4/8  tests ================================================
go test ./...
if errorlevel 1 goto :failed
where python >nul 2>nul
if %errorlevel%==0 (
  pushd sdk\python
  python -m unittest discover -s tests
  if errorlevel 1 ( popd & goto :failed )
  popd
) else (
  echo   (python not found - skipping SDK tests; CI runs them)
)

echo === 5/8  zero-dependency invariant ============================
findstr /B /C:"require" go.mod >nul 2>nul
if not errorlevel 1 (
  echo   *** go.mod has a require directive - invariant 5 broken.
  goto :failed
)
echo   go.mod clean - stdlib only

echo === 6/8  performance benchmarks ===============================
if "%CENTAURI_SKIP_BENCH%"=="1" (
  echo   skipped ^(CENTAURI_SKIP_BENCH=1^) - CI compares on every PR
  goto :bench_done
)
if not exist bench mkdir bench
go test -run "^$" -bench . -benchmem -count 3 -timeout 20m ./internal/store/ ./internal/ceql/ > bench\latest.txt
if errorlevel 1 (
  type bench\latest.txt
  goto :failed
)
findstr /C:"Benchmark" bench\latest.txt
if not exist bench\baseline.txt (
  copy /Y bench\latest.txt bench\baseline.txt >nul
  echo   first run: saved as bench\baseline.txt ^(committed - future runs compare against it^)
  goto :bench_done
)
REM benchstat compares baseline vs latest; downloaded on demand as a tool
REM (it does NOT touch go.mod - the zero-dependency invariant holds).
go run golang.org/x/perf/cmd/benchstat@latest bench\baseline.txt bench\latest.txt
if errorlevel 1 echo   (benchstat unavailable/offline - raw numbers above; CI still compares on PRs)
echo   to accept current numbers as the new baseline:  copy /Y bench\latest.txt bench\baseline.txt
:bench_done

echo === 7/8  stamp + commit + push ================================
powershell -NoProfile -Command "$j=[pscustomobject]@{built=(Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'); desc=$env:MSG} | ConvertTo-Json -Compress; [IO.File]::WriteAllText('internal\api\buildinfo.json',$j)"
git add -A
git commit -m "%MSG%"
if errorlevel 1 echo   nothing to commit - working tree clean
git push
if errorlevel 1 goto :failed
echo   pushed - CI, code review, perf review, website + Render all fire automatically

echo === 8/8  release ==============================================
if "%VERSION%"=="" (
  echo   no version tag requested - done.
  goto :done
)
git tag %VERSION%
if errorlevel 1 goto :failed
git push origin %VERSION%
if errorlevel 1 goto :failed
echo   tagged %VERSION% - GitHub Actions is now building the binaries,
echo   the Windows installer and checksums, and will publish the release:
echo   https://github.com/aniljacobv-lab/centauri/releases

:done
echo.
echo  ============================================
echo   Deployed: "%MSG%"
if not "%VERSION%"=="" echo   Release:  %VERSION% (building on GitHub Actions)
echo  ============================================
goto :eof

:failed
echo.
echo  *** NOT deployed - fix the errors above first. ***
exit /b 1
