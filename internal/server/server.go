// Package server 承载 HTTP 服务：路由、中间件、鉴权判定树、限速与 handlers。
// CORS 红线：全链路无任何 Access-Control-* 输出（CSRF 防线①）。
package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RexKang/QuotaClock/internal/collector"
	"github.com/RexKang/QuotaClock/internal/config"
	"github.com/RexKang/QuotaClock/internal/crypto"
	"github.com/RexKang/QuotaClock/internal/logx"
	"github.com/RexKang/QuotaClock/internal/persist"
	"github.com/RexKang/QuotaClock/internal/trend"

	"golang.org/x/crypto/bcrypt"
)

// SessionCookieName 管理员会话 cookie 名。
const SessionCookieName = "qc_session"

// maxBodyBytes 请求体上限。
const maxBodyBytes = 4 << 20

// Options 装配参数。
type Options struct {
	Version    string
	ConfigPath string
	MasterKey  []byte // token 密封密钥（key.bin）
	SessionKey []byte // HKDF 派生的会话签名密钥

	Store  *collector.Store  // 快照仓库
	Static []byte            // 内嵌静态页
	Icons  map[string][]byte // 内嵌图标（文件名 → 内容，白名单见 web.IconNames）

	// Trends 用量趋势（v0.4.0；nil = 不提供趋势接口，测试装配可省）
	Trends *trend.Store

	// OnConfigChanged 配置保存成功后的热生效钩子（main 注入 scheduler.Reload）。
	OnConfigChanged func(*config.Runtime)

	// FlagOverrides 启动参数对 config 的覆盖（flag > config 优先级在 PUT 热生效后仍保持）。
	FlagOverrides FlagOverrides
}

// FlagOverrides -interval/-addr/-port 覆盖值（零值 = 未覆盖）。
type FlagOverrides struct {
	IntervalBaseS int
	Host          string
	Port          int
}

// Server HTTP 服务。
type Server struct {
	version    string
	configPath string
	key        []byte
	static     []byte
	onChanged  func(*config.Runtime)
	overrides  FlagOverrides

	cfg      atomic.Pointer[config.Runtime]    // 运行时视图（鉴权/采集判据）
	file     atomic.Pointer[config.File]       // 落盘视图（GET /api/config 来源、PUT 合并基础）
	masks    atomic.Pointer[map[string]string] // 掩码 hint（D-5）
	pwDef    atomic.Bool                       // 默认密码未修改检测结果缓存
	sessions *Sessions
	limiter  *Limiter
	client   *collector.Client
	store    *collector.Store
	trends   *trend.Store
	icons    map[string][]byte
}

// New 装配服务。
func New(opt Options, initial *config.File, runtime *config.Runtime) *Server {
	s := &Server{
		version:    opt.Version,
		configPath: opt.ConfigPath,
		key:        opt.MasterKey,
		static:     opt.Static,
		onChanged:  opt.OnConfigChanged,
		overrides:  opt.FlagOverrides,
		sessions:   NewSessions(opt.SessionKey),
		limiter:    NewLimiter(),
		client:     collector.NewClient(opt.Version),
		store:      opt.Store,
		trends:     opt.Trends,
		icons:      opt.Icons,
	}
	s.cfg.Store(runtime)
	s.file.Store(initial)
	s.masks.Store(maskMap(initial, runtime))
	s.pwDef.Store(config.IsDefaultPassword(initial.Auth.PasswordHash))
	return s
}

func maskMap(f *config.File, r *config.Runtime) *map[string]string {
	m := map[string]string{}
	if r == nil {
		return &m
	}
	for _, p := range r.Providers {
		if p.TokenMasked != "" {
			m[p.ID] = p.TokenMasked
		}
	}
	return &m
}

// Handler 返回完整中间件链：recover → security headers → 路由（按权限矩阵）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /icons/{file}", s.handleIcon)
	mux.HandleFunc("GET /api/quotas", s.handleQuotas)
	mux.HandleFunc("GET /api/trends", s.handleTrends)
	mux.HandleFunc("POST /api/trends/reset", s.requireWrite(s.handleResetTrends))
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.requireWrite(s.handlePutConfig))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/test", s.requireWrite(s.handleTest))
	// v0.3.0：导出配置（含明文 token）按敏感端点对待，与 PUT/TEST 同一守卫
	mux.HandleFunc("GET /api/config/export", s.requireWrite(s.handleExportConfig))
	return s.recoverMW(s.securityMW(mux))
}

// recoverMW panic → ERROR 日志（含堆栈）+ 统一 500 INTERNAL_ERROR。
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logx.Errorf("panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityMW 安全响应头；/api/* 一律 no-store（响应随登录态变化，禁缓存）。
// 零 CORS 头红线：此处刻意不发送任何 Access-Control-* 头。
func (s *Server) securityMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// requireWrite 权限矩阵：admin 模式写操作（PUT config / POST test）须登录；none 模式开放。
func (s *Server) requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Load().IsAdmin() {
			next(w, r)
			return
		}
		if _, code := s.checkSession(r); code != "" {
			drainBody(r) // 先读干 body，否则连接被强关、客户端可能丢失这个 401
			writeAuthErr(w, code)
			return
		}
		next(w, r)
	}
}

// checkSession 鉴权判定树（设计 §8.3）：
// 无 cookie/格式坏 → UNAUTHENTICATED；签名失败/exp 过期 → SESSION_INVALID；sid 在拒绝名单 → SESSION_REVOKED。
func (s *Server) checkSession(r *http.Request) (sid, code string) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return "", "UNAUTHENTICATED"
	}
	claims, err := s.sessions.Verify(c.Value)
	if err != nil {
		if err == crypto.ErrSessionFormat {
			return "", "UNAUTHENTICATED"
		}
		return "", "SESSION_INVALID"
	}
	if s.sessions.Revoked(claims.SID) {
		return "", "SESSION_REVOKED"
	}
	return claims.SID, ""
}

// isAuthenticated 当前请求是否持有效会话（admin 模式下才有意义；none 模式恒 false → 掩码永不展示）。
func (s *Server) isAuthenticated(r *http.Request) bool {
	if !s.cfg.Load().IsAdmin() {
		return false
	}
	_, code := s.checkSession(r)
	return code == ""
}

// ---------- 通用响应 ----------

type errBody struct {
	Error errInner `json:"error"`
}

type errInner struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	RetryAfterS *int   `json:"retry_after_s,omitempty"`
}

type validationBody struct {
	Error   errInner                 `json:"error"`
	Details []config.ValidationError `json:"details"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errBody{Error: errInner{Code: code, Message: msg}})
}

func writeAuthErr(w http.ResponseWriter, code string) {
	var msg string
	switch code {
	case "UNAUTHENTICATED":
		msg = "请先登录"
	case "SESSION_INVALID":
		msg = "会话无效或已过期，请重新登录"
	case "SESSION_REVOKED":
		msg = "会话已登出，请重新登录"
	default:
		msg = "未认证"
	}
	writeErr(w, http.StatusUnauthorized, code, msg)
}

func writeOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// maxDrainBytes 读干 body 的上限（超过部分由 net/http 在关连接时丢弃）。
const maxDrainBytes = 64 << 10

// drainBody 读干请求体（有界）。
// 必要性：handler 未消费 body 就写响应时，net/http 会关闭连接（响应带 close 语义），
// 若客户端仍在发 body 就可能收到 RST、**丢掉这个响应**——实测 Windows 本机「带 body 的
// PUT 未登录 → 401」3% 概率丢失（300 次 9 次），浏览器里表现为「保存失败」而非「请先登录」。
// 故所有「不读 body 就拒绝」的路径（鉴权 401、登录的 400/429）先读干 body。
func drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrainBytes))
}

// ---------- handlers ----------

// handleIndex 内嵌静态页。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.static)
}

// iconContentTypes 图标扩展名 → Content-Type。
var iconContentTypes = map[string]string{
	".ico": "image/x-icon",
	".svg": "image/svg+xml",
	".png": "image/png",
}

// handleFavicon GET /favicon.ico：浏览器默认探测路径。
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	s.serveIconBytes(w, "favicon.ico")
}

// handleIcon GET /icons/{file}：白名单内的内嵌图标。
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	s.serveIconBytes(w, r.PathValue("file"))
}

func (s *Server) serveIconBytes(w http.ResponseWriter, name string) {
	b, ok := s.icons[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "未找到该资源")
		return
	}
	ct := "application/octet-stream"
	if t, ok := iconContentTypes[strings.ToLower(name[strings.LastIndexByte(name, '.'):])]; ok {
		ct = t
	}
	// 图标随二进制发布、内容不可变：可长缓存（静态页本身 no-cache，与此互不影响）
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// handleQuotas GET /api/quotas：快照序列化（公开）。
func (s *Server) handleQuotas(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Get()
	if snap == nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "快照不可用")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleTrends GET /api/trends：用量趋势视图（公开，与 /api/quotas 同级）。
//
// 为什么公开：趋势图里是「这个 Key 这些天用了多少」，与总览页展示的数据同级；
// 看板本身免登录可看（v0.2.x 既定），单独给趋势加压只会让页面上出现两个权限语汇。
// 真正的敏感物是 token（掩码仅登录可见），不在这里。
//
// 入参 days：回看天数（默认 400，上限 3650）。响应的 daily/hours 是**定长连续数组**，
// 缺数据的位置是 null——前端因此不需要自己对齐日期，也不会把「没采样」画成 0。
func (s *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "days 必须是正整数")
			return
		}
		days = n
	}
	if s.trends == nil {
		// 未装配趋势（部分测试/裁剪装配）：返回空视图而不是 500，前端自然显示空态
		writeJSON(w, http.StatusOK, &trend.View{Version: persist.TrendsVersion, Days: 0, Keys: []trend.KeyView{}})
		return
	}
	writeJSON(w, http.StatusOK, s.trends.Snapshot(time.Now(), days))
}

// handleResetTrends 清空趋势数据（admin）。body 可选 {"key":"<runtime id>"}，缺省 = 全部清空。
//
// 存在的理由：口径修正后，历史里按旧口径算出来的柱子**无法重算**（只存聚合值、不存原始样本），
// 所以必须给用户一个「清掉重来」的入口，而不是让人手工去删 trends.json。
func (s *Server) handleResetTrends(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if r.Body != nil {
		// 空 body 合法（= 全部清空）；解析失败也不当错误，按全部处理更安全？不——宁可报错，
		// 免得「本意只清一个 Key」写错字段名却把全部清光。
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体不是合法 JSON")
			return
		}
	}
	scope := "全部"
	ids := []string{}
	if k := strings.TrimSpace(body.Key); k != "" {
		ids = []string{k}
		scope = k
	}
	if s.trends == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": 0, "days": 0, "hours": 0})
		return
	}
	st, err := s.trends.Reset(ids)
	if err != nil {
		logx.Warnf("清空趋势数据写盘失败（内存已清、重启后会读回旧文件）: %v", err)
		writeErr(w, http.StatusInternalServerError, "TRENDS_RESET_FAILED", "趋势数据已从内存清除，但写盘失败")
		return
	}
	logx.Infof("清空趋势数据（%s）：%d 个 API Key / %d 天 / %d 个小时槽", scope, st.Keys, st.Days, st.Hours)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": st.Keys, "days": st.Days, "hours": st.Hours})
}

// purgeRemovedTrends 清理「已被删掉的 API Key」的趋势数据（用户需求 3）。
//
// 两层分工：确认框在前端（删除时告知「趋势数据也会被清理」），这里是**数据层兜底**——
// 导入配置、直接调用 PUT 都绕过 UI，不兜底就会留下永久孤儿数据。
// 只认 runtime id 的消失：改名（name 变）与停用（enabled=false）都不动数据。
func (s *Server) purgeRemovedTrends(old, next *config.File) {
	if s.trends == nil || old == nil || next == nil {
		return
	}
	keep := map[string]bool{}
	for i := range next.Providers {
		fp := &next.Providers[i]
		for j := range fp.AccessKeys {
			keep[config.RuntimeID(fp.Platform, fp.AccessKeys[j].ID)] = true
		}
	}
	var gone []string
	for i := range old.Providers {
		fp := &old.Providers[i]
		for j := range fp.AccessKeys {
			id := config.RuntimeID(fp.Platform, fp.AccessKeys[j].ID)
			if !keep[id] {
				gone = append(gone, id)
			}
		}
	}
	if len(gone) == 0 {
		return
	}
	removed, err := s.trends.Purge(gone)
	if err != nil {
		logx.Warnf("趋势数据清理写盘失败（内存已清理，重启后不再出现）: %v", err)
	}
	if len(removed) > 0 {
		logx.Infof("随配置删除清理了 %d 个 API Key 的趋势数据: %s", len(removed), strings.Join(removed, "、"))
	}
}

// handleGetConfig GET /api/config：脱敏视图（掩码仅登录可见）。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	f := s.file.Load()
	authed := s.isAuthenticated(r)
	writeJSON(w, http.StatusOK, config.BuildView(f, *s.masks.Load(), authed, s.pwDef.Load()))
}

// handleLogin POST /api/login：JSON-only（CSRF 防线③）→ 全局限速 → bcrypt 比对 → 签发会话。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		drainBody(r)
		writeErr(w, http.StatusBadRequest, "INVALID_CONTENT_TYPE", "Content-Type 必须为 application/json")
		return
	}
	if ok, wait := s.limiter.Allow(); !ok {
		drainBody(r)
		rs := int(wait/time.Second) + 1
		writeJSON(w, http.StatusTooManyRequests, errBody{Error: errInner{
			Code: "RATE_LIMITED", Message: "失败次数过多，登录已锁定，请稍后再试", RetryAfterS: &rs,
		}})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体非法")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体须为 JSON 对象")
		return
	}
	hash := s.file.Load().Auth.PasswordHash
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		s.limiter.Fail()
		logx.Warnf("登录失败（%s）", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "密码错误")
		return
	}
	s.limiter.Success()
	val, err := s.sessions.Issue()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "会话签发失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    val,
		Path:     "/",
		MaxAge:   int(crypto.SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 不设 Secure：纯 HTTP 部署下浏览器拒收（PRD r1 取舍）
	})
	logx.Infof("登录成功（%s）", clientIP(r))
	writeOK(w)
}

// handleLogout POST /api/logout：sid 入拒绝名单 + 清 cookie。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if claims, verr := s.sessions.Verify(c.Value); verr == nil {
			s.sessions.Revoke(claims.SID, claims.Exp)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: 0,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeOK(w)
}

// handlePutConfig PUT /api/config：唯一写入口。全量替换 + 空 token 保留原值 + 原子写盘成功后热生效。
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体过大或读取失败")
		return
	}
	put, perr := config.ParsePut(body)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, validationBody{
			Error:   errInner{Code: "VALIDATION_ERROR", Message: perr.Error()},
			Details: []config.ValidationError{{Field: "body", Message: perr.Error()}},
		})
		return
	}
	if details := config.Validate(put); len(details) > 0 {
		writeJSON(w, http.StatusBadRequest, validationBody{
			Error:   errInner{Code: "VALIDATION_ERROR", Message: "配置校验失败"},
			Details: details,
		})
		return
	}

	old := s.file.Load()
	if details := config.ValidateNewKeys(put, old); len(details) > 0 {
		writeJSON(w, http.StatusBadRequest, validationBody{
			Error:   errInner{Code: "VALIDATION_ERROR", Message: "配置校验失败"},
			Details: details,
		})
		return
	}
	newFile, masks, merr := s.mergeForSave(old, put)
	if merr != nil {
		writeErr(w, http.StatusInternalServerError, "SAVE_FAILED", merr.Error())
		return
	}
	// 旧版配置（version < 当前）：按「迁移」对待 —— 先把目标机器的原配置备份一份再覆盖。
	// 跨机器搬运时源机器可能是旧版，导出文件里带的版本号就是它的版本；这一路径要能直接吃下。
	if put.Version > 0 && put.Version < config.CurrentVersion {
		// 备份名按**被覆盖的那份配置**的版本取（本机当前版本），不是来料的版本号
		bakVer := config.CurrentVersion
		if old != nil && old.Version > 0 {
			bakVer = old.Version
		}
		if bak, berr := persist.BackupConfig(s.configPath, bakVer); berr != nil {
			logx.Warnf("旧版配置导入：备份失败（继续保存）：%v", berr)
		} else {
			logx.Infof("按 v%d 旧版配置导入：原配置（v%d）已备份到 %s，将按 v%d 落盘",
				put.Version, bakVer, bak, config.CurrentVersion)
		}
	}
	if err := persist.SaveConfig(s.configPath, newFile); err != nil {
		// 磁盘为准：写盘失败内存不变（所见即所得）
		logx.Errorf("配置写盘失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "SAVE_FAILED", "配置写盘失败："+err.Error())
		return
	}

	// 内存 swap → 热生效
	runtime := config.BuildRuntime(newFile, s.key)
	s.file.Store(newFile)
	s.cfg.Store(runtime)
	s.masks.Store(&masks)
	s.pwDef.Store(config.IsDefaultPassword(newFile.Auth.PasswordHash))
	if s.onChanged != nil {
		s.onChanged(runtime)
	}
	// 被删掉的 Key：连同其趋势数据一起清理（用户需求 3；确认框在前端，这里是数据层兜底）
	s.purgeRemovedTrends(old, newFile)
	keys := 0
	for i := range newFile.Providers {
		keys += newFile.Providers[i].KeyCount()
	}
	logx.Infof("配置已保存并热生效（%d 个平台 / %d 个 API Key，listen %s:%d）",
		len(newFile.Providers), keys, newFile.Listen.Host, newFile.Listen.Port)
	writeOK(w)
}

// mergeForSave 把 PUT 请求体合并为新的落盘形态：
// token 空 = 保留原 cipher（改凭据名/停用不影响 token）；非空 = 新明文加密替换 + 重算掩码 hint。
// 凭据身份 = <platform>.<keyID>：keyID 变了视为新凭据（不继承 token），与旧版「改 id 即新平台」语义一致。
// auth.password 空 = 保留原 hash；非空 = bcrypt 新 hash。
func (s *Server) mergeForSave(old *config.File, put *config.Put) (*config.File, map[string]string, error) {
	oldKeys := map[string]config.AccessKey{}
	for i := range old.Providers {
		fp := &old.Providers[i]
		for j := range fp.AccessKeys {
			k := fp.AccessKeys[j]
			oldKeys[config.RuntimeID(fp.Platform, k.ID)] = k
		}
	}
	newFile := &config.File{
		Version:   config.CurrentVersion,
		Listen:    put.Listen,
		Collector: put.Collector,
		Balance:   config.NormalizeBalance(put.Balance), // v0.3.0：漏了这行会把余额设置写回 0
		Auth:      config.FileAuth{Mode: put.Auth.Mode, PasswordHash: old.Auth.PasswordHash},
		Providers: make([]config.FileProvider, 0, len(put.Providers)),
	}
	if put.Auth.Password != "" {
		hash, err := config.HashPassword(put.Auth.Password)
		if err != nil {
			return nil, nil, err
		}
		newFile.Auth.PasswordHash = hash
	}
	masks := map[string]string{}
	for i := range put.Providers {
		pp := &put.Providers[i]
		fp := config.FileProvider{
			Platform:   pp.Platform,
			AccessKeys: make([]config.AccessKey, 0, len(pp.AccessKeys)),
		}
		for j := range pp.AccessKeys {
			pk := &pp.AccessKeys[j]
			id := config.RuntimeID(pp.Platform, pk.ID)
			ak := config.AccessKey{
				ID:      pk.ID,
				Name:    pk.Name,
				Enabled: config.CopyBoolPtr(pk.Enabled),
			}
			switch {
			case pk.Token != "":
				cipher, err := crypto.SealToken(s.key, pk.Token)
				if err != nil {
					return nil, nil, err
				}
				ak.TokenCipher = cipher
				masks[id] = crypto.Mask(pk.Token)
			default:
				if o, ok := oldKeys[id]; ok {
					ak.TokenCipher = o.TokenCipher
					if m := (*s.masks.Load())[id]; m != "" {
						masks[id] = m
					}
				}
			}
			fp.AccessKeys = append(fp.AccessKeys, ak)
		}
		newFile.Providers = append(newFile.Providers, fp)
	}
	// flag 覆盖保持（-interval/-addr/-port > config，即使保存后本进程内仍覆盖）
	s.applyOverrides(&newFile.Listen, &newFile.Collector)
	return newFile, masks, nil
}

func (s *Server) applyOverrides(l *config.Listen, c *config.Collector) {
	if s.overrides.IntervalBaseS > 0 {
		c.IntervalBaseS = s.overrides.IntervalBaseS
	}
	if s.overrides.Host != "" {
		l.Host = s.overrides.Host
	}
	if s.overrides.Port > 0 {
		l.Port = s.overrides.Port
	}
}

// handleTest POST /api/test：以 paths[0] 测连通性；临时 token 仅本次请求内存使用，不落盘不进日志。
//
// 支持两种入参（v0.2.5）：
//   - provider_id = "<platform>.<keyID>"：已保存的凭据（token 缺省用配置里的）
//   - platform = "<platform>" + token：**尚未保存**的新凭据——用户的工作流是先测通再保存，
//     所以不能要求它先落盘（preset 提供地址与路径，token 用请求里的）
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体过大或读取失败")
		return
	}
	var req struct {
		ProviderID string `json:"provider_id"`
		Platform   string `json:"platform"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请求体须为 JSON 对象")
		return
	}
	cfg := s.cfg.Load()

	var probe config.RuntimeProvider
	found := false
	if p := cfg.Provider(req.ProviderID); p != nil {
		probe = *p // 值拷贝：临时 token 仅本次请求内存使用
		found = true
	}
	// 未保存的新凭据：平台标识可用「platform 字段」或 provider_id 的前缀给出
	platform := req.Platform
	if platform == "" && req.ProviderID != "" {
		if i := strings.Index(req.ProviderID, "."); i > 0 {
			platform = req.ProviderID[:i]
		} else {
			platform = req.ProviderID
		}
	}
	if !found {
		preset, ok := config.PlatformByID(platform)
		if !ok {
			writeErr(w, http.StatusBadRequest, "PROVIDER_NOT_FOUND",
				"未找到该平台凭据："+req.ProviderID+"（可用平台："+strings.Join(config.PlatformIDs(), " / ")+"）")
			return
		}
		probe = config.RuntimeProvider{
			ID:       platform + "." + strings.TrimPrefix(strings.TrimPrefix(req.ProviderID, platform), "."),
			Platform: platform,
			Name:     preset.Name,
			BaseURL:  config.PlatformBaseURL(platform), // 与采集器同源（含测试用上游覆盖）
			Paths:    preset.Paths,
		}
	}
	if len(probe.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "该平台没有可用 path")
		return
	}
	if req.Token != "" {
		probe.Token = req.Token
		probe.DecryptFailed = false
		logx.RegisterSecret(req.Token) // 脱敏红线兜底
	}
	if probe.Token == "" {
		if probe.DecryptFailed {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "已保存的 token 无法解密，请重新录入后再测试")
			return
		}
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "请先填写 API Key 再测试")
		return
	}

	t0 := time.Now()
	res := s.client.FetchOnce(r.Context(), &probe, probe.Paths[0])
	latency := time.Since(t0).Milliseconds()
	if res.Class != collector.ClassOK {
		msg := res.Message
		if res.Summary != "" && res.Summary != res.Message {
			msg = res.Summary
		}
		if res.Status > 0 && !strings.HasPrefix(msg, "HTTP ") {
			msg = "HTTP " + strconv.Itoa(res.Status) + " " + msg
		}
		// 不透传上游响应头，token 及其派生值永不回显
		writeErr(w, http.StatusBadGateway, "UPSTREAM_ERROR", msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency})
}

// clientIP 返回请求来源 IP（仅日志展示用）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// readBody 读取请求体并限长。
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, errTooLarge
	}
	return data, nil
}

var errTooLarge = errStr("body too large")

type errStr string

func (e errStr) Error() string { return string(e) }
