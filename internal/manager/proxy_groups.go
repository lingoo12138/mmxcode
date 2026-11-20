package manager

import (
	"context"
	"strings"
	"time"
)

// ProxyGroupsResult says what the move of the outlet groups did.
type ProxyGroupsResult struct {
	// Moved is how many accounts took the address of their group as an
	// address of their own.
	Moved int
	// Kept is how many accounts already had an address of their own:
	// the group never decided anything for them, and their address
	// stays.
	Kept int
	// Dropped is how many accounts lost their group and got no address:
	// the group was not in the configuration.
	Dropped int
	// DroppedIDs are the ids of those accounts.
	DroppedIDs []string
}

// MaterializeProxyGroups turns the outlet group on an account into an
// address of its own.
//
// 出口组退役了：组名还在那些老账号的行上，而组名指向的地址在配置文件
// 里。开机的时候收拾一次 ——
//
//   - 账号自己已经有地址：地址本来就压过组，组名清掉，地址不动。
//   - 账号自己没有地址，配置里有那个组：组名清掉，账号自己的地址写成
//     那个组的地址。
//   - 账号自己没有地址，配置里也没有那个组（配置改过了，组名对不上）：
//     组名清掉，账号回到全局默认。这种账号的出口变了，要说出来。
//
// 它是开机时做一次的事。跑完一次，账号行上就没有组名了，以后开机不用
// 再管。
func (m *Manager) MaterializeProxyGroups(ctx context.Context, groups map[string]string) (ProxyGroupsResult, error) {
	var res ProxyGroupsResult
	list, err := m.Store.LegacyProxyGroups(ctx)
	if err != nil {
		return res, err
	}
	now := time.Now()
	for _, lp := range list {
		url := strings.TrimSpace(lp.URL)
		switch {
		case url != "":
			// 地址本来就压过组：组名清掉，地址不动。
			res.Kept++
		case strings.TrimSpace(groups[lp.Group]) != "":
			url = strings.TrimSpace(groups[lp.Group])
			res.Moved++
		default:
			// 组名在配置里对不上：账号回到全局默认。
			res.Dropped++
			res.DroppedIDs = append(res.DroppedIDs, lp.ID)
		}
		if err := m.Store.SetAccountProxy(ctx, lp.ID, url, now); err != nil {
			return res, err
		}
	}
	return res, nil
}
