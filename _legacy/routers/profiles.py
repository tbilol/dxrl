from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session

import profile_service as ps
import schemas
from auth import get_current_user, get_current_user_optional
from database import get_db
from logger import log, log_error, short_id
from models import Follow, Profile

router = APIRouter(prefix="/api/profiles", tags=["profiles"])


def _get_or_404(db: Session, profile_id: str) -> Profile:
    profile = db.get(Profile, profile_id)
    if profile is None:
        log_error("API", "Profil topilmadi", f"id={short_id(profile_id)}")
        raise HTTPException(status_code=404, detail="Profil topilmadi")
    return profile


def _ensure_owner(profile: Profile, current_user: Profile) -> None:
    if profile.id != current_user.id:
        log_error(
            "API",
            "Begona profilni o'zgartirishga urinish",
            f"target={short_id(profile.id)}, user={short_id(current_user.id)}",
        )
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="Faqat o'z profilingizni o'zgartira olasiz",
        )


def _row(db: Session, profile: Profile, viewer: Profile | None) -> schemas.FollowUserOut:
    """Kuzatuvchilar / kuzatilganlar ro'yxatidagi bitta qator."""
    return schemas.FollowUserOut(
        id=profile.id,
        name=profile.name,
        avatar_url=profile.avatar_url,
        role=profile.role,
        address=profile.address,
        is_followed=ps.is_following(db, viewer.id if viewer else None, profile.id),
        is_me=bool(viewer and viewer.id == profile.id),
    )


# ------------------------------- CRUD -------------------------------


@router.post("", response_model=schemas.ProfileOut, status_code=status.HTTP_201_CREATED)
def create_profile(
    payload: schemas.ProfileCreate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    profile = Profile(**payload.model_dump())
    if profile.phone:
        profile.phone_hash = ps.hash_phone(profile.phone)
    db.add(profile)
    db.commit()
    db.refresh(profile)
    return ps.serialize_profile(db, profile, viewer=current_user)


@router.get("", response_model=list[schemas.ProfileOut])
def list_profiles(
    db: Session = Depends(get_db),
    viewer: Profile | None = Depends(get_current_user_optional),
):
    profiles = db.query(Profile).order_by(Profile.created_at.desc()).all()
    return [ps.serialize_profile(db, p, viewer=viewer) for p in profiles]


@router.get("/{profile_id}", response_model=schemas.ProfileOut)
def get_profile(
    profile_id: str,
    db: Session = Depends(get_db),
    viewer: Profile | None = Depends(get_current_user_optional),
):
    """Profil + obunachilar soni, e'lonlar soni va joriy foydalanuvchi obunami."""
    return ps.serialize_profile(db, _get_or_404(db, profile_id), viewer=viewer)


@router.put("/{profile_id}", response_model=schemas.ProfileOut)
def update_profile(
    profile_id: str,
    payload: schemas.ProfileUpdate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    profile = _get_or_404(db, profile_id)
    _ensure_owner(profile, current_user)

    data = payload.model_dump(exclude_unset=True)

    # Telefon o'zgarsa — phone_hash ham yangilanadi (telefon orqali kirish uchun).
    if "phone" in data:
        new_phone = data["phone"] or ""
        new_hash = ps.hash_phone(new_phone) if new_phone else None
        if new_hash and new_hash != profile.phone_hash:
            taken = (
                db.query(Profile)
                .filter(Profile.phone_hash == new_hash, Profile.id != profile.id)
                .first()
            )
            if taken is not None:
                raise HTTPException(
                    status_code=409,
                    detail="Bu telefon raqami boshqa foydalanuvchiga tegishli",
                )
        profile.phone_hash = new_hash

    for field, value in data.items():
        setattr(profile, field, value)

    db.commit()
    db.refresh(profile)
    log("API", "PUT /api/profiles", f"id={short_id(profile.id)}, fields={list(data)}")
    return ps.serialize_profile(db, profile, viewer=current_user)


@router.delete("/{profile_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_profile(
    profile_id: str,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    profile = _get_or_404(db, profile_id)
    _ensure_owner(profile, current_user)
    # Obuna yozuvlari yetim qolmasin.
    db.query(Follow).filter(
        (Follow.follower_id == profile.id) | (Follow.following_id == profile.id)
    ).delete(synchronize_session=False)
    db.delete(profile)
    db.commit()


# ------------------------------- Obuna -------------------------------


@router.post("/{profile_id}/follow", response_model=schemas.FollowStateOut)
def follow_profile(
    profile_id: str,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Foydalanuvchiga obuna bo'lish. Takroriy so'rov xato bermaydi (idempotent)."""
    target = _get_or_404(db, profile_id)
    if target.id == current_user.id:
        raise HTTPException(status_code=400, detail="O'zingizga obuna bo'la olmaysiz")

    exists = (
        db.query(Follow)
        .filter(Follow.follower_id == current_user.id, Follow.following_id == target.id)
        .first()
    )
    if exists is None:
        db.add(Follow(follower_id=current_user.id, following_id=target.id))
        db.commit()
        log("API", "Obuna bo'lindi", f"{short_id(current_user.id)} -> {short_id(target.id)}")

    followers, _ = ps.follows_count(db, target.id)
    return schemas.FollowStateOut(is_followed=True, followers_count=followers)


@router.delete("/{profile_id}/follow", response_model=schemas.FollowStateOut)
def unfollow_profile(
    profile_id: str,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Obunani bekor qilish. Obuna bo'lmagan bo'lsa ham xato bermaydi."""
    target = _get_or_404(db, profile_id)
    removed = (
        db.query(Follow)
        .filter(Follow.follower_id == current_user.id, Follow.following_id == target.id)
        .delete(synchronize_session=False)
    )
    db.commit()
    if removed:
        log("API", "Obuna bekor qilindi", f"{short_id(current_user.id)} -> {short_id(target.id)}")

    followers, _ = ps.follows_count(db, target.id)
    return schemas.FollowStateOut(is_followed=False, followers_count=followers)


@router.get("/{profile_id}/followers", response_model=list[schemas.FollowUserOut])
def list_followers(
    profile_id: str,
    db: Session = Depends(get_db),
    viewer: Profile | None = Depends(get_current_user_optional),
):
    """Ushbu foydalanuvchini kuzatayotganlar."""
    _get_or_404(db, profile_id)
    rows = (
        db.query(Profile)
        .join(Follow, Follow.follower_id == Profile.id)
        .filter(Follow.following_id == profile_id)
        .order_by(Follow.created_at.desc())
        .all()
    )
    log("API", "GET /followers", f"profile={short_id(profile_id)}, count={len(rows)}")
    return [_row(db, p, viewer) for p in rows]


@router.get("/{profile_id}/following", response_model=list[schemas.FollowUserOut])
def list_following(
    profile_id: str,
    db: Session = Depends(get_db),
    viewer: Profile | None = Depends(get_current_user_optional),
):
    """Ushbu foydalanuvchi kuzatayotganlar."""
    _get_or_404(db, profile_id)
    rows = (
        db.query(Profile)
        .join(Follow, Follow.following_id == Profile.id)
        .filter(Follow.follower_id == profile_id)
        .order_by(Follow.created_at.desc())
        .all()
    )
    log("API", "GET /following", f"profile={short_id(profile_id)}, count={len(rows)}")
    return [_row(db, p, viewer) for p in rows]
