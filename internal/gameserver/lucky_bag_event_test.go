package gameserver

import (
	"context"
	"testing"
	"time"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
)

func TestLuckyBagCapacityMatchesClientLimit(t *testing.T) {
	if luckyBagMaxPlayers != 30 {
		t.Fatalf("福袋地图人数上限=%d，客户端显示为 30", luckyBagMaxPlayers)
	}

	tests := []struct {
		name        string
		count       int
		selfCounted bool
		want        bool
	}{
		{name: "new player enters with 29 online", count: 29, want: true},
		{name: "new player rejected at 30 online", count: 30, want: false},
		{name: "existing player can resume at 30 online", count: 30, selfCounted: true, want: true},
		{name: "existing player rejected above capacity", count: 31, selfCounted: true, want: false},
		{name: "negative count rejected", count: -1, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := luckyBagCapacityAllows(tt.count, tt.selfCounted); got != tt.want {
				t.Fatalf("luckyBagCapacityAllows(%d, %v)=%v, want %v",
					tt.count, tt.selfCounted, got, tt.want)
			}
		})
	}
}


// TestLuckyBagDirectCastCannotBypassExpiredTicket verifies 14/2 is protected
// even when a client skips the normal 14/1 cast-prepare request.
func TestLuckyBagDirectCastCannotBypassExpiredTicket(t *testing.T) {
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid int64 = 1000091
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPlayerLocation(ctx, uid, luckyBagMapKey, "", 0); err != nil {
		t.Fatal(err)
	}
	srv.setLevel(uid, luckyBagMapKey)
	now := time.Now()
	if _, err := db.SQL.Exec(`
		INSERT INTO lucky_bag_session(
			uid,origin_level_key,origin_scene,origin_spot,expires_at,
			paid_currency,paid_amount,purchases,purchased_ts,notified_ts)
		VALUES(?,?,?,?,?,?,?,?,?,0)`,
		uid, spawnLevelKey, sceneForLevel(spawnLevelKey), 0,
		float64(now.Add(-time.Minute).Unix()), "silver", luckyBagSilverPrice, 1,
		float64(now.Add(-31*time.Minute).Unix())); err != nil {
		t.Fatal(err)
	}

	sess := newProbeSess(srv)
	sess.Userid = uid
	capture := &ctxReplyCapture{}
	ctxCast := &Ctx{
		Sess: sess, Main: 14, Sub: 2,
		Tree: rigRequestTree(t, proto.Guid("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		DecodeOK: true, ExpectResponse: true, Log: logging.Discard(), replyCapture: capture,
	}
	body, handled := hCast(ctxCast)
	_, present, _, failed := capture.snapshot()
	if !handled || body != nil || !present || !failed {
		t.Fatalf("过期福袋券应阻止直接 14/2: handled=%v body=%d present=%v failed=%v",
			handled, len(body), present, failed)
	}
}


// TestLuckyBagPriceMatchesClientQuotes covers both display units and wallet units.
func TestLuckyBagPriceMatchesClientQuotes(t *testing.T) {
	tests := []struct {
		name  string
		gold  bool
		price int64
		want  bool
	}{
		{name: "silver wallet cents", price: luckyBagSilverPrice, want: true},
		{name: "silver client display", price: 1_000_000, want: true},
		{name: "wrong silver price", price: 1_000_001, want: false},
		{name: "gold wallet cents", gold: true, price: luckyBagGoldPrice, want: true},
		{name: "gold client display", gold: true, price: 8, want: true},
		{name: "wrong gold price", gold: true, price: 9, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := luckyBagPriceMatches(tt.gold, tt.price); got != tt.want {
				t.Fatalf("luckyBagPriceMatches(%v, %d)=%v, want %v",
					tt.gold, tt.price, got, tt.want)
			}
		})
	}
}
