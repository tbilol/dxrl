from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session

import schemas
from database import get_db
from models import Request

router = APIRouter(prefix="/api/requests", tags=["requests"])


def _get_or_404(db: Session, request_id: str) -> Request:
    item = db.get(Request, request_id)
    if item is None:
        raise HTTPException(status_code=404, detail="So'rov topilmadi")
    return item


@router.post("", response_model=schemas.RequestOut, status_code=status.HTTP_201_CREATED)
def create_request(payload: schemas.RequestCreate, db: Session = Depends(get_db)):
    item = Request(**payload.model_dump())
    db.add(item)
    db.commit()
    db.refresh(item)
    return item


@router.get("", response_model=list[schemas.RequestOut])
def list_requests(db: Session = Depends(get_db)):
    return (
        db.query(Request)
        .filter(Request.status == "active")
        .order_by(Request.created_at.desc())
        .all()
    )


@router.get("/{request_id}", response_model=schemas.RequestOut)
def get_request(request_id: str, db: Session = Depends(get_db)):
    return _get_or_404(db, request_id)


@router.put("/{request_id}", response_model=schemas.RequestOut)
def update_request(
    request_id: str,
    payload: schemas.RequestUpdate,
    db: Session = Depends(get_db),
):
    item = _get_or_404(db, request_id)
    for field, value in payload.model_dump(exclude_unset=True).items():
        setattr(item, field, value)
    db.commit()
    db.refresh(item)
    return item


@router.delete("/{request_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_request(request_id: str, db: Session = Depends(get_db)):
    item = _get_or_404(db, request_id)
    db.delete(item)
    db.commit()
