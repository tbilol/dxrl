import uuid
from datetime import datetime, timezone

from sqlalchemy import Column, DateTime, Float, String

from database import Base


def new_uuid() -> str:
    return str(uuid.uuid4())


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


class Auction(Base):
    __tablename__ = "auctions"

    id = Column(String(36), primary_key=True, default=new_uuid)
    farmer_name = Column(String(120), nullable=False)
    product = Column(String(120), nullable=False, index=True)
    quantity = Column(Float, nullable=False)
    price = Column(Float, nullable=True)
    phone = Column(String(40), nullable=False)
    address = Column(String(255), nullable=True)
    status = Column(String(20), nullable=False, default="active")
    created_at = Column(DateTime, nullable=False, default=utcnow)


class Request(Base):
    __tablename__ = "requests"

    id = Column(String(36), primary_key=True, default=new_uuid)
    buyer_name = Column(String(120), nullable=False)
    product = Column(String(120), nullable=False, index=True)
    quantity = Column(Float, nullable=False)
    offer_price = Column(Float, nullable=True)
    phone = Column(String(40), nullable=False)
    address = Column(String(255), nullable=True)
    status = Column(String(20), nullable=False, default="active")
    created_at = Column(DateTime, nullable=False, default=utcnow)


class Profile(Base):
    __tablename__ = "profiles"

    id = Column(String(36), primary_key=True, default=new_uuid)
    name = Column(String(120), nullable=False)
    role = Column(String(20), nullable=False, default="dehqon")
    phone = Column(String(40), nullable=False)
    address = Column(String(255), nullable=True)
    created_at = Column(DateTime, nullable=False, default=utcnow)
    updated_at = Column(DateTime, nullable=False, default=utcnow, onupdate=utcnow)
