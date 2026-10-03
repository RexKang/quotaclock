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
)

func rtConfig(interval, sMin, sMax int) *config.Collector {
	c := config.DefaultCollector()
	c.IntervalBaseS = interval
	c.StaggerMinS = sMin
	c.StaggerMaxS = sMax
	return &c
}

func mkRuntime(providers ...*config.RuntimeProvider) *config.Runtime {
	return &config.Runtime{
		Version: 3,
		Listen:  config.DefaultListen(),
		Collector: func() config.Collector {
			c := config.DefaultCollector()
			c.IntervalBaseS = 300
			return c
		}(),
		AuthMode:  config.AuthModeAdmin,
		Providers: providers,
	}
}

func TestPlanTickStagger(t *testing.T) { // C-col-05：第 k 个平台启动时刻 = 前 k-1 项 rand(stagger) 之和
	draws := []int{1, 5, 3} // 依次被消费（默认区间 [1,5]）
	i := 0
	s := &Scheduler{
		defs:     map[string]*config.RuntimeProvider{},
		states:   map[string]*ProviderState{},
		backoffs: map[string]*Backoff{},
		store:    NewStore("t"),
		clock:    time.Now,
		randFn: func(n int) int {
			d := draws[i%len(draws)]
			i++
			return d - 1 // [0, n) 注入 → +StaggerMin 还原
		},
	}
	provs := []*config.RuntimeProvider{
		{ID: "a", Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer},
		{ID: "b", Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer},
		{ID: "c", Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer},
	}
	cfg := mkRuntime(provs...)
	cfg.Collector = *rtConfig(300, 1, 5)
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)

	jobs := s.planTickLocked(cfg)
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	// 第 k 个 = 前 k-1 项之和：a=0，b=1s，c=1+5=6s
	want := []time.Duration{0, 1 * time.Second, 6 * time.Second}
	for k, w := range want {
		if jobs[k].delay != w {
			t.Fatalf("第 %d 个平台延迟 = %v, want %v", k+1, jobs[k].delay, w)
		}
	}
}

func TestPlanTickSkipsStoppedAndBackoff(t *testing.T) {
	s := &Scheduler{
		defs:     map[string]*config.RuntimeProvider{},
		states:   map[string]*ProviderState{},
		backoffs: map[string]*Backoff{},
		store:    NewStore("t"),
		clock:    time.Now,
		randFn:   func(n int) int { return 0 },
	}
	provs := []*config.RuntimeProvider{
		{ID: "ok", Paths: []string{"/"}},
		{ID: "stopped", Paths: []string{"/"}},                      // 401 停采
		{ID: "backoff", Paths: []string{"/"}},                      // 退避未到
		{ID: "decrypt", Paths: []string{"/"}, DecryptFailed: true}, // 解密失败停采
	}
	cfg := mkRuntime(provs...)
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)
	s.backoffs["stopped"].Stop()
	s.backoffs["backoff"].OnFailure(time.Now(), 300, 2, 1800)
	jobs := s.planTickLocked(cfg)
	if len(jobs) != 1 || jobs[0].p.ID != "ok" {
		t.Fatalf("应只调度 ok: %+v", jobs)
	}
}

func TestPlanTickStaggerFromConfig(t *testing.T) { // 区间取自 config（禁写死常量）
	s := &Scheduler{
		defs:     map[string]*config.RuntimeProvider{},
		states:   map[string]*ProviderState{},
		backoffs: map[string]*Backoff{},
		store:    NewStore("t"),
		clock:    time.Now,
	}
	seen := map[int]bool{}
	s.randFn = func(n int) int { seen[n] = true; return 0 }
	cfg := mkRuntime(&config.RuntimeProvider{ID: "a", Paths: []string{"/"}})
	cfg.Collector = *rtConfig(300, 7, 9)
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)
	s.planTickLocked(cfg)
	if !seen[3] { // span = 9-7 = 2 → randFn(3)
		t.Fatalf("stagger 区间应取自 config: seen=%v", seen)
	}
}

// TestConcurrentNotSerial C-col-05 后半：并发执行不互相等待（全部平台在一个 stagger 窗口内完成）。
func TestConcurrentNotSerial(t *testing.T) {
	var mu sync.Mutex
	starts := map[string]time.Time{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts[r.URL.Path] = time.Now()
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	provs := []*config.RuntimeProvider{}
	for i, id := range []string{"a", "b", "c", "d"} {
		// 各自独立平台（v0.2.5：同平台会被串行化，跨平台才是并发场景）
		provs = append(provs, &config.RuntimeProvider{ID: id, Name: id, Platform: "p" + id, KeyIndex: i, KeyCount: 1,
			BaseURL: srv.URL, Paths: []string{"/" + id}, AuthStyle: config.AuthStyleBearer})
	}
	cfg := mkRuntime(provs...)
	cfg.Collector = *rtConfig(300, 0, 0) // 无错峰：全部同时启动
	store := NewStore("t")
	s := NewScheduler(cfg, store, &Client{HTTP: srv.Client(), Version: "t"})
	s.client.HTTP = &http.Client{Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runTick(ctx)
	if len(starts) != 4 {
		t.Fatalf("平台数 = %d: %v", len(starts), starts)
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
	if max.Sub(min) > 200*time.Millisecond {
		t.Fatalf("平台启动时刻应几乎同时（错峰 0）：跨度 %v", max.Sub(min))
	}
}

// TestCollectAggregate 采集聚合：单 path 成功 / 多 path 深合并 / 401 停采 / 429 顺延 / 全失败退避。
func TestCollectAggregate(t *testing.T) {
	t.Run("单 path 成功", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"success":true,"data":{"percentage":50}}`)
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/q"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		s.collectProvider(context.Background(), cfg.Providers[0])
		st := s.states["a"]
		if st.Status != StatusOK || !strings.Contains(string(st.Data), "percentage") {
			t.Fatalf("状态错误: %+v", st)
		}
		if st.LastSuccessAt == nil || st.Error != nil {
			t.Fatalf("成功态错误: %+v", st)
		}
	})

	t.Run("多 path 深合并", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/usages":
				_, _ = io.WriteString(w, `{"code":"ok","data":{"usage":{"limit":100}}}`)
			case "/me":
				_, _ = io.WriteString(w, `{"code":"ok","data":{"name":"tester"}}`)
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "k", Name: "K", BaseURL: srv.URL, Paths: []string{"/usages", "/me"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		s.collectProvider(context.Background(), cfg.Providers[0])
		st := s.states["k"]
		var m map[string]any
		if err := json.Unmarshal(st.Data, &m); err != nil {
			t.Fatalf("合并结果非法: %v", err)
		}
		data := m["data"].(map[string]any)
		if _, ok := data["usage"]; !ok {
			t.Fatalf("/usages 数据丢失: %s", st.Data)
		}
		if _, ok := data["name"]; !ok {
			t.Fatalf("/me 数据丢失（应深合并）: %s", st.Data)
		}
	})

	t.Run("任一 path 401 → token_invalid 停采", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/bad" {
				w.WriteHeader(401)
				return
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/good", "/bad"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		s.collectProvider(context.Background(), cfg.Providers[0])
		st := s.states["a"]
		if st.Status != StatusTokenInvalid || st.Error.Code != CodeTokenInvalid {
			t.Fatalf("状态错误: %+v", st)
		}
		if !s.backoffs["a"].Stopped() {
			t.Fatal("401 应停采")
		}
		if st.Error.RetryAfterS != nil {
			t.Fatal("停采态不应有 retry_after_s")
		}
	})

	t.Run("429 按 Retry-After 顺延", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		s.collectProvider(context.Background(), cfg.Providers[0])
		st := s.states["a"]
		if st.Status != StatusFailed || st.Error.Code != CodeRateLimited {
			t.Fatalf("状态错误: %+v", st)
		}
		if st.Error.RetryAfterS == nil || *st.Error.RetryAfterS != 120 {
			t.Fatalf("retry_after_s 应为 120: %v", st.Error.RetryAfterS)
		}
		if next := s.backoffs["a"].NextAttemptAt(); time.Until(next) < 100*time.Second {
			t.Fatalf("nextAttemptAt 应 +120s: %v", next)
		}
	})

	t.Run("全失败退避 + 保留历史数据", func(t *testing.T) {
		var behavior atomic.Value // "ok" | "fail"
		behavior.Store("ok")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if behavior.Load().(string) == "ok" {
				_, _ = io.WriteString(w, `{"success":true,"data":{"v":1}}`)
				return
			}
			w.WriteHeader(500)
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "a", Name: "A", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		// 先成功一次
		s.collectProvider(context.Background(), cfg.Providers[0])
		// 再 5xx
		behavior.Store("fail")
		s.collectProvider(context.Background(), cfg.Providers[0])
		st := s.states["a"]
		if st.Status != StatusFailed || st.Error.Code != CodeServerError {
			t.Fatalf("状态错误: %+v", st)
		}
		if st.Data == nil || !strings.Contains(string(st.Data), `"v":1`) {
			t.Fatalf("曾成功后失败应保留 data: %s", st.Data)
		}
		if st.LastSuccessAt == nil {
			t.Fatal("last_success_at 应保留")
		}
		rs := st.Error.RetryAfterS
		if rs == nil || *rs != 300 {
			t.Fatalf("首次失败退避应为 300: %v", rs)
		}
	})

	t.Run("在途结果 id 已删则丢弃", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"success":true}`)
		}))
		defer srv.Close()
		s, cfg := newTestScheduler(t, &config.RuntimeProvider{ID: "gone", Name: "G", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer})
		s.client = &Client{HTTP: srv.Client(), Version: "t"}
		// 模拟热更删除：重建 defs（空）
		newCfg := mkRuntime()
		s.cfg.Store(newCfg)
		s.reloadLocked(newCfg)
		s.collectProvider(context.Background(), cfg.Providers[0])
		if _, exists := s.states["gone"]; exists {
			t.Fatal("已删 provider 的在途结果应丢弃")
		}
	})
}

// newTestScheduler 构造带单 provider 的调度器（真快照发布链路）。
func newTestScheduler(t *testing.T, p *config.RuntimeProvider) (*Scheduler, *config.Runtime) {
	t.Helper()
	cfg := mkRuntime(p)
	cfg.Collector = *rtConfig(300, 1, 5)
	store := NewStore("t")
	s := NewScheduler(cfg, store, &Client{Version: "t"})
	return s, cfg
}
