package gameserver

import "testing"

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
