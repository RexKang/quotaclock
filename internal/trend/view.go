package trend

import (
	"sort"
	"time"

	"github.com/RexKang/quotaclock/internal/persist"
)

// 视图窗口（GET /api/trends 的入参与出参规模）。
const (
	// DefaultDays 默认回看天数：够画「年度」（12 个月柱）用，且不把文件整份吐给前端。
	DefaultDays = 400
	// MaxDays 回看天数上限（文件本身最多保留 dayKeep 天）。
	MaxDays = 3650
	// HourWindow 小时槽窗口长度（48h：够「当日」24 根柱 + 跨零点算小时差）。
	HourWindow = 48
)

// View 趋势接口的响应体。
//
// 关键约定：`daily` / `hours` 都是**定长、连续、按时间升序**的数组，缺数据的槽位是 null。
// 这样前端不需要自己对齐日期（也不会因为某个日期没数据把柱子整体错位一格），
// 且「那天没采样」与「那天是 0」在数据层就分得开。
type View struct {
	Version     int       `json:"version"`
	GeneratedAt string    `json:"generated_at"`
	Today       string    `json:"today"` // 本地时区日期：daily 的最后一个是它
	Days        int       `json:"days"`  // daily 长度
	Keys        []KeyView `json:"keys"`
}

// KeyView 单个 API Key 的趋势视图。
type KeyView struct {
	ID        string `json:"id"` // runtime id = <platform>.<keyID>
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Kind      string `json:"kind"`       // percent | balance
	FirstDay  string `json:"first_day"`  // 最早有数据的一天（"" = 还没有数据）
	TotalDays int    `json:"total_days"` // 累计有数据的天数（含窗口之外的更早数据）
	LastAt    string `json:"last_at"`

	Daily []*persist.TrendDay  `json:"daily"` // 长度 = View.Days，升序，末位 = 今天
	Hours []*persist.TrendHour `json:"hours"` // 长度 = HourWindow，升序，末位 = 当前小时
}

// Snapshot 生成 API 视图（深拷贝：返回后与 Store 内部状态解耦，调用方序列化时不会被并发 Observe 动到）。
func (s *Store) Snapshot(now time.Time, days int) *View {
	if days <= 0 {
		days = DefaultDays
	}
	if days > MaxDays {
		days = MaxDays
	}
	now = now.Truncate(time.Second)

	s.mu.Lock()
	defer s.mu.Unlock()

	v := &View{
		Version:     persist.TrendsVersion,
		GeneratedAt: fmtTime(now),
		Today:       now.Format(dateLayout),
		Days:        days,
		Keys:        make([]KeyView, 0, len(s.file.Keys)),
	}

	// 日期槽（升序，末位 = 今天）与小时槽（升序，末位 = 当前小时）
	dates := make([]string, 0, days)
	for i := days - 1; i >= 0; i-- {
		dates = append(dates, now.AddDate(0, 0, -i).Format(dateLayout))
	}
	slots := make([]string, 0, HourWindow)
	for i := HourWindow - 1; i >= 0; i-- {
		slots = append(slots, now.Add(-time.Duration(i)*time.Hour).Format(hourLayout))
	}

	ids := make([]string, 0, len(s.file.Keys))
	for id := range s.file.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 输出稳定（前端按快照顺序重排，测试按 id 断言）

	for _, id := range ids {
		k := s.file.Keys[id]
		if k == nil {
			continue
		}
		kv := KeyView{
			ID:       k.ID,
			Name:     k.Name,
			Platform: k.Platform,
			Kind:     k.Kind,
			LastAt:   k.LastAt,
			Daily:    make([]*persist.TrendDay, len(dates)),
			Hours:    make([]*persist.TrendHour, len(slots)),
		}
		byDate := make(map[string]*persist.TrendDay, len(k.Days))
		for i := range k.Days {
			d := &k.Days[i]
			byDate[d.Date] = d
			if d.Samples > 0 {
				kv.TotalDays++
				if kv.FirstDay == "" {
					kv.FirstDay = d.Date
				}
			}
		}
		for i, date := range dates {
			if d := byDate[date]; d != nil {
				kv.Daily[i] = copyDay(d)
			}
		}
		bySlot := make(map[string]*persist.TrendHour, len(k.Hours))
		for i := range k.Hours {
			h := &k.Hours[i]
			bySlot[h.Slot] = h
		}
		for i, slot := range slots {
			if h := bySlot[slot]; h != nil {
				kv.Hours[i] = copyHour(h)
			}
		}
		v.Keys = append(v.Keys, kv)
	}
	return v
}

func copyDay(d *persist.TrendDay) *persist.TrendDay {
	out := *d
	if d.Pct != nil {
		m := *d.Pct
		out.Pct = &m
	}
	if d.Balance != nil {
		b := *d.Balance
		if d.Balance.Spent != nil {
			v := *d.Balance.Spent
			b.Spent = &v
		}
		out.Balance = &b
	}
	return &out
}

func copyHour(h *persist.TrendHour) *persist.TrendHour {
	out := *h
	if h.Pct != nil {
		m := *h.Pct
		out.Pct = &m
	}
	if h.Balance != nil {
		v := *h.Balance
		out.Balance = &v
	}
	return &out
}
