package gameserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
)

func TestLuckyBagCatchGrantIsBagOnlyAndIdempotent(t *testing.T) {
	t.Parallel()
	db := openItemTestDB(t)
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "lucky-bag"}}, logging.Discard(), nil)
	srv.SetDB(db)
	ctx := context.Background()
	const uid int64 = 1000083
	if err := srv.EnsurePlayer(ctx, uid); err != nil {
		t.Fatal(err)
	}

	// 测试库可能只导入 item_def；显式补齐 2023 年福袋定义。
	for tier := 1; tier <= 10; tier++ {
		itemID := fmt.Sprintf("gift_bag_ny23_%d", tier)
		numericID := int64(990230 + tier)
		flag := "null"
		if tier >= 9 {
			flag = "true"
		}
		attrs := fmt.Sprintf(`{"fpolceicapf":%s,"lpheefmebpg":"qst_ny2023","icjbboidfga":"gift_bag_%d","kmgbnidogek":0,"obifdihmcfn":%d,"dlmaaeecbci":0,"dmcgapifbdm":null,"hgaijieklek":16793600,"chfafilgifj":null}`,
			flag, tier, tier*10)
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
	}

	// 客户端 xmas-stalls100.bundle 只包含 2023 福袋 1/4/7/9 四个 prefab。
	supportedTiers := []int{1, 4, 7, 9}
	picked, err := srv.pickLuckyBagDropItem(ctx)
	if err != nil {
		t.Fatalf("读取默认福袋池: %v", err)
	}
	if !luckyBagItemIDSupported(picked) {
		t.Fatalf("默认福袋池抽到客户端缺少 prefab 的档位: %s", picked)
	}
	if len(defaultLuckyBagDropDefs) != len(supportedTiers) {
		t.Fatalf("默认福袋池应只有 4 个客户端可实例化档位，实得 %d", len(defaultLuckyBagDropDefs))
	}
	for tier := 1; tier <= 10; tier++ {
		itemID := fmt.Sprintf("gift_bag_ny23_%d", tier)
		want := tier == 1 || tier == 4 || tier == 7 || tier == 9
		if got := luckyBagItemIDSupported(itemID); got != want {
			t.Fatalf("福袋档位 %d supported=%v，want %v", tier, got, want)
		}
	}
	for _, tier := range supportedTiers {
		itemID := fmt.Sprintf("gift_bag_ny23_%d", tier)
		raw := fmt.Sprintf(`[{"item_id":%q,"weight":1}]`, itemID)
		if _, err := db.SQL.Exec(`
			INSERT INTO kv(k,v) VALUES(?,?)
			ON CONFLICT(k) DO UPDATE SET v=excluded.v`, luckyBagDropDefsKVKey, raw); err != nil {
			t.Fatal(err)
		}
		got, err := srv.pickLuckyBagDropItem(ctx)
		if err != nil || got != itemID {
			t.Fatalf("客户端可用福袋档位 %d 无法通过物品定义/模型/wire 校验: got=%q err=%v", tier, got, err)
		}
	}
	for _, entry := range defaultLuckyBagDropDefs {
		if !luckyBagItemIDSupported(entry.ItemID) {
			t.Fatalf("默认掉落池含客户端缺少 prefab 的物品: %s", entry.ItemID)
		}
	}

	// 即使后台误配了静态库存在、但客户端 bundle 没有 prefab 的第 2 档，
	// 运行时也必须拒绝，不允许 KV 覆盖绕过资源白名单。
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagDropDefsKVKey, `[{"item_id":"gift_bag_ny23_2","weight":1}]`); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.pickLuckyBagDropItem(ctx); err == nil {
		t.Fatalf("客户端缺少 prefab 的第 2 档不应被 KV 启用: %s", got)
	}

	// 用单一权重项固定测试结果，避免随机抽样影响断言。
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagDropDefsKVKey, `[{"item_id":"gift_bag_ny23_4","weight":1}]`); err != nil {
		t.Fatal(err)
	}

	const fishGUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	first, replayed, err := srv.grantLuckyBagCatch(ctx, uid, fishGUID)
	if err != nil {
		t.Fatalf("首次发放福袋: %v", err)
	}
	if replayed {
		t.Fatal("首次发放不应被标记为重放")
	}
	if first == nil || first.ItemID != "gift_bag_ny23_4" {
		t.Fatalf("发放物品=%v，期望 gift_bag_ny23_4", first)
	}
	if first.WireClass != "cdmnfgijenb" || first.TypeID != 13011 || first.Qty != 1 {
		t.Fatalf("福袋实例类型错误: wire=%s/%d qty=%d", first.WireClass, first.TypeID, first.Qty)
	}

	second, replayed, err := srv.grantLuckyBagCatch(ctx, uid, fishGUID)
	if err != nil {
		t.Fatalf("重放福袋结算: %v", err)
	}
	if !replayed || second == nil || second.GUID != first.GUID {
		t.Fatalf("重放未返回同一福袋: replayed=%v first=%v second=%v", replayed, first, second)
	}
	var count int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id=?`, uid, first.ItemID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("同一尾鱼重复发放福袋数量=%d，期望 1", count)
	}

	// 801.f0 为空、f8 只包含福袋实例，不把普通鱼作为本次渔获返回。
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	body, err := srv.buildLuckyBagOnlySettle801(sess, 123.5, first)
	if err != nil {
		t.Fatalf("构造福袋结算响应: %v", err)
	}
	decoded, _, err := proto.DecodeValue(body, 0, sess.Table())
	if err != nil {
		t.Fatalf("解码福袋结算响应: %v", err)
	}
	if decoded.Kind != proto.KindObj || len(decoded.Fields) == 0 ||
		decoded.Fields[0].Val == nil || decoded.Fields[0].Val.Kind != proto.KindNull {
		t.Fatalf("福袋结算不应返回普通鱼对象: kind=%s f0=%+v", decoded.Kind, decoded.Fields[0])
	}
	grants := decoded.Fields[8].Val
	if grants == nil || grants.Kind != proto.KindArgs || len(grants.Items) != 1 ||
		grants.Items[0] == nil || grants.Items[0].Class != "cdmnfgijenb" {
		t.Fatalf("801.f8 应只返回一个 category-73 福袋实例: %+v", grants)
	}
}

func TestLuckyBagCompletedCatchRecoveryDoesNotKeepFish(t *testing.T) {
	t.Parallel()
	db := openItemTestDB(t)
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "lucky-bag"}}, logging.Discard(), nil)
	srv.SetDB(db)
	ctx := context.Background()
	const uid int64 = 1000084
	if err := srv.EnsurePlayer(ctx, uid); err != nil {
		t.Fatal(err)
	}
	for tier := 1; tier <= 10; tier++ {
		itemID := fmt.Sprintf("gift_bag_ny23_%d", tier)
		numericID := int64(990330 + tier)
		attrs := fmt.Sprintf(`{"fpolceicapf":null,"lpheefmebpg":"qst_ny2023","icjbboidfga":"gift_bag_%d","kmgbnidogek":0,"obifdihmcfn":%d,"dlmaaeecbci":0,"dmcgapifbdm":null,"hgaijieklek":16793600,"chfafilgifj":null}`, tier, tier*10)
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
	}
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagDropDefsKVKey, `[{"item_id":"gift_bag_ny23_1","weight":1}]`); err != nil {
		t.Fatal(err)
	}

	st := srv.stateFor(uid)
	st.mu.Lock()
	st.Pending = &RolledFish{FishGUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", LocLevel: luckyBagMapKey}
	st.Hooked = true
	st.CastActive = false
	st.Resolved = false
	st.persist.recoverCompleted = true
	st.mu.Unlock()

	st.finishMu.Lock()
	entry, err := srv.recoverCompletedCatchLocked(st)
	st.finishMu.Unlock()
	if err != nil {
		t.Fatalf("恢复福袋地图待结算: %v", err)
	}
	if entry != nil {
		t.Fatalf("福袋地图恢复不应创建鱼护记录: %+v", entry)
	}
	st.mu.Lock()
	pending := st.Pending
	resolved := st.Resolved
	st.mu.Unlock()
	if pending != nil || !resolved {
		t.Fatalf("福袋地图恢复后状态未清理: pending=%v resolved=%v", pending, resolved)
	}
	var bags, catches int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='gift_bag_ny23_1'`, uid).Scan(&bags); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM keepnet_items WHERE uid=?`, uid).Scan(&catches); err != nil {
		t.Fatal(err)
	}
	if bags != 1 || catches != 0 {
		t.Fatalf("恢复结果不符: bags=%d keepnet=%d，期望 1/0", bags, catches)
	}
}


// TestLuckyBag14FourSettlementUsesEventBagHandler exercises the real 14/4 path,
// not only the bag grant helper: the ordinary fish must never enter the result.
func TestLuckyBag14FourSettlementUsesEventBagHandler(t *testing.T) {
	t.Parallel()
	srv, db := newBundleTestServer(t)
	const uid int64 = 7310
	bagInBackpack := luckyBagOpenTestSetup(t, srv, db, uid, "gift_bag_ny23_4", 997310, "qst_ny2023")
	if _, err := db.SQL.Exec(`
		INSERT INTO kv(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		luckyBagDropDefsKVKey, `[{"item_id":"gift_bag_ny23_4","weight":1}]`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.SQL.Exec(`
		INSERT INTO lucky_bag_session(
			uid,origin_level_key,origin_scene,origin_spot,expires_at,
			paid_currency,paid_amount,purchases,purchased_ts,notified_ts)
		VALUES(?,?,?,?,?,?,?,?,?,0)`,
		uid, spawnLevelKey, sceneForLevel(spawnLevelKey), 0,
		float64(now.Add(20*time.Minute).Unix()), "silver", luckyBagSilverPrice, 1,
		float64(now.Unix())); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPlayerLocation(context.Background(), uid, luckyBagMapKey, "", 0); err != nil {
		t.Fatal(err)
	}
	srv.setLevel(uid, luckyBagMapKey)

	sess := newProbeSess(srv)
	sess.Userid = uid
	sess.closed.Store(false)
	ctx := &Ctx{
		Sess: sess, Main: 14, Sub: 4, DecodeOK: true, ExpectResponse: true,
		Log: logging.Discard(), replyCapture: &ctxReplyCapture{},
	}
	st := srv.stateFor(uid)
	st.mu.Lock()
	st.Pending = &RolledFish{
		FishGUID: "cccccccccccccccccccccccccccccccc",
		LocLevel: luckyBagMapKey, Species: "perch", WeightG: 850,
	}
	st.Hooked = true
	st.CastActive = true
	st.InWater = true
	st.Resolved = false
	st.GearGuid = ""
	st.mu.Unlock()

	body, handled := hFishingEnd(ctx)
	if !handled || len(body) == 0 {
		t.Fatalf("14/4 福袋地图收竿失败: handled=%v bytes=%d", handled, len(body))
	}
	value, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind != proto.KindObj || value.TypeID != 801 {
		t.Fatalf("14/4 响应不是 801: kind=%s type=%d", value.Kind, value.TypeID)
	}
	if len(value.Fields) == 0 || value.Fields[0].Val == nil || value.Fields[0].Val.Kind != proto.KindNull {
		t.Fatalf("福袋地图 14/4 不应返回普通鱼对象，f0=%+v", value.Fields[0])
	}
	grants := value.Fields[8].Val
	if grants == nil || grants.Kind != proto.KindArgs || len(grants.Items) != 1 ||
		grants.Items[0] == nil || grants.Items[0].Class != "cdmnfgijenb" {
		t.Fatalf("14/4 奖励列表必须只包含福袋实例且不包含普通鱼卡: %+v", grants)
	}
	keptBag, err := db.GetItemByGUID(context.Background(), bagInBackpack.GUID)
	if err != nil || keptBag == nil {
		t.Fatalf("原有福袋实例应保持不变: bag=%+v err=%v", keptBag, err)
	}
	if got := luckyBagOpenItemQty(t, db, uid, "gift_bag_ny23_4"); got != 2 {
		t.Fatalf("原有福袋 + 本次钓获应为 2 个，实际 %d", got)
	}
	st.mu.Lock()
	pending, hooked, resolved := st.Pending, st.Hooked, st.Resolved
	st.mu.Unlock()
	if pending != nil || hooked || !resolved {
		t.Fatalf("14/4 后钓获状态未清理: pending=%v hooked=%v resolved=%v", pending, hooked, resolved)
	}
}
