package collector

import (
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/persist"
)

// TestRestoreFromCacheKeepsZone（v0.2.5 回归）：缓存恢复不得改写时区。
// 历史缺陷：RestoreFromCache 强制 ts.UTC()，导致同一次成功采集在重启前显示
// 「2026-09-12T19:35:52+08:00」、重启后变成「2026-09-12T11:35:52Z」。
func TestRestoreFromCacheKeepsZone(t *testing.T) {
	local := "2026-09-12T19:35:52+08:00"
	cache := &persist.Cache{
		Version: persist.CacheVersion,
		Providers: []persist.CacheEntry{{
			ID: "opencode.k1", Name: "OpenCode · K1",
			Data:          []byte(`{"percentage":42}`),
			LastSuccessAt: local,
		}},
	}
	cfg := &config.Runtime{Providers: []*config.RuntimeProvider{{
		ID: "opencode.k1", Platform: "opencode", Name: "OpenCode · K1", KeyCount: 1,
	}}}
	states := RestoreFromCache(cache, cfg)
	if len(states) != 1 || states[0].LastSuccessAt == nil {
		t.Fatalf("应恢复 1 条: %+v", states)
	}
	if got := FormatTime(*states[0].LastSuccessAt); got != local {
		t.Fatalf("恢复后时间戳被改写: %s（期望 %s）", got, local)
	}
	// 与「实时采集后写缓存」的形态一致：写出去再读回来仍是同一串
	if got := FormatTime(*states[0].LastSuccessAt); got != FormatTime(states[0].LastSuccessAt.In(time.FixedZone("CST", 8*3600))) {
		t.Fatalf("同一瞬间的本地表示不一致: %s", got)
	}
}

// TestDisabledWinsOverDecryptFailed（v0.2.5 回归）：停用是用户意图，优先于「token 无法解密」。
// 场景：key.bin 换了 + 某条凭据已停用 —— 该凭据应显示「已停用」，而不是被解密失败盖成「token 失效」。
func TestDisabledWinsOverDecryptFailed(t *testing.T) {
	off := false
	cfg := mkRuntime(
		&config.RuntimeProvider{ID: "opencode.k1", Platform: "opencode", Name: "OpenCode · K1",
			KeyCount: 2, KeyIndex: 0, Enabled: &off, DecryptFailed: true},
		&config.RuntimeProvider{ID: "opencode.k2", Platform: "opencode", Name: "OpenCode · K2",
			KeyCount: 2, KeyIndex: 1, DecryptFailed: true},
	)
	s := NewScheduler(cfg, NewStore("t"), nil)
	s.reloadLocked(cfg)

	if st := s.states["opencode.k1"]; st.Status != StatusDisabled || st.Error.Code != CodeDisabled {
		t.Fatalf("停用 + 解密失败应以停用为准: %+v", st)
	}
	if st := s.states["opencode.k2"]; st.Status != StatusTokenInvalid || st.Error.Code != CodeTokenDecryptFail {
		t.Fatalf("启用 + 解密失败应为 token_invalid: %+v", st)
	}
}
