package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
)

// TestRenameKeyKeepsCollectedState（v0.2.5 r4.1 回归）：只改 API Key 的名称不该重新采集。
// 走完整链路（真实采集 → 保存（Reload）→ 保存唤醒的额外 tick），因为这里有两层机制：
//  1. sameDefinition 曾把 Name 当定义字段 → 改名被判「定义变了」→ 状态重置成「等待采集」；
//  2. 成功态曾把 nextAttemptAt 清零 → 即使状态保留，保存唤醒的额外 tick 仍会重打一次上游。
func TestRenameKeyKeepsCollectedState(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"success":true,"data":{"total_balance":"88.5"}}`)
	}))
	defer srv.Close()

	mk := func(name string) *config.RuntimeProvider {
		return &config.RuntimeProvider{
			ID: "deepseek.k1", Platform: "deepseek", KeyID: "k1", Name: name,
			BaseURL: srv.URL, Paths: []string{"/user/balance"},
			AuthStyle: config.AuthStyleBearer, TokenCipher: "CIPHER-UNCHANGED",
			KeyIndex: 0, KeyCount: 1,
		}
	}
	cfg := mkRuntime(mk("DeepSeek · 默认"))
	cfg.Collector.IntervalBaseS = 300
	s := NewScheduler(cfg, NewStore("t"), nil)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}

	// 1) 真实采集一次成功
	s.collectProvider(context.Background(), cfg.Providers[0])
	s.mu.Lock()
	st := s.states["deepseek.k1"]
	s.mu.Unlock()
	if st.Status != StatusOK || st.LastSuccessAt == nil || hits.Load() != 1 {
		t.Fatalf("首次采集应成功（hits=%d）: %+v", hits.Load(), st)
	}
	onLast := FormatTime(*st.LastSuccessAt)

	// 2) 只改名称（token 密文、地址、路径、启用态都不变）后热生效
	s.Reload(mkRuntime(mk("DeepSeek · 备用")))

	s.mu.Lock()
	st = s.states["deepseek.k1"]
	if st == nil {
		s.mu.Unlock()
		t.Fatal("改名后状态丢失")
	}
	if st.Status != StatusOK || st.LastSuccessAt == nil || string(st.Data) == "" {
		s.mu.Unlock()
		t.Fatalf("改名把采集状态重置了（应保留 status/data/last_success_at）: %+v", st)
	}
	if got := FormatTime(*st.LastSuccessAt); got != onLast {
		s.mu.Unlock()
		t.Fatalf("改名改动了 last_success_at: %s（期望 %s）", got, onLast)
	}
	if st.Name != "DeepSeek · 备用" {
		s.mu.Unlock()
		t.Fatalf("展示名未同步到新名称: %q", st.Name)
	}
	if b := s.backoffs["deepseek.k1"]; b == nil {
		s.mu.Unlock()
		t.Fatal("退避记录丢失")
	}
	s.mu.Unlock()

	// 3) 保存唤醒的额外 tick 不得重采（采集周期 300s，刚采过）
	if jobs := s.planTickLocked(s.cfg.Load()); len(jobs) != 0 {
		t.Fatalf("改名后立刻重采了 %d 个 job（期望 0）", len(jobs))
	}
	if hits.Load() != 1 {
		t.Fatalf("改名后上游被多打了一次: hits=%d", hits.Load())
	}

	// 4) 快照带新名称且 revision 前进（前端 ≤1 轮询周期内换名，PRD r2.3 ⑤）
	s.mu.Lock()
	s.publishLocked()
	s.mu.Unlock()
	snap := s.store.Get()
	if len(snap.Providers) != 1 || snap.Providers[0].Name != "DeepSeek · 备用" {
		t.Fatalf("快照未反映新名称: %+v", snap.Providers)
	}
}

// TestSuccessAnchorsNextAttempt：成功后续按周期排期（而不是清零），
// 这是「保存配置不打断采集节奏」的前提；未采集过的 key 仍立即采集。
func TestSuccessAnchorsNextAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":7}}`)
	}))
	defer srv.Close()

	cfg := mkRuntime(&config.RuntimeProvider{
		ID: "a", Platform: "opencode", Name: "A", BaseURL: srv.URL, Paths: []string{"/q"},
		AuthStyle: config.AuthStyleBearer, KeyCount: 1,
	})
	cfg.Collector.IntervalBaseS = 300
	s := NewScheduler(cfg, NewStore("t"), nil)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}

	// 未采集过：立即调度
	if jobs := s.planTickLocked(cfg); len(jobs) != 1 {
		t.Fatalf("未采集过的 key 应立即调度，实际 %d 个 job", len(jobs))
	}
	s.collectProvider(context.Background(), cfg.Providers[0])
	s.mu.Lock()
	b := s.backoffs["a"]
	next := b.NextAttemptAt()
	s.mu.Unlock()
	d := time.Until(next)
	if d < 290*time.Second || d > 300*time.Second {
		t.Fatalf("成功后应排到 ~300s 之后，实际 %v", d)
	}
	// 周期内不重复采集
	if jobs := s.planTickLocked(cfg); len(jobs) != 0 {
		t.Fatalf("周期内不应再调度，实际 %d 个 job", len(jobs))
	}
}

// TestTokenChangeResetsState：对照组——换 token（密文变）仍必须重置，
// 否则新 token 会沿用旧数据的 last_success_at 与退避。
func TestTokenChangeResetsState(t *testing.T) {
	mk := func(cipher string) *config.RuntimeProvider {
		return &config.RuntimeProvider{
			ID: "deepseek.k1", Platform: "deepseek", KeyID: "k1", Name: "DeepSeek · 默认",
			BaseURL: "https://example.invalid", Paths: []string{"/user/balance"},
			AuthStyle: config.AuthStyleBearer, TokenCipher: cipher, KeyCount: 1,
		}
	}
	s := NewScheduler(mkRuntime(mk("CIPHER-OLD")), NewStore("t"), nil)
	lastOK := time.Now().Add(-10 * time.Second)
	s.mu.Lock()
	s.states["deepseek.k1"] = &ProviderState{ID: "deepseek.k1", Name: "DeepSeek · 默认",
		Status: StatusOK, Data: []byte(`{"v":1}`), LastSuccessAt: &lastOK}
	s.mu.Unlock()

	s.Reload(mkRuntime(mk("CIPHER-NEW")))

	st := s.states["deepseek.k1"]
	if st.Status != StatusFailed || st.Error == nil || st.Error.Code != CodeNotCollectedYet {
		t.Fatalf("换 token 应重置为等待采集: %+v", st)
	}
	if jobs := s.planTickLocked(s.cfg.Load()); len(jobs) != 1 {
		t.Fatalf("换 token 后应立刻重采，实际调度 %d 个 job", len(jobs))
	}
}

// TestMultiKeyKeepsPerTickCadence：同平台多 key 的节奏不能被这道闸门打折。
// 复刻真实时序：k1 在 tick 起点采、k2 在等分偏移 base/N 处采；下一轮 tick（base+抖动）到来时
// 两个 key 都必须可调度——若闸门周期取 interval_base_s，k2 会被整轮跳过、采集频率减半。
func TestMultiKeyKeepsPerTickCadence(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":7}}`)
	}))
	defer srv.Close()

	mk := func(idx int) *config.RuntimeProvider {
		return &config.RuntimeProvider{
			ID: "opencode.k" + string(rune('1'+idx)), Platform: "opencode",
			KeyID: "k" + string(rune('1'+idx)), Name: "OpenCode · K",
			BaseURL: srv.URL, Paths: []string{"/usage"}, AuthStyle: config.AuthStyleBearer,
			KeyIndex: idx, KeyCount: 2,
		}
	}
	cfg := mkRuntime(mk(0), mk(1)) // interval_base_s = 300（mkRuntime 默认）
	s := NewScheduler(cfg, NewStore("t"), nil)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}

	base := time.Now()
	s.clock = func() time.Time { return base }
	s.collectProvider(context.Background(), cfg.Providers[0])
	s.clock = func() time.Time { return base.Add(150 * time.Second) } // k2 的等分偏移
	s.collectProvider(context.Background(), cfg.Providers[1])

	s.clock = func() time.Time { return base.Add(305 * time.Second) } // 下一轮 tick（300 + 5s 抖动）
	if jobs := s.planTickLocked(s.cfg.Load()); len(jobs) != 2 {
		t.Fatalf("同平台第 2 个 key 被跳过（采集频率会减半）: 调度 %d 个 job（期望 2）", len(jobs))
	}
	// 同一个 tick 内不该重复采（闸门只是防同一周期内多采）
	if jobs := s.planTickLocked(s.cfg.Load()); len(jobs) != 2 {
		t.Fatalf("重复 plan 结果不一致: %d", len(jobs))
	}
}

// TestReloadWithoutChangeKeepsSchedule：保存一份没改动的配置也不得打断节奏
// （用户在设置里点保存、或只挪动别处的字段时不该产生额外请求）。
func TestReloadWithoutChangeKeepsSchedule(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":7}}`)
	}))
	defer srv.Close()

	mk := func() *config.RuntimeProvider {
		return &config.RuntimeProvider{
			ID: "kimi-code.k1", Platform: "kimi-code", KeyID: "k1", Name: "Kimi Code · 默认",
			BaseURL: srv.URL, Paths: []string{"/usages"}, AuthStyle: config.AuthStyleBearer,
			TokenCipher: "CIPHER-SAME", KeyCount: 1,
		}
	}
	cfg := mkRuntime(mk())
	cfg.Collector.IntervalBaseS = 300
	s := NewScheduler(cfg, NewStore("t"), nil)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Providers[0])

	s.Reload(mkRuntime(mk())) // 内容完全相同
	if jobs := s.planTickLocked(s.cfg.Load()); len(jobs) != 0 {
		t.Fatalf("无改动保存后不应重采，实际 %d 个 job", len(jobs))
	}
	if hits.Load() != 1 {
		t.Fatalf("无改动保存后上游被多打了一次: hits=%d", hits.Load())
	}
}
