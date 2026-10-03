package config

import (
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/crypto"
)

// FX-2：v0.1 导出 JSON（version 2）脱敏版——结构与真实 old-config.json 一致，token 为占位。
// 第 5 条（原 disabledprov）改为「同平台的第二个凭据且停用」，用于覆盖
// 「enabled=false 导入并保持停用」+「同平台多凭据合并」两条语义。
const fx2OldConfig = `{
  "version": 2,
  "active": "deepseek",
  "providers": [
    {
      "enabled": true,
      "id": "zhipu",
      "name": "智谱 GLM",
      "baseURL": "https://open.bigmodel.cn",
      "token": "eyJhbG...test",
      "endpoints": [
        { "method": "GET", "path": "/api/monitor/usage/quota/limit", "params": "{", "note": "周期额度" }
      ]
    },
    {
      "enabled": true,
      "id": "deepseek",
      "name": "DeepSeek",
      "baseURL": "https://api.deepseek.com",
      "token": "«redacted:sk-…»",
      "endpoints": [
        { "method": "GET", "path": "/user/balance", "params": "", "note": "余额" }
      ]
    },
    {
      "enabled": true,
      "id": "moonshot",
      "name": "Kimi Code",
      "baseURL": "http://127.0.0.1:8787/kimi",
      "token": "«redacted:sk-…»",
      "endpoints": [
        { "method": "GET", "path": "/usages", "params": "", "note": "周期额度" },
        { "method": "GET", "path": "/me", "params": "", "note": "账户信息" },
        { "method": "GET", "path": "/models", "params": "", "note": "模型列表" }
      ]
    },
    {
      "enabled": true,
      "id": "opencode",
      "name": "OpenCode Go",
      "baseURL": "http://127.0.0.1:8787/opencode",
      "token": "auth=Fe26.2**placeholder; oc_locale=zh",
      "endpoints": [
        { "method": "GET", "path": "/_server?id=deadbeef", "params": "", "note": "Go 套餐用量" }
      ]
    },
    {
      "enabled": false,
      "id": "deepseek-key2",
      "name": "DeepSeek 备用",
      "baseURL": "https://api.deepseek.com",
      "token": "«redacted:sk-2…»",
      "endpoints": [ { "method": "GET", "path": "/user/balance" } ]
    }
  ]
}`

func sealReal(plain string) (string, error) {
	key := bytes32()
	return crypto.SealToken(key, plain)
}

func bytes32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func providerByPlatform(t *testing.T, f *File, platform string) FileProvider {
	t.Helper()
	for _, p := range f.Providers {
		if p.Platform == platform {
			return p
		}
	}
	t.Fatalf("缺少平台 %s（实际 %+v）", platform, f.Providers)
	return FileProvider{}
}

func keyByID(t *testing.T, fp FileProvider, id string) AccessKey {
	t.Helper()
	for _, k := range fp.AccessKeys {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("平台 %s 缺少凭据 %s（实际 %+v）", fp.Platform, id, fp.AccessKeys)
	return AccessKey{}
}

// TestMigrateV1PlanA：v0.1 → v0.2.5（自动改写 + 平台归并 + 凭据化）。
func TestMigrateV1PlanA(t *testing.T) { // C-config-21
	out, logs, err := MigrateV1([]byte(fx2OldConfig), sealReal)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(logs, "\n")
	if out.Version != CurrentVersion {
		t.Fatalf("version = %d", out.Version)
	}
	if out.Listen != DefaultListen() || out.Collector != DefaultCollector() {
		t.Fatal("listen/collector 应为默认值")
	}
	if out.Auth.Mode != AuthModeAdmin || !IsDefaultPassword(out.Auth.PasswordHash) {
		t.Fatal("auth 应为 admin + 默认密码态")
	}
	if len(out.Providers) != 4 {
		t.Fatalf("应归并为 4 个平台（deepseek 两条合一），got %d: %+v", len(out.Providers), out.Providers)
	}

	// 智谱：直连地址 → 平台 zhipu-glm；token 加密落盘且可解
	zp := providerByPlatform(t, out, "zhipu-glm")
	zk := keyByID(t, zp, "zhipu")
	if !zk.IsEnabled() {
		t.Fatal("zhipu 凭据应启用")
	}
	if zk.Name != "" { // 旧名 "智谱 GLM" 与平台名相同 → 空（展示回落平台名）
		t.Fatalf("zhipu 凭据名应为空: %q", zk.Name)
	}
	if zk.TokenCipher == "" {
		t.Fatal("zhipu token 应加密落盘")
	}
	plain, err := crypto.OpenToken(bytes32(), zk.TokenCipher)
	if err != nil || plain != "eyJhbG...test" {
		t.Fatalf("zhipu token 密文不可解或值不符: %q %v", plain, err)
	}

	// DeepSeek：两条合一 → 第一个启用、第二个停用（enabled=false 导入并保持停用）
	ds := providerByPlatform(t, out, "deepseek")
	if len(ds.AccessKeys) != 2 {
		t.Fatalf("deepseek 应有 2 个凭据，got %d", len(ds.AccessKeys))
	}
	if !keyByID(t, ds, "deepseek").IsEnabled() {
		t.Fatal("deepseek 第一个凭据应启用")
	}
	if keyByID(t, ds, "deepseek-key2").IsEnabled() {
		t.Fatal("enabled=false 凭据应导入并保持停用")
	}
	if keyByID(t, ds, "deepseek-key2").TokenCipher == "" {
		t.Fatal("停用凭据 token 仍应加密落盘（不丢配置）")
	}
	if !strings.Contains(joined, "保持停用") || !strings.Contains(joined, "归并") {
		t.Fatalf("应记录停用导入与平台归并日志: %s", joined)
	}

	// Kimi：代理改写 → api.kimi.com/coding/v1；paths 收敛为预设 /usages
	kc := providerByPlatform(t, out, "kimi-code")
	if keyByID(t, kc, "moonshot").TokenCipher == "" {
		t.Fatal("kimi token 应迁移")
	}
	if !strings.Contains(joined, "https://api.kimi.com/coding/v1") {
		t.Fatalf("应 INFO 记录改写对照: %s", joined)
	}
	if !strings.Contains(joined, "/me") || !strings.Contains(joined, "收敛") {
		t.Fatalf("旧 3 条 path 应收敛并记录: %s", joined)
	}

	// OpenCode：改写为官方用量接口；旧 Cookie 无法转 API Key → token 不迁移 + WARN
	oc := providerByPlatform(t, out, "opencode")
	ok := keyByID(t, oc, "opencode")
	if ok.TokenCipher != "" {
		t.Fatal("opencode 旧 Cookie token 不应迁移（无法转 API Key）")
	}
	if !strings.Contains(joined, "无法转换") {
		t.Fatalf("应 WARN token 未迁移提示: %s", joined)
	}
	if !strings.Contains(joined, "/zen/go/v1") {
		t.Fatalf("应 INFO 记录改写对照: %s", joined)
	}

	// 运行时展开：预设地址生效（配置里已不含 base_url）
	rt := BuildRuntime(out, nil)
	if p := rt.Provider(RuntimeID("opencode", "opencode")); p == nil || p.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("运行时应由预设派生地址: %+v", p)
	}
}

// TestMigrateV1UnknownPlatformSkipped：地址不在平台清单 → 跳过 + WARN（不允许自建平台）。
func TestMigrateV1UnknownPlatformSkipped(t *testing.T) {
	src := `{"version":2,"providers":[
	  {"enabled":true,"id":"mine","name":"我的代理","baseURL":"https://my-own-proxy.example.com","token":"sk-x",
	   "endpoints":[{"method":"GET","path":"/usage"}]},
	  {"enabled":true,"id":"deepseek","name":"DeepSeek","baseURL":"https://api.deepseek.com","token":"sk-y",
	   "endpoints":[{"method":"GET","path":"/user/balance"}]}]}`
	out, logs, err := MigrateV1([]byte(src), sealReal)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || out.Providers[0].Platform != "deepseek" {
		t.Fatalf("应只保留 deepseek: %+v", out.Providers)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "my-own-proxy.example.com") || !strings.Contains(joined, "已知平台清单") {
		t.Fatalf("应 WARN 未知平台并给出清单指引: %s", joined)
	}
}

func TestMigrateV1Fallback(t *testing.T) { // C-config-22 兜底：非 8787 自定义代理 → 跳过 + WARN，其余照常
	fallback := `{
      "version": 2,
      "providers": [
        {
          "enabled": true, "id": "custom", "name": "自定义代理",
          "baseURL": "http://127.0.0.1:9000/mykimi", "token": "sk-x",
          "endpoints": [ { "method": "GET", "path": "/usages" } ]
        },
        {
          "enabled": true, "id": "deepseek", "name": "DeepSeek",
          "baseURL": "https://api.deepseek.com", "token": "sk-y",
          "endpoints": [ { "method": "GET", "path": "/user/balance" } ]
        }
      ]
    }`
	out, logs, err := MigrateV1([]byte(fallback), sealReal)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || out.Providers[0].Platform != "deepseek" {
		t.Fatalf("兜底应只保留 deepseek: %+v", out.Providers)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "127.0.0.1:9000") || !strings.Contains(joined, "README") {
		t.Fatalf("兜底 WARN 应含原地址与 README 指引: %s", joined)
	}
}

func TestMigrateV1NonGETWarn(t *testing.T) {
	src := `{"version":2,"providers":[{"id":"a","name":"A","baseURL":"https://api.deepseek.com","token":"t",
	  "endpoints":[{"method":"POST","path":"/go"},{"method":"GET","path":"/user/balance"}]}]}`
	out, logs, err := MigrateV1([]byte(src), sealReal)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || out.Providers[0].Platform != "deepseek" {
		t.Fatalf("应保留 deepseek: %+v", out.Providers)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "POST") {
		t.Fatal("非 GET 丢弃应 WARN")
	}
}

func TestMigrateV1BadJSON(t *testing.T) {
	if _, _, err := MigrateV1([]byte("{broken"), sealReal); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// ---------- v0.2.x（version 3）→ v0.2.5 ----------

// v3Fixture：一 key 一 provider 的旧形态（与用户线上 bin/config.json 同构）。
const v3Fixture = `{
  "version": 3,
  "listen": {"host": "127.0.0.1", "port": 8787},
  "collector": {"interval_base_s": 300, "jitter_min_s": 5, "jitter_max_s": 25, "stagger_min_s": 1, "stagger_max_s": 5, "backoff_multiplier": 2, "backoff_max_s": 1800},
  "auth": {"mode": "admin", "password_hash": "$2a$10$abcdefghijklmnopqrstuv"},
  "providers": [
    {"id": "zhipu", "name": "智谱 GLM", "base_url": "https://open.bigmodel.cn", "paths": ["/api/monitor/usage/quota/limit"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": "C1"},
    {"id": "deepseek", "name": "DeepSeek", "base_url": "https://api.deepseek.com", "paths": ["/user/balance"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": "C2"},
    {"id": "moonshot", "name": "Kimi Code", "base_url": "https://api.kimi.com/coding/v1", "paths": ["/usages", "/me", "/models"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": "C3"},
    {"id": "opencode-m1", "name": "OpenCode M1", "base_url": "https://opencode.ai/zen/go/v1", "paths": ["/usage"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": "C4"},
    {"id": "opencode-m2", "name": "OpenCode M2", "base_url": "https://opencode.ai/zen/go/v1", "paths": ["/usage"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": "C5", "enabled": false},
    {"id": "custom", "name": "自建", "base_url": "https://my-proxy.example.com", "paths": ["/x"], "token_cipher": "C6"}
  ]
}`

// TestMigrateV3：v3 → v4 保留 listen/collector/auth、平台归并、凭据 id/名派生、tokens 原样（已是密文）。
func TestMigrateV3(t *testing.T) {
	out, logs, err := MigrateV3([]byte(v3Fixture))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(logs, "\n")
	if out.Version != CurrentVersion {
		t.Fatalf("version = %d", out.Version)
	}
	if out.Listen.Host != "127.0.0.1" || out.Listen.Port != 8787 {
		t.Fatalf("listen 应原样保留: %+v", out.Listen)
	}
	if out.Collector.IntervalBaseS != 300 || out.Collector.BackoffMaxS != 1800 {
		t.Fatalf("collector 应原样保留: %+v", out.Collector)
	}
	if out.Auth.Mode != AuthModeAdmin || out.Auth.PasswordHash != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatalf("密码 hash 应原样保留（不能重置）: %+v", out.Auth)
	}
	// 未知平台条目被跳过（不再允许自建）
	if len(out.Providers) != 4 {
		t.Fatalf("应归并为 4 个平台，got %d: %+v", len(out.Providers), out.Providers)
	}
	if !strings.Contains(joined, "my-proxy.example.com") {
		t.Fatalf("未知平台应 WARN: %s", joined)
	}

	oc := providerByPlatform(t, out, "opencode")
	if len(oc.AccessKeys) != 2 {
		t.Fatalf("opencode 应合并 2 个凭据: %+v", oc.AccessKeys)
	}
	if keyByID(t, oc, "opencode-m1").TokenCipher != "C4" {
		t.Fatal("凭据密文应原样保留（已是密文，不再二次加密）")
	}
	if keyByID(t, oc, "opencode-m2").IsEnabled() {
		t.Fatal("enabled=false 应保持停用")
	}
	if n := keyByID(t, oc, "opencode-m1").Name; n != "M1" {
		t.Fatalf("凭据名应剥掉平台名前缀: %q", n)
	}
	kc := providerByPlatform(t, out, "kimi-code")
	if n := keyByID(t, kc, "moonshot").Name; n != "" {
		t.Fatalf("旧名与平台名相同 → 凭据名应为空，got %q", n)
	}
	if !strings.Contains(joined, "收敛") {
		t.Fatalf("kimi 旧 3 条 path 应收敛并记录: %s", joined)
	}
}

func TestMigrateV3BadJSON(t *testing.T) {
	if _, _, err := MigrateV3([]byte("{broken")); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// TestInferPlatform：按地址推断平台（迁移用）——精确前缀与子串都要认。
func TestInferPlatform(t *testing.T) {
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{"https://open.bigmodel.cn", "zhipu-glm", true},
		{"https://open.bigmodel.cn/api", "zhipu-glm", true},
		{"https://api.deepseek.com", "deepseek", true},
		{"https://api.deepseek.com/v1", "deepseek", true},
		{"https://api.kimi.com/coding/v1", "kimi-code", true},
		{"https://api.moonshot.cn/v1", "kimi-code", true},
		{"https://opencode.ai/zen/go/v1", "opencode", true},
		{"https://opencode.ai", "opencode", true},
		{"https://my-proxy.example.com", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := InferPlatform(tc.base)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("InferPlatform(%q) = (%q,%v)，期望 (%q,%v)", tc.base, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDeriveKeyName：凭据名派生规则。
func TestDeriveKeyName(t *testing.T) {
	cases := []struct{ old, platform, want string }{
		{"智谱 GLM", "智谱 GLM", ""},
		{"OpenCode M1", "OpenCode", "M1"},
		{"opencode M2", "OpenCode", "M2"},
		{"Kimi Code", "Kimi Code", ""},
		{"OpenCode", "OpenCode", ""},
		{"我的备用 Key", "OpenCode", "我的备用 Key"},
		{"", "OpenCode", ""},
	}
	for _, tc := range cases {
		if got := deriveKeyName(tc.old, tc.platform); got != tc.want {
			t.Fatalf("deriveKeyName(%q,%q) = %q，期望 %q", tc.old, tc.platform, got, tc.want)
		}
	}
}

// TestMigrateV4AddsBalance（v0.3.0）：v4 → v5 只补 balance 段，其余字段一字不动。
func TestMigrateV4AddsBalance(t *testing.T) {
	raw := []byte(`{"version":4,"listen":{"host":"127.0.0.1","port":8899},` + defaultCollectorJSON + `,
	  "auth":{"mode":"admin","password_hash":"$2a$10$keep"},"providers":[
	    {"platform":"deepseek","access_keys":[{"id":"k1","name":"主","token_cipher":"CIPHER","enabled":false}]}]}`)
	f, logs, err := MigrateV4(raw)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if f.Version != CurrentVersion {
		t.Fatalf("版本应升到 %d: %d", CurrentVersion, f.Version)
	}
	if f.Balance != DefaultBalance() {
		t.Fatalf("应补默认余额设置: %+v", f.Balance)
	}
	if f.Listen.Port != 8899 || f.Auth.PasswordHash != "$2a$10$keep" {
		t.Fatalf("其余字段被改动: %+v %+v", f.Listen, f.Auth)
	}
	if len(f.Providers) != 1 || f.Providers[0].AccessKeys[0].TokenCipher != "CIPHER" {
		t.Fatalf("凭据被改动: %+v", f.Providers)
	}
	if f.Providers[0].AccessKeys[0].IsEnabled() {
		t.Fatal("停用状态应原样保留")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "余额展示设置") {
		t.Fatalf("应记一条余额迁移日志: %v", logs)
	}
	// 已带 balance 的 v4（理论上不该有，但手写文件可能）：不覆盖
	raw2 := []byte(`{"version":4,"listen":{"host":"127.0.0.1","port":1},` + defaultCollectorJSON + `,
	  "balance":{"full":80,"green_pct":40,"warn_pct":10},"auth":{"mode":"admin"},"providers":[]}`)
	f2, logs2, err := MigrateV4(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Balance != (Balance{Full: 80, GreenPct: 40, WarnPct: 10}) {
		t.Fatalf("不应覆盖已有 balance: %+v", f2.Balance)
	}
	if len(logs2) != 0 {
		t.Fatalf("无需日志: %v", logs2)
	}
}
