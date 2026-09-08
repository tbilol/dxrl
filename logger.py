"""AGROGRAM — terminal uchun ranglangan loglar.

Uvicorn ishlayotgan terminalda ko'rinadi. Ranglar faqat haqiqiy terminalda
qo'llanadi: log fayllga yo'naltirilsa ANSI belgilari qo'shilmaydi.

O'chirish uchun:  AGROGRAM_DEBUG=0 uvicorn main:app
Xatolar (`log_error`) bu sozlamadan qat'i nazar doim chiqadi.
"""

import json
import os
import re
import sys
import traceback
from datetime import datetime

COLORS = {
    "BACK": "\033[92m",   # yashil  — ilova hodisalari
    "DB": "\033[93m",     # sariq   — baza, migratsiyalar
    "AUTH": "\033[95m",   # magenta — kirish, token
    "API": "\033[94m",    # ko'k    — so'rov va javoblar
    "CHAT": "\033[96m",   # moviy   — xabarlar
    "ERROR": "\033[91m",  # qizil   — xatolar
    "RESET": "\033[0m",
}

# Middleware uchun kengaytirilgan palitra.
PALETTE = {
    "GREEN": "\033[92m",
    "YELLOW": "\033[93m",
    "RED": "\033[91m",
    "CYAN": "\033[96m",
    "BLUE": "\033[94m",
    "MAGENTA": "\033[95m",
    "PURPLE": "\033[38;5;141m",
    "ORANGE": "\033[38;5;208m",
    "GRAY": "\033[90m",
}

# Marshrut bo'yicha rang — qaysi bo'lim ekani bir qarashda ko'rinsin.
ROUTE_COLORS = (
    ("/api/auth", PALETTE["ORANGE"]),
    ("/api/conversations", PALETTE["PURPLE"]),
    ("/api/auctions", PALETTE["GREEN"]),
    ("/api/requests", PALETTE["GREEN"]),
    ("/api/profiles", PALETTE["MAGENTA"]),
    ("/api/stats", PALETTE["CYAN"]),
    ("/api/products", PALETTE["CYAN"]),
)

# Javob kodi bo'yicha rang: 2xx yashil, 3xx moviy, 4xx sariq, 5xx qizil.
STATUS_COLORS = {2: PALETTE["GREEN"], 3: PALETTE["CYAN"], 4: PALETTE["YELLOW"], 5: PALETTE["RED"]}

# Logga hech qachon tushmasligi kerak bo'lgan maydonlar.
SENSITIVE = (
    "password",
    "confirm",
    "token",
    "access_token",
    "id_token",
    "secret",
    "client_secret",
    "code",
    "authorization",
)

# Rang faqat terminalda; quvurga (pipe) yozilganda toza matn qoladi.
_USE_COLOR = sys.stdout.isatty() and os.getenv("NO_COLOR") is None

# AGROGRAM_DEBUG=0 bo'lsa oddiy loglar jim bo'ladi (xatolar baribir chiqadi).
DEBUG = os.getenv("AGROGRAM_DEBUG", "1").lower() not in ("0", "false", "no", "off")


def _paint(tag: str, text: str) -> str:
    if not _USE_COLOR:
        return text
    return f"{COLORS.get(tag, COLORS['BACK'])}{text}{COLORS['RESET']}"


def _now() -> str:
    return datetime.now().strftime("%H:%M:%S")


def log(tag: str, message: str, data=None) -> None:
    """Oddiy log: log('API', 'GET /api/auctions', 'count=12')."""
    if not DEBUG:
        return
    out = _paint(tag, f"[{tag}] {_now()} — {message}")
    if data is not None:
        out += f" | {data}"
    print(out, flush=True)


def log_error(tag: str, message: str, error=None) -> None:
    """Xato logi — DEBUG o'chiq bo'lsa ham chiqadi."""
    out = _paint("ERROR", f"[{tag}:ERROR] {_now()} — {message}")
    if error is not None:
        out += f" | {error}"
    print(out, file=sys.stderr, flush=True)

    # Haqiqiy istisno bo'lsa — to'liq stack trace ham chiqadi.
    if isinstance(error, BaseException):
        traceback.print_exception(type(error), error, error.__traceback__)


def mask_phone(phone: str | None) -> str:
    """Telefonni logda to'liq yozmaymiz: '+998901112233' -> '+998 90 *** ** 33'.

    Raqam e'lonlarda ochiq bo'lsa-da, loglar ko'pincha faylga yig'iladi —
    shuning uchun shaxsiy ma'lumotni qisqartirib yozamiz.
    """
    digits = "".join(ch for ch in (phone or "") if ch.isdigit())
    if len(digits) < 6:
        return "***"
    return f"+{digits[:3]} {digits[3:5]} *** ** {digits[-2:]}"


def short_id(value: str | None, size: int = 8) -> str:
    """UUID'ni logda qisqartirib ko'rsatadi."""
    return (value or "")[:size] or "-"


# ------------------------------ HTTP middleware ------------------------------


def route_color(path: str) -> str:
    for prefix, color in ROUTE_COLORS:
        if path.startswith(prefix):
            return color
    return COLORS["API"]


def status_color(code: int) -> str:
    return STATUS_COLORS.get(code // 100, PALETTE["GRAY"])


def redact(value):
    """Parol/token kabi maydonlarni '***' ga almashtiradi (rekursiv)."""
    if isinstance(value, dict):
        return {
            k: ("***" if any(s in k.lower() for s in SENSITIVE) else redact(v))
            for k, v in value.items()
        }
    if isinstance(value, list):
        return [redact(v) for v in value]
    return value


def body_preview(raw: bytes, limit: int = 200) -> str:
    """So'rov tanasining qisqa ko'rinishi — maxfiy maydonlar yashiriladi."""
    if not raw:
        return ""
    try:
        text = json.dumps(redact(json.loads(raw)), ensure_ascii=False, separators=(",", ":"))
    except (ValueError, TypeError):
        # JSON emas (masalan form-data) — xom matndan maxfiy juftliklarni o'chiramiz.
        text = raw.decode("utf-8", "replace")
        for key in SENSITIVE:
            text = re.sub(
                rf"({key}[\"'\s:=]+)[^&,\s\"'}}]+", r"\1***", text, flags=re.IGNORECASE
            )
    text = " ".join(text.split())
    return text[:limit] + ("..." if len(text) > limit else "")


def _emit(text: str, color: str, stream=None) -> None:
    line = f"{color}{text}{COLORS['RESET']}" if _USE_COLOR else text
    print(line, file=stream or sys.stdout, flush=True)


def log_request_in(method: str, path: str, client: str, preview: str = "") -> None:
    """[API:IN] — kiruvchi so'rov."""
    if not DEBUG:
        return
    text = f"[API:IN] {_now()} — {method} {path} ← {client}"
    if preview:
        text += f" | body: {preview}"
    _emit(text, route_color(path))


def log_request_out(
    method: str,
    path: str,
    status: int,
    ms: float,
    count: int | None = None,
    error: str | None = None,
) -> None:
    """[API:OUT] — chiquvchi javob. Rang holat kodiga qarab tanlanadi."""
    is_error = status >= 400
    if not DEBUG and not is_error:
        return

    text = f"[API:OUT] {_now()} — {method} {path} → {status} | {ms:.0f}ms"
    if count is not None:
        text += f" | {count} ta"
    if error:
        text += f" | {error}"

    _emit(text, status_color(status), stream=sys.stderr if status >= 500 else None)
