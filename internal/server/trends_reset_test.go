package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/persist"
)

// POST /api/trends/reset 的契约：admin 才能调；缺省清全部；{"key":...} 只清一个；
// 非法 JSON 直接 400（不做「猜一个」的兜底——写错字段名却把全部清光是最坏的结果）。
func TestResetTrendsEndpoint(t *testing.T) {
	env := newTrendsEnv(t)
	seedTrends(env, time.Now())

	// ① 未登录 → 401（与 PUT/导出同守卫）
	code, _, _ := env.doJSON("POST", "/api/trends/reset", map[string]any{}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("未登录调用 = %d，期望 401", code)
	}
	// 数据必须一条没少（未授权不能有任何副作用）
	if ids, _ := trendKeyIDs(t, env); len(ids) != 2 {
		t.Fatalf("未登录调用居然动了数据：%v", ids)
	}

	ck := env.login(config.DefaultPassword)

	// ② 非法 JSON → 400（宁可报错也不「猜一个范围」：写错字段名却把全部清光是最坏结果）
	resp := env.do("POST", "/api/trends/reset", strings.NewReader("{bad"), map[string]string{
		"Content-Type": "application/json", "Cookie": SessionCookieName + "=" + ck,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON = %d，期望 400", resp.StatusCode)
	}
	if ids, _ := trendKeyIDs(t, env); len(ids) != 2 {
		t.Fatalf("400 之后数据被动过：%v", ids)
	}

	// ③ 只清一个 Key
	code, m, _ := env.doJSON("POST", "/api/trends/reset", map[string]string{"key": "opencode.a"}, authHdr(ck))
	if code != http.StatusOK || m["ok"] != true {
		t.Fatalf("单 Key 清理 = %d %v", code, m)
	}
	if m["keys"].(float64) != 1 {
		t.Errorf("清理规模 keys = %v，期望 1", m["keys"])
	}
	ids, _ := trendKeyIDs(t, env)
	if len(ids) != 1 || ids[0] != "opencode.b" {
		t.Fatalf("单 Key 清理后应只剩 opencode.b，实际 %v", ids)
	}

	// ④ 全部清空（空 body = 清全部）
	code, m, _ = env.doJSON("POST", "/api/trends/reset", map[string]any{}, authHdr(ck))
	if code != http.StatusOK {
		t.Fatalf("全清 = %d", code)
	}
	if m["keys"].(float64) != 1 {
		t.Errorf("全清规模 keys = %v，期望 1", m["keys"])
	}
	if ids, _ := trendKeyIDs(t, env); len(ids) != 0 {
		t.Fatalf("全清之后仍有数据：%v", ids)
	}
	// 文件里也必须空了（不是只清了内存）
	f, err := persist.LoadTrends(persist.TrendsPathOf(env.dir))
	if err != nil {
		t.Fatalf("读趋势文件失败：%v", err)
	}
	if f == nil || len(f.Keys) != 0 {
		t.Errorf("趋势文件里仍有数据：%+v", f)
	}

	// ⑤ 清空后重新采集：第一条样本的用量必须是 0（不许沿用清理前的值）
	seedTrends(env, time.Now().Add(5*time.Minute))
	ids, byID := trendKeyIDs(t, env)
	if len(ids) != 2 {
		t.Fatalf("重新采集后应有 2 个 Key，实际 %v", ids)
	}
	for id, raw := range byID {
		kv := raw.(map[string]any)
		daily := kv["daily"].([]any)
		last := daily[len(daily)-1].(map[string]any)
		pct := last["pct"].(map[string]any)
		if pct["gain"].(float64) != 0 {
			t.Errorf("%s 清理后第一条样本用量 = %v，期望 0", id, pct["gain"])
		}
		if pct["peak"].(float64) != 12 {
			t.Errorf("%s 峰值 = %v，期望 12", id, pct["peak"])
		}
	}

	// ⑥ 有 INFO 留痕（清理是破坏性操作，必须能从日志解释「数据怎么没了」）
	if !strings.Contains(env.logBuf.String(), "清空趋势数据") {
		t.Errorf("日志里没有清空痕迹：\n%s", env.logBuf.String())
	}
}
