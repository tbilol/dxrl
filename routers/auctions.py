from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query, status
from sqlalchemy.orm import Session

import schemas
from auth import get_current_user
from database import get_db
from logger import log, log_error, short_id
from models import Auction, Profile

router = APIRouter(prefix="/api/auctions", tags=["auctions"])


def _get_or_404(db: Session, auction_id: str) -> Auction:
    auction = db.get(Auction, auction_id)
    if auction is None:
        log_error("API", "Auksion topilmadi", f"id={short_id(auction_id)}")
        raise HTTPException(status_code=404, detail="Auksion topilmadi")
    return auction


def _ensure_owner(auction: Auction, current_user: Profile) -> None:
    # owner_id bo'sh bo'lgan eski e'lonlar (Google OAuth'dan oldingi) hamma uchun ochiq qoladi.
    if auction.owner_id is not None and auction.owner_id != current_user.id:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="Faqat o'z e'loningizni o'zgartira olasiz",
        )


@router.post("", response_model=schemas.AuctionOut, status_code=status.HTTP_201_CREATED)
def create_auction(
    payload: schemas.AuctionCreate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    auction = Auction(**payload.model_dump(), owner_id=current_user.id)
    db.add(auction)
    db.commit()
    db.refresh(auction)
    log(
        "API",
        "POST /api/auctions",
        f"product={auction.product}, qty={auction.quantity}, owner={short_id(current_user.id)}",
    )
    return auction


@router.get("", response_model=list[schemas.AuctionOut])
def list_auctions(
    owner_id: Optional[str] = Query(
        default=None, description="Faqat shu foydalanuvchining e'lonlari"
    ),
    db: Session = Depends(get_db),
):
    query = db.query(Auction)
    if owner_id:
        # "Mening auksionlarim" — barcha statuslar ko'rinadi.
        query = query.filter(Auction.owner_id == owner_id)
    else:
        query = query.filter(Auction.status == "active")
    results = query.order_by(Auction.created_at.desc()).all()
    log("API", "GET /api/auctions", f"count={len(results)}, owner={short_id(owner_id) if owner_id else 'all'}")
    return results


@router.get("/{auction_id}", response_model=schemas.AuctionOut)
def get_auction(auction_id: str, db: Session = Depends(get_db)):
    return _get_or_404(db, auction_id)


@router.put("/{auction_id}", response_model=schemas.AuctionOut)
def update_auction(
    auction_id: str,
    payload: schemas.AuctionUpdate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    auction = _get_or_404(db, auction_id)
    _ensure_owner(auction, current_user)
    changed = payload.model_dump(exclude_unset=True)
    for field, value in changed.items():
        setattr(auction, field, value)
    db.commit()
    db.refresh(auction)
    log("API", "PUT /api/auctions", f"id={short_id(auction.id)}, fields={list(changed)}")
    return auction


@router.delete("/{auction_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_auction(
    auction_id: str,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    auction = _get_or_404(db, auction_id)
    _ensure_owner(auction, current_user)
    db.delete(auction)
    db.commit()
    log("API", "DELETE /api/auctions", f"id={short_id(auction_id)}")
