#!/usr/bin/env bash
# ============================================================================
#  OmniRouter — one-command launcher (Linux / macOS)
#  First run:  ./start.sh
# ============================================================================
set -u
cd "$(dirname "$0")"

if [ ! -f .env ] && [ -f .env.example ]; then
    cp .env.example .env
    echo
    echo " ! -- First-run setup --------------------------------------------"
    echo "   A starter .env was created from .env.example."
    echo "   Open it and paste at least one provider credential:"
    echo "     notepad .env / nano .env"
    echo "     QWEN_TOKENS=...      (chat.qwen.ai cookie, optional — guest works)"
    echo "     DEEPSEEK_TOKENS=...  (run ./ds-login to grab one)"
    echo "     ZAI_TOKENS=...       (chat.z.ai, optional — guest works)"
    echo "   ------------------------------------------------------------------"
    echo
fi

BIN=""
for cand in omnirouter omnirouter-linux-amd64 omnirouter-darwin-arm64 omnirouter-darwin-amd64; do
    if [ -x "$cand" ]; then BIN="$cand"; break; fi
done

if [ -z "$BIN" ]; then
    if command -v go >/dev/null 2>&1; then
        echo "Building omnirouter with Go..."
        CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o omnirouter .
        BIN="omnirouter"
    else
        echo "No prebuilt binary and no Go toolchain found."
        echo "Install Go: https://go.dev/dl/   then re-run $0"
        echo "Or grab a release binary: https://github.com/Godde3s/omnirouter/releases"
        exit 1
    fi
fi

echo "Starting OmniRouter..."
exec "./$BIN"
