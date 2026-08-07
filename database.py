import os

from sqlalchemy import create_engine, inspect, text
from sqlalchemy.orm import DeclarativeBase, sessionmaker

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
DATABASE_URL = f"sqlite:///{os.path.join(BASE_DIR, 'agro_auksion.db')}"

engine = create_engine(
    DATABASE_URL,
    connect_args={"check_same_thread": False},
)

SessionLocal = sessionmaker(autocommit=False, autoflush=False, bind=engine)


class Base(DeclarativeBase):
    pass


def get_db():
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()


# SQLite'da Alembic ishlatilmaydi — mavjud bazaga yetishmayotgan ustunlarni qo'lda qo'shamiz.
# Har bir qadam mavjud ma'lumotlarni saqlab qoladi (faqat ADD COLUMN / CREATE INDEX).
_MIGRATIONS = {
    "profiles": {
        "email": "ALTER TABLE profiles ADD COLUMN email VARCHAR(255)",
        "avatar_url": "ALTER TABLE profiles ADD COLUMN avatar_url VARCHAR(500)",
    },
    "auctions": {
        "notes": "ALTER TABLE auctions ADD COLUMN notes TEXT",
        "owner_id": "ALTER TABLE auctions ADD COLUMN owner_id VARCHAR(36)",
    },
    "requests": {
        "notes": "ALTER TABLE requests ADD COLUMN notes TEXT",
        "owner_id": "ALTER TABLE requests ADD COLUMN owner_id VARCHAR(36)",
    },
}

_INDEXES = (
    "CREATE UNIQUE INDEX IF NOT EXISTS ix_profiles_email ON profiles (email)",
    "CREATE INDEX IF NOT EXISTS ix_auctions_owner_id ON auctions (owner_id)",
    "CREATE INDEX IF NOT EXISTS ix_requests_owner_id ON requests (owner_id)",
)


def run_migrations() -> None:
    """Eski bazada Google OAuth uchun kerakli ustunlar bo'lmasa, qo'shadi."""
    inspector = inspect(engine)
    with engine.begin() as conn:
        for table, columns in _MIGRATIONS.items():
            if not inspector.has_table(table):
                continue
            existing = {col["name"] for col in inspector.get_columns(table)}
            for name, ddl in columns.items():
                if name not in existing:
                    conn.execute(text(ddl))
        for ddl in _INDEXES:
            conn.execute(text(ddl))
