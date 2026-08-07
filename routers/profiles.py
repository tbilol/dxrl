from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session

import schemas
from database import get_db
from models import Profile

router = APIRouter(prefix="/api/profiles", tags=["profiles"])


def _get_or_404(db: Session, profile_id: str) -> Profile:
    profile = db.get(Profile, profile_id)
    if profile is None:
        raise HTTPException(status_code=404, detail="Profil topilmadi")
    return profile


@router.post("", response_model=schemas.ProfileOut, status_code=status.HTTP_201_CREATED)
def create_profile(payload: schemas.ProfileCreate, db: Session = Depends(get_db)):
    profile = Profile(**payload.model_dump())
    db.add(profile)
    db.commit()
    db.refresh(profile)
    return profile


@router.get("", response_model=list[schemas.ProfileOut])
def list_profiles(db: Session = Depends(get_db)):
    return db.query(Profile).order_by(Profile.created_at.desc()).all()


@router.get("/{profile_id}", response_model=schemas.ProfileOut)
def get_profile(profile_id: str, db: Session = Depends(get_db)):
    return _get_or_404(db, profile_id)


@router.put("/{profile_id}", response_model=schemas.ProfileOut)
def update_profile(
    profile_id: str,
    payload: schemas.ProfileUpdate,
    db: Session = Depends(get_db),
):
    profile = _get_or_404(db, profile_id)
    for field, value in payload.model_dump(exclude_unset=True).items():
        setattr(profile, field, value)
    db.commit()
    db.refresh(profile)
    return profile


@router.delete("/{profile_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_profile(profile_id: str, db: Session = Depends(get_db)):
    profile = _get_or_404(db, profile_id)
    db.delete(profile)
    db.commit()
