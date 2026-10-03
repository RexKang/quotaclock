package collector

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/logx"
)

// Scheduler 采集调度器：单 goroutine 串行 tick 循环（设计 §7.1），
// tick 内各平台按累计 stagger 延迟错峰并发启动、执行互不等待（D-10）。
type Scheduler struct {
	cfg    atomic.Pointer[config.Runtime]
	store  *Store
	client *Client

	mu               sync.Mutex
	defs             map[string]*config.RuntimeProvider
	states           map[string]*ProviderState
	backoffs         map[string]*Backoff // key 级退避（runtime id = <platform>.<keyID>）
	platformBackoffs map[string]*Backoff // 平台级退避（platform）：5xx/网络属平台异常，暂停该平台全部 key
	inflight         map[string]int      // 平台 → 在途采集数：同平台请求串行化（防同平台 key 并发互撞）

	cache CacheWriter // 上次成功数据落盘出口（nil = 不启用缓存，v0.2.4）
	// sampler 趋势采样出口（nil = 不记录趋势，v0.4.0）。
	sampler Sampler

	wake chan struct{} // 热生效唤醒：配置保存后尽快开跑下一 tick（验收 #9 ≤1 轮询周期红字的前提）

	clock  func() time.Time
	randFn func(n int) int // [0,n)

	runWG  sync.WaitGroup // Run 主循环
	tickWG sync.WaitGroup // 在途 fetch（优雅退出等待，25s 出站超时封顶）
}

// NewScheduler 初始化并发布首个快照（全部 provider = 尚未采集态；revision 0 → 1）。
func NewScheduler(cfg *config.Runtime, store *Store, client *Client) *Scheduler {
	s := &Scheduler{
		store:            store,
		client:           client,
		defs:             map[string]*config.RuntimeProvider{},
		states:           map[string]*ProviderState{},
		backoffs:         map[string]*Backoff{},
		platformBackoffs: map[string]*Backoff{},
		inflight:         map[string]int{},
		wake:             make(chan struct{}, 1),
		clock:            time.Now,
		randFn:           defaultRand,
	}
	s.cfg.Store(cfg)
	s.mu.Lock()
	s.reloadLocked(cfg)
	s.mu.Unlock()
	return s
}

func defaultRand(n int) int { return rand.IntN(n) }

// SetCacheWriter 挂载缓存落盘出口（main 装配；nil = 不写缓存）。
func (s *Scheduler) SetCacheWriter(w CacheWriter) {
	s.mu.Lock()
	s.cache = w
	s.mu.Unlock()
}

// SetSampler 挂载趋势采样出口（main 装配；nil = 不记趋势）。
func (s *Scheduler) SetSampler(sp Sampler) {
	s.mu.Lock()
	s.sampler = sp
	s.mu.Unlock()
}

// sampleLocked 把一次**真实成功**的样本投给趋势聚合器。
// 投递内容 = 该 Key 自己的 last_success_at（不是 tick 时刻）：同平台多 Key 是错峰采集的，
// 用 tick 时刻会把 5 分钟前的数据记成「现在」。窗口/缺口判定在 trend 侧完成。
// 纯内存操作（不落盘、不阻塞），落盘由 trend.Store 按小时自行安排。
func (s *Scheduler) sampleLocked(platform string, st *ProviderState) {
	if s.sampler == nil || st == nil || st.LastSuccessAt == nil || len(st.Data) == 0 {
		return
	}
	s.sampler.Observe(st.ID, st.Name, platform, *st.LastSuccessAt, st.Data)
}

// SeedCached 用 cache.json 恢复的状态填充「尚未完成首次采集」的空态（v0.2.4，启动时调用一次）：
// 只覆盖未采集态（NOT_COLLECTED_YET）与既有缓存态，绝不覆盖真实采集结果。
func (s *Scheduler) SeedCached(cached []ProviderState) int {
	if len(cached) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range cached {
		st := cached[i]
		if s.defs[st.ID] == nil {
			continue // 配置里已不存在
		}
		cur := s.states[st.ID]
		if cur == nil {
			continue
		}
		notCollected := cur.Status == StatusFailed && cur.Error != nil && cur.Error.Code == CodeNotCollectedYet
		if cur.Status != StatusCached && !notCollected {
			continue // 已有真实结果，缓存不覆盖
		}
		cp := st
		s.states[st.ID] = &cp
		n++
	}
	if n > 0 {
		s.publishLocked()
	}
	return n
}

// Reload 热生效：换配置指针并重建调度状态（设计 §11）——
// 未变 provider 保留退避计数与最近结果；变更/新增重置；被删的取消调度；
// 重建期间在途 fetch 照常完成，数据归属已删 id 的丢弃。
func (s *Scheduler) Reload(cfg *config.Runtime) {
	s.mu.Lock()
	s.cfg.Store(cfg)
	s.reloadLocked(cfg)
	s.saveCacheLocked() // 平台删除/停用后缓存立即收敛（不在 states 中即被剔出）
	s.mu.Unlock()
	logx.Infof("采集器已按新配置重建（%d 个平台）", len(cfg.Providers))
	// 唤醒主循环：下一 tick 立即开跑（新配置即刻参与采集；验收 #9 失败态传播时限的前提）
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) reloadLocked(cfg *config.Runtime) {
	s.ensureMapsLocked()
	newDefs := make(map[string]*config.RuntimeProvider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		newDefs[p.ID] = p
		if old := s.defs[p.ID]; old != nil && !p.DecryptFailed &&
			s.states[p.ID] != nil && s.backoffs[p.ID] != nil && sameDefinition(old, p) {
			// 未变：保留退避与历史。**仅展示名变化**时同步名称即可——
			// 采集状态（status/data/last_success_at）与退避节奏都不重置，
			// 下一轮仍按原节奏来；快照因 name 变化推 revision，前端自动重绘（PRD r2.3 ⑤）。
			if st := s.states[p.ID]; st != nil && st.Name != p.Name {
				st.Name = p.Name
			}
			continue
		}
		st := &ProviderState{ID: p.ID, Name: p.Name}
		if !p.IsEnabled() {
			// v0.2.4：停用的凭据灰显，错误码 DISABLED（前端识别渲染「已停用」徽标）。
			// 判定先于解密失败：停用是用户意图，此时 token 能否解密与展示无关
			st.Status = StatusDisabled
			st.Error = &ErrorInfo{Code: CodeDisabled, Message: "凭据已停用"}
		} else if p.DecryptFailed {
			// key.bin 丢失/不匹配的降级（设计 §6.4）：token_invalid，不 crash
			st.Status = StatusTokenInvalid
			st.Error = &ErrorInfo{Code: CodeTokenDecryptFail, Message: "token 无法解密，请在设置中重新录入"}
		} else {
			st.Status = StatusFailed
			st.Error = &ErrorInfo{Code: CodeNotCollectedYet, Message: "尚未完成首次采集"}
		}
		s.states[p.ID] = st
		s.backoffs[p.ID] = &Backoff{}
	}
	s.defs = newDefs
	// 平台级退避：保留仍在用的平台（配置保存不该丢掉平台退避），清掉已消失的平台
	livePlatforms := map[string]bool{}
	for _, p := range cfg.Providers {
		livePlatforms[p.Platform] = true
		if s.platformBackoffs[p.Platform] == nil {
			s.platformBackoffs[p.Platform] = &Backoff{}
		}
	}
	for plat := range s.platformBackoffs {
		if !livePlatforms[plat] {
			delete(s.platformBackoffs, plat)
		}
	}
	for id := range s.states {
		if _, ok := newDefs[id]; !ok {
			delete(s.states, id)
			delete(s.backoffs, id)
		}
	}
	s.publishLocked()
}

// sameDefinition 判断 provider 定义是否变化（token 变 → TokenCipher 变 → 重置）。
// enabled 也算定义变化：勾选/取消停用要立刻重建状态，否则卡片不会实时显示「已停用」（v0.2.4）。
// **Name 不算**（v0.2.5 r4.1）：名称是纯展示字段，改名不该把已采到的数据打回「等待采集」
// 并立刻重采——用户实测：只改了一个 API Key 的名称，采集却整个重跑了一遍。
func sameDefinition(a, b *config.RuntimeProvider) bool {
	if a.BaseURL != b.BaseURL || a.AuthStyle != b.AuthStyle || a.TokenCipher != b.TokenCipher {
		return false
	}
	if a.IsEnabled() != b.IsEnabled() {
		return false
	}
	if len(a.Paths) != len(b.Paths) {
		return false
	}
	for i := range a.Paths {
		if a.Paths[i] != b.Paths[i] {
			return false
		}
	}
	if len(a.ExtraHeaders) != len(b.ExtraHeaders) {
		return false
	}
	for k, v := range a.ExtraHeaders {
		if b.ExtraHeaders[k] != v {
			return false
		}
	}
	return true
}

// Run 调度主循环：runTick → sleep(max(0, base+jitter − elapsed))，锚定 tick 开始时刻。
// 配置热生效会唤醒休眠中的主循环，立即以新配置进入下一 tick。
func (s *Scheduler) Run(ctx context.Context) {
	s.runWG.Add(1)
	defer s.runWG.Done()
	for {
		tickStart := s.clock()
		s.runTick(ctx)
		if ctx.Err() != nil {
			return
		}
		cfg := s.cfg.Load()
		jitter := cfg.Collector.JitterMinS
		if span := cfg.Collector.JitterMaxS - cfg.Collector.JitterMinS; span > 0 {
			jitter += s.randFn(span + 1)
		}
		period := time.Duration(cfg.Collector.IntervalBaseS+jitter) * time.Second
		sleep := period - s.clock().Sub(tickStart)
		if sleep < 0 {
			sleep = 0
		}
		logx.Debugf("下一 tick 约 %s 后（基准 %ds + 抖动 %ds）", sleep, cfg.Collector.IntervalBaseS, jitter)
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			logx.Debug("配置热生效唤醒，立即进入下一 tick")
		case <-timer.C:
		}
	}
}

// Wait 等待 Run 主循环结束（含最后一个 tick 的全部在途 fetch）。
func (s *Scheduler) Wait() { s.runWG.Wait() }

// runTick 对「应采集」的 provider（非停采态且退避未到期）按累计错峰延迟并发发起采集，
// 并等待本 tick 全部 fetch 结束（§7.3）。
func (s *Scheduler) runTick(ctx context.Context) {
	s.mu.Lock()
	cfg := s.cfg.Load()
	jobs := s.planTickLocked(cfg)
	s.mu.Unlock()

	for _, j := range jobs {
		s.tickWG.Add(1)
		go func(j job) {
			defer s.tickWG.Done()
			if j.delay > 0 {
				t := time.NewTimer(j.delay)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			if ctx.Err() != nil {
				return
			}
			// 同平台串行化：同平台已有在途采集（上一次尚未回）→ 本次避让，
			// 避免同平台的多个 key 并发互撞（用户实测的冲突来源之一）。
			// Platform 为空（未经 BuildRuntime 的构造路径）不做分组约束。
			if j.p.Platform != "" && !s.beginInflight(j.p.Platform) {
				logx.Warnf("跳过 %s：同平台 %s 仍有在途采集，本轮避让", j.p.ID, j.p.Platform)
				return
			}
			if j.p.Platform != "" {
				defer s.endInflight(j.p.Platform)
			}
			s.collectProvider(ctx, j.p)
		}(j)
	}
	s.tickWG.Wait()
}

type job struct {
	p     *config.RuntimeProvider
	delay time.Duration
}

// planTickLocked 计算本 tick 的采集任务（v0.2.5 双层调度）：
//   - 平台级：平台退避（5xx/网络，基数 interval_base_s）未到期 → 该平台**全部 key** 跳过（不给异常平台添压）
//   - key 级：停用 / 解密失败 / 自身退避（429 等，基数 = 等分周期）未到期 → 该 key 跳过
//   - 同平台多 key：启动偏移 = KeyIndex × (interval_base_s / KeyCount)，把一个周期等分成 N 段
//   - 跨平台：在等分偏移之上再叠加累计 rand(stagger)（首个立即启动，C-col-05）
func (s *Scheduler) planTickLocked(cfg *config.Runtime) []job {
	now := s.clock()
	var jobs []job
	acc := time.Duration(0)
	notified := map[string]bool{}
	for _, p := range cfg.Providers {
		if p.DecryptFailed {
			continue // token 无法解密：停采态
		}
		if !p.IsEnabled() {
			continue // 凭据停用（enabled=false）不参与采集
		}
		if pb := s.platformBackoffs[p.Platform]; pb != nil && !pb.Eligible(now) {
			if !notified[p.Platform] {
				notified[p.Platform] = true
				logx.Debugf("跳过平台 %s 的全部 key（平台级退避未到）", p.Platform)
			}
			continue
		}
		b := s.backoffs[p.ID]
		if b == nil || !b.Eligible(now) {
			logx.Debugf("跳过 %s（停采或退避未到）", p.ID)
			continue
		}
		start := acc + s.keyOffset(cfg, p)
		acc += time.Duration(s.randStagger(cfg)) * time.Second
		jobs = append(jobs, job{p, start})
	}
	return jobs
}

// keyOffset 同平台多 key 的等分偏移：KeyIndex × (interval_base_s / KeyCount)。
// 语义：IntervalBaseS 是每个 key 的采集周期，同平台 N 个 key 在该周期内等分错开，
// 使同平台的请求彼此拉开（用户实测：同平台不同 key 一起打会互相冲突）。
func (s *Scheduler) keyOffset(cfg *config.Runtime, p *config.RuntimeProvider) time.Duration {
	if p.KeyCount <= 1 || p.KeyIndex <= 0 {
		return 0
	}
	period := time.Duration(cfg.Collector.IntervalBaseS) * time.Second / time.Duration(p.KeyCount)
	return time.Duration(p.KeyIndex) * period
}

// keyPeriodS 单 key 的等分周期（秒）：interval_base_s / keyCount，至少 1s。
// 用作 key 级退避基数（429 属单 key 限流：平台正常，该 key 紧凑退避即可）。
func (s *Scheduler) keyPeriodS(cfg *config.Runtime, p *config.RuntimeProvider) int {
	if p.KeyCount <= 1 {
		return cfg.Collector.IntervalBaseS
	}
	period := cfg.Collector.IntervalBaseS / p.KeyCount
	if period < 1 {
		period = 1
	}
	return period
}

// isPlatformLevel 该错误分类是否属「平台异常」（暂停该平台全部 key）。
// 5xx/网络：上游整体异常或本机网络故障 → 保守暂停全平台；
// 其余（4xx / 业务失败）可能是单 key 问题（额度耗尽、key 被禁）→ 只影响该 key。
func isPlatformLevel(c ErrClass) bool {
	return c == ClassNetwork || c == ClassServerErr
}

// ensureMapsLocked 惰性初始化内部 map：让 Scheduler 零值（测试直接字面量构造、
// 或将来新的构造路径）也可用，避免「忘记初始化 → 写 nil map panic」这类零值陷阱。
func (s *Scheduler) ensureMapsLocked() {
	if s.defs == nil {
		s.defs = map[string]*config.RuntimeProvider{}
	}
	if s.states == nil {
		s.states = map[string]*ProviderState{}
	}
	if s.backoffs == nil {
		s.backoffs = map[string]*Backoff{}
	}
	if s.platformBackoffs == nil {
		s.platformBackoffs = map[string]*Backoff{}
	}
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
}

// beginInflight 尝试占用平台的在途名额（同平台请求串行化）；已占用返回 false。
func (s *Scheduler) beginInflight(platform string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	if s.inflight[platform] > 0 {
		return false
	}
	s.inflight[platform]++
	return true
}

func (s *Scheduler) endInflight(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != nil && s.inflight[platform] > 0 {
		s.inflight[platform]--
	}
}

// randStagger 抽取 stagger 区间 [min, max]（秒），禁写死常量、取自运行时 config。
func (s *Scheduler) randStagger(cfg *config.Runtime) int {
	span := cfg.Collector.StaggerMaxS - cfg.Collector.StaggerMinS
	if span < 0 {
		span = 0
	}
	return s.randFn(span+1) + cfg.Collector.StaggerMinS
}

type pathResult struct {
	path string
	res  Classified
}

// collectProvider 采集单个 provider：全部 path 并发请求 → 分类聚合 → 更新状态并发布快照。
func (s *Scheduler) collectProvider(ctx context.Context, p *config.RuntimeProvider) {
	t0 := s.clock()
	results := make([]pathResult, len(p.Paths))
	var wg sync.WaitGroup
	for i := range p.Paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = pathResult{path: p.Paths[i], res: s.client.FetchOnce(ctx, p, p.Paths[i])}
		}(i)
	}
	wg.Wait()
	elapsed := s.clock().Sub(t0)

	var okIdx []int
	var tokenInvalid, rateLimited, otherErr *pathResult
	for i := range results {
		r := &results[i]
		switch r.res.Class {
		case ClassOK:
			okIdx = append(okIdx, i)
		case ClassTokenInvalid:
			if tokenInvalid == nil {
				tokenInvalid = r
			}
		case ClassRateLimited:
			if rateLimited == nil {
				rateLimited = r
			}
		default:
			if otherErr == nil {
				otherErr = r
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg.Load()
	if cfg.Provider(p.ID) == nil {
		logx.Debugf("采集结果丢弃：%s 已从配置删除", p.ID)
		return
	}
	b := s.backoffs[p.ID]
	if b == nil {
		b = &Backoff{}
		s.backoffs[p.ID] = b
	}
	prior := s.states[p.ID]

	st := ProviderState{ID: p.ID, Name: p.Name}
	switch {
	case tokenInvalid != nil:
		// 401/403：停采该 key，不做退避（退避会掩盖 token 失效）
		b.Stop()
		st.Status = StatusTokenInvalid
		st.Error = &ErrorInfo{Code: CodeTokenInvalid, Message: tokenInvalid.res.Message}
		logx.Warnf("采集 %s：token 失效，已停采", p.ID)
	case len(okIdx) > 0:
		now := s.clock()
		// key 级成功：按「该 key 的等分周期」排下一次（不是清零）——避免「保存配置」唤醒的
		// 额外 tick 立刻重采。周期取 keyPeriodS（单 key 平台 = interval_base_s；同平台 N 个
		// key = interval_base_s ÷ N）：这是「同一个 key 两次请求之间的最小间距」。
		// 不能用 interval_base_s——同平台第 2 个 key 的请求本来就在 tick 内偏移 base/N 处发出，
		// 若再等整个 base 才算到期，它会被下一轮整个跳过，采集频率减半。
		// 注意节奏由 tick 决定（tick 周期 ≥ base），这里只是一道「别在同一周期内重复采」的闸门。
		b.OnSuccessAt(now, time.Duration(s.keyPeriodS(cfg, p))*time.Second)
		// 平台恢复：清掉平台级退避（任一 key 成功即视为平台可用）
		if pb := s.platformBackoffs[p.Platform]; pb != nil {
			pb.OnSuccess()
		}
		st.Status = StatusOK
		st.Data = mergePathData(results, okIdx)
		st.LastSuccessAt = &now
		st.Error = nil
		logx.Debugf("采集 %s 成功（%s）", p.ID, elapsed)
	case rateLimited != nil:
		// 429：key 级限流（平台正常）→ 只停该 key，退避基数用等分周期（紧凑恢复）；
		// 有 Retry-After 则按其顺延
		now := s.clock()
		delay := b.OnRateLimited(now, rateLimited.res.RetryAfterS,
			s.keyPeriodS(cfg, p), cfg.Collector.BackoffMultiplier, cfg.Collector.BackoffMaxS)
		st.Status = StatusFailed
		st.Data = priorData(prior)
		st.LastSuccessAt = priorSuccess(prior)
		rs := int(delay / time.Second)
		st.Error = &ErrorInfo{Code: CodeRateLimited, Message: rateLimited.res.Message, RetryAfterS: &rs}
		logx.Warnf("采集 %s：429 限流（key 级），%ds 后重试", p.ID, rs)
	default:
		// 其余失败：按「谁的错」分级退避（v0.2.5）
		now := s.clock()
		cls := otherErr.res.Class
		mult := cfg.Collector.BackoffMultiplier
		maxS := cfg.Collector.BackoffMaxS
		var delay time.Duration
		level := "key 级"
		if isPlatformLevel(cls) {
			// 平台异常（5xx/网络）：暂停该平台全部 key，基数用 interval_base_s（保守）
			level = "平台级"
			pb := s.platformBackoffs[p.Platform]
			if pb == nil {
				pb = &Backoff{}
				s.platformBackoffs[p.Platform] = pb
			}
			if pb.Eligible(now) {
				delay = pb.OnFailure(now, cfg.Collector.IntervalBaseS, mult, maxS)
			} else {
				// 同 tick 内该平台已有 key 触发过平台级失败：不再升级，仅按剩余时长展示
				delay = pb.NextAttemptAt().Sub(now)
			}
		} else {
			// key 级失败（其他 4xx / 业务失败）：只停该 key，基数用等分周期
			delay = b.OnFailure(now, s.keyPeriodS(cfg, p), mult, maxS)
		}
		if delay < 0 {
			delay = 0
		}
		st.Status = StatusFailed
		st.Data = priorData(prior)
		st.LastSuccessAt = priorSuccess(prior)
		rs := int(delay / time.Second)
		st.Error = &ErrorInfo{Code: classCode(cls), Message: otherErr.res.Message, RetryAfterS: &rs}
		logx.Warnf("采集 %s：%s（%s），%ds 后重试", p.ID, cls, level, rs)
	}
	s.states[p.ID] = &st
	s.publishLocked()
	if st.Status == StatusOK {
		// 只有真实成功才刷新缓存（失败态同上：保留上次成功数据，不覆写）
		s.saveCacheLocked()
		s.sampleLocked(p.Platform, &st)
	}
}

// saveCacheLocked 用当前状态集投递缓存快照（调用方持锁；非阻塞，落盘在后台 goroutine）。
func (s *Scheduler) saveCacheLocked() {
	if s.cache == nil {
		return
	}
	cfg := s.cfg.Load()
	states := make([]ProviderState, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if st := s.states[p.ID]; st != nil {
			states = append(states, *st)
		}
	}
	s.cache.Save(CacheFromStates(s.clock(), states))
}

func priorData(p *ProviderState) json.RawMessage {
	if p == nil {
		return nil
	}
	return p.Data
}

func priorSuccess(p *ProviderState) *time.Time {
	if p == nil {
		return nil
	}
	return p.LastSuccessAt
}

func classCode(c ErrClass) string {
	switch c {
	case ClassNetwork:
		return CodeNetwork
	case ClassServerErr:
		return CodeServerError
	case ClassClientErr:
		return CodeClientError
	case ClassBizError:
		return CodeBizError
	default:
		return CodeNetwork
	}
}

// mergePathData 多 path 成功结果的合并：单成功 → 原文；多成功 → JSON 对象深合并
// （键冲突先到路径优先；非对象结果丢弃，DEBUG 可查）。空值不变量 §9.3 的 data 形态由此保证。
func mergePathData(results []pathResult, okIdx []int) json.RawMessage {
	if len(okIdx) == 1 {
		return results[okIdx[0]].res.Data
	}
	var acc map[string]json.RawMessage
	for _, i := range okIdx {
		var m map[string]json.RawMessage
		if json.Unmarshal(results[i].res.Data, &m) != nil || m == nil {
			continue
		}
		if acc == nil {
			acc = m
			continue
		}
		mergeMaps(acc, m)
	}
	if acc == nil {
		return results[okIdx[0]].res.Data
	}
	b, err := json.Marshal(acc)
	if err != nil {
		return results[okIdx[0]].res.Data
	}
	return b
}

// mergeMaps 把 b 深合并进 a：键冲突且双方均为对象 → 递归合并；否则保留 a（先到路径优先）。
func mergeMaps(a, b map[string]json.RawMessage) {
	for k, v := range b {
		if av, ok := a[k]; ok {
			var am, bm map[string]json.RawMessage
			if json.Unmarshal(av, &am) == nil && json.Unmarshal(v, &bm) == nil && am != nil && bm != nil {
				mergeMaps(am, bm)
				if nb, err := json.Marshal(am); err == nil {
					a[k] = nb
				}
			}
			continue
		}
		a[k] = v
	}
}

// publishLocked 按 cfg 顺序重建完整快照并发布（diff 无变化则不发布，revision 不动）。
func (s *Scheduler) publishLocked() {
	cfg := s.cfg.Load()
	states := make([]ProviderState, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if st := s.states[p.ID]; st != nil {
			states = append(states, *st)
		}
	}
	s.store.Publish(s.clock(), states)
}
