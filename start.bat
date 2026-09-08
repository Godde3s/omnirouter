@echo off
REM ============================================================================
REM  OmniRouter - one-command launcher (Windows)
REM  First run:  double-click, or:  start.bat
REM ============================================================================
setlocal enabledelayedexpansion
cd /d "%~dp0"

if not exist .env (
    copy .env.example .env >nul
    echo.
    echo  ! -- First-run setup ------------------------------------------
    echo   A starter .env was created from .env.example.
    echo   Open it and paste at least one provider credential:
    echo       notepad .env
    echo       QWEN_TOKENS=...      ^(chat.qwen.ai cookie, optional^)
    echo       DEEPSEEK_TOKENS=...  ^(run ds-login.exe to grab one^)
    echo  ----------------------------------------------------------------
    echo.
)

if exist omnirouter.exe (
    set BIN=omnirouter.exe
    goto :run
)

if exist omnirouter-windows-amd64.exe (
    copy omnirouter-windows-amd64.exe omnirouter.exe >nul
    set BIN=omnirouter.exe
    goto :run
)

where go >nul 2>nul
if %errorlevel%==0 (
    echo Building omnirouter with Go...
    go build -trimpath -ldflags="-s -w" -o omnirouter.exe .
    set BIN=omnirouter.exe
    goto :run
)

echo No prebuilt binary and no Go toolchain found.
echo Install Go: https://go.dev/dl/   then re-run start.bat
echo Or grab a release binary: https://github.com/Godde3s/omnirouter/releases
pause
exit /b 1

:run
echo Starting OmniRouter...
"%BIN%"
