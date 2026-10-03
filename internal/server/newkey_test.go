package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/crypto"
)

// TestPutNewKeyRequiresToken（v0.2.5）：新增凭据必须带 token；已存在凭据（含迁移产出的
// 空 token 凭据）不受影响——否则用户填好别的平台后反而存不下去。
func TestPutNewKeyRequiresToken(t *testing.T) {
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)

	// ① 新增凭据不带 token → 400，details 指到该字段
	body := putOneKey(env, "opencode", "newk", "新 Key", "")
	code, m, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != 400 {
		t.Fatalf("新增无 token 应 400，实际 %d %v", code, m)
	}
	details, _ := m["details"].([]any)
	if len(details) == 0 {
		t.Fatalf("应给出 details: %v", m)
	}
	d0, _ := details[0].(map[string]any)
	if d0["field"] != "providers[0].access_keys[0].token" {
		t.Fatalf("details 字段定位错误: %v", d0)
	}

	// ② 带 token → 200
	body = putOneKey(env, "opencode", "newk", "新 Key", "sk-new-key-token")
	if code, m, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatalf("带 token 应 200，实际 %d %v", code, m)
	}

	// ③ 已存在凭据（改名/停用）不带 token → 200（= 保留原密文）
	body = putOneKey(env, "opencode", "newk", "改名后", "")
	if code, m, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatalf("已存在凭据留空应保留原值并 200，实际 %d %v", code, m)
	}
	if got := env.srv.cfg.Load().Provider("opencode.newk").Token; got != "sk-new-key-token" {
		t.Fatalf("空 token 应保留原值: %q", got)
	}
}

// TestPutMigratedTokenlessKeySavable（v0.2.5）：迁移产出的「无 token 凭据」不阻断保存——
// 它们已在旧配置里存在，用户只是想改别的字段。
func TestPutMigratedTokenlessKeySavable(t *testing.T) {
	env := newEnv(t, func(key []byte, f *config.File) {
		// 模拟 v0.1 迁移结果：opencode 凭据存在但没有密文（旧 Cookie 无法转 API Key）
		f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode",
			AccessKeys: []config.AccessKey{{ID: "legacy", Name: "旧凭据"}}})
	})
	ck := env.login(config.DefaultPassword)

	body := putOneKey(env, "opencode", "legacy", "旧凭据", "")
	if code, m, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatalf("迁移留下的空 token 凭据应可保存（改别的字段），实际 %d %v", code, m)
	}
}

// TestTestEndpointBeforeSave（v0.2.5）：未保存的新凭据可以直接测连通（先测通再保存）。
func TestTestEndpointBeforeSave(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer up.Close()

	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	setUpstream(t, env, up.URL)

	// ① 未保存的凭据（只给 platform + token）→ 200，且用请求里的 token
	code, m, _ := env.doJSON("POST", "/api/test",
		map[string]any{"platform": "opencode", "token": "sk-unsaved-probe"}, authHdr(ck))
	if code != 200 || m["ok"] != true {
		t.Fatalf("未保存凭据应可测试，实际 %d %v", code, m)
	}
	if gotAuth != "Bearer sk-unsaved-probe" {
		t.Fatalf("未保存凭据应使用请求携带的 token，实际 %q", gotAuth)
	}

	// ② 未保存且未带 token → 400，提示先填 API Key
	code, m, _ = env.doJSON("POST", "/api/test", map[string]any{"platform": "opencode"}, authHdr(ck))
	if code != 400 || !strings.Contains(mustJSON(m), "请先填写 API Key") {
		t.Fatalf("无 token 测试应 400 并给出提示，实际 %d %v", code, m)
	}

	// ③ 未知平台 → 400，提示可用平台清单
	code, m, _ = env.doJSON("POST", "/api/test",
		map[string]any{"platform": "not-a-platform", "token": "x"}, authHdr(ck))
	if code != 400 || !strings.Contains(mustJSON(m), "zhipu-glm") {
		t.Fatalf("未知平台应 400 并列出可用平台，实际 %d %v", code, m)
	}

	// ④ 未保存凭据的 token 不入日志（脱敏红线）
	if strings.Contains(env.logBuf.String(), "sk-unsaved-probe") {
		t.Fatal("测试用 token 出现在日志里")
	}
}

// TestTestEndpointSavedKey：已保存凭据的测试仍按原语义（token 缺省用配置里的）。
func TestTestEndpointSavedKey(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer up.Close()

	env := newEnv(t, func(key []byte, f *config.File) {
		cipher, _ := crypto.SealToken(key, "sk-saved-token")
		f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode",
			AccessKeys: []config.AccessKey{{ID: "k1", Name: "K1", TokenCipher: cipher}}})
	})
	ck := env.login(config.DefaultPassword)
	setUpstream(t, env, up.URL)

	code, m, _ := env.doJSON("POST", "/api/test", map[string]any{"provider_id": "opencode.k1"}, authHdr(ck))
	if code != 200 {
		t.Fatalf("已保存凭据应可测试，实际 %d %v", code, m)
	}
	if gotAuth != "Bearer sk-saved-token" {
		t.Fatalf("应使用配置里的 token，实际 %q", gotAuth)
	}

	// provider_id 前缀也能定位平台（未保存的新 id + 请求 token）
	code, m, _ = env.doJSON("POST", "/api/test",
		map[string]any{"provider_id": "opencode.brandnew", "token": "sk-brand-new"}, authHdr(ck))
	if code != 200 || gotAuth != "Bearer sk-brand-new" {
		t.Fatalf("provider_id 前缀定位平台失败: %d %v auth=%q", code, m, gotAuth)
	}
}

// TestValidateNewKeysUnit：纯函数语义（含「旧配置里存在即放行」的边界）。
func TestValidateNewKeysUnit(t *testing.T) {
	old := &config.File{Providers: []config.FileProvider{{
		Platform:   "opencode",
		AccessKeys: []config.AccessKey{{ID: "kept"}, {ID: "tokenless"}},
	}}}
	put := &config.Put{Providers: []config.PutProvider{{
		Platform: "opencode",
		AccessKeys: []config.PutAccessKey{
			{ID: "kept"},                  // 已存在 → 放行
			{ID: "tokenless"},             // 已存在（迁移留下的空 token）→ 放行
			{ID: "brandnew", Token: "sk"}, // 新增且带 token → 放行
		},
	}}}
	if errs := config.ValidateNewKeys(put, old); len(errs) != 0 {
		t.Fatalf("不应报错: %v", errs)
	}
	put.Providers[0].AccessKeys = append(put.Providers[0].AccessKeys, config.PutAccessKey{ID: "bare"})
	errs := config.ValidateNewKeys(put, old)
	if len(errs) != 1 || !strings.Contains(errs[0].Field, "brandnew") {
		// 定位到新增且无 token 的那一项（下标 3）
		if len(errs) != 1 || errs[0].Field != "providers[0].access_keys[3].token" {
			t.Fatalf("应只报新增无 token 项: %v", errs)
		}
	}
	if errs := config.ValidateNewKeys(put, nil); errs != nil {
		t.Fatalf("无旧配置（首次保存）时不做此校验: %v", errs)
	}
	_ = json.Marshal
}
