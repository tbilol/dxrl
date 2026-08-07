from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from database import Base, engine
from routers import auctions, profiles, requests, stats

Base.metadata.create_all(bind=engine)

app = FastAPI(
    title="Agro Auksion API",
    description="Qishloq xo'jaligi auksion platformasi uchun API",
    version="1.0.0",
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

app.include_router(auctions.router)
app.include_router(requests.router)
app.include_router(stats.router)
app.include_router(profiles.router)


@app.get("/")
def root():
    return {"name": "Agro Auksion API", "status": "ok", "docs": "/docs"}


@app.get("/api/health")
def health():
    return {"status": "ok"}
