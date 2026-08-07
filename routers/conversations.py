"""Ichki xabar almashish (Instagram uslubidagi DM)."""

from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query, status
from sqlalchemy import func, or_
from sqlalchemy.orm import Session

import schemas
from auth import get_current_user
from database import get_db
from models import Conversation, Message, Profile, utcnow

router = APIRouter(prefix="/api/conversations", tags=["conversations"])

MESSAGE_PREVIEW_LEN = 120


def _get_conversation_or_404(
    db: Session, conversation_id: str, current_user: Profile
) -> Conversation:
    conv = db.get(Conversation, conversation_id)
    if conv is None:
        raise HTTPException(status_code=404, detail="Suhbat topilmadi")
    if current_user.id not in (conv.participant_1, conv.participant_2):
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="Bu suhbat sizga tegishli emas",
        )
    return conv


@router.post("", response_model=schemas.ConversationOut, status_code=status.HTTP_201_CREATED)
def create_or_get_conversation(
    payload: schemas.ConversationCreate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Suhbat bo'lsa — o'shani, bo'lmasa yangisini qaytaradi."""
    other_id = payload.other_user_id
    if other_id == current_user.id:
        raise HTTPException(status_code=400, detail="O'zingizga xabar yoza olmaysiz")

    other = db.get(Profile, other_id)
    if other is None:
        raise HTTPException(status_code=404, detail="Foydalanuvchi topilmadi")

    conv = (
        db.query(Conversation)
        .filter(
            or_(
                (Conversation.participant_1 == current_user.id)
                & (Conversation.participant_2 == other_id),
                (Conversation.participant_1 == other_id)
                & (Conversation.participant_2 == current_user.id),
            )
        )
        .first()
    )

    if conv is None:
        conv = Conversation(
            participant_1=current_user.id,
            participant_2=other_id,
            last_message_at=utcnow(),
        )
        db.add(conv)
        db.commit()
        db.refresh(conv)

    return _serialize_conversation(db, conv, current_user)


def _serialize_conversation(
    db: Session, conv: Conversation, current_user: Profile
) -> schemas.ConversationOut:
    last = (
        db.query(Message)
        .filter(Message.conversation_id == conv.id)
        .order_by(Message.created_at.desc())
        .first()
    )
    unread = (
        db.query(func.count(Message.id))
        .filter(
            Message.conversation_id == conv.id,
            Message.receiver_id == current_user.id,
            Message.is_read.is_(False),
        )
        .scalar()
        or 0
    )
    other = conv.other_user(current_user.id)

    return schemas.ConversationOut(
        id=conv.id,
        other_user=schemas.OwnerOut.model_validate(other),
        last_message=(last.content[:MESSAGE_PREVIEW_LEN] if last else None),
        last_message_at=conv.last_message_at,
        last_message_sender_id=(last.sender_id if last else None),
        unread_count=unread,
        created_at=conv.created_at,
    )


@router.get("", response_model=list[schemas.ConversationOut])
def list_conversations(
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Foydalanuvchining barcha suhbatlari — eng so'nggisi yuqorida."""
    convs = (
        db.query(Conversation)
        .filter(
            or_(
                Conversation.participant_1 == current_user.id,
                Conversation.participant_2 == current_user.id,
            )
        )
        .order_by(Conversation.last_message_at.desc())
        .all()
    )
    return [_serialize_conversation(db, c, current_user) for c in convs]


@router.get("/unread-count", response_model=schemas.UnreadCountOut)
def unread_count(
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Barcha suhbatlardagi o'qilmagan xabarlar soni."""
    total = (
        db.query(func.count(Message.id))
        .filter(Message.receiver_id == current_user.id, Message.is_read.is_(False))
        .scalar()
        or 0
    )
    return schemas.UnreadCountOut(unread_count=total)


@router.get("/{conversation_id}/messages", response_model=list[schemas.MessageOut])
def list_messages(
    conversation_id: str,
    limit: int = Query(default=50, ge=1, le=200),
    before: Optional[str] = Query(
        default=None, description="Shu xabar ID'sidan oldingilarini yuklash"
    ),
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    """Suhbatdagi xabarlar (eskidan yangiga). Ochilganda kelgan xabarlar o'qilgan deb belgilanadi."""
    conv = _get_conversation_or_404(db, conversation_id, current_user)

    query = db.query(Message).filter(Message.conversation_id == conv.id)
    if before:
        anchor = db.get(Message, before)
        if anchor is not None:
            query = query.filter(Message.created_at < anchor.created_at)

    # Oxirgi `limit` ta xabarni olamiz, so'ng vaqt bo'yicha to'g'ri tartibga qaytaramiz.
    batch = query.order_by(Message.created_at.desc()).limit(limit).all()
    batch.reverse()

    unread = [m for m in batch if m.receiver_id == current_user.id and not m.is_read]
    if unread:
        for m in unread:
            m.is_read = True
        db.commit()

    return batch


@router.post(
    "/{conversation_id}/messages",
    response_model=schemas.MessageOut,
    status_code=status.HTTP_201_CREATED,
)
def send_message(
    conversation_id: str,
    payload: schemas.MessageCreate,
    db: Session = Depends(get_db),
    current_user: Profile = Depends(get_current_user),
):
    conv = _get_conversation_or_404(db, conversation_id, current_user)
    receiver_id = (
        conv.participant_2 if conv.participant_1 == current_user.id else conv.participant_1
    )

    message = Message(
        conversation_id=conv.id,
        sender_id=current_user.id,
        receiver_id=receiver_id,
        content=payload.content,
    )
    db.add(message)
    conv.last_message_at = utcnow()
    db.commit()
    db.refresh(message)
    return message
