from fastapi import APIRouter, Depends
from sqlalchemy import func
from sqlalchemy.orm import Session

import schemas
from database import get_db
from models import Auction, Request

router = APIRouter(prefix="/api/stats", tags=["stats"])


def _top_products(db: Session, model) -> list[schemas.TopProduct]:
    rows = (
        db.query(
            model.product.label("product"),
            func.sum(model.quantity).label("total_quantity"),
            func.count(model.id).label("count"),
        )
        .filter(model.status == "active")
        .group_by(model.product)
        .order_by(func.sum(model.quantity).desc())
        .limit(10)
        .all()
    )
    return [
        schemas.TopProduct(
            product=row.product,
            total_quantity=round(float(row.total_quantity or 0), 3),
            count=int(row.count),
        )
        for row in rows
    ]


@router.get("/top-auctions", response_model=list[schemas.TopProduct])
def top_auctions(db: Session = Depends(get_db)):
    return _top_products(db, Auction)


@router.get("/top-requests", response_model=list[schemas.TopProduct])
def top_requests(db: Session = Depends(get_db)):
    return _top_products(db, Request)


@router.get("/summary")
def summary(db: Session = Depends(get_db)):
    return {
        "auctions_count": db.query(func.count(Auction.id))
        .filter(Auction.status == "active")
        .scalar(),
        "requests_count": db.query(func.count(Request.id))
        .filter(Request.status == "active")
        .scalar(),
    }
