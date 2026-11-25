#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:$PATH"

if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: Go belum terpasang di /usr/local/go/bin."
  exit 1
fi

echo "==> 1. git pull..."
git pull --ff-only origin main

echo "==> 2. Build binary Go..."
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o zivar-absensi.new .
chmod +x zivar-absensi.new
mv -f zivar-absensi.new zivar-absensi

echo "==> 3. Restart service zivar-absensi..."
sudo systemctl restart zivar-absensi
sudo systemctl status zivar-absensi --no-pager -l
