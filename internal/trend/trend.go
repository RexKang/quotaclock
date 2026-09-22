// Package trend 维护「用量趋势」：按每次成功采集更新内存聚合，每小时（与退出时）落盘，
// 对外提供按日/按小时的聚合视图。它只关心可量化事实（额度百分比、余额金额），
// 不含任何展示文案——文案与配色归前端（与采集状态契约同一分工）。
package trend

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/RexKang/QuotaClock/internal/logx"
	"github.com/RexKang/QuotaClock/internal/persist"
)

// 聚合与保留策略（常量集中在此，便于核对存储量）。
const (
	// gapThreshold 采样间隔超过它即记为一次缺口：进程没跑、采集连续失败。
	// 取 90 分钟是因为单 key 的采集周期可能被同平台多 key 等分成 interval_base_s÷N
	// （默认 300s÷3 = 100s），要容得下若干轮失败或一次短暂停机。
	gapThreshold = 90 * time.Minute
	// hourKeepDays 小时槽保留天数（供「当日」视图与跨零点的小时差）。
	hourKeepDays = 14
	// dayKeep 天记录保留上限（约 2.2 年，超出后丢最旧的）。
	dayKeep = 800
	// partialThreshold 当日首样本晚于零点这么多 → 该日余额差不完整（进程中途启动）。
	partialThreshold = 5 * time.Minute
	// FlushInterval 默认落盘周期（用户要求「每小时保存」）。
	FlushInterval = time.Hour

	dateLayout = "2006-01-02"
	hourLayout = "2006-01-02T15"
)

// Store 趋势聚合器。并发安全：Observe/Flush/Purge/Snapshot 全部走同一把锁。
type Store struct {
	path string

	mu      sync.Mutex
	file    *persist.TrendsFile
	lastAt  map[string]time.Time // runtime id → 上一次纳入聚合的样本时间
	prevPct map[string]float64   // runtime id → 上一次样本的额度百分比
	hasPct  map[string]bool      // 上一次样本是否给了百分比（区分「没有值」与「值就是 0」）
	dirty   bool                 // 有未落盘的变动（避免每小时无谓重写同一个文件）

	now func() time.Time
}

// New 创建 Store 并加载既有趋势文件。
// 文件缺失（首次启动）→ 空表；解析失败/版本不符 → WARN 后忽略（从零积累，绝不阻塞启动，
// 与校验和缓存同一处置原则：历史数据不值得让服务起不来）。
func New(path string) *Store {
	s := &Store{
		path:    path,
		file:    &persist.TrendsFile{Version: persist.TrendsVersion, Keys: map[string]*persist.TrendKey{}},
		lastAt:  map[string]time.Time{},
		prevPct: map[string]float64{},
		hasPct:  map[string]bool{},
		now:     time.Now,
	}
	f, err := persist.LoadTrends(path)
	if err != nil {
		logx.Warnf("趋势数据不可用，已忽略（将重新积累）: %v", err)
		return s
	}
	if f != nil {
		s.file = f
		for id, k := range f.Keys {
			if k == nil {
				continue
			}
			// 用最后一条记录恢复「上次样本时间」：重启后第一条样本据此判定缺口，
			// 而不是把停机时长当作正常间隔。
			if t, ok := parseAt(k.LastAt); ok {
				s.lastAt[id] = t
			}
			// 增量要跨小时/跨天连续：重启后沿用最后一条记录的末值，
			// 否则重启后的第一条样本会被当成「没有上一次」而丢掉那段用量。
			if n := len(k.Days); n > 0 && k.Days[n-1].Pct != nil {
				s.prevPct[id] = k.Days[n-1].Pct.Last
				s.hasPct[id] = true
			}
		}
	}
	return s
}

// Loaded 已积累的 API Key 数量（启动日志与测试用）。
func (s *Store) Loaded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.file.Keys)
}

// Observe 纳入一次**成功采集**的样本（失败/缓存态不投递，由采集器保证）。
//
// 幂等：同一个 last_success_at 重复投递只计一次（重启后由 cache 恢复的数据不会被当新样本）。
// 缺口：与上一次样本间隔超过 gapThreshold → 该日 Gaps+1（余额差因此不再可信，见 Spent）。
func (s *Store) Observe(id, name, platform string, at time.Time, data json.RawMessage) {
	ex := Extract(platform, data)
	if ex.Pct == nil && ex.Balance == nil {
		return // 该平台这次没给出可记录的数：不写 0、不伪造（NO_DATA ≠ 0）
	}
	at = at.Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()

	k := s.file.Keys[id]
	if k == nil {
		k = &persist.TrendKey{ID: id, Platform: platform, Kind: KindOf(platform)}
		s.file.Keys[id] = k
	}
	if name != "" && k.Name != name {
		k.Name = name
	}
	if last, ok := s.lastAt[id]; ok && !at.After(last) {
		return // 同一份成功结果重复投递
	}
	prev, hadPrev := s.lastAt[id]

	// 该样本相对上次样本的用量（百分比口径）。三处要点：
	//  ① 上一次的值**跨小时、跨天**沿用（不是在当天/当小时内比），否则每个时段第一条样本的用量会被丢掉；
	//  ② 数值下跌 = 5 小时窗口换窗（重置）：**新窗口从 0 起算，所以这一步的用量就是本次值 cur**。
	//     不要补「窗口打满还差多少」—— 换窗时没被用掉的剩余额度是**作废**的，不是用掉的。
	//     用户给的例子：采集序列 88 → 90 → 91 → 0 → 1，实际用量 = (91−88) + (1−0) = 4
	//     （老窗口用了 3，换窗后新窗口用了 1）；逐样本看就是 +2、+1、换窗记 cur=0、+1。
	//  ③ 若某平台的百分比是**滚动窗口**（旧请求逐渐过期会让数值慢慢下降，而不是换窗），
	//     这里会把下降量记成用量 —— 两个样本之间无法区分，属于已知高估；Resets 计数可用于观察频率。
	var delta float64
	var isReset bool
	hadPrevPct := s.hasPct[id] // 本样本之前是否已有上一次百分比（供当日与当小时两处累加判断）
	if ex.Pct != nil {
		if hadPrevPct {
			delta = *ex.Pct - s.prevPct[id]
			if delta < 0 {
				delta = *ex.Pct // 换窗：新窗口自 0 起算
				isReset = true
			}
		}
		s.prevPct[id] = *ex.Pct
		s.hasPct[id] = true
	}

	day := s.dayLocked(k, at)
	if day == nil {
		return // 乱序样本（旧日期的迟到投递）：丢弃，不回头改历史
	}
	day.Samples++
	if hadPrev && at.Sub(prev) > gapThreshold {
		day.Gaps++
	}
	if ex.Pct != nil {
		if day.Pct == nil {
			day.Pct = &persist.TrendMetric{}
		}
		m := day.Pct
		cur := *ex.Pct
		if m.Samples == 0 || cur > m.Peak {
			m.Peak = cur
		}
		m.Last = cur
		m.Samples++
		if hadPrevPct {
			m.Gain += delta
			if isReset {
				m.Resets++
			}
		}
	}
	if ex.Balance != nil {
		if day.Balance == nil {
			day.Balance = &persist.TrendBalance{Currency: ex.Balance.Currency}
		}
		b := day.Balance
		v := ex.Balance.Amount
		if b.Samples == 0 {
			b.First = v
			// 首样本不在当日零点附近（进程中途启动）→ 零点到此刻的消费无从得知
			b.Partial = at.Sub(startOfDay(at)) > partialThreshold
			if b.Currency == "" {
				b.Currency = ex.Balance.Currency
			}
		}
		b.Samples++
		b.Last = v
		spent := b.First - v
		switch {
		case spent < 0:
			// 余额增加：当日发生过充值（或赠送）。差值为负，不能当成「负消费」画出去。
			b.Topup = true
			b.Spent = nil
		case day.Gaps > 0 || b.Partial:
			// 有采样缺口 / 起点不完整：首尾差不再等于当日消费，宁可不给值
			b.Spent = nil
		default:
			s := spent
			b.Spent = &s
		}
	}

	s.updateHourLocked(k, at, ex, delta, isReset, hadPrevPct)
	k.LastAt = fmtTime(at)
	if k.FirstAt == "" {
		k.FirstAt = k.LastAt
	}
	s.lastAt[id] = at
	s.dirty = true
}

// dayLocked 取当日记录（不存在则新建）；乱序（比最后一条更早）返回 nil。
func (s *Store) dayLocked(k *persist.TrendKey, at time.Time) *persist.TrendDay {
	key := at.Format(dateLayout)
	n := len(k.Days)
	if n > 0 {
		last := &k.Days[n-1]
		if last.Date == key {
			return last
		}
		if last.Date > key {
			return nil
		}
	}
	k.Days = append(k.Days, persist.TrendDay{Date: key})
	if len(k.Days) > dayKeep {
		k.Days = append([]persist.TrendDay(nil), k.Days[len(k.Days)-dayKeep:]...)
	}
	return &k.Days[len(k.Days)-1]
}

// updateHourLocked 更新小时槽（当日视图的数据源）。
func (s *Store) updateHourLocked(k *persist.TrendKey, at time.Time, ex Extracted, delta float64, isReset, hourHasPrev bool) {
	slot := at.Format(hourLayout)
	n := len(k.Hours)
	if n == 0 || k.Hours[n-1].Slot != slot {
		k.Hours = append(k.Hours, persist.TrendHour{Slot: slot})
	}
	h := &k.Hours[len(k.Hours)-1]
	if h.Slot != slot {
		return // 乱序样本：不回头改历史
	}
	if ex.Pct != nil {
		if h.Pct == nil {
			h.Pct = &persist.TrendMetric{}
		}
		m, cur := h.Pct, *ex.Pct
		if m.Samples == 0 || cur > m.Peak {
			m.Peak = cur
		}
		m.Last = cur
		m.Samples++
		// 用量记在「后一个样本所在的小时」里（跨小时的那段用量算给新小时），与当日口径一致
		if hourHasPrev {
			m.Gain += delta
			if isReset {
				m.Resets++
			}
		}
	}
	if ex.Balance != nil {
		v := ex.Balance.Amount
		h.Balance = &v
	}
	s.trimHoursLocked(k, at)
}

// trimHoursLocked 丢弃超过保留窗口的小时槽。
func (s *Store) trimHoursLocked(k *persist.TrendKey, now time.Time) {
	cut := now.Add(-hourKeepDays * 24 * time.Hour).Format(hourLayout)
	i := 0
	for i < len(k.Hours) && k.Hours[i].Slot < cut {
		i++
	}
	if i > 0 {
		k.Hours = append([]persist.TrendHour(nil), k.Hours[i:]...)
	}
}

// Flush 落盘（无变动则跳过）。调用方可以是每小时 tick，也可以是关停序列。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if !s.dirty {
		return nil
	}
	s.file.Version = persist.TrendsVersion
	s.file.WrittenAt = fmtTime(s.now())
	if err := persist.SaveTrends(s.path, s.file); err != nil {
		return err
	}
	s.dirty = false
	logx.Debugf("趋势数据已落盘（%d 个 API Key → %s）", len(s.file.Keys), s.path)
	return nil
}

// Start 每小时落盘一次，并在 ctx 结束时最后落一次（进程退出不丢当日尾部数据）。
func (s *Store) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = FlushInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := s.Flush(); err != nil {
					logx.Warnf("退出前趋势数据落盘失败: %v", err)
				}
				return
			case <-t.C:
				if err := s.Flush(); err != nil {
					logx.Warnf("趋势数据落盘失败（下次仍会重试）: %v", err)
				}
			}
		}
	}()
}

// Purge 删除指定 runtime id 的全部趋势数据（用户删除 AK 时调用），返回被清掉的 id。
// 这里只做数据层的事：**确认框在前端**，后端兜底覆盖「直接 PUT / 导入配置」等绕过 UI 的路径。
func (s *Store) Purge(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []string
	for _, id := range ids {
		if _, ok := s.file.Keys[id]; !ok {
			delete(s.lastAt, id)
			continue
		}
		delete(s.file.Keys, id)
		delete(s.lastAt, id)
		removed = append(removed, id)
		s.dirty = true
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.flushLocked(); err != nil {
		// 数据已从内存剔除，只是没写下去：告警但不让调用方失败（配置保存才是主操作）
		return removed, err
	}
	return removed, nil
}

// pathsForTests 仅供包内测试观察文件形态。
func (s *Store) fileForTests() *persist.TrendsFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file
}

// ---------- 时间小工具（与 collector.FormatTime 同语义：本地时区、秒级） ----------

func fmtTime(t time.Time) string { return t.Truncate(time.Second).Format(time.RFC3339) }

func parseAt(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
