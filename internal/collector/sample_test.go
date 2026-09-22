package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RexKang/QuotaClock/internal/config"
)

// recordingSampler 记录投递进来的趋势样本。
type recordingSampler struct {
	mu    sync.Mutex
	calls []sampleCall
}

type sampleCall struct {
	id       string
	name     string
	platform string
	at       time.Time
	data     json.RawMessage
}

func (r *recordingSampler) Observe(id, name, platform string, at time.Time, data json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, sampleCall{id, name, platform, at, data})
}

func (r *recordingSampler) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingSampler) last() sampleCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

const sampleOKBody = `{"code":200,"msg":"操作成功","data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":12}]},"success":true}`

// TestSamplerOnlyOnRealSuccess：趋势样本只来自**真实采集成功**——
// 失败态与缓存恢复态都不能投递（否则「没采到」会被写成 0 或旧值）。
func TestSamplerOnlyOnRealSuccess(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := int(status.Load())
		w.WriteHeader(code)
		if code == 200 {
			_, _ = w.Write([]byte(sampleOKBody))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	p := &config.RuntimeProvider{
		ID: "zhipu-glm.k1", Platform: "zhipu-glm", KeyID: "k1", Name: "智谱 GLM",
		BaseURL: srv.URL, Paths: []string{"/api/monitor/usage/quota/limit"},
		AuthStyle: config.AuthStyleBearer,
	}
	cfg := mkRuntime(p)
	store := NewStore("t")
	s := NewScheduler(cfg, store, NewClient("t"))
	rec := &recordingSampler{}
	s.SetSampler(rec)

	// 成功 → 投递一次，且带的是该 Key 自己的成功时间与原始应答
	s.collectProvider(context.Background(), cfg.Providers[0])
	if rec.count() != 1 {
		t.Fatalf("成功采集应投递 1 次，实际 %d", rec.count())
	}
	c := rec.last()
	if c.id != "zhipu-glm.k1" || c.platform != "zhipu-glm" || c.name != "智谱 GLM" {
		t.Errorf("投递元信息不对：%+v", c)
	}
	st := s.states["zhipu-glm.k1"]
	if st == nil || st.LastSuccessAt == nil || !c.at.Equal(*st.LastSuccessAt) {
		t.Errorf("样本时间应为该 Key 的 last_success_at（错峰采集下不等于 tick 时刻）：%v / %v", c.at, st.LastSuccessAt)
	}
	if len(c.data) == 0 {
		t.Error("样本应携带原始应答（提取口径在 trend 侧，采集侧不裁剪）")
	}

	// 500 → 失败态不投递
	status.Store(500)
	s.collectProvider(context.Background(), cfg.Providers[0])
	if rec.count() != 1 {
		t.Errorf("失败采集不该投递样本，实际 %d 次", rec.count())
	}

	// 429 → 同样不投递
	status.Store(429)
	s.collectProvider(context.Background(), cfg.Providers[0])
	if rec.count() != 1 {
		t.Errorf("限流不该投递样本，实际 %d 次", rec.count())
	}

	// 401 → token 失效，不投递
	status.Store(401)
	s.collectProvider(context.Background(), cfg.Providers[0])
	if rec.count() != 1 {
		t.Errorf("token 失效不该投递样本，实际 %d 次", rec.count())
	}

	// 缓存恢复（重启路径）不是新样本
	at := time.Now().Add(-time.Hour)
	n := s.SeedCached([]ProviderState{{
		ID: "zhipu-glm.k1", Name: "智谱 GLM", Status: StatusCached,
		Data: json.RawMessage(sampleOKBody), LastSuccessAt: &at,
	}})
	if n != 0 { // 该 Key 已是 token_invalid 真实结果，缓存不覆盖；这里只验证采样不受影响
		t.Logf("缓存恢复覆盖了 %d 条状态", n)
	}
	if rec.count() != 1 {
		t.Errorf("缓存恢复不该投递样本，实际 %d 次", rec.count())
	}
}

// TestSamplerNilSafe：未装配采样器时采集链路照常工作（零值安全）。
func TestSamplerNilSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleOKBody))
	}))
	defer srv.Close()
	p := &config.RuntimeProvider{
		ID: "zhipu-glm.k1", Platform: "zhipu-glm", KeyID: "k1", Name: "智谱 GLM",
		BaseURL: srv.URL, Paths: []string{"/q"}, AuthStyle: config.AuthStyleBearer,
	}
	cfg := mkRuntime(p)
	s := NewScheduler(cfg, NewStore("t"), NewClient("t"))
	s.collectProvider(context.Background(), cfg.Providers[0])
	st := s.states["zhipu-glm.k1"]
	if st == nil || st.Status != StatusOK {
		t.Fatalf("状态应为 ok，实际 %+v", st)
	}
}

// TestSamplerGetsUnknownShapeData：认不出的平台形状也照样投递（提取与否归 trend 决定），
// 采集侧不做业务判断——避免出现「卡片有数据但趋势没有」的两套语义。
func TestSamplerGetsUnknownShapeData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"weird":true}`))
	}))
	defer srv.Close()
	p := &config.RuntimeProvider{
		ID: "opencode.k9", Platform: "opencode", KeyID: "k9", Name: "OC",
		BaseURL: srv.URL, Paths: []string{"/usage"}, AuthStyle: config.AuthStyleBearer,
	}
	cfg := mkRuntime(p)
	s := NewScheduler(cfg, NewStore("t"), NewClient("t"))
	rec := &recordingSampler{}
	s.SetSampler(rec)
	s.collectProvider(context.Background(), cfg.Providers[0])
	if rec.count() != 1 {
		t.Fatalf("成功采集就应投递，实际 %d", rec.count())
	}
}
