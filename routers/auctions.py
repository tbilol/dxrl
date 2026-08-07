from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session

import schemas
from database import get_db
from models import Auction

router = APIRouter(prefix="/api/auctions", tags=["auctions"])


def _get_or_404(db: Session, auction_id: str) -> Auction:
    auction = db.get(Auction, auction_id)
    if auction is None:
        raise HTTPException(status_code=404, detail="Auksion topilmadi")
    return auction


@router.post("", response_model=schemas.AuctionOut, status_code=status.HTTP_201_CREATED)
def create_auction(payload: schemas.AuctionCreate, db: Session = Depends(get_db)):
    auction = Auction(**payload.model_dump())
    db.add(auction)
    db.commit()
    db.refresh(auction)
    return auction


@router.get("", response_model=list[schemas.AuctionOut])
def list_auctions(db: Session = Depends(get_db)):
    return (
        db.query(Auction)
        .filter(Auction.status == "active")
        .order_by(Auction.created_at.desc())
        .all()
    )


@router.get("/{auction_id}", response_model=schemas.AuctionOut)
def get_auction(auction_id: str, db: Session = Depends(get_db)):
    return _get_or_404(db, auction_id)


@router.put("/{auction_id}", response_model=schemas.AuctionOut)
def update_auction(
    auction_id: str,
    payload: schemas.AuctionUpdate,
    db: Session = Depends(get_db),
):
    auction = _get_or_404(db, auction_id)
    for field, value in payload.model_dump(exclude_unset=True).items():
        setattr(auction, field, value)
    db.commit()
    db.refresh(auction)
    return auction


@router.delete("/{auction_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_auction(auction_id: str, db: Session = Depends(get_db)):
    auction = _get_or_404(db, auction_id)
    db.delete(auction)
    db.commit()
