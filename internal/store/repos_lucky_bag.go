package store

import (
	"context"
	"database/sql"
	"errors"
)

// LuckyBagSession 是福袋活动的 30 min 墙钟入场券。
// ExpiresAt 使用 Unix 秒；与普通 trip_state 的在线剩余时长严格分离。
type LuckyBagSession struct {
	UID            int64
	OriginLevelKey string
	OriginScene    string
	OriginSpot     int64
	ExpiresAt      float64
	PaidCurrency   string
	PaidAmount     int64
	Purchases      int64
	PurchasedTS    float64
	NotifiedTS     float64
}

// GetLuckyBagSession 读取玩家当前/最近一次福袋活动入场券。
// 过期行保留原始位置，供玩家离线跨过期后登录恢复。
func (d *DB) GetLuckyBagSession(ctx context.Context, uid int64) (*LuckyBagSession, error) {
	var v LuckyBagSession
	err := d.SQL.QueryRowContext(ctx, `
		SELECT uid, origin_level_key, origin_scene, origin_spot, expires_at,
		       paid_currency, paid_amount, purchases, purchased_ts, notified_ts
		FROM lucky_bag_session WHERE uid=?`, uid).Scan(
		&v.UID, &v.OriginLevelKey, &v.OriginScene, &v.OriginSpot, &v.ExpiresAt,
		&v.PaidCurrency, &v.PaidAmount, &v.Purchases, &v.PurchasedTS, &v.NotifiedTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// UpsertLuckyBagSessionTx 在扣款事务内创建/续建入场券。
func (d *DB) UpsertLuckyBagSessionTx(ctx context.Context, tx *sql.Tx, v LuckyBagSession) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO lucky_bag_session(
			uid, origin_level_key, origin_scene, origin_spot, expires_at,
			paid_currency, paid_amount, purchases, purchased_ts, notified_ts)
		VALUES(?,?,?,?,?,?,?,?,?,0)
		ON CONFLICT(uid) DO UPDATE SET
			origin_level_key=excluded.origin_level_key,
			origin_scene=excluded.origin_scene,
			origin_spot=excluded.origin_spot,
			expires_at=excluded.expires_at,
			paid_currency=excluded.paid_currency,
			paid_amount=excluded.paid_amount,
			purchases=lucky_bag_session.purchases+1,
			purchased_ts=excluded.purchased_ts,
			notified_ts=0`,
		v.UID, v.OriginLevelKey, v.OriginScene, v.OriginSpot, v.ExpiresAt,
		v.PaidCurrency, v.PaidAmount, max64(v.Purchases, 1), v.PurchasedTS)
	return err
}

// MarkLuckyBagSessionNotifiedTx 将到期券标记为已通知，CAS 保证只成功一次。
func (d *DB) MarkLuckyBagSessionNotifiedTx(ctx context.Context, tx *sql.Tx, uid int64, now float64) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE lucky_bag_session SET notified_ts=?
		WHERE uid=? AND expires_at<=? AND notified_ts=0`, now, uid, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
