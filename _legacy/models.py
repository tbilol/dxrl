import uuid
from datetime import datetime, timezone

from sqlalchemy import (
    Boolean,
    Column,
    DateTime,
    Float,
    ForeignKey,
    String,
    Text,
    UniqueConstraint,
)
from sqlalchemy.orm import relationship

from database import Base


def new_uuid() -> str:
    return str(uuid.uuid4())


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


class Auction(Base):
    __tablename__ = "auctions"

    id = Column(String(36), primary_key=True, default=new_uuid)
    owner_id = Column(String(36), ForeignKey("profiles.id"), nullable=True, index=True)
    farmer_name = Column(String(120), nullable=False)
    product = Column(String(120), nullable=False, index=True)
    quantity = Column(Float, nullable=False)
    price = Column(Float, nullable=True)
    phone = Column(String(40), nullable=False)
    address = Column(String(255), nullable=True)
    notes = Column(Text, nullable=True)
    status = Column(String(20), nullable=False, default="active")
    created_at = Column(DateTime, nullable=False, default=utcnow)

    owner = relationship("Profile", lazy="joined")


class Request(Base):
    __tablename__ = "requests"

    id = Column(String(36), primary_key=True, default=new_uuid)
    owner_id = Column(String(36), ForeignKey("profiles.id"), nullable=True, index=True)
    buyer_name = Column(String(120), nullable=False)
    product = Column(String(120), nullable=False, index=True)
    quantity = Column(Float, nullable=False)
    offer_price = Column(Float, nullable=True)
    phone = Column(String(40), nullable=False)
    address = Column(String(255), nullable=True)
    notes = Column(Text, nullable=True)
    status = Column(String(20), nullable=False, default="active")
    created_at = Column(DateTime, nullable=False, default=utcnow)

    owner = relationship("Profile", lazy="joined")


class Profile(Base):
    __tablename__ = "profiles"

    id = Column(String(36), primary_key=True, default=new_uuid)
    name = Column(String(120), nullable=False)
    email = Column(String(255), unique=True, index=True, nullable=True)
    avatar_url = Column(String(500), nullable=True)
    role = Column(String(20), nullable=False, default="dehqon")
    phone = Column(String(40), nullable=False, default="")
    address = Column(String(255), nullable=True)

    # Telefon + parol orqali kirish. phone_hash — normallashtirilgan raqamning
    # SHA-256 yig'indisi: takrorlanmas indeks va tez qidiruv uchun.
    phone_hash = Column(String(64), unique=True, index=True, nullable=True)
    password_hash = Column(String(255), nullable=True)

    # Apple (iCloud) Sign-In identifikatori.
    apple_id = Column(String(255), unique=True, index=True, nullable=True)

    created_at = Column(DateTime, nullable=False, default=utcnow)
    updated_at = Column(DateTime, nullable=False, default=utcnow, onupdate=utcnow)


class Follow(Base):
    """Obuna: `follower_id` foydalanuvchisi `following_id` ni kuzatadi."""

    __tablename__ = "follows"
    __table_args__ = (
        UniqueConstraint("follower_id", "following_id", name="uq_follow_pair"),
    )

    id = Column(String(36), primary_key=True, default=new_uuid)
    follower_id = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    following_id = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    created_at = Column(DateTime, nullable=False, default=utcnow)

    follower = relationship("Profile", foreign_keys=[follower_id], lazy="joined")
    following = relationship("Profile", foreign_keys=[following_id], lazy="joined")


class Conversation(Base):
    __tablename__ = "conversations"

    id = Column(String(36), primary_key=True, default=new_uuid)
    participant_1 = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    participant_2 = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    last_message_at = Column(DateTime, nullable=False, default=utcnow, index=True)
    created_at = Column(DateTime, nullable=False, default=utcnow)

    user_1 = relationship("Profile", foreign_keys=[participant_1], lazy="joined")
    user_2 = relationship("Profile", foreign_keys=[participant_2], lazy="joined")

    def other_user(self, user_id: str) -> Profile:
        """Suhbatdagi ikkinchi ishtirokchini qaytaradi."""
        return self.user_2 if self.participant_1 == user_id else self.user_1


class Message(Base):
    __tablename__ = "messages"

    id = Column(String(36), primary_key=True, default=new_uuid)
    conversation_id = Column(
        String(36), ForeignKey("conversations.id"), nullable=False, index=True
    )
    sender_id = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    receiver_id = Column(String(36), ForeignKey("profiles.id"), nullable=False, index=True)
    content = Column(Text, nullable=False)
    is_read = Column(Boolean, nullable=False, default=False)
    created_at = Column(DateTime, nullable=False, default=utcnow, index=True)

    sender = relationship("Profile", foreign_keys=[sender_id], lazy="joined")
