package server

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/RexKang/QuotaClock/internal/config"
	"github.com/RexKang/QuotaClock/internal/crypto"
	"github.com/RexKang/QuotaClock/internal/persist"
)

// newTrendsEnv 造「两个 OpenCode Key」的环境（趋势数据用 keyID = a / b，断言直观）。
func newTrendsEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnv(t, func(key []byte, f *config.File) {
		ca, _ := crypto.SealToken(key, "token-a-1111")
		cb, _ := crypto.SealToken(key, "token-b-2222")
		f.Providers = []config.FileProvider{{
			Platform: "opencode",
			AccessKeys: []config.AccessKey{
				{ID: "a", Name: "A", TokenCipher: ca},
				{ID: "b", Name: "B", TokenCipher: cb},
			},
		}}
	})
}

// seedTrends 给两个 Key 各写一条当日样本。
func seedTrends(env *testEnv, at time.Time) {
	body := json.RawMessage(`{"usage":{"rolling":{"percent":12}}}`)
	env.trends.Observe("opencode.a", "A", "opencode", at, body)
	env.trends.Observe("opencode.b", "B", "opencode", at, body)
}

// trendKeyIDs 读 GET /api/trends 里出现过的 id。
func trendKeyIDs(t *testing.T, env *testEnv) ([]string, map[string]any) {
	t.Helper()
	code, m, _ := env.doJSON("GET", "/api/trends", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/trends = %d", code)
	}
	raw, ok := m["keys"].([]any)
	if !ok {
		t.Fatalf("keys 必须是非 null 数组（前端 .map 会崩）：%v", m["keys"])
	}
	ids := make([]string, 0, len(raw))
	byID := map[string]any{}
	for _, v := range raw {
		kv := v.(map[string]any)
		id, _ := kv["id"].(string)
		ids = append(ids, id)
		byID[id] = kv
	}
	return ids, byID
}

// TestTrendsEndpoint：GET /api/trends 的契约（公开、形状、days 参数）。
func TestTrendsEndpoint(t *testing.T) {
	env := newTrendsEnv(t)

	// 空态：200 且 keys = []（不是 null）
	ids, _ := trendKeyIDs(t, env)
	if len(ids) != 0 {
		t.Fatalf("尚无数据时应为空数组，实际 %v", ids)
	}

	// 无鉴权也能读（与 /api/quotas 同级；敏感物只有 token，不在这里）
	// 样本时间必须用「运行当天」：此处断言的是「daily 末位 = 今天」的窗口对齐 +
	// 「采到即可见」。写死日期会让断言只在编写当天成立——隔天再跑必红。
	at := time.Now()
	seedTrends(env, at)
	ids, byID := trendKeyIDs(t, env)
	if len(ids) != 2 || ids[0] != "opencode.a" || ids[1] != "opencode.b" {
		t.Fatalf("keys = %v，期望 [opencode.a opencode.b]", ids)
	}
	kv := byID["opencode.a"].(map[string]any)
	if kv["kind"] != "percent" || kv["platform"] != "opencode" {
		t.Errorf("元信息 = %v", kv)
	}
	daily, _ := kv["daily"].([]any)
	if len(daily) != 400 { // 默认回看 400 天
		t.Errorf("daily 长度 = %d，期望默认 400", len(daily))
	}
	if daily[len(daily)-1] == nil {
		t.Error("daily 末位 = 今天，刚采过样不该为空")
	}
	if hours, _ := kv["hours"].([]any); len(hours) != 48 {
		t.Errorf("hours 长度 = %d，期望 48", len(hours))
	}
	if today := at.Format("2006-01-02"); kv["total_days"] != float64(1) || kv["first_day"] != today {
		t.Errorf("累计天数/起始日 = %v / %v（期望 1 / %s）", kv["total_days"], kv["first_day"], today)
	}

	// days 参数：合法值生效，非法值 400（不猜）
	code, m, _ := env.doJSON("GET", "/api/trends?days=7", nil, nil)
	if code != http.StatusOK || m["days"] != float64(7) {
		t.Errorf("days=7 → %d / %v", code, m["days"])
	}
	for _, bad := range []string{"0", "-3", "abc", "1.5"} {
		if code, _, _ := env.doJSON("GET", "/api/trends?days="+bad, nil, nil); code != http.StatusBadRequest {
			t.Errorf("days=%s 应 400，实际 %d", bad, code)
		}
	}
}

// TestDeleteKeyPurgesTrends：删除 AK 时趋势数据一并清理（用户需求 3）。
// 确认框在前端，这里是数据层兜底——直接 PUT / 导入配置也走这条路。
func TestDeleteKeyPurgesTrends(t *testing.T) {
	env := newTrendsEnv(t)
	ck := env.login(config.DefaultPassword)
	at := time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local)
	seedTrends(env, at)
	if err := env.trends.Flush(); err != nil {
		t.Fatal(err)
	}

	body := validPutBody(env)
	provs := body["providers"].([]any)
	provs[0].(map[string]any)["access_keys"] = []any{map[string]any{"id": "a", "name": "A"}}
	code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != http.StatusOK {
		t.Fatalf("PUT 失败: %d", code)
	}

	ids, _ := trendKeyIDs(t, env)
	if len(ids) != 1 || ids[0] != "opencode.a" {
		t.Fatalf("删除后的可见趋势 = %v，期望只剩 opencode.a", ids)
	}
	// 磁盘上也不该再有（内存清了但没落盘 = 重启后又冒出来）
	f, err := persist.LoadTrends(persist.TrendsPathOf(env.dir))
	if err != nil {
		t.Fatalf("读趋势文件失败: %v", err)
	}
	if _, ok := f.Keys["opencode.b"]; ok {
		t.Error("趋势文件里仍留有待删 Key（用户要求删除信息与数据记录）")
	}
	if _, ok := f.Keys["opencode.a"]; !ok {
		t.Error("未删的 Key 数据不该被动")
	}
}

// TestRenameAndDisableKeepTrends：改名 / 停用**不得**清数据（趋势键 = runtime id，与展示名无关）。
func TestRenameAndDisableKeepTrends(t *testing.T) {
	env := newTrendsEnv(t)
	ck := env.login(config.DefaultPassword)
	seedTrends(env, time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local))

	// 改 b 的名字
	body := validPutBody(env)
	keys := body["providers"].([]any)[0].(map[string]any)["access_keys"].([]any)
	for _, k := range keys {
		if k.(map[string]any)["id"] == "b" {
			k.(map[string]any)["name"] = "B 改名后"
		}
	}
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != http.StatusOK {
		t.Fatalf("改名 PUT 失败: %d", code)
	}
	ids, byID := trendKeyIDs(t, env)
	if len(ids) != 2 {
		t.Fatalf("改名不该清趋势：%v", ids)
	}
	if byID["opencode.b"].(map[string]any)["total_days"] != float64(1) {
		t.Error("改名后历史天数丢失")
	}

	// 停用 b（enabled=false）
	body = validPutBody(env)
	keys = body["providers"].([]any)[0].(map[string]any)["access_keys"].([]any)
	for _, k := range keys {
		if k.(map[string]any)["id"] == "b" {
			k.(map[string]any)["enabled"] = false
		}
	}
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != http.StatusOK {
		t.Fatalf("停用 PUT 失败: %d", code)
	}
	if ids, _ = trendKeyIDs(t, env); len(ids) != 2 {
		t.Fatalf("停用不该清趋势：%v", ids)
	}
}

// TestDeletePlatformPurgesAllItsKeys：整个平台被删 → 其下所有 Key 的趋势一并清理。
func TestDeletePlatformPurgesAllItsKeys(t *testing.T) {
	env := newTrendsEnv(t)
	ck := env.login(config.DefaultPassword)
	seedTrends(env, time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local))

	body := validPutBody(env)
	body["providers"] = []any{}
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != http.StatusOK {
		t.Fatalf("PUT 失败: %d", code)
	}
	if ids, _ := trendKeyIDs(t, env); len(ids) != 0 {
		t.Fatalf("平台删除后趋势应清空，实际 %v", ids)
	}
	raw, err := os.ReadFile(persist.TrendsPathOf(env.dir))
	if err != nil {
		t.Fatal(err)
	}
	var f persist.TrendsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Keys) != 0 {
		t.Errorf("趋势文件仍有 %d 个 Key", len(f.Keys))
	}
}
