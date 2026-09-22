"""Profil bilan ishlashning umumiy qismlari.

Bu modul `auth.py` va `routers/profiles.py` o'rtasida bo'lishiladi, shuning
uchun u hech qaysi routerni import qilmaydi (aylanma import bo'lmasligi uchun).
"""

import hashlib
import re

import bcrypt
from sqlalchemy import func
from sqlalchemy.orm import Session

import schemas
from models import Auction, Follow, Profile, Request

# --------------------------------- Telefon ---------------------------------

PHONE_RE = re.compile(r"\D+")


def normalize_phone(phone: str) -> str:
    """'+998 90 123 45 67' -> '998901234567'. Bo'sh bo'lsa '' qaytaradi."""
    digits = PHONE_RE.sub("", phone or "")
    if not digits:
        return ""
    # Foydalanuvchi '90 123 45 67' yoki '8 90 ...' ko'rinishida kiritishi mumkin.
    if digits.startswith("998"):
        return digits
    return f"998{digits.lstrip('0')}"


def is_valid_uz_phone(phone: str) -> bool:
    """O'zbekiston raqami: 998 + 9 raqam."""
    return bool(re.fullmatch(r"998\d{9}", normalize_phone(phone)))


def hash_phone(phone: str) -> str:
    """Takrorlanmas indeks uchun determinlashgan yig'indi (qidiruv ham shu orqali)."""
    return hashlib.sha256(normalize_phone(phone).encode("utf-8")).hexdigest()


def find_by_phone(db: Session, phone: str) -> Profile | None:
    normalized = normalize_phone(phone)
    if not normalized:
        return None
    return (
        db.query(Profile)
        .filter(Profile.phone_hash == hash_phone(normalized))
        .one_or_none()
    )


# ---------------------------------- Parol ----------------------------------


def hash_password(password: str) -> str:
    return bcrypt.hashpw(password.encode("utf-8"), bcrypt.gensalt()).decode("utf-8")


def verify_password(password: str, password_hash: str | None) -> bool:
    if not password_hash:
        return False
    try:
        return bcrypt.checkpw(password.encode("utf-8"), password_hash.encode("utf-8"))
    except ValueError:
        # Buzilgan yoki bcrypt bo'lmagan hash — kirishga ruxsat bermaymiz.
        return False


# ------------------------------ Serializatsiya ------------------------------


def follows_count(db: Session, profile_id: str) -> tuple[int, int]:
    """(kuzatuvchilar, kuzatilganlar) sonini qaytaradi."""
    followers = (
        db.query(func.count(Follow.id))
        .filter(Follow.following_id == profile_id)
        .scalar()
        or 0
    )
    following = (
        db.query(func.count(Follow.id))
        .filter(Follow.follower_id == profile_id)
        .scalar()
        or 0
    )
    return followers, following


def listings_count(db: Session, profile_id: str) -> int:
    """Foydalanuvchining auksion + so'rov e'lonlari soni."""
    auctions = (
        db.query(func.count(Auction.id)).filter(Auction.owner_id == profile_id).scalar() or 0
    )
    requests = (
        db.query(func.count(Request.id)).filter(Request.owner_id == profile_id).scalar() or 0
    )
    return auctions + requests


def is_following(db: Session, follower_id: str | None, following_id: str) -> bool:
    if not follower_id or follower_id == following_id:
        return False
    return (
        db.query(Follow.id)
        .filter(Follow.follower_id == follower_id, Follow.following_id == following_id)
        .first()
        is not None
    )


def serialize_profile(
    db: Session, profile: Profile, viewer: Profile | None = None
) -> schemas.ProfileOut:
    """Profilni obuna/e'lon ko'rsatkichlari bilan birga qaytaradi."""
    followers, following = follows_count(db, profile.id)
    viewer_id = viewer.id if viewer else None
    is_me = viewer_id == profile.id

    return schemas.ProfileOut(
        id=profile.id,
        name=profile.name,
        role=profile.role,
        phone=profile.phone or "",
        address=profile.address,
        email=profile.email,
        avatar_url=profile.avatar_url,
        created_at=profile.created_at,
        updated_at=profile.updated_at,
        followers_count=followers,
        following_count=following,
        total_listings_count=listings_count(db, profile.id),
        is_followed=is_following(db, viewer_id, profile.id),
        is_me=is_me,
        # Ulangan kirish usullarini faqat profil egasiga ko'rsatamiz.
        has_google=bool(profile.email) if is_me else False,
        has_password=bool(profile.password_hash) if is_me else False,
        has_apple=bool(profile.apple_id) if is_me else False,
    )
