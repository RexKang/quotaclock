package config

import (
	"github.com/RexKang/quotaclock/internal/crypto"
)

// BuildRuntime 把落盘形态转为运行时视图：按平台预设展开 base_url/paths/auth_style，
// 每个 AccessKey 展开为一条 RuntimeProvider（扁平，便于采集器/快照/缓存复用既有模型），
// 逐 key 解密 token、生成掩码 hint，并标注同平台内的序号与总数（调度等分错峰用）。
// 解密失败不视为致命（key.bin 与密文不匹配的降级场景，设计 §6.4）：该 key 标记
// DecryptFailed，采集器将报 token_invalid / TOKEN_DECRYPT_FAILED。
func BuildRuntime(f *File, masterKey []byte) *Runtime {
	r := &Runtime{
		Version:      f.Version,
		Listen:       f.Listen,
		Collector:    f.Collector,
		Balance:      NormalizeBalance(f.Balance),
		AuthMode:     f.Auth.Mode,
		PasswordHash: []byte(f.Auth.PasswordHash),
	}
	for i := range f.Providers {
		fp := &f.Providers[i]
		preset, ok := PlatformByID(fp.Platform)
		if !ok {
			continue // 未知平台：校验阶段已拒绝；此处兜底跳过（不猜地址）
		}
		total := len(fp.AccessKeys)
		for j := range fp.AccessKeys {
			k := &fp.AccessKeys[j]
			p := &RuntimeProvider{
				ID:           RuntimeID(fp.Platform, k.ID),
				Platform:     fp.Platform,
				KeyID:        k.ID,
				KeyName:      k.Name,
				Name:         keyDisplayName(preset.Name, k.Name),
				BaseURL:      applyUpstreamOverride(preset.BaseURL),
				Paths:        append([]string(nil), preset.Paths...),
				AuthStyle:    preset.AuthStyle,
				ExtraHeaders: preset.ExtraHeaders,
				TokenCipher:  k.TokenCipher,
				Enabled:      CopyBoolPtr(k.Enabled),
				KeyIndex:     j,
				KeyCount:     total,
			}
			if p.AuthStyle == "" {
				p.AuthStyle = AuthStyleBearer
			}
			if p.TokenCipher != "" {
				plain, err := crypto.OpenToken(masterKey, p.TokenCipher)
				if err != nil {
					p.DecryptFailed = true
				} else {
					p.Token = plain
					p.TokenMasked = crypto.Mask(plain)
				}
			}
			r.Providers = append(r.Providers, p)
		}
	}
	return r
}

// keyDisplayName 凭据展示名："平台名 · 凭据名"；凭据名为空则只用平台名。
func keyDisplayName(platformName, keyName string) string {
	if keyName == "" {
		return platformName
	}
	if platformName == "" {
		return keyName
	}
	return platformName + " · " + keyName
}

// BuildView 构造 GET /api/config 脱敏视图。
// authenticated：当前请求是否持有效会话——仅登录者可见 token_masked（PRD §6.4）；
// passwordIsDefault：默认密码未修改检测的结果，始终如实返回（契约增量 #1）；
// masks：运行时 provider id（<platform>.<keyID>）→ 掩码 hint（由调用方在 token 加载/导入/PUT 时
// 从明文计算，设计 §6.5 D-5，避免每次 GET 解密；hint 不落盘，明文/密文/掩码均不落日志）。
func BuildView(f *File, masks map[string]string, authenticated, passwordIsDefault bool) *View {
	v := &View{
		Version:   f.Version,
		Listen:    f.Listen,
		Collector: f.Collector,
		Balance:   NormalizeBalance(f.Balance),
		Auth: ViewAuth{
			Mode:              f.Auth.Mode,
			PasswordIsDefault: passwordIsDefault,
			Authenticated:     authenticated,
		},
		// 空配置序列化为 [] 而非 null（前端按数组直用，nil 切片序列化成 null 会让
		// 「新环境添加平台」在 push 处崩溃）。
		Providers: []ViewProvider{},
		Platforms: []ViewPlatform{},
	}
	for _, preset := range Platforms() {
		v.Platforms = append(v.Platforms, ViewPlatform{
			ID:      preset.ID,
			Name:    preset.Name,
			BaseURL: applyUpstreamOverride(preset.BaseURL),
			Paths:   preset.Paths,
		})
	}
	for i := range f.Providers {
		fp := &f.Providers[i]
		preset, ok := PlatformByID(fp.Platform)
		if !ok {
			continue
		}
		vp := ViewProvider{
			Platform:     fp.Platform,
			PlatformName: preset.Name,
			BaseURL:      preset.BaseURL,
			Paths:        append([]string(nil), preset.Paths...),
			// 空平台也要序列化为 []（同 providers 的理由：前端直接 push）
			AccessKeys: []ViewAccessKey{},
		}
		for j := range fp.AccessKeys {
			k := &fp.AccessKeys[j]
			kk := ViewAccessKey{
				ID:       k.ID,
				Name:     k.Name,
				HasToken: k.TokenCipher != "",
				Enabled:  k.IsEnabled(),
			}
			if authenticated && kk.HasToken {
				kk.TokenMasked = masks[RuntimeID(fp.Platform, k.ID)]
			}
			vp.AccessKeys = append(vp.AccessKeys, kk)
		}
		v.Providers = append(v.Providers, vp)
	}
	return v
}
