#!/usr/bin/env bash
# Agro Auksion — backend ishga tushirish skripti
set -e

cd "$(dirname "$0")"

if [ ! -d ".venv" ]; then
  echo "==> Virtual muhit yaratilmoqda (.venv)..."
  python3 -m venv .venv
fi

# shellcheck disable=SC1091
source .venv/bin/activate

echo "==> Kutubxonalar o'rnatilmoqda..."
pip install -q --upgrade pip
pip install -q -r requirements.txt

echo "==> Server ishga tushmoqda: http://localhost:8000 (docs: /docs)"
exec uvicorn main:app --reload --host 0.0.0.0 --port 8000
