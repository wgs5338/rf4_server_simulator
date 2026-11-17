package gameserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

const (
	luckyBagEventKey       = "level_lucky_bag"
	luckyBagMapKey         = "level_ban"
	luckyBagDurationSec    = int64(30 * 60)
	luckyBagMaxPlayers     = 20
	luckyBagSilverPrice    = int64(100_000_000) // 1,000,000 银；钱包单位为分
	luckyBagGoldPrice      = int64(800)         // 8 金；钱包单位为金币百分位
)

func isLuckyBagEventKey(key string) bool {
	return key == luckyBagEventKey || canonicalLevelKey(key) == luckyBagMapKey
}

func luckyBagPriceMatches(gold bool, price int64) bool {
	if gold {
		return price == luckyBagGoldPrice || price == 8
	}
	// 兼容客户端活动面板按显示银币数传参；服务端始终按钱包分单位扣款。
	return price == luckyBagSilverPrice || price == 1_000_000
}

func (s *Server) luckyBagSession(ctx context.Context, uid int64, now time.Time) (*store.LuckyBagSession, bool, error) {
	if s == nil || s.DB == nil {
		return nil, false, errors.New("lucky bag database unavailable")
	}
	v, err := s.DB.GetLuckyBagSession(ctx, uid)
	if err != nil || v == nil {
		return v, false, err
	}
	active := v.ExpiresAt > float64(now.Unix()) && v.NotifiedTS == 0
	return v, active, nil
}

// luckyBagOnlinePlayers 只统计当前在线且已定位到惩戒池塘的唯一 UID。
func (s *Server) luckyBagOnlinePlayers() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, uid := range s.OnlineUIDs() {
		if s.currentLevel(uid) == luckyBagMapKey {
			n++
		}
	}
	return n
}

func (s *Server) canEnterLuckyBag(uid int64) bool {
	if s == nil {
		return false
	}
	// 已在场玩家的同图重定位不额外占用名额。
	if s.currentLevel(uid) == luckyBagMapKey {
		return true
	}
	return s.luckyBagOnlinePlayers() < luckyBagMaxPlayers
}

// buyLuckyBagTicket 处理 3/38 的福袋入场券购买。
// 票券购买即启表，expires_at 是 Unix 墙钟时间，离线不会暂停。
func (s *Server) buyLuckyBagTicket(ctx *Ctx, requestedPlace string, gold bool, quotedPrice int64) ([]byte, bool) {
	uid := ctx.Sess.Userid
	cctx := ctx.Sess.pushCtx()
	now := time.Now()

	if !luckyBagPriceMatches(gold, quotedPrice) {
		ctx.Log.Info("福袋入场券报价不符，不扣款",
			"place", requestedPlace, "gold", gold, "price", quotedPrice)
		ctx.ReplyFailure("System.InvalidOperationException", "福袋入场券价格已变化，请重新打开活动界面")
		return nil, true
	}
	if err := s.DB.EnsureProfile(cctx, uid); err != nil {
		ctx.Log.Warn("福袋入场券建立玩家档案失败", "err", err)
		return nil, false
	}

	// 过期时先回收在线地图位置并撤销旧券；有效期内重复点击只返回现有券，不再扣款。
	s.expireLuckyBagSession(ctx.Sess, now)
	if existing, active, err := s.luckyBagSession(cctx, uid, now); err != nil {
		ctx.Log.Warn("福袋入场券状态读取失败", "err", err)
		return nil, false
	} else if active {
		p, err := s.BuildProfile307W(cctx, uid, worldTotalGameMin(now))
		if err != nil {
			ctx.Log.Warn("福袋有效券档案构造失败", "err", err)
			return nil, false
		}
		body, err := proto.EncodeValue(p, ctx.Sess.Table())
		if err != nil {
			return nil, false
		}
		ctx.Log.Info("福袋入场券仍有效，复用现有资格",
			"expires_at", existing.ExpiresAt, "purchases", existing.Purchases)
		return body, true
	}

	s.luckyBagMu.Lock()
	defer s.luckyBagMu.Unlock()

	current := s.currentLevel(uid)
	if current != luckyBagMapKey && s.luckyBagOnlinePlayers() >= luckyBagMaxPlayers {
		ctx.Log.Info("福袋地图已满，拒绝新购票", "uid", uid, "capacity", luckyBagMaxPlayers)
		ctx.ReplyFailure("System.InvalidOperationException", "福袋地图当前已满，请等待其他玩家退出")
		return nil, true
	}

	tx, err := beginEconomyTx(cctx, s.DB)
	if err != nil {
		ctx.Log.Warn("福袋入场券开启事务失败", "err", err)
		return nil, false
	}
	defer func() { _ = tx.Rollback() }()

	// 事务内锁定玩家档案，防止并发购票重复扣款。
	if _, err := tx.ExecContext(cctx, `UPDATE profile SET uid=uid WHERE uid=?`, uid); err != nil {
		ctx.Log.Warn("福袋入场券锁定玩家档案失败", "err", err)
		return nil, false
	}
	var level int64
	var originLevel, originScene string
	var originSpot int64
	if err := tx.QueryRowContext(cctx, `
		SELECT level, COALESCE(last_level_key,''), COALESCE(last_scene,''), COALESCE(last_spot,0)
		FROM profile WHERE uid=?`, uid).Scan(&level, &originLevel, &originScene, &originSpot); err != nil {
		ctx.Log.Warn("福袋入场券读取玩家等级/位置失败", "err", err)
		return nil, false
	}
	if level <= 25 {
		ctx.Log.Info("玩家等级不足，拒绝进入福袋地图", "uid", uid, "level", level)
		ctx.ReplyFailure("System.InvalidOperationException", "等级达到 26 级后才能进入福袋地图")
		return nil, true
	}

	// 防止两个并发 3/38 在第一次 active 查询后同时扣款。
	var old store.LuckyBagSession
	oldErr := tx.QueryRowContext(cctx, `
		SELECT uid, origin_level_key, origin_scene, origin_spot, expires_at,
		       paid_currency, paid_amount, purchases, purchased_ts, notified_ts
		FROM lucky_bag_session WHERE uid=?`, uid).Scan(
		&old.UID, &old.OriginLevelKey, &old.OriginScene, &old.OriginSpot, &old.ExpiresAt,
		&old.PaidCurrency, &old.PaidAmount, &old.Purchases, &old.PurchasedTS, &old.NotifiedTS)
	if oldErr != nil && !errors.Is(oldErr, sql.ErrNoRows) {
		ctx.Log.Warn("福袋入场券读取旧状态失败", "err", oldErr)
		return nil, false
	}
	if oldErr == nil && old.ExpiresAt > float64(now.Unix()) && old.NotifiedTS == 0 {
		if err := tx.Rollback(); err != nil {
			ctx.Log.Warn("福袋有效券事务回滚失败", "err", err)
		}
		p, err := s.BuildProfile307W(cctx, uid, worldTotalGameMin(now))
		if err != nil {
			return nil, false
		}
		body, err := proto.EncodeValue(p, ctx.Sess.Table())
		return body, err == nil
	}

	originLevel = travelLevelKey(originLevel)
	// 玩家若在旧券过期后仍停留在活动地图，下一张券仍以旧券记录的原地图为返回点。
	if current == luckyBagMapKey || originLevel == luckyBagMapKey {
		if oldErr == nil && old.OriginLevelKey != "" && validLevelKey(old.OriginLevelKey) {
			originLevel, originScene, originSpot = old.OriginLevelKey, old.OriginScene, old.OriginSpot
		} else {
			originLevel, originScene, originSpot = spawnLevelKey, sceneForLevel(spawnLevelKey), 0
		}
	}
	if originLevel == "" || !validLevelKey(originLevel) || originLevel == luckyBagMapKey {
		originLevel, originScene, originSpot = spawnLevelKey, sceneForLevel(spawnLevelKey), 0
	}
	if originScene == "" {
		originScene = sceneForLevel(originLevel)
	}

	payAmount := luckyBagSilverPrice
	currency := "silver"
	silverDelta, goldDelta := -luckyBagSilverPrice, int64(0)
	if gold {
		payAmount = luckyBagGoldPrice
		currency = "gold"
		silverDelta, goldDelta = 0, -luckyBagGoldPrice
	}
	if err := applyWalletTx(cctx, tx, uid, silverDelta, goldDelta,
		"buy", luckyBagEventKey, "", "lucky_bag_ticket"); err != nil {
		if errors.Is(err, errInsufficientFunds) {
			ctx.Log.Info("福袋入场券余额不足", "uid", uid, "currency", currency, "amount", payAmount)
			ctx.ReplyFailure("System.InvalidOperationException", "余额不足，未购买福袋入场券")
			return nil, true
		}
		ctx.Log.Warn("福袋入场券扣款失败", "currency", currency, "amount", payAmount, "err", err)
		return nil, false
	}

	purchasedTS := float64(now.Unix())
	session := store.LuckyBagSession{
		UID: uid, OriginLevelKey: originLevel, OriginScene: originScene, OriginSpot: originSpot,
		ExpiresAt: purchasedTS + float64(luckyBagDurationSec),
		PaidCurrency: currency, PaidAmount: payAmount, Purchases: 1, PurchasedTS: purchasedTS,
	}
	if err := s.DB.UpsertLuckyBagSessionTx(cctx, tx, session); err != nil {
		ctx.Log.Warn("福袋入场券状态写入失败", "err", err)
		return nil, false
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Warn("福袋入场券事务提交失败", "err", err)
		return nil, false
	}

	ctx.Log.Info("购买福袋入场券", "uid", uid, "currency", currency, "amount", payAmount,
		"expires_at", session.ExpiresAt, "origin", originLevel, "origin_spot", originSpot)
	p, err := s.BuildProfile307W(cctx, uid, worldTotalGameMin(now))
	if err != nil {
		ctx.Log.Warn("福袋入场券档案构造失败", "err", err)
		return nil, false
	}
	body, err := proto.EncodeValue(p, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	ctx.Sess.Push2_1()
	return body, true
}

// expireLuckyBagSession 处理墙钟到期。若玩家仍在活动地图，服务端先把持久化位置
// 切回入场前地图；随后推送 2/1 移除 f23 入场券，客户端 LuckyBagReturn hook
// 负责同步完成场景返回。离线到期由 1/13 登录恢复路径调用同一函数。
func (s *Server) expireLuckyBagSession(sess *Session, now time.Time) bool {
	if s == nil || s.DB == nil || sess == nil || sess.Userid <= 0 {
		return false
	}
	s.luckyBagMu.Lock()
	defer s.luckyBagMu.Unlock()

	ctx := sess.pushCtx()
	v, err := s.DB.GetLuckyBagSession(ctx, sess.Userid)
	if err != nil || v == nil || v.ExpiresAt > float64(now.Unix()) || v.NotifiedTS > 0 {
		return false
	}
	inEventMap := s.currentLevel(sess.Userid) == luckyBagMapKey

	tx, err := s.DB.SQL.BeginTx(ctx, nil)
	if err != nil {
		s.log.Warn("福袋入场券到期事务开启失败", "uid", sess.Userid, "err", err)
		return false
	}
	defer func() { _ = tx.Rollback() }()
	marked, err := s.DB.MarkLuckyBagSessionNotifiedTx(ctx, tx, sess.Userid, float64(now.Unix()))
	if err != nil || !marked {
		if err != nil {
			s.log.Warn("福袋入场券到期标记失败", "uid", sess.Userid, "err", err)
		}
		return false
	}
	originLevel := v.OriginLevelKey
	if originLevel == "" || !validLevelKey(originLevel) || originLevel == luckyBagMapKey {
		originLevel = spawnLevelKey
	}
	originScene := v.OriginScene
	if originScene == "" {
		originScene = sceneForLevel(originLevel)
	}
	if inEventMap {
		if err := s.DB.SetPlayerLocationTx(ctx, tx, sess.Userid, originLevel, originScene, v.OriginSpot); err != nil {
			s.log.Warn("福袋到期恢复原地图位置失败", "uid", sess.Userid, "err", err)
			return false
		}
	}
	if err := tx.Commit(); err != nil {
		s.log.Warn("福袋入场券到期事务提交失败", "uid", sess.Userid, "err", err)
		return false
	}
	if inEventMap {
		s.setLevel(sess.Userid, originLevel)
		sess.rememberWeather(originLevel, now.Unix())
	}
	sess.Push2_1()
	s.log.Info("福袋入场券到期", "uid", sess.Userid, "was_in_map", inEventMap,
		"origin", originLevel, "expires_at", v.ExpiresAt)
	return true
}
