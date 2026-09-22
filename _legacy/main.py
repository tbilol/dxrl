import json
import os
import time

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from starlette.middleware.sessions import SessionMiddleware

import auth
from database import Base, engine, run_migrations
from logger import DEBUG, body_preview, log, log_error, log_request_in, log_request_out
from routers import auctions, conversations, products, profiles, requests, stats

log("BACK", "AGROGRAM backend ishga tushmoqda", "v3.0.0")

Base.metadata.create_all(bind=engine)
run_migrations()

app = FastAPI(
    title="AGROGRAM API",
    description="AGROGRAM — qishloq xo'jaligi auksion platformasi uchun API",
    version="3.0.0",
)

# OAuth `state` parametrini saqlash uchun (Authlib shu sessiyadan foydalanadi).
app.add_middleware(
    SessionMiddleware,
    secret_key=auth.JWT_SECRET or "agrogram-dev-session",
    same_site="lax",
    https_only=False,
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=[
        "http://localhost:5173",
        "http://127.0.0.1:5173",
    ],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Tanasi o'qiladigan metodlar (parol kabi maydonlar logda yashiriladi).
_BODY_METHODS = {"POST", "PUT", "PATCH"}

# Log uchun javobni buferlash chegarasi — kattaroq javoblar sanalmaydi.
_MAX_BUFFER = 512 * 1024


@app.middleware("http")
async def log_requests(request: Request, call_next):
    """Har bir so'rov va javobni [API:IN] / [API:OUT] qatorlari bilan yozadi.

    Rang: kiruvchi qator marshrutga qarab, chiquvchi qator holat kodiga qarab.
    Qo'shimcha yuk minimal — tana faqat POST/PUT/PATCH da o'qiladi, javob esa
    faqat JSON bo'lsa buferlanadi.
    """
    method = request.method
    path = request.url.path
    client = request.client.host if request.client else "-"

    # ---------------------------- Kiruvchi so'rov ----------------------------
    preview = ""
    if DEBUG and method in _BODY_METHODS:
        raw = await request.body()
        preview = body_preview(raw)

        # Tanani "qaytarib qo'yamiz" — aks holda endpoint uni o'qiy olmaydi.
        async def receive():
            return {"type": "http.request", "body": raw, "more_body": False}

        request._receive = receive

    log_request_in(method, path, client, preview)

    # ------------------------------ Ishlov berish ----------------------------
    started = time.perf_counter()
    try:
        response = await call_next(request)
    except Exception as err:  # noqa: BLE001 — log qilib qayta ko'taramiz
        ms = (time.perf_counter() - started) * 1000
        log_request_out(method, path, 500, ms, error=str(err) or type(err).__name__)
        log_error("API", f"{method} {path} — ishlov berishda xato", err)
        raise

    ms = (time.perf_counter() - started) * 1000

    # ---------------------------- Chiquvchi javob ----------------------------
    count = None
    error = None

    # Javob faqat kerak bo'lganda buferlanadi: DEBUG yoqilgan yoki xato qaytgan.
    # Aks holda javob oqimiga umuman tegilmaydi — qo'shimcha yuk nolga teng.
    needs_body = (DEBUG or response.status_code >= 400) and "application/json" in response.headers.get(
        "content-type", ""
    )
    if needs_body:
        chunks = [chunk async for chunk in response.body_iterator]
        body = b"".join(chunks)

        async def replay():
            yield body

        # Sarlavhalar (shu jumladan content-length) o'zgarmaydi.
        response.body_iterator = replay()

        if len(body) <= _MAX_BUFFER:
            try:
                if body[:1] == b"[":
                    count = len(json.loads(body))
                elif response.status_code >= 400:
                    error = json.loads(body).get("detail")
            except (ValueError, AttributeError):
                pass

    log_request_out(method, path, response.status_code, ms, count=count, error=error)
    return response


app.include_router(auth.router)
app.include_router(auctions.router)
app.include_router(requests.router)
app.include_router(stats.router)
app.include_router(profiles.router)
app.include_router(conversations.router)
app.include_router(products.router)


@app.on_event("startup")
def on_startup():
    routes = len([r for r in app.routes if getattr(r, "methods", None)])
    host = os.getenv("HOST", "127.0.0.1")
    port = os.getenv("PORT", "8000")
    log("BACK", "AGROGRAM backend ishga tushdi", f"endpointlar={routes}")
    log("BACK", "Server manzili", f"http://{host}:{port}")
    log("BACK", "API hujjatlari", f"http://{host}:{port}/docs")
    log("BACK", "Loglarni o'chirish uchun", "AGROGRAM_DEBUG=0 uvicorn main:app")


@app.on_event("shutdown")
def on_shutdown():
    log("BACK", "Backend to'xtatildi")


@app.get("/")
def root():
    return {"name": "AGROGRAM API", "status": "ok", "docs": "/docs"}


@app.get("/api/health")
def health():
    return {"status": "ok"}
