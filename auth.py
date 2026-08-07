"""Google OAuth 2.0 orqali autentifikatsiya va JWT tokenlar."""

import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import urlencode

from authlib.integrations.starlette_client import OAuth, OAuthError
from dotenv import load_dotenv
from fastapi import APIRouter, Depends, HTTPException, Request, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from fastapi.responses import RedirectResponse
from jose import JWTError, jwt
from sqlalchemy.orm import Session

import schemas
from database import get_db
from models import Profile

BASE_DIR = Path(__file__).resolve().parent

# .env back/ ichida ham, loyiha ildizida ham bo'lishi mumkin.
load_dotenv(BASE_DIR / ".env")
load_dotenv(BASE_DIR.parent / ".env")

GOOGLE_CLIENT_ID = os.getenv("GOOGLE_CLIENT_ID", "")
GOOGLE_CLIENT_SECRET = os.getenv("GOOGLE_CLIENT_SECRET", "")
JWT_SECRET = os.getenv("JWT_SECRET", "")
JWT_ALGORITHM = "HS256"
ACCESS_TOKEN_EXPIRE_DAYS = int(os.getenv("JWT_EXPIRE_DAYS", "30"))

# Google Console'dagi "Authorized redirect URI" bilan bir xil bo'lishi shart.
GOOGLE_REDIRECT_URI = os.getenv(
    "GOOGLE_REDIRECT_URI", "http://localhost:8000/api/auth/google/callback"
)
# Kirishdan keyin foydalanuvchi qaytariladigan frontend manzili.
FRONTEND_URL = os.getenv("FRONTEND_URL", "http://localhost:5173").rstrip("/")
FRONTEND_CALLBACK_PATH = os.getenv("FRONTEND_CALLBACK_PATH", "/profil")

oauth = OAuth()
oauth.register(
    name="google",
    client_id=GOOGLE_CLIENT_ID,
    client_secret=GOOGLE_CLIENT_SECRET,
    server_metadata_url="https://accounts.google.com/.well-known/openid-configuration",
    client_kwargs={"scope": "openid email profile"},
)

router = APIRouter(prefix="/api/auth", tags=["auth"])
bearer_scheme = HTTPBearer(auto_error=False)


# ------------------------------- JWT yordamchilari -------------------------------


def create_access_token(user_id: str) -> str:
    """user_id (profiles.id) uchun imzolangan JWT qaytaradi."""
    now = datetime.now(timezone.utc)
    payload = {
        "sub": user_id,
        "iat": int(now.timestamp()),
        "exp": int((now + timedelta(days=ACCESS_TOKEN_EXPIRE_DAYS)).timestamp()),
    }
    return jwt.encode(payload, JWT_SECRET, algorithm=JWT_ALGORITHM)


def verify_token(token: str) -> dict:
    """Tokenni tekshiradi va payload qaytaradi. Yaroqsiz bo'lsa 401."""
    try:
        payload = jwt.decode(token, JWT_SECRET, algorithms=[JWT_ALGORITHM])
    except JWTError:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Token yaroqsiz yoki muddati tugagan",
            headers={"WWW-Authenticate": "Bearer"},
        )
    if not payload.get("sub"):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Token yaroqsiz",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return payload


# --------------------------------- Dependency ---------------------------------


def get_current_user(
    credentials: HTTPAuthorizationCredentials | None = Depends(bearer_scheme),
    db: Session = Depends(get_db),
) -> Profile:
    """Authorization: Bearer <token> sarlavhasidan joriy foydalanuvchini oladi."""
    if credentials is None or not credentials.credentials:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Avval Google orqali kiring",
            headers={"WWW-Authenticate": "Bearer"},
        )

    payload = verify_token(credentials.credentials)
    user = db.get(Profile, payload["sub"])
    if user is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Foydalanuvchi topilmadi",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return user


def get_current_user_optional(
    credentials: HTTPAuthorizationCredentials | None = Depends(bearer_scheme),
    db: Session = Depends(get_db),
) -> Profile | None:
    """Token bo'lsa foydalanuvchini, bo'lmasa None qaytaradi (ochiq endpointlar uchun)."""
    if credentials is None or not credentials.credentials:
        return None
    try:
        payload = verify_token(credentials.credentials)
    except HTTPException:
        return None
    return db.get(Profile, payload["sub"])


# ---------------------------------- Endpointlar ----------------------------------


def _frontend_redirect(**params: str) -> RedirectResponse:
    return RedirectResponse(
        f"{FRONTEND_URL}{FRONTEND_CALLBACK_PATH}?{urlencode(params)}"
    )


@router.get("/google/login")
async def google_login(request: Request):
    """Foydalanuvchini Google ruxsat oynasiga yo'naltiradi."""
    if not GOOGLE_CLIENT_ID or not GOOGLE_CLIENT_SECRET:
        raise HTTPException(
            status_code=500,
            detail="GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET .env faylida ko'rsatilmagan",
        )
    return await oauth.google.authorize_redirect(request, GOOGLE_REDIRECT_URI)


@router.get("/google/callback")
async def google_callback(request: Request, db: Session = Depends(get_db)):
    """Google'dan kelgan `code` ni tokenga almashtiradi va JWT bilan frontendga qaytaradi."""
    try:
        token = await oauth.google.authorize_access_token(request)
    except OAuthError as err:
        return _frontend_redirect(auth_error=err.description or "Google bilan kirish bekor qilindi")

    userinfo = token.get("userinfo")
    if not userinfo:
        userinfo = await oauth.google.userinfo(token=token)

    email = (userinfo.get("email") or "").strip().lower()
    if not email:
        return _frontend_redirect(auth_error="Google akkauntidan email olinmadi")

    name = (userinfo.get("name") or email.split("@")[0]).strip()
    picture = userinfo.get("picture")

    user = db.query(Profile).filter(Profile.email == email).one_or_none()
    if user is None:
        user = Profile(
            name=name,
            email=email,
            avatar_url=picture,
            role="dehqon",
            phone="",  # foydalanuvchi keyin profilda to'ldiradi
        )
        db.add(user)
    else:
        # Google'dagi ism/rasm o'zgargan bo'lishi mumkin.
        user.name = name or user.name
        user.avatar_url = picture or user.avatar_url

    db.commit()
    db.refresh(user)

    return _frontend_redirect(token=create_access_token(user.id))


@router.get("/me", response_model=schemas.ProfileOut)
def read_me(current_user: Profile = Depends(get_current_user)):
    """Joriy foydalanuvchi profili."""
    return current_user
