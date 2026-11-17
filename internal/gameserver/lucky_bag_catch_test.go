package gameserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"rf4server/internal/proto"
)

func seedLuckyBagDropDefsForTest(t *testing.T, db interface {
	Exec(query string, args ...any) (any, error)
}) {
	t.Helper()
}

func TestLuckyBagCatchGrantIsBagOnlyAndIdempotent(t *testing.T) {
	t.Parallel()
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid int64 = 1000083
	if err := srv.EnsurePlayer(ctx, uid); err != nil {
		t.Fatal(err)
	}

	// 测试库可能只导入 item_def；显式补齐 2023 年 1..10 福袋定义。
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

	// 默认池只取客户端登录器已有 2023 福袋系列。
	picked, err := srv.pickLuckyBagDropItem(ctx)
	if err != nil {
		t.Fatalf("读取默认福袋池: %v", err)
	}
	if !strings.HasPrefix(picked, "gift_bag_ny23_") {
		t.Fatalf("默认福袋池抽到非 2023 定义: %s", picked)
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
	sess := newProbeSess(srv)
	body, err := srv.buildLuckyBagOnlySettle801(sess, 123.5, first)
	if err != nil {
		t.Fatalf("构造福袋结算响应: %v", err)
	}
	decoded, _, err := proto.DecodeValue(body, 0, sess.Table())
	if err != nil {
		t.Fatalf("解码福袋结算响应: %v", err)
	}
	if decoded.Kind != proto.KindObj || decoded.Fields[0].Kind != proto.KindNull {
		t.Fatalf("福袋结算不应返回普通鱼对象: kind=%s f0=%v", decoded.Kind, decoded.Fields[0])
	}
	grants := decoded.Fields[8]
	if grants == nil || len(grants.Items) != 2 || grants.Items[1].Class != "cdmnfgijenb" {
		t.Fatalf("801.f8 未返回 category-73 福袋实例: %+v", grants)
	}
}

func TestLuckyBagCompletedCatchRecoveryDoesNotKeepFish(t *testing.T) {
	t.Parallel()
	srv, db := newRigTestServer(t)
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

	st := srv.newFishState(uid, nil)
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
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM keepnet WHERE uid=?`, uid).Scan(&catches); err != nil {
		t.Fatal(err)
	}
	if bags != 1 || catches != 0 {
		t.Fatalf("恢复结果不符: bags=%d keepnet=%d，期望 1/0", bags, catches)
	}
}
