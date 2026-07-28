@echo off
REM ============================================================
REM  start-all.bat - EVERYTHING in one go (same as run-centauri.bat):
REM  database + dashboard + Studio + local AI setup + opens browser.
REM ============================================================
call "%~dp0run-centauri.bat" %*
