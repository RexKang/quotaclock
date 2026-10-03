package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/crypto"
)

// validPut 返回一份合法 PUT 基准样本（各规则用例在其上做单项变异）。
func validPut() *Put {
	return &Put{
		Version:   CurrentVersion,
		Listen:    Listen{Host: "127.0.0.1", Port: 8787},
		Collector: DefaultCollector(),
		Auth:      PutAuth{Mode: AuthModeAdmin},
		Providers: []PutProvider{{
			Platform:   "deepseek",
			AccessKeys: []PutAccessKey{{ID: "k1", Name: "A1", Token: "sk-abcdefgh"}},
		}},
	}
}

func fieldOf(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

// TestValidateRules 逐规则合法/非法样本（v0.2.5：provider 侧改为 platform 白名单 + access_keys）。
func TestValidateRules(t *testing.T) {
	mkPut := func(platform string, keys ...PutAccessKey) PutProvider {
		return PutProvider{Platform: platform, AccessKeys: keys}
	}
	cases := []struct {
		name   string
		mutate func(*Put)
		field  string
		legal  bool
	}{
		// r5：旧版本号（1~CurrentVersion）一律接受，按「旧版配置导入」处理（见 server.handlePutConfig）
		{"R1 version 旧版接受", func(p *Put) { p.Version = 3 }, "version", true},
		{"R1 version 0 拒绝", func(p *Put) { p.Version = 0 }, "version", false},
		{"R1 version 未来版本拒绝", func(p *Put) { p.Version = CurrentVersion + 1 }, "version", false},
		{"R1 version ok", func(p *Put) { p.Version = CurrentVersion }, "version", true},
		{"R2 port low", func(p *Put) { p.Listen.Port = 0 }, "listen.port", false},
		{"R2 port high", func(p *Put) { p.Listen.Port = 65536 }, "listen.port", false},
		{"R2 port ok", func(p *Put) { p.Listen.Port = 65535 }, "listen.port", true},
		{"R3 host bad", func(p *Put) { p.Listen.Host = "localhost" }, "listen.host", false},
		{"R3 host ok", func(p *Put) { p.Listen.Host = "0.0.0.0" }, "listen.host", true},
		{"R4 interval low", func(p *Put) { p.Collector.IntervalBaseS = 29 }, "collector.interval_base_s", false},
		{"R4 interval ok", func(p *Put) { p.Collector.IntervalBaseS = 30 }, "collector.interval_base_s", true},
		{"R5 jitter inv", func(p *Put) { p.Collector.JitterMinS = 10; p.Collector.JitterMaxS = 5 }, "collector.jitter_min_s", false},
		{"R5 jitter ok", func(p *Put) { p.Collector.JitterMinS = 5; p.Collector.JitterMaxS = 25 }, "collector.jitter_min_s", true},
		{"R6 stagger inv", func(p *Put) { p.Collector.StaggerMinS = 6; p.Collector.StaggerMaxS = 5 }, "collector.stagger_min_s", false},
		{"R6 stagger ok", func(p *Put) { p.Collector.StaggerMinS = 1; p.Collector.StaggerMaxS = 5 }, "collector.stagger_min_s", true},
		{"R7 mult low", func(p *Put) { p.Collector.BackoffMultiplier = 0 }, "collector.backoff_multiplier", false},
		{"R7 mult high", func(p *Put) { p.Collector.BackoffMultiplier = 9 }, "collector.backoff_multiplier", false},
		{"R7 mult ok", func(p *Put) { p.Collector.BackoffMultiplier = 8 }, "collector.backoff_multiplier", true},
		{"R8 backoff max high", func(p *Put) { p.Collector.BackoffMaxS = 86401 }, "collector.backoff_max_s", false},
		{"R8 backoff max ok", func(p *Put) { p.Collector.BackoffMaxS = 86400 }, "collector.backoff_max_s", true},
		{"R9 mode bad", func(p *Put) { p.Auth.Mode = "root" }, "auth.mode", false},
		{"R9 mode none", func(p *Put) { p.Auth.Mode = AuthModeNone }, "auth.mode", true},

		// R10 平台白名单（不允许自建平台）
		{"R10 platform 空", func(p *Put) { p.Providers[0].Platform = "" }, "providers[0].platform", false},
		{"R10 platform 未知", func(p *Put) { p.Providers[0].Platform = "my-custom-platform" }, "providers[0].platform", false},
		{"R10 platform 重复", func(p *Put) {
			p.Providers = append(p.Providers, mkPut("deepseek", PutAccessKey{ID: "k2"}))
		}, "providers[1].platform", false},
		{"R10 platform 四种预设均合法", func(p *Put) {
			p.Providers = []PutProvider{
				mkPut("zhipu-glm", PutAccessKey{ID: "z1"}),
				mkPut("deepseek", PutAccessKey{ID: "d1"}),
				mkPut("kimi-code", PutAccessKey{ID: "m1"}),
				mkPut("opencode", PutAccessKey{ID: "o1"}),
			}
		}, "", true},

		// R11 至少一个凭据
		{"R11 access_keys 空", func(p *Put) { p.Providers[0].AccessKeys = nil }, "providers[0].access_keys", false},

		// R12 凭据 id
		{"R12 key id 空", func(p *Put) { p.Providers[0].AccessKeys[0].ID = "" }, "providers[0].access_keys[0].id", false},
		{"R12 key id 重复", func(p *Put) {
			p.Providers[0].AccessKeys = append(p.Providers[0].AccessKeys, PutAccessKey{ID: "k1"})
		}, "providers[0].access_keys[1].id", false},
		{"R12 多凭据合法", func(p *Put) {
			p.Providers[0].AccessKeys = []PutAccessKey{{ID: "k1"}, {ID: "k2"}, {ID: "k3"}}
		}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPut()
			tc.mutate(p)
			errs := Validate(p)
			if tc.legal {
				if len(errs) != 0 {
					t.Fatalf("合法样本被判非法: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatal("非法样本通过校验")
			}
			if tc.field != "" && !fieldOf(errs, tc.field) {
				t.Fatalf("details 未含 %s: %v", tc.field, errs)
			}
		})
	}
	// 合法基准
	if errs := Validate(validPut()); len(errs) != 0 {
		t.Fatalf("基准样本非法: %v", errs)
	}
	// 凭据名可空（展示回落平台名）
	p := validPut()
	p.Providers[0].AccessKeys[0].Name = ""
	if errs := Validate(p); len(errs) != 0 {
		t.Fatalf("空凭据名应合法: %v", errs)
	}
}

func TestParseUnknownField(t *testing.T) {
	body := `{"version":5,"listenn":{"host":"127.0.0.1","port":8787},"collector":{},"auth":{"mode":"admin"},"providers":[]}`
	if _, err := ParsePut([]byte(body)); err == nil {
		t.Fatal("未知字段未拒绝")
	}
}

func TestParseSensitiveAndComputed(t *testing.T) {
	// 敏感字段 → 400（token_cipher）
	sensitive := `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"admin"},"providers":[{"platform":"deepseek","access_keys":[{"id":"k1","token_cipher":"AAAA"}]}]}`
	if _, err := ParsePut([]byte(sensitive)); err == nil {
		t.Fatal("token_cipher 直写未拒绝")
	} else if !strings.Contains(err.Error(), "token_cipher") {
		t.Fatalf("错误应指出 token_cipher: %v", err)
	}
	hashOnly := `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"admin","password_hash":"$2a$10$xyz"},"providers":[]}`
	if _, err := ParsePut([]byte(hashOnly)); err == nil {
		t.Fatal("password_hash 直写未拒绝")
	}
	// 计算字段 + 只读派生字段（GET 响应整体回传）→ 剥离并通过
	computed := `{
		"version":5,"listen":{"host":"127.0.0.1","port":8787},
		"collector":{"interval_base_s":300,"jitter_min_s":5,"jitter_max_s":25,"stagger_min_s":1,"stagger_max_s":5,"backoff_multiplier":2,"backoff_max_s":1800},
		"auth":{"mode":"admin","password_is_default":true,"authenticated":false},
		"providers":[{"platform":"kimi-code","platform_name":"Kimi Code","base_url":"https://api.kimi.com/coding/v1","paths":["/usages"],"auth_style":"bearer","extra_headers":{},"id":"kimi-code","name":"Kimi Code",
			"access_keys":[{"id":"k1","name":"A1","has_token":true,"token_masked":"ab****ef"}]}]
	}`
	put, err := ParsePut([]byte(computed))
	if err != nil {
		t.Fatalf("计算/只读字段回传被拒: %v", err)
	}
	if errs := Validate(put); len(errs) != 0 {
		t.Fatalf("剥离后应通过: %v", errs)
	}
	if put.Providers[0].Platform != "kimi-code" || len(put.Providers[0].AccessKeys) != 1 {
		t.Fatalf("平台与凭据应被保留: %+v", put.Providers[0])
	}
}

func TestValidateNoShortCircuit(t *testing.T) {
	p := validPut()
	p.Listen.Port = 70000                                                                                       // R2
	p.Providers[0].AccessKeys = nil                                                                             // R11
	p.Providers = append(p.Providers, PutProvider{Platform: "deepseek", AccessKeys: []PutAccessKey{{ID: "k"}}}) // R10 平台重复
	errs := Validate(p)
	if len(errs) < 3 {
		t.Fatalf("应同时返回全部错误项，got %d: %v", len(errs), errs)
	}
	if !fieldOf(errs, "listen.port") || !fieldOf(errs, "providers[0].access_keys") || !fieldOf(errs, "providers[1].platform") {
		t.Fatalf("details 不全: %v", errs)
	}
}

// viewFixture 构造带两个平台的 File（一个双凭据、一个单凭据）。
func viewFixture() (*File, map[string]string) {
	cipher, err := sealForTest("abcd1234")
	if err != nil {
		panic(err)
	}
	f := &File{
		Version:   CurrentVersion,
		Listen:    DefaultListen(),
		Collector: DefaultCollector(),
		Auth:      FileAuth{Mode: AuthModeAdmin, PasswordHash: "$2a$10$abcdefghijklmnopqrstuv"},
		Providers: []FileProvider{
			{Platform: "opencode", AccessKeys: []AccessKey{
				{ID: "k1", Name: "A1", TokenCipher: cipher},
				{ID: "k2", Name: "A2"},
			}},
			{Platform: "deepseek", AccessKeys: []AccessKey{{ID: "d1", TokenCipher: cipher}}},
		},
	}
	masks := map[string]string{RuntimeID("opencode", "k1"): "ab****34"}
	return f, masks
}

func TestViewLoggedIn(t *testing.T) {
	f, masks := viewFixture()
	v := BuildView(f, masks, true, true)
	if !v.Auth.PasswordIsDefault || !v.Auth.Authenticated {
		t.Fatalf("auth 元数据错误: %+v", v.Auth)
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if strings.Contains(s, "token_cipher") || strings.Contains(s, `"token"`) {
		t.Fatal("视图含 token 字段")
	}
	if len(v.Providers) != 2 {
		t.Fatalf("平台数 = %d", len(v.Providers))
	}
	if v.Providers[0].PlatformName != "OpenCode" || v.Providers[0].BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("应带出平台预设展示信息: %+v", v.Providers[0])
	}
	k1 := v.Providers[0].AccessKeys[0]
	if !k1.HasToken || k1.TokenMasked != "ab****34" || k1.Name != "A1" {
		t.Fatalf("登录态凭据应含掩码: %+v", k1)
	}
	if v.Providers[0].AccessKeys[1].HasToken {
		t.Fatal("k2 不应有 token")
	}
	if !strings.Contains(s, `"enabled":true`) {
		t.Fatalf("视图应恒含 enabled: %s", s)
	}
}

func TestViewNotLoggedIn(t *testing.T) {
	f, masks := viewFixture()
	v := BuildView(f, masks, false, true)
	b, _ := json.Marshal(v)
	s := string(b)
	if strings.Contains(s, "token_masked") {
		t.Fatal("未登录不应含 token_masked")
	}
	if !strings.Contains(s, `"has_token":false`) {
		t.Fatal("has_token=false 字段应在（无 omitempty）")
	}
	if !v.Auth.PasswordIsDefault {
		t.Fatal("password_is_default 应始终如实返回")
	}
	if v.Auth.Authenticated {
		t.Fatal("未登录 authenticated 应为 false")
	}
}

func TestViewRuneMask(t *testing.T) {
	cipher, _ := sealForTest("中文token🚀")
	f := &File{Version: CurrentVersion, Listen: DefaultListen(), Collector: DefaultCollector(),
		Auth:      FileAuth{Mode: AuthModeAdmin},
		Providers: []FileProvider{{Platform: "zhipu-glm", AccessKeys: []AccessKey{{ID: "k1", TokenCipher: cipher}}}}}
	v := BuildView(f, map[string]string{RuntimeID("zhipu-glm", "k1"): crypto.Mask("中文token🚀")}, true, false)
	// 中文token🚀 = 8 runes：前 2（中文）+ 后 2（n🚀）
	if got := v.Providers[0].AccessKeys[0].TokenMasked; got != "中文****n🚀" {
		t.Fatalf("rune 掩码错误: %q", got)
	}
}

// TestViewEmptyProviders：空配置 providers 序列化为 [] 而非 null（前端按数组直用），
// access_keys 同理（否则前端在 push 处崩溃）。
func TestViewEmptyProviders(t *testing.T) {
	f := &File{Version: CurrentVersion, Listen: DefaultListen(), Collector: DefaultCollector(),
		Auth: FileAuth{Mode: AuthModeAdmin}, Providers: []FileProvider{}}
	v := BuildView(f, nil, false, true)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"providers":[]`) {
		t.Fatalf("空配置 providers 应序列化为 [] 而非 null: %s", b)
	}

	f2 := &File{Version: CurrentVersion, Listen: DefaultListen(), Collector: DefaultCollector(),
		Auth:      FileAuth{Mode: AuthModeAdmin},
		Providers: []FileProvider{{Platform: "deepseek", AccessKeys: []AccessKey{}}}}
	b2, _ := json.Marshal(BuildView(f2, nil, false, true))
	if !strings.Contains(string(b2), `"access_keys":[]`) {
		t.Fatalf("空凭据列表应序列化为 []: %s", b2)
	}
}

func TestValidateFileParity(t *testing.T) {
	f, _ := viewFixture()
	if errs := ValidateFile(f); len(errs) != 0 {
		t.Fatalf("合法 File 校验失败: %v", errs)
	}
	f.Providers[0].Platform = "unknown-platform"
	if errs := ValidateFile(f); !fieldOf(errs, "providers[0].platform") {
		t.Fatalf("File 校验应含平台白名单: %v", errs)
	}
}
