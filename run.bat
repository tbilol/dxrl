@echo off
REM Agro Auksion - backend ishga tushirish skripti (Windows)
cd /d "%~dp0"

if not exist ".venv" (
  echo ==^> Virtual muhit yaratilmoqda ^(.venv^)...
  python -m venv .venv
)

call .venv\Scripts\activate.bat

echo ==^> Kutubxonalar o'rnatilmoqda...
pip install -q --upgrade pip
pip install -q -r requirements.txt

echo ==^> Server: http://localhost:8000  (docs: /docs)
uvicorn main:app --reload --host 0.0.0.0 --port 8000
