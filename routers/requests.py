from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query, status
from sqlalchemy.orm import Session

import schemas
from auth import get_current_user
from database import get_db
from models import Profile, Request

router = APIRouter(prefix="/api/requests", tags=["requests"])


def _get_or_404(db: Session, request_id: str) -> Request:
    item = db.get(Request, request_id)
    if item is None:
        raise HTTPException(status_code=404, detail="So'rov topilmadi")
    return item


def _ensure_owner(item: Request, current_user: Profile) -> None:
    # owner_id bo'sh bo'lgan eski so'rovlar (Google OAuth'dan oldingi) hamma uchun ochiq qoladi.
    if item.owner_id is not None and item.owner_id != current_user.id:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="Faqat o'z so'rovingizni o'zgartira olasiz",
        )


@router.post("", response_model=schemas.RequestOut, status_code=status.HTTP_201_CREATED)
def create_request(
    payload: schemas.RequestCreate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    item = Request(**payload.model_dump(), owner_id=current_user.id)
    db.add(item)
    db.commit()
    db.refresh(item)
    return item


@router.get("", response_model=list[schemas.RequestOut])
def list_requests(
    owner_id: Optional[str] = Query(
        default=None, description="Faqat shu foydalanuvchining so'rovlari"
    ),
    db: Session = Depends(get_db),
):
    query = db.query(Request)
    if owner_id:
        # "Mening so'rovlarim" — barcha statuslar ko'rinadi.
        query = query.filter(Request.owner_id == owner_id)
    else:
        query = query.filter(Request.status == "active")
    return query.order_by(Request.created_at.desc()).all()


@router.get("/{request_id}", response_model=schemas.RequestOut)
def get_request(request_id: str, db: Session = Depends(get_db)):
    return _get_or_404(db, request_id)


@router.put("/{request_id}", response_model=schemas.RequestOut)
def update_request(
    request_id: str,
    payload: schemas.RequestUpdate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    item = _get_or_404(db, request_id)
    _ensure_owner(item, current_user)
    for field, value in payload.model_dump(exclude_unset=True).items():
        setattr(item, field, value)
    db.commit()
    db.refresh(item)
    return item


@router.delete("/{request_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_request(
    request_id: str,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    item = _get_or_404(db, request_id)
    _ensure_owner(item, current_user)
    db.delete(item)
    db.commit()
