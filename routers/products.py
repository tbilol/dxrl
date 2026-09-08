"""Mahsulot nomlari bo'yicha avtomatik to'ldirish."""

from fastapi import APIRouter, Depends, Query
from sqlalchemy.orm import Session

import schemas
from database import get_db
from logger import log
from models import Auction, Request

router = APIRouter(prefix="/api/products", tags=["products"])

# Baza bo'sh bo'lganda ham foydali takliflar chiqishi uchun.
FALLBACK_PRODUCTS = [
    "Pomidor",
    "Bodring",
    "Kartoshka",
    "Piyoz",
    "Sabzi",
    "Bug'doy",
    "Sholi",
    "Paxta",
    "Olma",
    "Uzum",
    "O'rik",
    "Shaftoli",
    "Anor",
    "Behi",
    "Qovun",
    "Tarvuz",
    "Karam",
    "Bulg'or qalampiri",
    "Sarimsoq",
    "Loviya",
    "Makkajo'xori",
    "Yeryong'oq",
    "Bodom",
    "Yong'oq",
]


@router.get("/suggestions", response_model=list[schemas.ProductSuggestion])
def product_suggestions(
    q: str = Query(default="", description="Qidiruv matni"),
    limit: int = Query(default=8, ge=1, le=30),
    db: Session = Depends(get_db),
):
    """Bazadagi mavjud mahsulot nomlari + standart ro'yxatdan mos keladiganlari."""
    needle = q.strip().lower()

    counts: dict[str, int] = {}
    for model in (Auction, Request):
        for (name,) in db.query(model.product).all():
            if not name:
                continue
            key = name.strip()
            if not key:
                continue
            counts[key] = counts.get(key, 0) + 1

    # Bazada yo'q, lekin ro'yxatdagi nomlarni ham taklif qilamiz (count = 0).
    for name in FALLBACK_PRODUCTS:
        counts.setdefault(name, 0)

    matches = [
        schemas.ProductSuggestion(product=name, count=count)
        for name, count in counts.items()
        if not needle or needle in name.lower()
    ]
    # Ko'p ishlatilganlari yuqorida, so'ng alifbo tartibida.
    matches.sort(key=lambda s: (-s.count, s.product.lower()))
    top = matches[:limit]
    log("API", "GET /api/products/suggestions", f"q='{needle}', count={len(top)}")
    return top
