package gameserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

const luckyBagOpenDefsKVKey = "lucky_bag_open_defs"

// luckyBagOpenDef 为某个 2023 福袋或档位配置开袋奖励。
// item_id 优先匹配具体袋子；tier 匹配静态字段 obifdihmcfn 的原始数值
//（10、20…100），可作为同档位袋子的通用配置。
// rewards 沿用 loot_box_defs 的 item_id/qty/weight 结构。
type luckyBagOpenDef struct {
	ItemID  string          `json:"item_id"`
	Tier    int64           `json:"tier"`
	Rewards []lootBoxReward `json:"rewards"`
}

func isLuckyBag2023Def(def *store.ItemDef) bool {
	if def == nil || def.Category != 73 {
		return false
	}
	if strings.HasPrefix(def.ItemID, "gift_bag_ny23_") {
		return true
	}
	attrs, err := bundleAttrs(def)
	if err != nil {
		return false
	}
	quest, _ := attrs["lpheefmebpg"].(string)
	return quest == "qst_ny2023"
}

func luckyBagTier(def *store.ItemDef) int64 {
	attrs, err := bundleAttrs(def)
	if err != nil || attrs == nil {
		return 0
	}
	tier, err := bundlePositiveQuantity(attrs["obifdihmcfn"])
	if err != nil {
		return 0
	}
	return tier
}

func (s *Server) luckyBagOpenLootDef(ctx context.Context, bagDef *store.ItemDef) (lootBoxDef, error) {
	out := lootBoxDef{ID: bagDef.ItemID, Name: "福袋"}
	var raw string
	err := s.DB.SQL.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, luckyBagOpenDefsKVKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && strings.TrimSpace(raw) == "") {
		return out, fmt.Errorf("未配置 2023 福袋奖励表")
	}
	if err != nil {
		return out, err
	}
	var configured []luckyBagOpenDef
	if err := json.Unmarshal([]byte(raw), &configured); err != nil {
		return out, fmt.Errorf("福袋开袋奖励配置解析失败: %w", err)
	}
	tier := luckyBagTier(bagDef)
	// 精确 item_id 优先，避免同档位不同外观的袋子互相覆盖。
	for _, entry := range configured {
		if strings.EqualFold(strings.TrimSpace(entry.ItemID), bagDef.ItemID) {
			if len(entry.Rewards) == 0 {
				return out, fmt.Errorf("福袋 %s 的奖励表为空", bagDef.ItemID)
			}
			out.Rewards = entry.Rewards
			return out, nil
		}
	}
	for _, entry := range configured {
		if strings.TrimSpace(entry.ItemID) == "" && entry.Tier > 0 && entry.Tier == tier {
			if len(entry.Rewards) == 0 {
				return out, fmt.Errorf("福袋档位 %d 的奖励表为空", tier)
			}
			out.Rewards = entry.Rewards
			return out, nil
		}
	}
	return out, fmt.Errorf("未找到福袋 %s（档位 %d）的奖励配置", bagDef.ItemID, tier)
}

// pickLuckyBagOpenReward 只从明确配置的福袋奖励表中抽取。
// 与通用 loot box 不同，官方福袋奖励表缺失时绝不回退到随机商店商品。
func (s *Server) pickLuckyBagOpenReward(ctx context.Context, rewards []lootBoxReward) (string, int64, string) {
	eligible := make([]lootBoxReward, 0, len(rewards))
	var total float64
	for _, reward := range rewards {
		if strings.TrimSpace(reward.ItemID) == "" || reward.Weight <= 0 ||
			!s.lootRewardItemEligible(ctx, reward.ItemID) {
			continue
		}
		eligible = append(eligible, reward)
		total += reward.Weight
		if math.IsInf(total, 0) || math.IsNaN(total) {
			return "", 0, ""
		}
	}
	if total <= 0 || len(eligible) == 0 {
		return "", 0, ""
	}
	roll := rand.Float64() * total
	for _, reward := range eligible {
		if roll < reward.Weight {
			qty := reward.Qty
			if qty <= 0 {
				qty = 1
			}
			return reward.ItemID, qty, s.lootItemName(ctx, reward.ItemID)
		}
		roll -= reward.Weight
	}
	return "", 0, ""
}

func (s *Server) prepareLuckyBagOpenReward(ctx context.Context, uid int64, itemID string, qty int64) ([]*store.Item, error) {
	if qty <= 0 {
		qty = 1
	}
	if qty > maxBundlePreparedItems {
		return nil, fmt.Errorf("福袋奖励数量超过上限: %d", qty)
	}
	def, err := s.DB.GetItemDefByItemID(ctx, itemID)
	if err != nil || def == nil {
		return nil, fmt.Errorf("福袋奖励定义不存在: %s: %w", itemID, err)
	}
	if !lootRewardCategoryEligible(def.Category) {
		return nil, fmt.Errorf("福袋奖励类别不可发放: %s category=%d", itemID, def.Category)
	}
	if countableCategories[def.Category] {
		row, err := s.prepareNewItemWithCount(ctx, uid, itemID, "backpack", qty, "")
		if err != nil {
			return nil, err
		}
		return []*store.Item{row}, nil
	}
	items := make([]*store.Item, 0, int(qty))
	for i := int64(0); i < qty; i++ {
		row, err := s.prepareNewItem(ctx, uid, itemID, "backpack", 1, "")
		if err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return items, nil
}

// useLuckyBag 处理 category-73 的 2023 福袋 4/8 使用请求。
// 先在事务外准备奖励，再在同一库存事务中删除福袋、发放奖励；任一步失败都回滚。
// 必须从 kv.lucky_bag_open_defs 读取明确的官方奖励配置；未配置时拒绝开袋，
// 不以随机商店商品冒充官方奖励。
func (s *Server) useLuckyBag(ctx *Ctx) ([]byte, bool) {
	if !s.requireDB(ctx) {
		return nil, false
	}
	if ctx.Tree == nil || ctx.Tree.Kind != proto.KindArgs || len(ctx.Tree.Items) != 2 {
		return nil, false
	}
	storage, hasStorage := ctx.ArgInt(0)
	arg := ctx.Tree.ArgItem(1)
	if arg == nil || arg.Kind != proto.KindGuid || !validRigGUID(arg.Hex) {
		return nil, false
	}
	guid := strings.ToLower(arg.Hex)
	cctx, uid := ctx.Sess.pushCtx(), ctx.Sess.Userid
	row, err := s.DB.GetItemByGUID(cctx, guid)
	if err != nil || row == nil || row.UID != uid {
		return nil, false
	}
	bagDef, err := s.DB.GetItemDefByItemID(cctx, row.ItemID)
	if err != nil || !isLuckyBag2023Def(bagDef) {
		return nil, false
	}
	fail := func(message string) ([]byte, bool) {
		ctx.ReplyFailure("System.InvalidOperationException", message)
		return nil, true
	}
	if ctx.Sess.Closed() {
		return fail("会话已关闭，请重新连接")
	}
	if !hasStorage || storage != 0 || row.Container != "backpack" || row.RigGUID != "" {
		return fail("福袋只能在背包中打开")
	}

	lootDef, err := s.luckyBagOpenLootDef(cctx, bagDef)
	if err != nil {
		ctx.Log.Warn("福袋奖励配置读取失败", "item", row.ItemID, "err", err)
		return fail("福袋奖励暂时无法读取，请重试")
	}
	rewardID, rewardQty, rewardName := s.pickLuckyBagOpenReward(cctx, lootDef.Rewards)
	if rewardID == "" || rewardQty <= 0 {
		ctx.Log.Warn("福袋没有可用奖励", "item", row.ItemID)
		return fail("福袋暂时没有可发放的奖励")
	}
	prepared, err := s.prepareLuckyBagOpenReward(cctx, uid, rewardID, rewardQty)
	if err != nil || len(prepared) == 0 {
		ctx.Log.Warn("福袋奖励构造失败", "bag", row.ItemID, "reward", rewardID, "err", err)
		return fail("福袋奖励构造失败，请重试")
	}

	tx, err := beginEconomyTx(cctx, s.DB)
	if err != nil {
		ctx.Log.Warn("福袋开启事务失败", "err", err)
		return fail("福袋打开未完成，请重试")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(cctx, `UPDATE profile SET uid=uid WHERE uid=?`, uid); err != nil {
		ctx.Log.Warn("福袋开启锁定玩家档案失败", "err", err)
		return fail("福袋打开未完成，请重试")
	}
	fresh, err := getOwnedItemTx(cctx, tx, uid, guid)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("福袋不存在或已经打开")
	}
	if err != nil {
		ctx.Log.Warn("福袋开启读取物品失败", "guid", guid, "err", err)
		return fail("福袋打开未完成，请重试")
	}
	if fresh.ItemID != row.ItemID || fresh.Container != "backpack" || fresh.RigGUID != "" {
		return fail("福袋状态已变化，请刷新背包")
	}
	removed, err := removeItemsTx(cctx, tx, uid, []string{guid})
	if err != nil || removed != 1 {
		ctx.Log.Warn("福袋删除失败", "guid", guid, "removed", removed, "err", err)
		return fail("福袋打开未完成，请重试")
	}
	granted := make([]*store.Item, 0, len(prepared))
	for _, item := range prepared {
		out, _, grantErr := grantPreparedItemTx(cctx, tx, item, true)
		if grantErr != nil {
			ctx.Log.Warn("福袋奖励入库失败", "bag", row.ItemID, "reward", item.ItemID, "err", grantErr)
			return fail("福袋奖励未能保存，请重试")
		}
		granted = append(granted, out)
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Warn("福袋开启事务提交失败", "bag", row.ItemID, "err", err)
		return fail("福袋打开未完成，请重试")
	}

	added := make([]*proto.Value, 0, len(granted))
	fallbackPush := make([]*store.Item, 0)
	for _, item := range granted {
		tree, treeErr := s.ItemToTree(cctx, item)
		if treeErr != nil {
			ctx.Log.Warn("福袋奖励树构造失败，改用背包推送", "item", item.ItemID, "err", treeErr)
			fallbackPush = append(fallbackPush, item)
			continue
		}
		added = append(added, tree)
	}
	body, err := proto.EncodeValue(buildRigSnapshot800(added, nil, []string{guid}), ctx.Sess.Table())
	if err != nil {
		ctx.Log.Warn("福袋开启响应构造失败", "bag", row.ItemID, "err", err)
		ctx.ReplyFailure("System.IO.IOException", "福袋已打开，请刷新背包查看奖励")
		return nil, true
	}
	if rewardName == "" {
		rewardName = s.lootItemName(cctx, rewardID)
	}
	if rewardName == "" {
		rewardName = rewardID
	}
	sess := ctx.Sess
	ctx.AfterResponse(func() {
		for _, item := range fallbackPush {
			s.pushItemUpdate(uid, item)
		}
		sess.pushSystemChat(fmt.Sprintf("打开福袋，获得 %s ×%d", rewardName, rewardQty))
	})
	ctx.Log.Info("打开 2023 福袋", "uid", uid, "bag", row.ItemID, "reward", rewardID, "qty", rewardQty)
	return body, true
}
