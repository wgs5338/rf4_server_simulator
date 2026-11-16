-- Preserve player-to-player sent mail independently from the recipient inbox.
-- Inbox claims clear items_json and inbox deletes remove mail rows; neither action
-- should erase the sender's native sent-mail history.
CREATE TABLE IF NOT EXISTS sent_mail (
    message_id INTEGER PRIMARY KEY,
    recipient INTEGER NOT NULL,
    sender INTEGER NOT NULL,
    kind INTEGER NOT NULL DEFAULT 0,
    text TEXT NOT NULL DEFAULT '',
    items_json TEXT,
    created_ts REAL NOT NULL DEFAULT 0,
    read INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_sent_mail_sender_recipient_message
    ON sent_mail(sender, recipient, message_id DESC);
CREATE INDEX IF NOT EXISTS idx_sent_mail_sender_message
    ON sent_mail(sender, message_id DESC);

-- Backfill all still-present player-to-player mail. Attachments already claimed
-- before this migration cannot be reconstructed because the inbox row cleared
-- their items_json; future sends are archived by the trigger below.
INSERT OR IGNORE INTO sent_mail(
    message_id, recipient, sender, kind, text, items_json, created_ts, read
)
SELECT id, uid, sender, kind, COALESCE(text,''), items_json,
       COALESCE(created_ts,0), COALESCE(read,0)
FROM mail
WHERE sender > 0 AND uid > 0 AND sender <> uid;

CREATE TRIGGER IF NOT EXISTS mail_sent_archive_after_insert
AFTER INSERT ON mail
WHEN NEW.sender > 0 AND NEW.uid > 0 AND NEW.sender <> NEW.uid
BEGIN
    INSERT OR IGNORE INTO sent_mail(
        message_id, recipient, sender, kind, text, items_json, created_ts, read
    ) VALUES(
        NEW.id, NEW.uid, NEW.sender, NEW.kind, COALESCE(NEW.text,''),
        NEW.items_json, COALESCE(NEW.created_ts,0), COALESCE(NEW.read,0)
    );
END;

CREATE TRIGGER IF NOT EXISTS mail_sent_archive_read_after_update
AFTER UPDATE OF read ON mail
WHEN NEW.read <> OLD.read
BEGIN
    UPDATE sent_mail SET read=NEW.read WHERE message_id=NEW.id;
END;
