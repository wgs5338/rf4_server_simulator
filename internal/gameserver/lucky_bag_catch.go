package gameserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

const luckyBagDropDefsKVKey = "lucky_bag_drop_defs"

type luckyBagDropDef struct {
	ItemID string `json:"item_id"`
	Weight int64  `json:"weight"`
}

// 静态库中有 2023 年 1..10 档定义，但客户端实际随本地 xmas-stalls100.bundle
// 提供的活动 prefab 只有 1/4/7/9（gift_bag_ny23_{1,4,7,9}）。
// 掉落池必须与客户端可实例化资源对齐，不能仅因静态 item_def 存在就发放缺 prefab 的档位。
// 默认等权仅作本地可运行兜底，不代表官方掉率；管理员可通过
// kv.lucky_bag_drop_defs 覆盖这四档权重，取得官方掉率后应以实测值替换。
var defaultLuckyBagDropDefs = []luckyBagDropDef{
	{ItemID: "gift_bag_ny23_1", Weight: 1},
	{ItemID: "gift_bag_ny23_4", Weight: 1},
	{ItemID: "gift_bag_ny23_7", Weight: 1},
	{ItemID: "gift_bag_ny23_9", Weight: 1},
}

func luckyBagItemIDSupported(itemID string) bool {
	switch itemID {
	case "gift_bag_ny23_1", "gift_bag_ny23_4", "gift_bag_ny23_7", "gift_bag_ny23_9":
		return true
	default:
		return false
	}
}

func (s *Server) luckyBagDropDefs(ctx context.Context) ([]luckyBagDropDef, error) {
	var raw string
	err := s.DB.SQL.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, luckyBagDropDefsKVKey).Scan(&raw)
	if err == nil {
		var configured []luckyBagDropDef
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("福袋掉落配置为空")
		}
		if err := json.Unmarshal([]byte(raw), &configured); err != nil {
			return nil, fmt.Errorf("福袋掉落配置解析失败: %w", err)
		}
		return configured, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return append([]luckyBagDropDef(nil), defaultLuckyBagDropDefs...), nil
}

func (s *Server) pickLuckyBagDropItem(ctx context.Context) (string, error) {
	entries, err := s.luckyBagDropDefs(ctx)
	if err != nil {
		return "", err
	}
	type eligibleEntry struct {
		itemID string
		weight int64
	}
	eligible := make([]eligibleEntry, 0, len(entries))
	var total int64
	for _, entry := range entries {
		itemID := strings.TrimSpace(entry.ItemID)
		if itemID == "" || entry.Weight <= 0 {
			continue
		}
		if !luckyBagItemIDSupported(itemID) {
			if s.log != nil {
				s.log.Warn("福袋掉落表忽略不在 2023 福袋 1..10 档位内的物品", "item", itemID)
			}
			continue
		}
		def, derr := s.DB.GetItemDefByItemID(ctx, itemID)
		if derr != nil || def == nil || def.Category != 73 || !isLuckyBag2023Def(def) {
			if s.log != nil {
				s.log.Warn("福袋掉落表忽略非 2023 category-73 定义", "item", itemID, "err", derr)
			}
			continue
		}
		attrs, attrsErr := bundleAttrs(def)
		model, modelOK := attrs["icjbboidfga"].(string)
		expectedModel := "gift_bag_" + strings.TrimPrefix(itemID, "gift_bag_ny23_")
		if attrsErr != nil || !modelOK || model != expectedModel {
			if s.log != nil {
				s.log.Warn("福袋掉落表忽略模型映射不匹配的定义", "item", itemID, "model", model, "expected", expectedModel, "err", attrsErr)
			}
			continue
		}
		cls, ok := resolveWireClass(def)
		if !ok || cls.Class != "cdmnfgijenb" || cls.TypeID != 13011 {
			if s.log != nil {
				s.log.Warn("福袋掉落表忽略错误 wire 类", "item", itemID, "wire", cls.Class, "type_id", cls.TypeID)
			}
			continue
		}
		if total > (1<<63-1)-entry.Weight {
			return "", fmt.Errorf("福袋掉落权重总和溢出")
		}
		total += entry.Weight
		eligible = append(eligible, eligibleEntry{itemID: itemID, weight: entry.Weight})
	}
	if total <= 0 || len(eligible) == 0 {
		return "", fmt.Errorf("没有可用的 category-73 福袋定义")
	}
	roll := rand.Int63n(total)
	for _, entry := range eligible {
		if roll < entry.weight {
			return entry.itemID, nil
		}
		roll -= entry.weight
	}
	return eligible[len(eligible)-1].itemID, nil
}

func luckyBagCatchReceiptKey(uid int64, fishGUID string) string {
	return fmt.Sprintf("lucky-bag-catch:%d:%s", uid, strings.ToLower(strings.TrimSpace(fishGUID)))
}

// grantLuckyBagCatch 以本次钓获 GUID 为幂等键；一尾上钩鱼只生成一个独立福袋实例。
func (s *Server) grantLuckyBagCatch(ctx context.Context, uid int64, fishGUID string) (*store.Item, bool, error) {
	if s == nil || s.DB == nil || uid <= 0 || strings.TrimSpace(fishGUID) == "" {
		return nil, false, fmt.Errorf("福袋钓获参数不完整")
	}
	key := luckyBagCatchReceiptKey(uid, fishGUID)

	var existingGUID string
	err := s.DB.SQL.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, key).Scan(&existingGUID)
	if err == nil {
		existingGUID = strings.TrimSpace(existingGUID)
		if existingGUID == "" {
			return nil, true, fmt.Errorf("福袋钓获回执为空: %s", key)
		}
		row, rowErr := s.DB.GetItemByGUID(ctx, existingGUID)
		if errors.Is(rowErr, sql.ErrNoRows) || (rowErr == nil && row == nil) {
			return nil, true, nil
		}
		if rowErr != nil {
			return nil, true, rowErr
		}
		if row.UID != uid {
			return nil, true, fmt.Errorf("福袋钓获回执所有者不匹配")
		}
		return row, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	itemID, err := s.pickLuckyBagDropItem(ctx)
	if err != nil {
		return nil, false, err
	}
	prepared, err := s.prepareNewItem(ctx, uid, itemID, "backpack", 1, "")
	if err != nil {
		return nil, false, err
	}

	tx, err := beginEconomyTx(ctx, s.DB)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureProfileTx(ctx, tx, uid); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profile SET uid=uid WHERE uid=?`, uid); err != nil {
		return nil, false, err
	}

	// 同一尾鱼的并发重放只能有一次插入；第二个请求读取第一份回执。
	err = tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, key).Scan(&existingGUID)
	if err == nil {
		existingGUID = strings.TrimSpace(existingGUID)
		row, rowErr := getOwnedItemTx(ctx, tx, uid, existingGUID)
		if errors.Is(rowErr, sql.ErrNoRows) {
			return nil, true, nil
		}
		if rowErr != nil {
			return nil, true, rowErr
		}
		return row, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if err := insertItemTx(ctx, tx, prepared); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kv(k,v) VALUES(?,?)`, key, prepared.GUID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return prepared, false, nil
}

func (s *Server) buildLuckyBagOnlySettle801(sess *Session, distanceM float64, bag *store.Item) ([]byte, error) {
	grants := proto.Args(proto.S("61"))
	if bag != nil {
		tree, err := s.ItemToTree(sess.pushCtx(), bag)
		if err != nil {
			return nil, err
		}
		grants.Items = append(grants.Items, tree)
	}
	obj801, err := wiregen.MakeObj("caagocpofin", map[int]*proto.Value{
		0:  proto.Null(),
		1:  proto.U32(proto.F32Bits(float32(distanceM))),
		2:  proto.U32(f32Const801F2),
		3:  s.build5003FromStates(nil, s.buildSeason5012()),
		4:  proto.Null(),
		5:  proto.Null(),
		6:  proto.Null(),
		7:  proto.Null(),
		8:  grants,
		9:  proto.Args(proto.S("61")),
		10: proto.Null(),
	})
	if err != nil {
		return nil, err
	}
	return proto.EncodeValue(obj801, sess.Table())
}

func clearLuckyBagCatchState(st *FishState, pending *RolledFish) {
	if st == nil {
		return
	}
	st.mu.Lock()
	if pending == nil || st.Pending == pending {
		st.Pending = nil
		st.CastActive = false
		st.Hooked = false
		st.Resolved = true
		st.persist.recoverCompleted = false
		st.persist.recoverPending = false
		st.pushing = nil
		st.cancelBiteTimerLocked()
		st.cancelPendingTimerLocked()
	}
	st.mu.Unlock()
}

func (s *Server) reloadBaitAfterLuckyBagSettle(ctx *Ctx, uid int64, gearGUID string) {
	if gearGUID == "" {
		return
	}
	cctx := ctx.Sess.pushCtx()
	if change, err := s.reloadBaitForCast(cctx, uid, gearGUID); err != nil {
		ctx.Log.Warn("福袋地图收竿后补饵失败", "rod", shortGUID(gearGUID), "err", err)
	} else if len(change.Updated) > 0 || len(change.Removed) > 0 {
		sess := ctx.Sess
		ctx.AfterResponse(func() { sess.pushBaitChange(change) })
	}
}

// fishingEndLuckyBag 保留原有咬钩/搏鱼过程，但 14/4 不再结算普通鱼：
// 上钩成功后直接把 category-73 福袋放入背包，且不写普通鱼图鉴、经验和护网。
func (s *Server) fishingEndLuckyBag(ctx *Ctx, st *FishState, pending *RolledFish, hooked bool, castProf castDistanceProfile, gearGUID string) ([]byte, bool) {
	uid := ctx.Sess.Userid
	cctx := ctx.Sess.pushCtx()
	if pending == nil || !hooked {
		if pending != nil {
			ctx.Log.Info("福袋地图未成功上鱼，不生成普通鱼或福袋", "fish", shortGUID(pending.FishGUID))
		}
		body, err := s.buildLuckyBagOnlySettle801(ctx.Sess, castProf.DistanceM, nil)
		if err != nil {
			return nil, false
		}
		clearLuckyBagCatchState(st, pending)
		s.reloadBaitAfterLuckyBagSettle(ctx, uid, gearGUID)
		return body, true
	}

	// 记录原地图键，保证服务端重启时不会把这尾活动渔获按普通鱼恢复入护。
	st.mu.Lock()
	if st.Pending == pending && pending.LocLevel == "" {
		pending.LocLevel = luckyBagMapKey
	}
	st.mu.Unlock()

	now := time.Now()
	s.expireLuckyBagSession(ctx.Sess, now)
	// 把“券仍有效”的检查与福袋入库串行化，避免到期 keepalive 在检查后、
	// 发奖前抢先撤销资格。
	s.luckyBagMu.Lock()
	_, active, err := s.luckyBagSession(cctx, uid, time.Now())
	if err != nil {
		s.luckyBagMu.Unlock()
		ctx.Log.Warn("福袋地图结算读取入场券失败", "uid", uid, "err", err)
		ctx.ReplyFailure("System.IO.IOException", "福袋入场资格暂时无法验证，请重试")
		return nil, true
	}
	if !active {
		s.luckyBagMu.Unlock()
		clearLuckyBagCatchState(st, pending)
		ctx.ReplyFailure("System.InvalidOperationException", "福袋入场券已过期，请返回原地图")
		return nil, true
	}

	bag, replayed, err := s.grantLuckyBagCatch(cctx, uid, pending.FishGUID)
	s.luckyBagMu.Unlock()
	if err != nil {
		ctx.Log.Warn("福袋地图奖励发放失败", "uid", uid, "fish", shortGUID(pending.FishGUID), "err", err)
		ctx.ReplyFailure("System.IO.IOException", "福袋奖励未能保存，请重试")
		return nil, true
	}
	body, err := s.buildLuckyBagOnlySettle801(ctx.Sess, castProf.DistanceM, bag)
	if err != nil {
		ctx.Log.Warn("福袋地图结算响应构造失败", "uid", uid, "fish", shortGUID(pending.FishGUID), "err", err)
		ctx.ReplyFailure("System.IO.IOException", "福袋已生成，但结算响应暂时无法构造，请重试")
		return nil, true
	}
	clearLuckyBagCatchState(st, pending)
	bagItemID := ""
	if bag != nil {
		bagItemID = bag.ItemID
	}
	ctx.Log.Info("福袋地图钓获福袋", "uid", uid, "fish", shortGUID(pending.FishGUID),
		"item", bagItemID, "replayed", replayed)
	if bag != nil {
		row := bag
		sess := ctx.Sess
		name := s.lootItemName(cctx, row.ItemID)
		if name == "" {
			name = row.ItemID
		}
		ctx.AfterResponse(func() {
			s.pushItemUpdate(uid, row)
			if !replayed {
				sess.pushSystemChat(fmt.Sprintf("福袋地图：获得 %s", name))
			}
		})
	}
	s.reloadBaitAfterLuckyBagSettle(ctx, uid, gearGUID)
	return body, true
}
