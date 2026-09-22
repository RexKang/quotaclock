package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TrendsName 用量趋势数据文件（v0.4.0 新增白名单项，写盘白名单第 5 类）。
// 只存**聚合后的指标**（每日/每小时一条），不含任何 token/密文，也不含原始应答。
const TrendsName = "trends.json"

// TrendsVersion 趋势文件 schema 版本（与 config version 独立演进：
// 不匹配即忽略并从头积累，不阻塞启动——与 cache.json 同一处置原则）。
const TrendsVersion = 1

// TrendsFile 趋势文件落盘形态。
type TrendsFile struct {
	Version   int                  `json:"version"`
	WrittenAt string               `json:"written_at"` // RFC3339 本地时区秒级
	Keys      map[string]*TrendKey `json:"keys"`       // runtime id → 该 API Key 的趋势
}

// TrendKind 指标口径：percent = 5 小时额度百分比；balance = 账户余额（元）。
type TrendKind = string

const (
	KindPercent TrendKind = "percent"
	KindBalance TrendKind = "balance"
)

// TrendKey 单个 API Key 的趋势记录（键 = runtime id `<platform>.<keyID>`）。
//
// 为什么按 runtime id 而不是名称：改名是纯展示操作（v0.2.5 既定语义），
// 以名称为键会让一次改名把历史断成两截；id 只随「删除该 Key」消失。
type TrendKey struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Platform string      `json:"platform"`
	Kind     TrendKind   `json:"kind"`
	FirstAt  string      `json:"first_at,omitempty"`
	LastAt   string      `json:"last_at,omitempty"`
	Days     []TrendDay  `json:"days"`  // 按日期升序，长期保留（上限见 trend.dayKeep 天）
	Hours    []TrendHour `json:"hours"` // 按小时槽升序，滚动保留（供「当日」视图）
}

// TrendDay 某一天的聚合：采样点数、缺口数、以及当日峰值/增量与余额差。
type TrendDay struct {
	Date    string        `json:"date"` // YYYY-MM-DD（本地时区）
	Samples int           `json:"samples"`
	Gaps    int           `json:"gaps"` // 采样间隔超过阈值（进程没跑/采集失败）的次数
	Pct     *TrendMetric  `json:"pct,omitempty"`
	Balance *TrendBalance `json:"balance,omitempty"`
}

// TrendMetric 额度类指标（百分比）的当日/当小时聚合。
// Peak/Last/Gain 都是「该区间内观测到的事实」，不引入任何外推。
type TrendMetric struct {
	Peak    float64 `json:"peak"` // 区间峰值：当日最紧到过多少（主口径，可精确复算）
	Last    float64 `json:"last"` // 区间最后一个样本值
	Gain    float64 `json:"gain"` // 区间增量估计：相邻样本 max(0,Δ) 之和（跨窗口重置只能估）
	Samples int     `json:"samples"`
}

// TrendBalance 余额类指标（DeepSeek）的当日聚合。
// Spent 为 nil 表示「不出值」——首样本不在当日零点（进程中途启动）或当日有采样缺口/充值，
// 此时首尾差不再等于当日消费，宁可不画也不给一个错的数字（前端按 Partial/Topup/Gaps 注原因）。
type TrendBalance struct {
	First    float64  `json:"first"`
	Last     float64  `json:"last"`
	Spent    *float64 `json:"spent"`
	Topup    bool     `json:"topup,omitempty"`
	Partial  bool     `json:"partial,omitempty"`
	Samples  int      `json:"samples"`
	Currency string   `json:"currency,omitempty"`
}

// TrendHour 单小时槽的聚合（当日视图；余额类存该小时最后一个余额，消费由前端做小时差）。
type TrendHour struct {
	Slot    string       `json:"slot"` // YYYY-MM-DDTHH（本地时区）
	Pct     *TrendMetric `json:"pct,omitempty"`
	Balance *float64     `json:"balance,omitempty"`
}

// TrendsPathOf 返回目录下的 trends.json 路径。
func TrendsPathOf(dir string) string { return filepath.Join(dir, TrendsName) }

// LoadTrends 读取趋势文件：
//   - 不存在 → (nil, nil)（首次启动的正常情形，从零开始积累）
//   - 解析失败 / 版本不符 → (nil, err)：调用方 WARN 后忽略即可，坏文件**不得**阻塞启动
func LoadTrends(path string) (*TrendsFile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取趋势数据失败: %s: %w", path, err)
	}
	var f TrendsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("趋势数据 JSON 解析失败: %s: %w", path, err)
	}
	if f.Version != TrendsVersion {
		return nil, fmt.Errorf("趋势数据版本 %d 不受支持（当前 %d）: %s", f.Version, TrendsVersion, path)
	}
	if f.Keys == nil {
		f.Keys = map[string]*TrendKey{}
	}
	return &f, nil
}

// SaveTrends 原子写趋势文件（0600，与配置同权限约定）。
func SaveTrends(path string, f *TrendsFile) error {
	if f == nil {
		return fmt.Errorf("趋势数据为空")
	}
	if f.Keys == nil {
		f.Keys = map[string]*TrendKey{}
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("趋势数据序列化失败: %w", err)
	}
	return AtomicWrite(path, data)
}
