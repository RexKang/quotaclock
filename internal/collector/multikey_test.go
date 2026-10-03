package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
)

// ---------- v0.2.5 双层调度：同平台多 key 等分错峰 ----------

// mkKeys 构造同平台 N 个 key 的运行时视图（KeyIndex 0..N-1，KeyCount=N）。
func mkKeys(platform string, n int) []*config.RuntimeProvider {
	out := make([]*config.RuntimeProvider, 0, n)
	for i := 0; i < n; i++ {
		id := config.RuntimeID(platform, fmt.Sprintf("k%d", i+1))
		out = append(out, &config.RuntimeProvider{
			ID: id, Platform: platform, KeyID: fmt.Sprintf("k%d", i+1), Name: id,
			BaseURL: "https://example.invalid", Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer,
			KeyIndex: i, KeyCount: n,
		})
	}
	return out
}

func newTickScheduler(provs ...*config.RuntimeProvider) (*Scheduler, *config.Runtime) {
	cfg := mkRuntime(provs...)
	cfg.Collector = *rtConfig(300, 0, 0) // 不叠加跨平台错峰，便于断言等分偏移
	s := NewScheduler(cfg, NewStore("t"), nil)
	return s, cfg
}

// TestSamePlatformKeysEqualSplit：同平台 2 个 key → 偏移 0 与 150s（300/2）；
// 3 个 key → 0/100/200s；单 key → 0。
func TestSamePlatformKeysEqualSplit(t *testing.T) {
	cases := []struct {
		n    int
		want []time.Duration
	}{
		{1, []time.Duration{0}},
		{2, []time.Duration{0, 150 * time.Second}},
		{3, []time.Duration{0, 100 * time.Second, 200 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d个key", tc.n), func(t *testing.T) {
			s, cfg := newTickScheduler(mkKeys("opencode", tc.n)...)
			jobs := s.planTickLocked(cfg)
			if len(jobs) != tc.n {
				t.Fatalf("应调度 %d 个 job，实际 %d", tc.n, len(jobs))
			}
			for i, j := range jobs {
				if j.delay != tc.want[i] {
					t.Fatalf("第 %d 个 key 偏移 = %v，期望 %v", i+1, j.delay, tc.want[i])
				}
			}
		})
	}
}

// TestMultiPlatformIndependentSplit：不同平台各自等分，互不干扰。
func TestMultiPlatformIndependentSplit(t *testing.T) {
	var provs []*config.RuntimeProvider
	provs = append(provs, mkKeys("opencode", 3)...) // 300/3 = 100s
	provs = append(provs, mkKeys("deepseek", 2)...) // 300/2 = 150s
	s, cfg := newTickScheduler(provs...)
	jobs := s.planTickLocked(cfg)
	if len(jobs) != 5 {
		t.Fatalf("应调度 5 个 job，实际 %d", len(jobs))
	}
	got := map[string]time.Duration{}
	for _, j := range jobs {
		got[j.p.ID] = j.delay
	}
	want := map[string]time.Duration{
		"opencode.k1": 0, "opencode.k2": 100 * time.Second, "opencode.k3": 200 * time.Second,
		"deepseek.k1": 0, "deepseek.k2": 150 * time.Second,
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s 偏移 = %v，期望 %v（全量 %v）", id, got[id], w, got)
		}
	}
}

// TestSamePlatformSerialized：同平台请求不并发——慢 key 在途时，同平台另一个 key 的采集被避让。
func TestSamePlatformSerialized(t *testing.T) {
	var concurrent, maxConcurrent int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
				break
			}
		}
		<-gate // 卡住第一个请求，让第二个 key 的窗口重叠
		atomic.AddInt32(&concurrent, -1)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	provs := mkKeys("opencode", 2)
	provs[0].BaseURL = srv.URL
	provs[0].Paths = []string{"/"}
	provs[1].BaseURL = srv.URL
	provs[1].Paths = []string{"/"}
	s, cfg := newTickScheduler(provs...)
	// 人为让两个 key 的偏移相同（都为 0）→ 同时进入就绪，验证串行化守卫
	for _, p := range cfg.Providers {
		p.KeyIndex = 0
	}
	s.client = &Client{HTTP: srv.Client(), Version: "t"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.runTick(ctx); close(done) }()
	time.Sleep(400 * time.Millisecond) // 两个 job 都已到点
	close(gate)
	<-done

	if got := atomic.LoadInt32(&maxConcurrent); got > 1 {
		t.Fatalf("同平台并发采集 = %d，应串行（≤1）", got)
	}
}

// ---------- v0.2.5 双层退避 ----------

// TestPlatformLevelBackoffStopsAllKeys：5xx（平台异常）→ 该平台全部 key 在退避期内都不再调度，
// 且平台级退避基数为 interval_base_s（300s）。
func TestPlatformLevelBackoffStopsAllKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	provs := mkKeys("opencode", 2)
	for _, p := range provs {
		p.BaseURL = srv.URL
		p.Paths = []string{"/"}
	}
	// 另一个平台作为对照：必须不受影响
	other := mkKeys("deepseek", 1)[0]
	other.BaseURL = srv.URL
	s, cfg := newTickScheduler(append(provs, other)...)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Provider("opencode.k1"))

	st := s.states["opencode.k1"]
	if st.Status != StatusFailed || st.Error.Code != CodeServerError {
		t.Fatalf("状态应为 failed/SERVER_ERROR: %+v", st)
	}
	if rs := st.Error.RetryAfterS; rs == nil || *rs != 300 {
		t.Fatalf("平台级退避基数应为 interval_base_s=300，实际 %v", rs)
	}
	// 同平台另一个 key 也应被平台级退避挡住；对照平台照常调度
	jobs := s.planTickLocked(cfg)
	for _, j := range jobs {
		if j.p.Platform == "opencode" {
			t.Fatalf("平台级退避期内同平台 key 不应调度: %s", j.p.ID)
		}
	}
	if len(jobs) != 1 || jobs[0].p.Platform != "deepseek" {
		t.Fatalf("对照平台应照常调度: %+v", jobs)
	}
}

// TestKeyLevelBackoffUsesPeriod：429（key 级）→ 只停该 key，退避基数 = 300/2 = 150s；
// 同平台另一个 key 不受影响。
func TestKeyLevelBackoffUsesPeriod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429) // 无 Retry-After → 用当前退避值（= 等分周期）
	}))
	defer srv.Close()

	provs := mkKeys("opencode", 2)
	for _, p := range provs {
		p.BaseURL = srv.URL
		p.Paths = []string{"/"}
	}
	s, cfg := newTickScheduler(provs...)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Provider("opencode.k1"))

	st := s.states["opencode.k1"]
	if st.Error == nil || st.Error.Code != CodeRateLimited {
		t.Fatalf("应为 RATE_LIMITED: %+v", st)
	}
	if rs := st.Error.RetryAfterS; rs == nil || *rs != 150 {
		t.Fatalf("key 级退避基数应为 150（300/2），实际 %v", rs)
	}
	jobs := s.planTickLocked(cfg)
	if len(jobs) != 1 || jobs[0].p.ID != "opencode.k2" {
		t.Fatalf("仅被限流的 key 应停调度，另一个 key 照常: %+v", jobs)
	}
}

// TestSingleKeyBackoffKeepsBase：单 key 平台的退避基数仍为 interval_base_s（行为与旧版一致）。
func TestSingleKeyBackoffKeepsBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	p := mkKeys("deepseek", 1)[0]
	p.BaseURL = srv.URL
	p.Paths = []string{"/"}
	s, cfg := newTickScheduler(p)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Provider(p.ID))
	if rs := s.states[p.ID].Error.RetryAfterS; rs == nil || *rs != 300 {
		t.Fatalf("单 key 平台退避基数应为 300，实际 %v", rs)
	}
}

// TestPlatformRecoveryClearsBackoff：平台级退避后任一 key 成功 → 平台退避复位（同平台 key 恢复调度）。
func TestPlatformRecoveryClearsBackoff(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	provs := mkKeys("opencode", 2)
	for _, p := range provs {
		p.BaseURL = srv.URL
		p.Paths = []string{"/"}
	}
	s, cfg := newTickScheduler(provs...)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	s.collectProvider(context.Background(), cfg.Provider("opencode.k1")) // 平台级失败
	if jobs := s.planTickLocked(cfg); len(jobs) != 0 {
		t.Fatalf("平台退避期内不应有任务: %+v", jobs)
	}
	healthy.Store(true)
	// 人为清掉平台退避的到期时间（等价于退避到期），再由另一 key 成功
	s.platformBackoffs["opencode"].OnSuccess()
	s.collectProvider(context.Background(), cfg.Provider("opencode.k2"))
	if jobs := s.planTickLocked(cfg); len(jobs) == 0 {
		t.Fatal("平台恢复后应恢复调度")
	}
}

// ---------- v0.2.5 本地时间 ----------

// TestFormatTimeLocalZone：时间戳按服务端当地时区输出（不再强制 UTC / Z 结尾）。
func TestFormatTimeLocalZone(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 9, 12, 18, 20, 0, 0, loc)
	got := FormatTime(at)
	if got != "2026-09-12T18:20:00+08:00" {
		t.Fatalf("应按当地时区输出：%s", got)
	}
	// 同一时刻的 UTC 表示在各自时区下都指向同一瞬间（可解析）
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("应为合法 RFC3339: %v", err)
	}
	if tz, _ := time.Now().Zone(); tz == "" {
		t.Fatal("本地时区应可解析")
	}
}

// TestConcurrentAcrossPlatformsNotSerial：跨平台仍并发（同平台才串行）。
func TestConcurrentAcrossPlatformsNotSerial(t *testing.T) {
	var mu sync.Mutex
	starts := map[string]time.Time{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts[r.URL.Path] = time.Now()
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	var provs []*config.RuntimeProvider
	for _, platform := range []string{"opencode", "deepseek", "zhipu-glm", "kimi-code"} {
		p := mkKeys(platform, 1)[0]
		p.BaseURL = srv.URL
		p.Paths = []string{"/" + platform}
		provs = append(provs, p)
	}
	s, cfg := newTickScheduler(provs...)
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)
	s.client = &Client{HTTP: srv.Client(), Version: "t"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runTick(ctx)
	if len(starts) != 4 {
		t.Fatalf("四个平台都应发出请求，实际 %d: %v", len(starts), starts)
	}
	var min, max time.Time
	for _, ts := range starts {
		if min.IsZero() || ts.Before(min) {
			min = ts
		}
		if ts.After(max) {
			max = ts
		}
	}
	if max.Sub(min) > 150*time.Millisecond {
		t.Fatalf("跨平台应几乎同时（错峰 0）：跨度 %v", max.Sub(min))
	}
}
