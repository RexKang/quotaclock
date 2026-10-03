package collector

import (
	"encoding/json"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/logx"
	"github.com/RexKang/quotaclock/internal/persist"
)

// CacheWriter 缓存落盘出口（persist.CacheWriter 实现）。nil = 不启用缓存。
type CacheWriter interface {
	Save(c *persist.Cache)
}

// Sampler 趋势采样出口（v0.4.0，trend.Store 实现）。nil = 不记录趋势。
// 只在**真实采集成功**时投递：缓存恢复的数据（cached）不是新样本，
// 失败态更不能写 0——「没采到」和「用了 0%」在趋势图上必须是两件事。
type Sampler interface {
	Observe(id, name, platform string, at time.Time, data json.RawMessage)
}

// CacheFromStates 把当前状态集转成缓存内容（v0.2.4）：
// 仅收 ok/cached 且有数据的平台——失败态不覆写上次成功数据，故不入档；
// 已删除/已停用的平台不在 states 中（或数据为空），缓存因此自然收敛，无需单独清理。
func CacheFromStates(now time.Time, states []ProviderState) *persist.Cache {
	c := &persist.Cache{
		Version:   persist.CacheVersion,
		WrittenAt: FormatTime(now),
		Providers: []persist.CacheEntry{},
	}
	for _, st := range states {
		if st.Data == nil {
			continue
		}
		if st.Status != StatusOK && st.Status != StatusCached {
			continue
		}
		if st.LastSuccessAt == nil {
			continue
		}
		c.Providers = append(c.Providers, persist.CacheEntry{
			ID:            st.ID,
			Name:          st.Name,
			Data:          st.Data,
			LastSuccessAt: FormatTime(*st.LastSuccessAt),
		})
	}
	return c
}

// RestoreFromCache 用缓存填充启动态（v0.2.4）：只恢复「当前配置里仍有、已启用、token 可解密」的平台；
// 命中项标 StatusCached 并保留上次成功时间——前端据此显示「缓存数据」而非冒充实时。
// 缓存里已消失的配置项（删平台/停用）天然被过滤掉。
func RestoreFromCache(c *persist.Cache, cfg *config.Runtime) []ProviderState {
	if c == nil || cfg == nil {
		return nil
	}
	out := make([]ProviderState, 0, len(c.Providers))
	for _, e := range c.Providers {
		p := cfg.Provider(e.ID)
		if p == nil || !p.IsEnabled() || p.DecryptFailed {
			continue
		}
		if len(e.Data) == 0 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.LastSuccessAt)
		if err != nil {
			logx.Warnf("缓存项 %s 的时间戳无法解析（%q），跳过恢复", e.ID, e.LastSuccessAt)
			continue
		}
		// 保留原时区（v0.2.5：时间戳按当地时区展示；此处若强制 UTC，重启后同一次成功
		// 采集的时间会从「19:35:52+08:00」变成「11:35:52Z」，与实时态不一致）
		at := ts
		name := e.Name
		if name == "" {
			name = p.Name
		}
		out = append(out, ProviderState{
			ID:            e.ID,
			Name:          name,
			Status:        StatusCached,
			Data:          e.Data,
			LastSuccessAt: &at,
		})
	}
	return out
}
