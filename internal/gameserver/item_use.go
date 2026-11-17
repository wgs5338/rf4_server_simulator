package gameserver

import "rf4server/internal/proto"

// hItemUse 处理 4/8 物品使用：专用礼盒、2023 福袋、饮品/消耗品优先，
// 其他尚无专用服务端效果的物品沿用客户端通用 800 背包快照回执。
func hItemUse(ctx *Ctx) ([]byte, bool) {
	if ctx == nil || ctx.Sess == nil || ctx.Sess.Srv == nil || !ctx.ExpectResponse {
		return nil, false
	}
	srv := ctx.Sess.Srv
	if body, handled := srv.useGiftBox(ctx); handled {
		return body, true
	}
	if body, handled := srv.useLuckyBag(ctx); handled {
		return body, true
	}
	if body, handled := srv.useAlcoholDrink(ctx); handled {
		return body, true
	}
	body, err := proto.EncodeValue(buildRigSnapshot800(nil, nil, nil), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}
