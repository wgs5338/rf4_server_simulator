package gameserver

import (
	"context"
	"fmt"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func luckyBagOpenTestSetup(t *testing.T, srv *Server, db *store.DB, uid int64, itemID string, numericID int64, quest string) *store.Item {
	t.Helper()
	attrs := fmt.Sprintf(`{"lpheefmebpg":%q,"icjbboidfga":"gift_bag_4","obifdihmcfn":40,"hgaijieklek":16793600}`, quest)
	if _, err := db.SQL.Exec(`
		INSERT OR IGNORE INTO item_def(numeric_id,item_id,category,kind,brand,attrs_json)
		VALUES(?,?,?,?,?,?)`, numericID, itemID, 73, "gift", "", attrs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`
		INSERT OR IGNORE INTO item_catalog(item_id,name,name_zh,kind,numeric_id)
		VALUES(?,?,?,?,?)`, itemID, itemID, "福袋", "misc", numericID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	row, err := srv.prepareNewItem(context.Background(), uid, itemID, "backpack", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertItem(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return row
}

func luckyBagUseTestCtx(srv *Server, uid int64, row *store.Item) *Ctx {
	sess := newProbeSess(srv)
	sess.Userid = uid
	sess.closed.Store(false)
	tree := proto.Args524(proto.U64(0), proto.Guid(row.GUID))
	body, err := proto.EncodeValue(tree, srv.Table())
	if err != nil {
		panic(err)
	}
	return &Ctx{Sess: sess, Main: 4, Sub: 8, Tree: tree, Body: body,
		DecodeOK: true, ExpectResponse: true, Log: logging.Discard(), replyCapture: &ctxReplyCapture{}}
}

func luckyBagOpenItemQty(t *testing.T, db *store.DB, uid int64, itemID string) int64 {
	t.Helper()
	rows, err := db.ListItemsByContainer(context.Background(), uid, "backpack")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, row := range rows {
		if row.ItemID == itemID {
			total += row.Qty
		}
	}
	return total
}

func TestLuckyBagUseOpensAndGrantsConfiguredReward(t *testing.T) {
	t.Parallel()
	srv, db := newBundleTestServer(t)
	const uid int64 = 7301
	bag := luckyBagOpenTestSetup(t, srv, db, uid, "gift_bag_ny23_4", 997304, "qst_ny2023")
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagOpenDefsKVKey, `[{"item_id":"gift_bag_ny23_4","rewards":[{"item_id":"food_egg","qty":5,"weight":1}]}]`); err != nil {
		t.Fatal(err)
	}

	body, handled := hItemUse(luckyBagUseTestCtx(srv, uid, bag))
	if !handled || len(body) == 0 {
		t.Fatalf("4/8 开袋失败: handled=%v bytes=%d", handled, len(body))
	}
	value, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind != proto.KindObj || value.Class != "dlbahppbdad" || value.TypeID != 800 {
		t.Fatalf("开袋响应不是 800: kind=%s class=%s type=%d", value.Kind, value.Class, value.TypeID)
	}
	added := value.Fields[0].Val
	removed := value.Fields[2].Val
	if added == nil || added.Kind != proto.KindArgs || len(added.Items) != 1 {
		t.Fatalf("800.f0 应返回 1 个奖励实例: %+v", added)
	}
	if removed == nil || removed.Kind != proto.KindGuidArr || len(removed.Items) != 1 || removed.Items[0].Hex != bag.GUID {
		t.Fatalf("800.f2 应移除福袋 GUID: %+v", removed)
	}
	if n := luckyBagOpenItemQty(t, db, uid, "gift_bag_ny23_4"); n != 0 {
		t.Fatalf("开袋后福袋仍存在: %d", n)
	}
	reward, err := db.ListItemsByContainer(context.Background(), uid, "backpack")
	if err != nil {
		t.Fatal(err)
	}
	var qty int64
	for _, row := range reward {
		if row.ItemID == "food_egg" {
			qty += row.Qty
		}
	}
	if qty != 5 {
		t.Fatalf("开袋奖励 food_egg 数量=%d，期望5", qty)
	}
}

func TestLuckyBagUseLeavesOtherEventBagsUntouched(t *testing.T) {
	t.Parallel()
	srv, db := newBundleTestServer(t)
	const uid int64 = 7302
	bag := luckyBagOpenTestSetup(t, srv, db, uid, "gift_bag_ny22_4", 997204, "qst_ny2022")
	body, handled := hItemUse(luckyBagUseTestCtx(srv, uid, bag))
	if !handled || len(body) == 0 {
		t.Fatalf("非 2023 物品应走通用 800 回执: handled=%v bytes=%d", handled, len(body))
	}
	if n := luckyBagOpenItemQty(t, db, uid, "gift_bag_ny22_4"); n != 1 {
		t.Fatalf("非 2023 福袋不应被打开: %d", n)
	}
}

func TestLuckyBagUseRollbackKeepsBagWhenRewardInsertFails(t *testing.T) {
	t.Parallel()
	srv, db := newBundleTestServer(t)
	const uid int64 = 7303
	bag := luckyBagOpenTestSetup(t, srv, db, uid, "gift_bag_ny23_4", 997304, "qst_ny2023")
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagOpenDefsKVKey, `[{"item_id":"gift_bag_ny23_4","rewards":[{"item_id":"food_egg","qty":5,"weight":1}]}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`CREATE TRIGGER reject_lucky_bag_reward BEFORE INSERT ON item
		WHEN NEW.item_id='food_egg' BEGIN SELECT RAISE(ABORT,'test reward failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := luckyBagUseTestCtx(srv, uid, bag)
	ctx.replyCapture = &ctxReplyCapture{}
	body, handled := hItemUse(ctx)
	_, present, _, failed := ctx.replyCapture.snapshot()
	if !handled || body != nil || !present || !failed {
		t.Fatalf("奖励入库失败应返回失败回执: handled=%v body=%d present=%v failed=%v", handled, len(body), present, failed)
	}
	if n := luckyBagOpenItemQty(t, db, uid, "gift_bag_ny23_4"); n != 1 {
		t.Fatalf("奖励事务回滚后福袋应保留: %d", n)
	}
	if n := luckyBagOpenItemQty(t, db, uid, "food_egg"); n != 0 {
		t.Fatalf("奖励事务回滚后不应残留奖励: %d", n)
	}
}


// TestLuckyBagUseReplayDoesNotDuplicateReward ensures a repeated 4/8 request
// cannot consume the same bag twice or grant a second reward.
func TestLuckyBagUseReplayDoesNotDuplicateReward(t *testing.T) {
	t.Parallel()
	srv, db := newBundleTestServer(t)
	const uid int64 = 7304
	bag := luckyBagOpenTestSetup(t, srv, db, uid, "gift_bag_ny23_4", 997304, "qst_ny2023")
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagOpenDefsKVKey, `[{"item_id":"gift_bag_ny23_4","rewards":[{"item_id":"food_egg","qty":5,"weight":1}]}]`); err != nil {
		t.Fatal(err)
	}

	firstBody, firstHandled := hItemUse(luckyBagUseTestCtx(srv, uid, bag))
	if !firstHandled || len(firstBody) == 0 {
		t.Fatalf("首次开袋失败: handled=%v bytes=%d", firstHandled, len(firstBody))
	}
	replayBody, replayHandled := hItemUse(luckyBagUseTestCtx(srv, uid, bag))
	if !replayHandled || len(replayBody) == 0 {
		t.Fatalf("重复请求应安全返回空背包快照: handled=%v bytes=%d", replayHandled, len(replayBody))
	}
	if got := luckyBagOpenItemQty(t, db, uid, "food_egg"); got != 5 {
		t.Fatalf("重复请求导致奖励重复发放: food_egg=%d，期望5", got)
	}
	if got := luckyBagOpenItemQty(t, db, uid, "gift_bag_ny23_4"); got != 0 {
		t.Fatalf("重复请求后福袋不应重新出现: %d", got)
	}
}
