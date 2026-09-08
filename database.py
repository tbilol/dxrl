import os

from sqlalchemy import create_engine, inspect, text
from sqlalchemy.orm import DeclarativeBase, sessionmaker

from logger import log, log_error

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
DATABASE_URL = f"sqlite:///{os.path.join(BASE_DIR, 'agro_auksion.db')}"

engine = create_engine(
    DATABASE_URL,
    connect_args={"check_same_thread": False},
)

SessionLocal = sessionmaker(autocommit=False, autoflush=False, bind=engine)

log("DB", "Baza ulandi", os.path.join(BASE_DIR, "agro_auksion.db"))


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
        # Telefon + parol va Apple Sign-In uchun (AGROGRAM 3.0).
        "phone_hash": "ALTER TABLE profiles ADD COLUMN phone_hash VARCHAR(64)",
        "password_hash": "ALTER TABLE profiles ADD COLUMN password_hash VARCHAR(255)",
        "apple_id": "ALTER TABLE profiles ADD COLUMN apple_id VARCHAR(255)",
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
    "CREATE UNIQUE INDEX IF NOT EXISTS ix_profiles_phone_hash ON profiles (phone_hash)",
    "CREATE UNIQUE INDEX IF NOT EXISTS ix_profiles_apple_id ON profiles (apple_id)",
    "CREATE INDEX IF NOT EXISTS ix_auctions_owner_id ON auctions (owner_id)",
    "CREATE INDEX IF NOT EXISTS ix_requests_owner_id ON requests (owner_id)",
    "CREATE INDEX IF NOT EXISTS ix_follows_follower_id ON follows (follower_id)",
    "CREATE INDEX IF NOT EXISTS ix_follows_following_id ON follows (following_id)",
    "CREATE UNIQUE INDEX IF NOT EXISTS uq_follow_pair ON follows (follower_id, following_id)",
)


def run_migrations() -> None:
    """Eski bazaga yetishmayotgan ustun/indekslarni qo'shadi (ma'lumot yo'qolmaydi).

    `Base.metadata.create_all()` yangi jadvallarni (masalan `follows`) o'zi
    yaratadi, bu yerda esa mavjud jadvallarga ustun qo'shiladi.
    """
    inspector = inspect(engine)
    added = 0
    try:
        with engine.begin() as conn:
            for table, columns in _MIGRATIONS.items():
                if not inspector.has_table(table):
                    continue
                existing = {col["name"] for col in inspector.get_columns(table)}
                for name, ddl in columns.items():
                    if name not in existing:
                        log("DB", "Migratsiya: ustun qo'shilmoqda", f"{table}.{name}")
                        conn.execute(text(ddl))
                        added += 1
            for ddl in _INDEXES:
                table = ddl.rsplit(" ON ", 1)[1].split(" ", 1)[0]
                if inspector.has_table(table):
                    conn.execute(text(ddl))
    except Exception as err:  # noqa: BLE001 — log qilib qayta ko'taramiz
        log_error("DB", "Migratsiya bajarilmadi", err)
        raise

    log("DB", "Migratsiya tugadi", f"yangi ustunlar={added}, indekslar={len(_INDEXES)}")
