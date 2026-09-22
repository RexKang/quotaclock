package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RexKang/QuotaClock/internal/collector"
	"github.com/RexKang/QuotaClock/internal/config"
	"github.com/RexKang/QuotaClock/internal/crypto"
	"github.com/RexKang/QuotaClock/internal/logx"
	"github.com/RexKang/QuotaClock/internal/persist"
	"github.com/RexKang/QuotaClock/internal/trend"
	"github.com/RexKang/QuotaClock/web"
)

// testEnv 进程内全链路测试环境：真实 persist/key/session + httptest。
type testEnv struct {
	t          *testing.T
	dir        string
	configPath string
	srv        *Server
	http       *httptest.Server
	sched      *collector.Scheduler
	store      *collector.Store
	trends     *trend.Store
	logBuf     *syncBuffer
	key        []byte
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (e *testEnv) do(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *testEnv) doJSON(method, path string, body any, hdr map[string]string) (int, map[string]any, *http.Response) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	hdr2 := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		hdr2[k] = v
	}
	resp := e.do(method, path, rd, hdr2)
	defer resp.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, resp
}

// login 用正确密码登录并返回会话 cookie。
func (e *testEnv) login(password string) string {
	e.t.Helper()
	code, _, resp := e.doJSON("POST", "/api/login", map[string]string{"password": password}, nil)
	if code != 200 {
		e.t.Fatalf("登录失败: %d", code)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return c.Value
		}
	}
	e.t.Fatal("未签发会话 cookie")
	return ""
}

func authHdr(cookie string) map[string]string {
	return map[string]string{"Cookie": SessionCookieName + "=" + cookie}
}

// newEnv 构造测试环境（可选注入上游 mock 与 provider 配置）。
func newEnv(t *testing.T, configure func(key []byte, f *config.File)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	key, err := crypto.EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := persist.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	f := res.File
	if configure != nil {
		configure(key, f)
	}
	if err := persist.SaveConfig(cfgPath, f); err != nil {
		t.Fatal(err)
	}
	runtime := config.BuildRuntime(f, key)

	logBuf := &syncBuffer{}
	logx.SetOutput(logBuf)
	logx.RegisterSecret("test-token-plain")

	store := collector.NewStore("test")
	sched := collector.NewScheduler(runtime, store, collector.NewClient("test"))
	// v0.4.0：趋势存储与生产装配一致（同目录 trends.json），便于断言「删 Key 连带清理」
	trends := trend.New(persist.TrendsPathOf(dir))
	srv := New(Options{
		Version:         "test-1.0",
		ConfigPath:      cfgPath,
		MasterKey:       key,
		SessionKey:      crypto.SessionKey(key),
		Store:           store,
		Trends:          trends,
		Static:          []byte("<html>QuotaClock static</html>"),
		Icons:           web.Icons(),
		OnConfigChanged: sched.Reload,
	}, f, runtime)

	env := &testEnv{t: t, dir: dir, configPath: cfgPath, srv: srv, sched: sched, store: store, trends: trends, logBuf: logBuf, key: key}
	env.http = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { env.http.Close(); logx.SetOutput(os.Stdout) })
	return env
}

func codeOf(m map[string]any) string {
	if m == nil {
		return ""
	}
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

// ---------- C-srv-01 鉴权判定树 ----------

func TestAuthTree(t *testing.T) { // C-srv-01
	env := newEnv(t, nil)
	// 无 cookie
	code, m, _ := env.doJSON("PUT", "/api/config", map[string]any{}, nil)
	if code != 401 || codeOf(m) != "UNAUTHENTICATED" {
		t.Fatalf("无 cookie: %d %s", code, codeOf(m))
	}
	// 坏格式 cookie
	code, m, _ = env.doJSON("PUT", "/api/config", map[string]any{}, map[string]string{"Cookie": SessionCookieName + "=not-a-token"})
	if code != 401 || codeOf(m) != "UNAUTHENTICATED" {
		t.Fatalf("坏格式: %d %s", code, codeOf(m))
	}
	// 坏签名
	code, m, _ = env.doJSON("PUT", "/api/config", map[string]any{}, map[string]string{"Cookie": SessionCookieName + "=eyJzaWQiOiJhYmMifQ.sig"})
	if code != 401 || codeOf(m) != "SESSION_INVALID" {
		t.Fatalf("坏签名: %d %s", code, codeOf(m))
	}
	// 过期
	expired, _ := crypto.SignSession(crypto.SessionKey(env.key), crypto.SessionClaims{SID: "aaaa", Exp: time.Now().Add(-time.Hour).Unix()})
	code, m, _ = env.doJSON("PUT", "/api/config", map[string]any{}, map[string]string{"Cookie": SessionCookieName + "=" + expired})
	if code != 401 || codeOf(m) != "SESSION_INVALID" {
		t.Fatalf("过期: %d %s", code, codeOf(m))
	}
	// 已登出 sid
	ck := env.login(config.DefaultPassword)
	code, _, _ = env.doJSON("POST", "/api/logout", nil, authHdr(ck))
	if code != 200 {
		t.Fatalf("logout = %d", code)
	}
	code, m, _ = env.doJSON("PUT", "/api/config", map[string]any{}, authHdr(ck))
	if code != 401 || codeOf(m) != "SESSION_REVOKED" {
		t.Fatalf("已登出: %d %s", code, codeOf(m))
	}
}

// ---------- C-srv-02 登录 JSON-only ----------

func TestLoginContentType(t *testing.T) { // C-srv-02
	env := newEnv(t, nil)
	resp := env.do("POST", "/api/login", strings.NewReader(`{"password":"x"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(readAll(resp), "INVALID_CONTENT_TYPE") {
		t.Fatalf("非 JSON Content-Type 应 400: %d", resp.StatusCode)
	}
}

// ---------- C-srv-03 限速 ----------

func TestLoginRateLimit(t *testing.T) { // C-srv-03
	env := newEnv(t, nil)
	now := time.Unix(1000000, 0)
	env.srv.limiter.clock = func() time.Time { return now }
	for i := 1; i <= 5; i++ {
		code, m, _ := env.doJSON("POST", "/api/login", map[string]string{"password": "wrong"}, nil)
		if i < 5 {
			if code != 401 || codeOf(m) != "INVALID_CREDENTIALS" {
				t.Fatalf("第 %d 次应 401: %d %s", i, code, codeOf(m))
			}
		}
	}
	// 第 6 次（锁定期内正确密码）→ 429
	code, m, _ := env.doJSON("POST", "/api/login", map[string]string{"password": config.DefaultPassword}, nil)
	if code != 429 || codeOf(m) != "RATE_LIMITED" {
		t.Fatalf("锁定期正确密码应 429: %d %s", code, codeOf(m))
	}
	// +10min → 恢复且计数清零
	now = now.Add(10 * time.Minute)
	code, _, _ = env.doJSON("POST", "/api/login", map[string]string{"password": config.DefaultPassword}, nil)
	if code != 200 {
		t.Fatalf("锁定期结束应恢复: %d", code)
	}
}

// ---------- C-srv-04/05 会话往返与登出 ----------

func TestSessionRoundTripAndLogout(t *testing.T) { // C-srv-04/05
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	code, _, _ := env.doJSON("PUT", "/api/config", validPutBody(env), authHdr(ck))
	if code != 200 {
		t.Fatalf("有效会话 PUT 应 200: %d", code)
	}
	// 篡改 1 字节
	dot := strings.LastIndexByte(ck, '.')
	bad := ck[:dot-1]
	if ck[dot-1] == 'A' {
		bad = ck[:dot-1] + "B"
	} else {
		bad = ck[:dot-1] + "A"
	}
	badCookie := bad + ck[dot:]
	code, m, _ := env.doJSON("PUT", "/api/config", validPutBody(env), map[string]string{"Cookie": SessionCookieName + "=" + badCookie})
	if code != 401 || codeOf(m) != "SESSION_INVALID" {
		t.Fatalf("篡改 cookie 应 SESSION_INVALID: %d %s", code, codeOf(m))
	}
	// 登出 → cookie Max-Age=0 + 立即失效
	code, _, resp := env.doJSON("POST", "/api/logout", nil, authHdr(ck))
	if code != 200 {
		t.Fatalf("logout = %d", code)
	}
	var maxAge int = -999
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			maxAge = c.MaxAge
		}
	}
	if maxAge != 0 {
		t.Fatalf("logout cookie Max-Age = %d", maxAge)
	}
	code, m, _ = env.doJSON("PUT", "/api/config", validPutBody(env), authHdr(ck))
	if code != 401 || codeOf(m) != "SESSION_REVOKED" {
		t.Fatalf("登出后旧 cookie 应 SESSION_REVOKED: %d %s", code, codeOf(m))
	}
}

// validPutBody 构造合法 PUT 请求体（基于当前 env 配置）。
// v0.2.5：provider 只含 platform + access_keys（回传凭据时不带 token = 保留原值）。
func validPutBody(env *testEnv) map[string]any {
	f := env.srv.file.Load()
	body := map[string]any{
		"version":   config.CurrentVersion,
		"listen":    map[string]any{"host": f.Listen.Host, "port": f.Listen.Port},
		"collector": f.Collector,
		"auth":      map[string]any{"mode": f.Auth.Mode},
		"providers": []any{},
	}
	for _, p := range f.Providers {
		keys := []any{}
		for _, k := range p.AccessKeys {
			keys = append(keys, map[string]any{"id": k.ID, "name": k.Name})
		}
		body["providers"] = append(body["providers"].([]any), map[string]any{
			"platform": p.Platform, "access_keys": keys,
		})
	}
	return body
}

// putOneKey 用「单平台单凭据」PUT 覆盖 providers 段（测试主体），保持 listen/collector/auth 合法。
// platform/key/token 为空时给默认值；token 为空表示保留。
func putOneKey(env *testEnv, platform, keyID, keyName, token string) map[string]any {
	body := validPutBody(env)
	if platform == "" {
		platform = "opencode"
	}
	if keyID == "" {
		keyID = "a"
	}
	k := map[string]any{"id": keyID, "name": keyName}
	if token != "" {
		k["token"] = token
	}
	body["providers"] = []any{map[string]any{"platform": platform, "access_keys": []any{k}}}
	return body
}

// ---------- C-srv-06 PUT 校验 details 全量返回 ----------

func TestPutValidationDetails(t *testing.T) { // C-srv-06
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	body := validPutBody(env)
	body["listen"] = map[string]any{"host": "bad-host", "port": 70000}
	body["providers"] = []any{
		map[string]any{"platform": "", "access_keys": []any{}},
	}
	code, m, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != 400 {
		t.Fatalf("校验失败应 400: %d", code)
	}
	details, _ := m["details"].([]any)
	if len(details) < 4 {
		t.Fatalf("应返回全部错误项: %v", m)
	}
}

// ---------- C-srv-07 GET 脱敏两态 + no-store ----------

func TestGetConfigMasking(t *testing.T) { // C-srv-07 + C-config-17/18
	env := newEnv(t, func(key []byte, f *config.File) {
		cipher, _ := crypto.SealToken(key, "abcd1234")
		f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode",
			AccessKeys: []config.AccessKey{{ID: "k1", Name: "A1", TokenCipher: cipher}}})
	})
	// 未登录
	code, m, resp := env.doJSON("GET", "/api/config", nil, nil)
	if code != 200 {
		t.Fatal(code)
	}
	s := mustJSON(m)
	if strings.Contains(s, "token_masked") || strings.Contains(s, "abcd1234") {
		t.Fatalf("未登录不应含掩码: %s", s)
	}
	if !strings.Contains(s, `"has_token":true`) {
		t.Fatal("应含 has_token")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	// 登录
	ck := env.login(config.DefaultPassword)
	code, m, _ = env.doJSON("GET", "/api/config", nil, authHdr(ck))
	if code != 200 {
		t.Fatal(code)
	}
	s = mustJSON(m)
	if !strings.Contains(s, `"token_masked":"ab****34"`) {
		t.Fatalf("登录态应含掩码: %s", s)
	}
	if strings.Contains(s, "abcd1234") || strings.Contains(s, "token_cipher") {
		t.Fatal("掩码外不得有明文/密文")
	}
}

// TestGetConfigEmptyProviders：新环境（模板空配置）GET /api/config 的 providers
// 必须是 JSON 数组 [] 而非 null——前端添加平台按钮直接对其 push（260906 实测回归）。
func TestGetConfigEmptyProviders(t *testing.T) {
	env := newEnv(t, nil) // 不注入 provider：模板即空配置
	code, m, _ := env.doJSON("GET", "/api/config", nil, nil)
	if code != 200 {
		t.Fatal(code)
	}
	if arr, ok := m["providers"].([]any); !ok || len(arr) != 0 {
		t.Fatalf("providers 应为 JSON 数组 []，got %v (%T)", m["providers"], m["providers"])
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------- C-srv-08 CORS 红线 / C-srv-09 安全头 ----------

func TestCORSSecurityHeaders(t *testing.T) { // C-srv-08/09
	env := newEnv(t, nil)
	for _, path := range []string{"/", "/api/quotas", "/api/config"} {
		resp := env.do("GET", path, nil, nil)
		for h := range resp.Header {
			if strings.HasPrefix(strings.ToLower(h), "access-control-") {
				t.Fatalf("%s 出现 CORS 头: %s", path, h)
			}
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
			resp.Header.Get("X-Frame-Options") != "DENY" ||
			resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s 安全头缺失: %v", path, resp.Header)
		}
		resp.Body.Close()
	}
	// OPTIONS → 无预检放行（405/404）
	resp := env.do("OPTIONS", "/api/config", nil, nil)
	if resp.StatusCode != 405 && resp.StatusCode != 404 {
		t.Fatalf("OPTIONS 应 405/404: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 未知路径 → 404
	resp = env.do("GET", "/nope", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("未知路径应 404: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------- C-srv-10/11 /api/test ----------

func TestAPI(t *testing.T) { // C-srv-10 临时 token 不落盘 + C-srv-11 502 纪律 + C-srv-12 静态页
	env := newEnv(t, func(key []byte, f *config.File) {
		f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode",
			AccessKeys: []config.AccessKey{{ID: "a", Name: "A"}}})
	})
	// 静态页
	resp := env.do("GET", "/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") ||
		resp.Header.Get("Cache-Control") != "no-cache" || !strings.Contains(readAll(resp), "QuotaClock static") {
		t.Fatalf("静态页错误: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	ck := env.login(config.DefaultPassword)

	// 成功：临时 token（仅请求内存使用）
	before, _ := os.ReadFile(env.configPath)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token-plain" {
			t.Errorf("临时 token 未生效: %q", got)
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer up.Close()
	setUpstream(t, env, up.URL)
	afterSetup, _ := os.ReadFile(env.configPath)
	if !bytes.Equal(before, afterSetup) {
		t.Fatal("内存修改不应落盘（测试前提）")
	}
	code, m, _ := env.doJSON("POST", "/api/test", map[string]any{"provider_id": "opencode.a", "token": "test-token-plain"}, authHdr(ck))
	if code != 200 || m["ok"] != true || m["latency_ms"] == nil {
		t.Fatalf("test 应 200: %d %v", code, m)
	}
	if strings.Contains(mustJSON(m), "test-token-plain") {
		t.Fatal("响应不得回显 token")
	}
	after, _ := os.ReadFile(env.configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("临时 token 落盘了")
	}
	if strings.Contains(env.logBuf.String(), "test-token-plain") {
		t.Fatal("日志出现 token（脱敏红线）")
	}

	// 502 纪律：message 含上游状态码与摘要；无上游响应头透传
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Internal-Secret", "super-secret-value")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"msg":"deny-all"}`)
	}))
	defer up2.Close()
	setUpstream(t, env, up2.URL)
	code, m, _ = env.doJSON("POST", "/api/test", map[string]any{"provider_id": "opencode.a", "token": "test-token-plain"}, authHdr(ck))
	if code != 502 || codeOf(m) != "UPSTREAM_ERROR" {
		t.Fatalf("上游失败应 502: %d %v", code, m)
	}
	e := m["error"].(map[string]any)
	msg := e["message"].(string)
	if !strings.Contains(msg, "HTTP 403") || !strings.Contains(msg, "deny-all") {
		t.Fatalf("502 message 应含状态码与摘要: %s", msg)
	}
	if strings.Contains(msg, "super-secret-value") {
		t.Fatal("不应透传上游响应头")
	}
}

// setUpstream 把采集上游指向 mock（v0.2.5：平台地址由预设内置，测试通过环境开关覆盖 origin）。
func setUpstream(t *testing.T, env *testEnv, url string) {
	t.Helper()
	t.Setenv(config.EnvUpstreamOverride, url)
	f := env.srv.file.Load()
	runtime := config.BuildRuntime(f, env.key)
	env.srv.cfg.Store(runtime)
	env.sched.Reload(runtime)
}

// ---------- C-srv-12 补充：404 语义已在 CORS 用例覆盖 ----------

// ---------- C-srv-13 权限矩阵 ----------

func TestPermissionMatrix(t *testing.T) { // C-srv-13
	env := newEnv(t, nil)
	// admin 模式：GET 公开
	if code, _, _ := env.doJSON("GET", "/api/quotas", nil, nil); code != 200 {
		t.Fatal("GET quotas 应公开")
	}
	if code, _, _ := env.doJSON("GET", "/api/config", nil, nil); code != 200 {
		t.Fatal("GET config 应公开")
	}
	// PUT/TEST 未登录 401
	if code, _, _ := env.doJSON("PUT", "/api/config", validPutBody(env), nil); code != 401 {
		t.Fatal("PUT 未登录应 401")
	}
	if code, _, _ := env.doJSON("POST", "/api/test", map[string]any{"provider_id": "x"}, nil); code != 401 {
		t.Fatal("TEST 未登录应 401")
	}
	// 切 none 模式（直接换运行时 + file）
	f := *env.srv.file.Load()
	f.Auth.Mode = config.AuthModeNone
	runtime := config.BuildRuntime(&f, env.key)
	env.srv.file.Store(&f)
	env.srv.cfg.Store(runtime)
	if code, _, _ := env.doJSON("PUT", "/api/config", validPutBody(env), nil); code != 200 {
		t.Fatal("none 模式 PUT 应免登录")
	}
	if code, _, _ := env.doJSON("POST", "/api/test", map[string]any{"provider_id": "x"}, nil); code == 401 {
		t.Fatal("none 模式 TEST 不应 401")
	}
}

// ---------- C-srv-14 改密热生效（-admin-password 的服务端等价链路 + I-6） ----------

func TestPasswordChangeHotReload(t *testing.T) { // C-srv-14
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	body := validPutBody(env)
	body["auth"] = map[string]any{"mode": "admin", "password": "NewPass@123"}
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatal("改密 PUT 失败")
	}
	// 旧密码 401、新密码 200
	code, m, _ := env.doJSON("POST", "/api/login", map[string]string{"password": config.DefaultPassword}, nil)
	if code != 401 || codeOf(m) != "INVALID_CREDENTIALS" {
		t.Fatalf("旧密码应 401: %d %s", code, codeOf(m))
	}
	code, _, _ = env.doJSON("POST", "/api/login", map[string]string{"password": "NewPass@123"}, nil)
	if code != 200 {
		t.Fatalf("新密码应 200: %d", code)
	}
	// config.json hash 已更新（旧默认 hash 不在）
	b, _ := os.ReadFile(env.configPath)
	s := string(b)
	if strings.Contains(s, "NewPass@123") {
		t.Fatal("明文密码落盘")
	}
	// 新密码不再是默认密码 → password_is_default=false
	ck2 := env.login("NewPass@123")
	_, m2, _ := env.doJSON("GET", "/api/config", nil, authHdr(ck2))
	authObj := m2["auth"].(map[string]any)
	if authObj["password_is_default"] != false {
		t.Fatalf("改密后 password_is_default 应 false: %v", authObj)
	}
}

// ---------- C-config-19/20 PUT token 空=保留 / 新 token 更新 ----------

func TestPutTokenMerge(t *testing.T) { // C-config-19/20
	env := newEnv(t, func(key []byte, f *config.File) {
		cipher, _ := crypto.SealToken(key, "orig-token-9999")
		f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode",
			AccessKeys: []config.AccessKey{{ID: "a", Name: "A", TokenCipher: cipher}}})
	})
	ck := env.login(config.DefaultPassword)
	before, _ := os.ReadFile(env.configPath)
	// PUT 不带 token（改名）→ cipher 保留
	body := validPutBody(env)
	body["providers"] = []any{map[string]any{"platform": "opencode",
		"access_keys": []any{map[string]any{"id": "a", "name": "A2"}}}}
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatalf("改名 PUT = %d", code)
	}
	mid, _ := os.ReadFile(env.configPath)
	if string(mid) == string(before) {
		t.Fatal("改名应已写盘")
	}
	rt := env.srv.cfg.Load()
	if rt.Provider("opencode.a").Token != "orig-token-9999" {
		t.Fatalf("空 token 应保留原值: %q", rt.Provider("opencode.a").Token)
	}
	// PUT 新 token → 更新 + 掩码重算 + 明文不留痕
	body2 := validPutBody(env)
	body2["providers"] = []any{map[string]any{"platform": "opencode",
		"access_keys": []any{map[string]any{"id": "a", "name": "A2", "token": "new-token-7777"}}}}
	if code, _, _ := env.doJSON("PUT", "/api/config", body2, authHdr(ck)); code != 200 {
		t.Fatalf("新 token PUT = %d", code)
	}
	b, _ := os.ReadFile(env.configPath)
	if strings.Contains(string(b), "new-token-7777") || strings.Contains(string(b), "orig-token-9999") {
		t.Fatal("明文 token 落盘")
	}
	if got := env.srv.cfg.Load().Provider("opencode.a").Token; got != "new-token-7777" {
		t.Fatalf("新 token 未生效: %q", got)
	}
	if got := (*env.srv.masks.Load())["opencode.a"]; got != "ne****77" {
		t.Fatalf("掩码 hint 未重算: %q", got)
	}
}

// ---------- I-1 管理全链路 ----------

func TestIntegrationAdminChain(t *testing.T) { // I-1
	env := newEnv(t, nil)
	// 未登录 GET（无掩码）
	_, m1, _ := env.doJSON("GET", "/api/config", nil, nil)
	if strings.Contains(mustJSON(m1), "token_masked") {
		t.Fatal("未登录不应有掩码")
	}
	// 登录
	ck := env.login(config.DefaultPassword)
	// GET（掩码在——配置里还没有 token，先 PUT 一个）
	body := putOneKey(env, "opencode", "a", "A", "tok-12345678")
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatal("PUT token 失败")
	}
	_, m2, _ := env.doJSON("GET", "/api/config", nil, authHdr(ck))
	if !strings.Contains(mustJSON(m2), `"token_masked":"to****78"`) {
		t.Fatalf("登录后 GET 应含掩码: %s", mustJSON(m2))
	}
	// PUT 改名（空 token 保留）
	body2 := putOneKey(env, "opencode", "a", "A-renamed", "")
	if code, _, _ := env.doJSON("PUT", "/api/config", body2, authHdr(ck)); code != 200 {
		t.Fatal("改名失败")
	}
	// logout → 掩码消失 → PUT 401
	if code, _, _ := env.doJSON("POST", "/api/logout", nil, authHdr(ck)); code != 200 {
		t.Fatal("logout 失败")
	}
	_, m3, _ := env.doJSON("GET", "/api/config", nil, nil)
	if strings.Contains(mustJSON(m3), "token_masked") {
		t.Fatal("登出后掩码应消失")
	}
	if code, _, _ := env.doJSON("PUT", "/api/config", body2, nil); code != 401 {
		t.Fatal("登出后 PUT 应 401")
	}
}

// ---------- I-2 热生效（结构变化 → revision 立即 +1） ----------

func TestIntegrationHotReload(t *testing.T) { // I-2
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	rev0 := env.store.Get().Revision
	// PUT 增加一个平台
	body := putOneKey(env, "opencode", "a", "A", "sk-hotreload")
	if code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck)); code != 200 {
		t.Fatal("PUT 失败")
	}
	snap := env.store.Get()
	if snap.Revision <= rev0 {
		t.Fatalf("结构变化应推 revision: %d → %d", rev0, snap.Revision)
	}
	if len(snap.Providers) != 1 || snap.Providers[0].ID != "opencode.a" {
		t.Fatalf("结构未更新: %+v", snap.Providers)
	}
	if snap.Providers[0].Status != collector.StatusFailed || snap.Providers[0].Error.Code != collector.CodeNotCollectedYet {
		t.Fatalf("新平台应为未采集态: %+v", snap.Providers[0])
	}
	// PUT 删除 → 结构变化（显式空 providers）
	body2 := validPutBody(env)
	body2["providers"] = []any{}
	if code, _, _ := env.doJSON("PUT", "/api/config", body2, authHdr(ck)); code != 200 {
		t.Fatal("PUT 失败")
	}
	snap2 := env.store.Get()
	if len(snap2.Providers) != 0 {
		t.Fatalf("删除未传播: %+v", snap2.Providers)
	}
	if snap2.Revision <= snap.Revision {
		t.Fatal("删除应推 revision")
	}
}

// ---------- I-3 采集→快照（端到端：mock 上游四形态） ----------

func TestIntegrationCollectSnapshot(t *testing.T) { // I-3
	up := map[string]http.HandlerFunc{
		"/ok": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":42}}`)
		},
		"/bad401": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
		"/rl429":  func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "77"); w.WriteHeader(429) },
		"/slowok": func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, `{"success":true}`)
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := up[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		h(w, r)
	}))
	defer srv.Close()

	env := newEnv(t, nil)
	provs := []*config.RuntimeProvider{}
	for _, id := range []string{"ok", "bad401", "rl429", "slowok"} {
		provs = append(provs, &config.RuntimeProvider{ID: id, Name: id, BaseURL: srv.URL, Paths: []string{"/" + id}, AuthStyle: config.AuthStyleBearer})
	}
	runtime := mkRuntime(provs...)
	env.sched.Reload(runtime)
	env.srv.cfg.Store(runtime)
	// 调度器首 tick 立即执行（Run 循环先采集后休眠）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.sched.Run(ctx)
	// 等待全部 4 个平台离开「未采集」态（stagger 1~5s + 请求）
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s := env.store.Get()
		if len(s.Providers) == 4 {
			done := true
			for _, p := range s.Providers {
				if p.Error != nil && p.Error.Code == collector.CodeNotCollectedYet {
					done = false
				}
			}
			if done {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()

	snap := env.store.Get()
	byID := map[string]collector.ProviderState{}
	for _, p := range snap.Providers {
		byID[p.ID] = p
	}
	if byID["ok"].Status != collector.StatusOK || !strings.Contains(string(byID["ok"].Data), "42") {
		t.Fatalf("ok 平台状态错误: %+v", byID["ok"])
	}
	if byID["bad401"].Status != collector.StatusTokenInvalid {
		t.Fatalf("401 平台应 token_invalid: %+v", byID["bad401"])
	}
	if byID["rl429"].Status != collector.StatusFailed || byID["rl429"].Error.RetryAfterS == nil || *byID["rl429"].Error.RetryAfterS != 77 {
		t.Fatalf("429 平台应带 retry_after_s=77: %+v", byID["rl429"])
	}
	if byID["slowok"].Status != collector.StatusOK {
		t.Fatalf("慢平台应成功: %+v", byID["slowok"])
	}
	// /api/quotas 端到端
	code, m, _ := env.doJSON("GET", "/api/quotas", nil, nil)
	if code != 200 || int(m["revision"].(float64)) != int(snap.Revision) {
		t.Fatalf("quotas 端点 revision 不一致: %d %v", code, m["revision"])
	}
	if m["version"] != "test" {
		t.Fatalf("version 字段缺失: %v", m["version"])
	}
}

func mkRuntime(providers ...*config.RuntimeProvider) *config.Runtime {
	c := config.DefaultCollector()
	return &config.Runtime{
		Version: config.CurrentVersion, Listen: config.DefaultListen(), Collector: c,
		AuthMode: config.AuthModeAdmin, Providers: providers,
	}
}

func readAll(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func readAll2(r io.Reader) string { b, _ := io.ReadAll(r); return string(b) }

var _ = fmt.Sprintf
var _ = readAll2
