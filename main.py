from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from starlette.middleware.sessions import SessionMiddleware

import auth
from database import Base, engine, run_migrations
from routers import auctions, conversations, products, profiles, requests, stats

Base.metadata.create_all(bind=engine)
run_migrations()

app = FastAPI(
    title="Agro Auksion API",
    description="Qishloq xo'jaligi auksion platformasi uchun API",
    version="2.0.0",
)

# OAuth `state` parametrini saqlash uchun (Authlib shu sessiyadan foydalanadi).
app.add_middleware(
    SessionMiddleware,
    secret_key=auth.JWT_SECRET or "agro-auksion-dev-session",
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

app.include_router(auth.router)
app.include_router(auctions.router)
app.include_router(requests.router)
app.include_router(stats.router)
app.include_router(profiles.router)
app.include_router(conversations.router)
app.include_router(products.router)


@app.get("/")
def root():
    return {"name": "Agro Auksion API", "status": "ok", "docs": "/docs"}


@app.get("/api/health")
def health():
    return {"status": "ok"}
