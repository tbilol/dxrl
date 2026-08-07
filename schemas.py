from datetime import datetime
from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field, field_validator

Role = Literal["dehqon", "sotib_oluvchi"]


class _Trimmed(BaseModel):
    @field_validator("*", mode="before")
    @classmethod
    def strip_strings(cls, v):
        return v.strip() if isinstance(v, str) else v


# --------------------------- Auctions ---------------------------


class AuctionBase(_Trimmed):
    farmer_name: str = Field(min_length=1, max_length=120)
    product: str = Field(min_length=1, max_length=120)
    quantity: float = Field(gt=0, description="tonna")
    price: Optional[float] = Field(default=None, ge=0, description="so'm/kg")
    phone: str = Field(min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)


class AuctionCreate(AuctionBase):
    status: str = "active"


class AuctionUpdate(_Trimmed):
    farmer_name: Optional[str] = Field(default=None, min_length=1, max_length=120)
    product: Optional[str] = Field(default=None, min_length=1, max_length=120)
    quantity: Optional[float] = Field(default=None, gt=0)
    price: Optional[float] = Field(default=None, ge=0)
    phone: Optional[str] = Field(default=None, min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    status: Optional[str] = Field(default=None, max_length=20)


class AuctionOut(AuctionBase):
    model_config = ConfigDict(from_attributes=True)

    id: str
    status: str
    created_at: datetime


# --------------------------- Requests ---------------------------


class RequestBase(_Trimmed):
    buyer_name: str = Field(min_length=1, max_length=120)
    product: str = Field(min_length=1, max_length=120)
    quantity: float = Field(gt=0, description="tonna")
    offer_price: Optional[float] = Field(default=None, ge=0, description="so'm/kg")
    phone: str = Field(min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)


class RequestCreate(RequestBase):
    status: str = "active"


class RequestUpdate(_Trimmed):
    buyer_name: Optional[str] = Field(default=None, min_length=1, max_length=120)
    product: Optional[str] = Field(default=None, min_length=1, max_length=120)
    quantity: Optional[float] = Field(default=None, gt=0)
    offer_price: Optional[float] = Field(default=None, ge=0)
    phone: Optional[str] = Field(default=None, min_length=1, max_length=40)
    address: Optional[str] = Field(default=None, max_length=255)
    status: Optional[str] = Field(default=None, max_length=20)


class RequestOut(RequestBase):
    model_config = ConfigDict(from_attributes=True)

    id: str
    status: str
    created_at: datetime


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
    created_at: datetime
    updated_at: datetime


# --------------------------- Stats ---------------------------


class TopProduct(BaseModel):
    product: str
    total_quantity: float
    count: int
