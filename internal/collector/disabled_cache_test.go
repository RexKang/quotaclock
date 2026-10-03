package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/persist"
)

// ---------- v0.2.4 ① enabled 回归 ----------

// TestDisabledSkipAndToggle：停用平台不进任务集合、状态为 disabled；
// 勾选切换（热生效）后状态立刻重建——依赖 sameDefinition 比较 enabled。
func TestDisabledSkipAndToggle(t *testing.T) {
	s := &Scheduler{
		defs:     map[string]*config.RuntimeProvider{},
		states:   map[string]*ProviderState{},
		backoffs: map[string]*Backoff{},
		store:    NewStore("t"),
		clock:    time.Now,
		randFn:   func(n int) int { return 0 },
	}
	provs := func(off bool) []*config.RuntimeProvider {
		e := config.BoolPtr(!off)
		return []*config.RuntimeProvider{
			{ID: "on", Name: "ON", Paths: []string{"/"}},
			{ID: "off", Name: "OFF", Paths: []string{"/"}, Enabled: e},
		}
	}
	cfg := mkRuntime(provs(true)...)
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)

	if st := s.states["off"]; st.Status != StatusDisabled || st.Error == nil || st.Error.Code != CodeDisabled {
		t.Fatalf("停用平台状态应为 disabled/DISABLED: %+v", st)
	}
	if st := s.states["on"]; st.Status != StatusFailed || st.Error.Code != CodeNotCollectedYet {
		t.Fatalf("启用平台应处于未采集态: %+v", st)
	}
	if jobs := s.planTickLocked(cfg); len(jobs) != 1 || jobs[0].p.ID != "on" {
		t.Fatalf("停用平台不应参与采集: %+v", jobs)
	}

	// 勾选启用 → 立刻回到未采集态（可采集）
	cfg2 := mkRuntime(provs(false)...)
	s.cfg.Store(cfg2)
	s.reloadLocked(cfg2)
	if st := s.states["off"]; st.Status != StatusFailed || st.Error.Code != CodeNotCollectedYet {
		t.Fatalf("启用后应重建为未采集态: %+v", st)
	}
	if jobs := s.planTickLocked(cfg2); len(jobs) != 2 {
		t.Fatalf("启用后应参与采集: %+v", jobs)
	}

	// 取消勾选 → 立刻变 disabled（同一 token/paths，仅 enabled 变）
	cfg3 := mkRuntime(provs(true)...)
	s.cfg.Store(cfg3)
	s.reloadLocked(cfg3)
	if st := s.states["off"]; st.Status != StatusDisabled {
		t.Fatalf("取消启用后应立即显示已停用: %+v", st)
	}
}

// TestDisabledProviderNotCollected：停用平台整轮不发任何请求，且快照里可见 disabled 态。
func TestDisabledProviderNotCollected(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	s, cfg := newTestScheduler(t, &config.RuntimeProvider{
		ID: "off", Name: "OFF", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer,
		Enabled: config.BoolPtr(false),
	})
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runTick(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("停用平台不应发请求，实际 %d 次", n)
	}
	snap := s.store.Get()
	if len(snap.Providers) != 1 || snap.Providers[0].Status != StatusDisabled {
		t.Fatalf("快照应含 disabled 平台: %+v", snap.Providers)
	}
	if st := s.states[cfg.Providers[0].ID]; st.Error == nil || st.Error.Code != CodeDisabled {
		t.Fatalf("错误码应为 DISABLED: %+v", st)
	}
}

// ---------- v0.2.4 ② 缓存 ----------

// TestRestoreFromCacheFilters：只恢复「配置里仍有 + 已启用 + 未解密失败 + 时间戳可解析 + 有数据」的项。
func TestRestoreFromCacheFilters(t *testing.T) {
	at := time.Date(2026, 9, 12, 10, 30, 0, 0, time.UTC)
	c := &persist.Cache{
		Version:   persist.CacheVersion,
		WrittenAt: FormatTime(time.Now()),
		Providers: []persist.CacheEntry{
			{ID: "a", Name: "缓存名", Data: json.RawMessage(`{"v":1}`), LastSuccessAt: FormatTime(at)},
			{ID: "gone", Data: json.RawMessage(`{"v":2}`), LastSuccessAt: FormatTime(at)},
			{ID: "off", Data: json.RawMessage(`{"v":3}`), LastSuccessAt: FormatTime(at)},
			{ID: "dec", Data: json.RawMessage(`{"v":4}`), LastSuccessAt: FormatTime(at)},
			{ID: "badtime", Data: json.RawMessage(`{"v":5}`), LastSuccessAt: "2026/09/12 10:30"},
			{ID: "nodata", Data: nil, LastSuccessAt: FormatTime(at)},
		},
	}
	cfg := mkRuntime(
		&config.RuntimeProvider{ID: "a", Name: "A", Paths: []string{"/"}},
		&config.RuntimeProvider{ID: "off", Name: "OFF", Paths: []string{"/"}, Enabled: config.BoolPtr(false)},
		&config.RuntimeProvider{ID: "dec", Name: "DEC", Paths: []string{"/"}, DecryptFailed: true},
		&config.RuntimeProvider{ID: "badtime", Name: "BAD", Paths: []string{"/"}},
		&config.RuntimeProvider{ID: "nodata", Name: "ND", Paths: []string{"/"}},
	)
	got := RestoreFromCache(c, cfg)
	if len(got) != 1 {
		t.Fatalf("应只恢复 a，实际 %+v", got)
	}
	st := got[0]
	if st.ID != "a" || st.Status != StatusCached || st.Error != nil {
		t.Fatalf("恢复态错误: %+v", st)
	}
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(at) {
		t.Fatalf("上次成功时间应保留: %v", st.LastSuccessAt)
	}
	if string(st.Data) != `{"v":1}` {
		t.Fatalf("数据应原样恢复: %s", st.Data)
	}
	if got := RestoreFromCache(nil, cfg); got != nil {
		t.Fatalf("无缓存应返回 nil: %+v", got)
	}
}

// TestCacheFromStatesFilters：只收本次状态集里「ok/cached + 有数据 + 有成功时间」的平台。
func TestCacheFromStatesFilters(t *testing.T) {
	now := time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC)
	prev := now.Add(-5 * time.Minute)
	states := []ProviderState{
		{ID: "ok", Name: "OK", Status: StatusOK, Data: json.RawMessage(`{"v":1}`), LastSuccessAt: &now},
		{ID: "cached", Name: "C", Status: StatusCached, Data: json.RawMessage(`{"v":2}`), LastSuccessAt: &prev},
		{ID: "failed", Name: "F", Status: StatusFailed, Data: json.RawMessage(`{"v":3}`), LastSuccessAt: &prev}, // 失败态不入档
		{ID: "disabled", Name: "D", Status: StatusDisabled},
		{ID: "nodata", Name: "N", Status: StatusOK},
		{ID: "notime", Name: "T", Status: StatusOK, Data: json.RawMessage(`{"v":4}`)},
	}
	c := CacheFromStates(now, states)
	if c.Version != persist.CacheVersion || c.WrittenAt != FormatTime(now) {
		t.Fatalf("缓存头错误: %+v", c)
	}
	ids := []string{}
	for _, e := range c.Providers {
		ids = append(ids, e.ID)
	}
	if len(c.Providers) != 2 || ids[0] != "ok" || ids[1] != "cached" {
		t.Fatalf("入档集合错误: %v", ids)
	}
	if c.Providers[1].LastSuccessAt != FormatTime(prev) {
		t.Fatalf("上次成功时间错误: %s", c.Providers[1].LastSuccessAt)
	}
}

// fakeCacheWriter 记录落盘调用（不碰磁盘）。
type fakeCacheWriter struct {
	mu    sync.Mutex
	calls []*persist.Cache
}

func (f *fakeCacheWriter) Save(c *persist.Cache) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeCacheWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestSchedulerCachesOnSuccessOnly：真实成功才投递缓存；失败复用上次数据但不重写缓存。
func TestSchedulerCachesOnSuccessOnly(t *testing.T) {
	var behavior atomic.Value // "ok" | "fail"
	behavior.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if behavior.Load().(string) == "ok" {
			_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":42}}`)
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	s, cfg := newTestScheduler(t, &config.RuntimeProvider{
		ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/q"}, AuthStyle: config.AuthStyleBearer,
	})
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	fw := &fakeCacheWriter{}
	s.SetCacheWriter(fw)

	s.collectProvider(context.Background(), cfg.Providers[0])
	if fw.count() != 1 {
		t.Fatalf("成功应投递一次缓存，实际 %d", fw.count())
	}
	c := fw.calls[0]
	if len(c.Providers) != 1 || c.Providers[0].ID != "a" || !strings.Contains(string(c.Providers[0].Data), "percentage") {
		t.Fatalf("缓存内容错误: %+v", c.Providers)
	}

	behavior.Store("fail")
	s.collectProvider(context.Background(), cfg.Providers[0])
	if fw.count() != 1 {
		t.Fatalf("失败不应重写缓存，实际 %d", fw.count())
	}
}

// TestSeedCached：缓存填入未采集态并发布快照；不覆盖真实结果；配置已删的 id 不恢复。
func TestSeedCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":7}}`)
	}))
	defer srv.Close()

	s, cfg := newTestScheduler(t, &config.RuntimeProvider{
		ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/q"}, AuthStyle: config.AuthStyleBearer,
	})
	at := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	cached := []ProviderState{
		{ID: "a", Name: "A", Status: StatusCached, Data: json.RawMessage(`{"v":9}`), LastSuccessAt: &at},
		{ID: "ghost", Name: "G", Status: StatusCached, Data: json.RawMessage(`{"v":8}`), LastSuccessAt: &at},
	}
	if n := s.SeedCached(cached); n != 1 {
		t.Fatalf("应只恢复配置中存在的 a，实际 %d", n)
	}
	if st := s.states["a"]; st.Status != StatusCached || string(st.Data) != `{"v":9}` {
		t.Fatalf("缓存态未生效: %+v", st)
	}
	if snap := s.store.Get(); len(snap.Providers) != 1 || snap.Providers[0].Status != StatusCached {
		t.Fatalf("恢复后应发布快照: %+v", snap.Providers)
	}

	// 真实采集成功后再 Seed：不得覆盖
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Providers[0])
	if st := s.states["a"]; st.Status != StatusOK {
		t.Fatalf("采集后应为 ok: %+v", st)
	}
	if n := s.SeedCached(cached); n != 0 {
		t.Fatalf("已有真实结果时不应再填缓存，实际 %d", n)
	}
	if st := s.states["a"]; st.Status != StatusOK || !strings.Contains(string(st.Data), "percentage") {
		t.Fatalf("缓存覆盖了真实结果: %+v", st)
	}
}
