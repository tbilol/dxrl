"""AGROGRAM autentifikatsiyasi: Google OAuth, Apple (iCloud) va telefon + parol."""

import json
import os
import secrets
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import urlencode

import httpx
from authlib.integrations.starlette_client import OAuth, OAuthError
from dotenv import load_dotenv
from fastapi import APIRouter, Depends, HTTPException, Request, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from fastapi.responses import RedirectResponse
from jose import JWTError, jwt
from sqlalchemy.orm import Session

import profile_service as ps
import schemas
from database import get_db
from logger import log, log_error, mask_phone, short_id
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

# ------------------------------ Apple (iCloud) ------------------------------
# Apple Developer -> Certificates, Identifiers & Profiles -> Keys (Sign in with Apple)
APPLE_CLIENT_ID = os.getenv("APPLE_CLIENT_ID", "")  # Services ID, masalan com.agrogram.web
APPLE_TEAM_ID = os.getenv("APPLE_TEAM_ID", "")
APPLE_KEY_ID = os.getenv("APPLE_KEY_ID", "")
# .p8 faylining mazmuni. .env da \n bilan bitta qatorda bo'lishi mumkin.
APPLE_PRIVATE_KEY = os.getenv("APPLE_PRIVATE_KEY", "").replace("\\n", "\n")
APPLE_REDIRECT_URI = os.getenv(
    "APPLE_REDIRECT_URI", "http://localhost:8000/api/auth/apple/callback"
)
APPLE_AUTH_URL = "https://appleid.apple.com/auth/authorize"
APPLE_TOKEN_URL = "https://appleid.apple.com/auth/token"

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
    except JWTError as err:
        log_error("AUTH", "Token yaroqsiz yoki muddati tugagan", err)
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
            detail="Avval tizimga kiring",
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
        log_error("AUTH", "Google sozlanmagan", "GOOGLE_CLIENT_ID/SECRET yo'q")
        raise HTTPException(
            status_code=500,
            detail="GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET .env faylida ko'rsatilmagan",
        )
    log("AUTH", "Google orqali kirish boshlandi")
    return await oauth.google.authorize_redirect(request, GOOGLE_REDIRECT_URI)


@router.get("/google/callback")
async def google_callback(request: Request, db: Session = Depends(get_db)):
    """Google'dan kelgan `code` ni tokenga almashtiradi va JWT bilan frontendga qaytaradi."""
    try:
        token = await oauth.google.authorize_access_token(request)
    except OAuthError as err:
        log_error("AUTH", "Google OAuth xatosi", err.description or err)
        return _frontend_redirect(auth_error=err.description or "Google bilan kirish bekor qilindi")

    userinfo = token.get("userinfo")
    if not userinfo:
        userinfo = await oauth.google.userinfo(token=token)

    email = (userinfo.get("email") or "").strip().lower()
    if not email:
        log_error("AUTH", "Google email qaytarmadi")
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
        log("AUTH", "Yangi foydalanuvchi yaratildi (Google)", email)
    else:
        # Google'dagi ism/rasm o'zgargan bo'lishi mumkin.
        user.name = name or user.name
        user.avatar_url = picture or user.avatar_url
        log("AUTH", "Mavjud foydalanuvchi kirdi (Google)", email)

    db.commit()
    db.refresh(user)

    log("AUTH", "Token berildi", f"user={short_id(user.id)}")
    return _frontend_redirect(token=create_access_token(user.id))


# ------------------------------ Telefon + parol ------------------------------


def _token_response(db: Session, user: Profile) -> schemas.TokenOut:
    return schemas.TokenOut(
        access_token=create_access_token(user.id),
        profile=ps.serialize_profile(db, user, viewer=user),
    )


@router.post(
    "/phone/register",
    response_model=schemas.TokenOut,
    status_code=status.HTTP_201_CREATED,
)
def phone_register(payload: schemas.PhoneRegisterIn, db: Session = Depends(get_db)):
    """Telefon raqam + parol bilan ro'yxatdan o'tish (SMS tasdiqlashsiz)."""
    if not ps.is_valid_uz_phone(payload.phone):
        log_error("AUTH", "Ro'yxatdan o'tish: raqam formati noto'g'ri", mask_phone(payload.phone))
        raise HTTPException(status_code=400, detail="Telefon raqami +998 formatida bo'lishi kerak")

    normalized = ps.normalize_phone(payload.phone)

    # XAVFSIZLIK: raqam allaqachon biror profilга biriktirilgan bo'lsa — rad etamiz.
    # Telefon raqamlari e'lonlarda ochiq ko'rinadi, shuning uchun "paroli yo'q
    # profilga parol o'rnatish" imkonini bu yerda bermaymiz — aks holda begona
    # odam Google orqali kirgan akkauntni egallab olishi mumkin edi.
    # Mavjud profilga parol qo'shish uchun /phone/link (token talab qiladi) bor.
    if ps.find_by_phone(db, normalized) is not None:
        log_error("AUTH", "Raqam band", mask_phone(normalized))
        raise HTTPException(status_code=409, detail="Bu raqam allaqachon ro'yxatdan o'tgan")

    user = Profile(
        name=payload.name,
        role=payload.role,
        phone=payload.phone,
        phone_hash=ps.hash_phone(normalized),
        password_hash=ps.hash_password(payload.password),
    )
    db.add(user)
    db.commit()
    db.refresh(user)
    log(
        "AUTH",
        "Yangi foydalanuvchi yaratildi (telefon)",
        f"{mask_phone(normalized)}, user={short_id(user.id)}",
    )
    return _token_response(db, user)


@router.post("/phone/link", response_model=schemas.ProfileOut)
def phone_link(
    payload: schemas.PhoneLinkIn,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Joriy (Google/Apple orqali kirgan) profilga telefon + parol qo'shadi.

    Token talab qilinadi — shuning uchun faqat akkaunt egasi bog'lay oladi.
    """
    if not ps.is_valid_uz_phone(payload.phone):
        raise HTTPException(status_code=400, detail="Telefon raqami +998 formatida bo'lishi kerak")

    normalized = ps.normalize_phone(payload.phone)
    new_hash = ps.hash_phone(normalized)

    taken = (
        db.query(Profile)
        .filter(Profile.phone_hash == new_hash, Profile.id != current_user.id)
        .first()
    )
    if taken is not None:
        raise HTTPException(
            status_code=409, detail="Bu telefon raqami boshqa foydalanuvchiga tegishli"
        )

    current_user.phone = payload.phone
    current_user.phone_hash = new_hash
    current_user.password_hash = ps.hash_password(payload.password)
    db.commit()
    db.refresh(current_user)
    log(
        "AUTH",
        "Telefon + parol profilga bog'landi",
        f"user={short_id(current_user.id)}, {mask_phone(normalized)}",
    )
    return ps.serialize_profile(db, current_user, viewer=current_user)


@router.delete("/phone/link", response_model=schemas.ProfileOut)
def phone_unlink(
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Parol bilan kirishni o'chiradi. Boshqa kirish usuli qolmasa — rad etiladi."""
    if not (current_user.email or current_user.apple_id):
        raise HTTPException(
            status_code=400,
            detail="Yagona kirish usulini o'chirib bo'lmaydi. Avval Google yoki iCloud'ni ulang.",
        )
    current_user.password_hash = None
    db.commit()
    db.refresh(current_user)
    log("AUTH", "Parol bilan kirish uzildi", f"user={short_id(current_user.id)}")
    return ps.serialize_profile(db, current_user, viewer=current_user)


@router.post("/phone/login", response_model=schemas.TokenOut)
def phone_login(payload: schemas.PhoneLoginIn, db: Session = Depends(get_db)):
    """Telefon raqam + parol bilan kirish."""
    log("AUTH", "Telefon orqali kirish", mask_phone(payload.phone))
    user = ps.find_by_phone(db, payload.phone)
    if user is None or not ps.verify_password(payload.password, user.password_hash):
        # Raqam bor-yo'qligini oshkor qilmaymiz — bitta umumiy xabar.
        log_error("AUTH", "Parol yoki raqam noto'g'ri", mask_phone(payload.phone))
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Telefon raqam yoki parol noto'g'ri",
        )
    log("AUTH", "Kirish muvaffaqiyatli", f"user={short_id(user.id)}")
    return _token_response(db, user)


# ----------------------------- Apple (iCloud) --------------------------------


def _apple_client_secret() -> str:
    """Apple client_secret — ES256 bilan imzolangan, .p8 kaliti orqali yaratiladi."""
    now = datetime.now(timezone.utc)
    payload = {
        "iss": APPLE_TEAM_ID,
        "iat": int(now.timestamp()),
        "exp": int((now + timedelta(days=180)).timestamp()),
        "aud": "https://appleid.apple.com",
        "sub": APPLE_CLIENT_ID,
    }
    return jwt.encode(
        payload,
        APPLE_PRIVATE_KEY,
        algorithm="ES256",
        headers={"kid": APPLE_KEY_ID},
    )


def _apple_configured() -> bool:
    return all((APPLE_CLIENT_ID, APPLE_TEAM_ID, APPLE_KEY_ID, APPLE_PRIVATE_KEY))


@router.get("/apple/login")
async def apple_login(request: Request):
    """Foydalanuvchini Apple ruxsat oynasiga yo'naltiradi."""
    if not _apple_configured():
        log_error("AUTH", "Apple sozlanmagan", "APPLE_* .env qiymatlari yo'q")
        raise HTTPException(
            status_code=500,
            detail="APPLE_CLIENT_ID / APPLE_TEAM_ID / APPLE_KEY_ID / APPLE_PRIVATE_KEY "
            ".env faylida ko'rsatilmagan",
        )

    log("AUTH", "Apple (iCloud) orqali kirish boshlandi")
    state = secrets.token_urlsafe(24)
    request.session["apple_state"] = state

    params = {
        "client_id": APPLE_CLIENT_ID,
        "redirect_uri": APPLE_REDIRECT_URI,
        "response_type": "code",
        "scope": "name email",
        # Ism/email so'ralganda Apple javobni POST form sifatida yuboradi.
        "response_mode": "form_post",
        "state": state,
    }
    return RedirectResponse(f"{APPLE_AUTH_URL}?{urlencode(params)}")


@router.api_route("/apple/callback", methods=["GET", "POST"])
async def apple_callback(request: Request, db: Session = Depends(get_db)):
    """Apple'dan kelgan `code` ni tokenga almashtiradi va JWT bilan frontendga qaytaradi."""
    if request.method == "POST":
        form = await request.form()
        data = {k: v for k, v in form.items()}
    else:
        data = dict(request.query_params)

    if data.get("error"):
        log_error("AUTH", "Apple kirishni rad etdi", data.get("error"))
        return _frontend_redirect(auth_error="Apple bilan kirish bekor qilindi")

    expected_state = request.session.pop("apple_state", None)
    if expected_state and data.get("state") != expected_state:
        log_error("AUTH", "Apple state mos kelmadi (CSRF himoyasi)")
        return _frontend_redirect(auth_error="Apple javobi tasdiqlanmadi (state mos kelmadi)")

    code = data.get("code")
    if not code:
        return _frontend_redirect(auth_error="Apple `code` qaytarmadi")

    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.post(
                APPLE_TOKEN_URL,
                data={
                    "client_id": APPLE_CLIENT_ID,
                    "client_secret": _apple_client_secret(),
                    "code": code,
                    "grant_type": "authorization_code",
                    "redirect_uri": APPLE_REDIRECT_URI,
                },
                headers={"Content-Type": "application/x-www-form-urlencoded"},
            )
        if resp.status_code != 200:
            log_error("AUTH", "Apple token almashinuvi muvaffaqiyatsiz", resp.status_code)
            return _frontend_redirect(auth_error="Apple tokenni bermadi")
        id_token = resp.json().get("id_token")
    except httpx.HTTPError as err:
        log_error("AUTH", "Apple serveriga ulanib bo'lmadi", err)
        return _frontend_redirect(auth_error="Apple serveriga ulanib bo'lmadi")

    if not id_token:
        return _frontend_redirect(auth_error="Apple `id_token` qaytarmadi")

    # id_token to'g'ridan-to'g'ri Apple'dan TLS orqali keldi, shuning uchun
    # imzo qayta tekshirilmaydi (JWKS so'rovi shart emas) — faqat da'volarni o'qiymiz.
    claims = jwt.get_unverified_claims(id_token)
    apple_id = claims.get("sub")
    if not apple_id:
        return _frontend_redirect(auth_error="Apple foydalanuvchi ID'sini bermadi")

    email = (claims.get("email") or "").strip().lower() or None

    # Ism faqat birinchi ruxsatda `user` maydonida keladi.
    name = ""
    raw_user = data.get("user")
    if raw_user:
        try:
            person = json.loads(raw_user).get("name") or {}
            name = f"{person.get('firstName', '')} {person.get('lastName', '')}".strip()
        except (ValueError, AttributeError):
            name = ""

    user = db.query(Profile).filter(Profile.apple_id == apple_id).one_or_none()
    if user is None and email:
        # Xuddi shu email bilan Google orqali kirgan bo'lsa — o'sha profilga bog'laymiz.
        user = db.query(Profile).filter(Profile.email == email).one_or_none()

    if user is None:
        user = Profile(
            name=name or (email.split("@")[0] if email else "AGROGRAM"),
            email=email,
            apple_id=apple_id,
            role="dehqon",
            phone="",
        )
        db.add(user)
        log("AUTH", "Yangi foydalanuvchi yaratildi (Apple)", email or "email yashirilgan")
    else:
        log("AUTH", "Mavjud foydalanuvchi kirdi (Apple)", email or "email yashirilgan")
        user.apple_id = apple_id
        if name:
            user.name = name
        if email and not user.email:
            user.email = email

    db.commit()
    db.refresh(user)

    log("AUTH", "Token berildi", f"user={short_id(user.id)}")
    return _frontend_redirect(token=create_access_token(user.id))


# ---------------------------------- Joriy user -------------------------------


@router.get("/me", response_model=schemas.ProfileOut)
def read_me(
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Joriy foydalanuvchi profili (obuna ko'rsatkichlari bilan)."""
    return ps.serialize_profile(db, current_user, viewer=current_user)
