-- 福袋活动入场券：墙钟绝对到期时间，离线不暂停。
-- 购买时保存进入活动前的位置；活动有效期内可反复进出，不重复扣款。
-- notified_ts 用于保证在线到期只推送一次档案刷新。
CREATE TABLE IF NOT EXISTS lucky_bag_session (
    uid INTEGER PRIMARY KEY,
    origin_level_key TEXT NOT NULL,
    origin_scene TEXT NOT NULL DEFAULT '',
    origin_spot INTEGER NOT NULL DEFAULT 0,
    expires_at REAL NOT NULL,
    paid_currency TEXT NOT NULL,
    paid_amount INTEGER NOT NULL,
    purchases INTEGER NOT NULL DEFAULT 1,
    purchased_ts REAL NOT NULL,
    notified_ts REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_lucky_bag_session_expiry
    ON lucky_bag_session(expires_at, notified_ts);
