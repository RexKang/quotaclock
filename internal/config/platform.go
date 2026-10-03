package config

import (
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/RexKang/quotaclock/internal/logx"
)

// Platform 内置平台预设（v0.2.5）：base_url / paths / 鉴权方式 / 额外请求头全部由代码内置，
// 配置里只存「平台标识 + 访问凭据（access_keys）」。
//
// 为什么不让用户自建平台：各平台的返回结构需要专用解析逻辑（前端 extractQuotaItems 的
// 字段假设也按平台实测得来）。允许自填 base_url/paths 会让「配置值」与「解析假设」解耦，
// 地址一写错就是静默错配（接口通了、字段认不出来）。平台地址变化时改代码发版即可，
// 由代码保证「地址与解析」始终配对。
type Platform struct {
	ID           string
	Name         string
	BaseURL      string
	Paths        []string
	AuthStyle    string
	ExtraHeaders map[string]string
}

// platforms 平台清单（顺序 = 前端下拉顺序）。
//
// Paths 说明（按实测收敛为最小必要集合）：
//   - 智谱：单 path 返回周期额度（含 usageDetails 分模型用量）
//   - DeepSeek：单 path 返回账户余额
//   - Kimi Code：只需 /usages（周期额度）。v0.1 时代还打了 /me（账户信息，页面从未展示）
//     与 /models（当时用于「验证 key 有效性」），两者结果从未被渲染，却让每个 key 每轮多发
//     2 个请求——多 key 场景下是限流的放大器，v0.2.5 起不再采集
//   - OpenCode：官方用量接口（260907 切换，Bearer API Key）
var platforms = []Platform{
	{
		ID: "zhipu-glm", Name: "智谱 GLM",
		BaseURL: "https://open.bigmodel.cn",
		Paths:   []string{"/api/monitor/usage/quota/limit"},
	},
	{
		ID: "deepseek", Name: "DeepSeek",
		BaseURL: "https://api.deepseek.com",
		Paths:   []string{"/user/balance"},
	},
	{
		ID: "kimi-code", Name: "Kimi Code",
		BaseURL: "https://api.kimi.com/coding/v1",
		Paths:   []string{"/usages"},
	},
	{
		ID: "opencode", Name: "OpenCode",
		BaseURL: "https://opencode.ai/zen/go/v1",
		Paths:   []string{"/usage"},
	},
}

// PlatformByID 按标识取预设。
func PlatformByID(id string) (Platform, bool) {
	for _, p := range platforms {
		if p.ID == id {
			if p.AuthStyle == "" {
				p.AuthStyle = AuthStyleBearer
			}
			return p, true
		}
	}
	return Platform{}, false
}

// Platforms 返回全部预设（只读副本；调用方不得修改内部切片）。
func Platforms() []Platform {
	out := make([]Platform, 0, len(platforms))
	for _, p := range platforms {
		q := p
		q.Paths = append([]string(nil), p.Paths...)
		if q.AuthStyle == "" {
			q.AuthStyle = AuthStyleBearer
		}
		out = append(out, q)
	}
	return out
}

// IsKnownPlatform 标识是否在预设白名单内。
func IsKnownPlatform(id string) bool {
	_, ok := PlatformByID(id)
	return ok
}

// PlatformIDs 返回全部平台标识（错误提示与校验文案用）。
func PlatformIDs() []string {
	out := make([]string, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, p.ID)
	}
	return out
}

// PlatformBaseURL 返回平台预设的接口地址（含本地测试用上游覆盖）——
// 运行时构建与 /api/test 探针共用，保证「探针打哪、采集就打哪」。
func PlatformBaseURL(id string) string {
	p, ok := PlatformByID(id)
	if !ok {
		return ""
	}
	return applyUpstreamOverride(p.BaseURL)
}

// platformSignatures 迁移期的 base_url → 平台推断表（子串匹配，按特异性从高到低）。
var platformSignatures = []struct {
	contains string
	platform string
}{
	{"open.bigmodel.cn", "zhipu-glm"},
	{"api.deepseek.com", "deepseek"},
	{"api.kimi.com", "kimi-code"},
	{"moonshot.cn", "kimi-code"}, // 更名前的旧域名
	{"opencode.ai", "opencode"},
}

// InferPlatform 按 base_url 推断平台（v3/v0.1 配置迁移用）；无法判定返回 false。
func InferPlatform(baseURL string) (string, bool) {
	u := strings.ToLower(strings.TrimSpace(baseURL))
	for _, s := range platformSignatures {
		if strings.Contains(u, s.contains) {
			return s.platform, true
		}
	}
	return "", false
}

// ---------- 本地测试用上游覆盖（e2e / 集成测试打 mock 上游） ----------

// EnvUpstreamOverride 环境变量名：设成 http://127.0.0.1:PORT 时，
// 所有平台预设的 origin（scheme://host:port）被替换为该值，paths 与鉴权方式保持不变。
//
// 存在理由：v0.2.5 起平台地址由代码内置，配置里不再有 base_url，集成测试/e2e 无法再通过
// 配置把采集指向 mock 上游。此开关只影响「请求发往哪里」，不进入配置文件、不出现在界面。
// 生效时打印显眼 WARN（避免误留在正式实例上把 token 发往非官方地址）。
const EnvUpstreamOverride = "QUOTACLOCK_UPSTREAM_OVERRIDE"

var overrideOnce sync.Once

// UpstreamOrigin 返回生效中的上游覆盖 origin（未设置/非法时返回空串）。
// 每次调用都重新读环境变量（测试用 t.Setenv 逐个用例切换 mock 地址）；WARN 只打一次。
func UpstreamOrigin() string {
	raw := strings.TrimSpace(os.Getenv(EnvUpstreamOverride))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		overrideOnce.Do(func() {
			logx.Warnf("%s 取值非法（应为 http://host:port 形式）：%q，已忽略", EnvUpstreamOverride, raw)
		})
		return ""
	}
	origin := u.Scheme + "://" + u.Host
	overrideOnce.Do(func() {
		logx.Warnf("%s 已生效：所有平台请求将发往 %s（paths 不变）；该开关仅供本地测试，正式实例请勿设置",
			EnvUpstreamOverride, origin)
	})
	return origin
}

// applyUpstreamOverride 用覆盖值替换 base 的 scheme://host（保留 base 自身的 path 前缀）。
func applyUpstreamOverride(base string) string {
	origin := UpstreamOrigin()
	if origin == "" {
		return base
	}
	bu, err := url.Parse(base)
	if err != nil {
		return origin
	}
	ou, err := url.Parse(origin)
	if err != nil {
		return base
	}
	bu.Scheme = ou.Scheme
	bu.Host = ou.Host
	return strings.TrimRight(bu.String(), "/")
}
