"""Membership-row writes used inside existing authorized SQLite transactions."""


async def write_membership(db, user_id, active, actor, updated_at):
    await db.execute(
        """INSERT INTO private_members VALUES (?, ?, ?, ?)
        ON CONFLICT(user_id) DO UPDATE SET active=excluded.active,
            actor=excluded.actor, updated_at=excluded.updated_at""",
        (user_id, int(active), actor, updated_at),
    )
