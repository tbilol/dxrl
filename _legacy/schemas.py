from datetime import datetime
from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field, field_validator

Role = Literal["dehqon", "sotib_oluvchi"]


class _Trimmed(BaseModel):
    @field_validator("*", mode="before")
    @classmethod
    def strip_strings(cls, v):
        return v.strip() if isinstance(v, str) else v


# --------------------------- Umumiy (owner) ---------------------------


class OwnerOut(BaseModel):
    """E'lon egasi haqidagi qisqa ma'lumot (kartalarda va chatda ishlatiladi)."""

    model_config = ConfigDict(from_attributes=True)

    id: str
    name: str
    avatar_url: Optional[str] = None
    role: Optional[str] = None


# --------------------------- Auctions ---------------------------


class AuctionBase(_Trimmed):
    farmer_name: str = Field(min_length=1, max_length=120)
    product: str = Field(min_length=1, max_length=120)
    quantity: float = Field(gt=0, description="tonna")
    price: Optional[float] = Field(default=None, ge=0, description="so'm/kg")
    phone: str = Field(min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    notes: Optional[str] = Field(default=None, max_length=2000, description="Izoh")


class AuctionCreate(AuctionBase):
    status: str = "active"


class AuctionUpdate(_Trimmed):
    farmer_name: Optional[str] = Field(default=None, min_length=1, max_length=120)
    product: Optional[str] = Field(default=None, min_length=1, max_length=120)
    quantity: Optional[float] = Field(default=None, gt=0)
    price: Optional[float] = Field(default=None, ge=0)
    phone: Optional[str] = Field(default=None, min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    notes: Optional[str] = Field(default=None, max_length=2000)
    status: Optional[str] = Field(default=None, max_length=20)


class AuctionOut(AuctionBase):
    model_config = ConfigDict(from_attributes=True)

    id: str
    status: str
    created_at: datetime
    owner_id: Optional[str] = None
    owner: Optional[OwnerOut] = None


# --------------------------- Requests ---------------------------


class RequestBase(_Trimmed):
    buyer_name: str = Field(min_length=1, max_length=120)
    product: str = Field(min_length=1, max_length=120)
    quantity: float = Field(gt=0, description="tonna")
    offer_price: Optional[float] = Field(default=None, ge=0, description="so'm/kg")
    phone: str = Field(min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    notes: Optional[str] = Field(default=None, max_length=2000, description="Izoh")


class RequestCreate(RequestBase):
    status: str = "active"


class RequestUpdate(_Trimmed):
    buyer_name: Optional[str] = Field(default=None, min_length=1, max_length=120)
    product: Optional[str] = Field(default=None, min_length=1, max_length=120)
    quantity: Optional[float] = Field(default=None, gt=0)
    offer_price: Optional[float] = Field(default=None, ge=0)
    phone: Optional[str] = Field(default=None, min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    notes: Optional[str] = Field(default=None, max_length=2000)
    status: Optional[str] = Field(default=None, max_length=20)


class RequestOut(RequestBase):
    model_config = ConfigDict(from_attributes=True)

    id: str
    status: str
    created_at: datetime
    owner_id: Optional[str] = None
    owner: Optional[OwnerOut] = None


# --------------------------- Profiles ---------------------------


class ProfileBase(_Trimmed):
    name: str = Field(min_length=1, max_length=120)
    role: Role = "dehqon"
    phone: str = Field(min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)


class ProfileCreate(ProfileBase):
    pass


class ProfileUpdate(_Trimmed):
    name: Optional[str] = Field(default=None, min_length=1, max_length=120)
    role: Optional[Role] = None
    phone: Optional[str] = Field(default=None, min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)


class ProfileOut(ProfileBase):
    model_config = ConfigDict(from_attributes=True)

    id: str
    # Google orqali yangi kirgan foydalanuvchida telefon hali bo'sh bo'ladi.
    phone: str = Field(default="", max_length=40)
    email: Optional[str] = None
    avatar_url: Optional[str] = None
    created_at: datetime
    updated_at: datetime

    # --- Instagram uslubidagi profil ko'rsatkichlari ---
    # Bu maydonlarni profile_service.serialize_profile() to'ldiradi.
    followers_count: int = 0
    following_count: int = 0
    total_listings_count: int = 0
    is_followed: bool = False
    is_me: bool = False

    # --- Ulangan kirish usullari (sozlamalar sahifasi uchun) ---
    has_google: bool = False
    has_password: bool = False
    has_apple: bool = False


class FollowUserOut(BaseModel):
    """Kuzatuvchilar / kuzatilganlar ro'yxatidagi bitta qator."""

    model_config = ConfigDict(from_attributes=True)

    id: str
    name: str
    avatar_url: Optional[str] = None
    role: Optional[str] = None
    address: Optional[str] = None
    is_followed: bool = False
    is_me: bool = False


class FollowStateOut(BaseModel):
    """Obuna tugmasi bosilgandan keyingi yangi holat."""

    is_followed: bool
    followers_count: int


# --------------------------- Autentifikatsiya ---------------------------


class TokenOut(BaseModel):
    access_token: str
    token_type: str = "bearer"
    profile: ProfileOut


class PhoneRegisterIn(_Trimmed):
    name: str = Field(min_length=1, max_length=120)
    phone: str = Field(min_length=1, max_length=40)
    password: str = Field(min_length=6, max_length=128)
    role: Role = "dehqon"


class PhoneLoginIn(_Trimmed):
    phone: str = Field(min_length=1, max_length=40)
    password: str = Field(min_length=1, max_length=128)


class PhoneLinkIn(_Trimmed):
    """Mavjud profilga telefon + parol qo'shish (token talab qilinadi)."""

    phone: str = Field(min_length=1, max_length=40)
    password: str = Field(min_length=6, max_length=128)


# --------------------------- Xabarlar ---------------------------


class ConversationCreate(BaseModel):
    other_user_id: str = Field(min_length=1, max_length=36)


class MessageCreate(_Trimmed):
    content: str = Field(min_length=1, max_length=4000)


class MessageOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: str
    conversation_id: str
    sender_id: str
    receiver_id: str
    content: str
    is_read: bool
    created_at: datetime


class ConversationOut(BaseModel):
    """Suhbatlar ro'yxati uchun — suhbatdosh, oxirgi xabar va o'qilmaganlar soni."""

    id: str
    other_user: OwnerOut
    last_message: Optional[str] = None
    last_message_at: datetime
    last_message_sender_id: Optional[str] = None
    unread_count: int = 0
    created_at: datetime


class UnreadCountOut(BaseModel):
    unread_count: int


# --------------------------- Stats / yordamchi ---------------------------


class TopProduct(BaseModel):
    product: str
    total_quantity: float
    count: int


class ProductSuggestion(BaseModel):
    product: str
    count: int
