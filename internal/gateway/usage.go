package gateway

import (
	"encoding/json"
	"log"
	"net/http"

	"mmxcode/internal/card"
)

// usageCard says the gateway's usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子，卡片就是从那里来的：magpie
// 照着 plan、until、balance、windows 这几个字段画。我们是自己的端点，那
// 个钩子得由网关自己顶上来 —— 给 magpie 一个 magpie 认得的形状，它就能
// 把我们这些账号的积分画出来。
//
// 卡片说的是上一次读到的数（调度器每 usage.refresh.scan_every 扫一遍，
// 默认 30 秒；把它写成 0 才回到老的 schedule.usage_refresh），后台页面上
// 也有一个「刷新」按钮。要现读一遍就在地址上加 refresh=1：那一遍只现读
// 最热的几个账号 —— 卡片等不了所有账号的账。
//
// 这个接口在聊天那扇门后面：手里有那把 key 的（聊天用的、或者后台登录
// 用的）都能问。问不到的就是没带 key。
func (s *Server) usageCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Get("refresh") == "1" && s.Mgr != nil {
		// 卡片那条路只现读最热的几个：卡片永远几秒内回来。其余的账
		// 用上一次读到的数。
		res, err := s.round().RefreshHot(ctx)
		if err != nil {
			// 读不到不是没有卡片：上一次读到的数照样摆出来，只是它是
			// 上一次的。理由记在日志里。
			log.Printf("usage card: the refresh failed: %v", err)
		} else {
			for _, reason := range res.Reasons {
				log.Printf("usage card: %s", reason)
			}
		}
	}
	snaps, err := s.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	accounts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	// 标记也摆进去：被记了「额度用完」的账号，卡上要写着用完，它的积分也
	// 不算在余额里 —— 网关不会再把它给客户端，卡上不能还许着积分。
	spent, err := s.Store.ListExhausted(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(card.Of(snaps, accounts, spent))
}
