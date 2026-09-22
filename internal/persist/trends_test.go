package persist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrendsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := TrendsPathOf(dir)
	if path != filepath.Join(dir, "trends.json") {
		t.Fatalf("路径 = %s", path)
	}
	// 不存在 → (nil, nil)：首次启动的正常情形
	f, err := LoadTrends(path)
	if err != nil || f != nil {
		t.Fatalf("缺失文件应返回 (nil, nil)，实际 %v / %v", f, err)
	}
	spent := 3.96
	in := &TrendsFile{
		Version:   TrendsVersion,
		WrittenAt: "2026-09-22T10:00:00+08:00",
		Keys: map[string]*TrendKey{
			"deepseek.ds": {
				ID: "deepseek.ds", Name: "DeepSeek", Platform: "deepseek", Kind: KindBalance,
				Days: []TrendDay{{
					Date: "2026-09-22", Samples: 288,
					Balance: &TrendBalance{First: 22.35, Last: 18.39, Spent: &spent, Samples: 288, Currency: "CNY"},
				}},
				Hours: []TrendHour{{Slot: "2026-09-22T09", Balance: f64(18.39)}},
			},
		},
	}
	if err := SaveTrends(path, in); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := LoadTrends(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	k := got.Keys["deepseek.ds"]
	if k == nil || k.Kind != KindBalance || len(k.Days) != 1 {
		t.Fatalf("读回内容不符: %+v", k)
	}
	if k.Days[0].Balance.Spent == nil || *k.Days[0].Balance.Spent != spent {
		t.Errorf("spent 未往返: %+v", k.Days[0].Balance)
	}
	if k.Hours[0].Balance == nil || *k.Hours[0].Balance != 18.39 {
		t.Errorf("小时余额未往返: %+v", k.Hours[0])
	}
	if got.WrittenAt != in.WrittenAt {
		t.Errorf("written_at = %q", got.WrittenAt)
	}
}

func TestLoadTrendsRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trends.json")

	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrends(path); err == nil {
		t.Error("坏 JSON 应报错（调用方 WARN 后忽略，不阻塞启动）")
	}

	if err := os.WriteFile(path, []byte(`{"version":99,"keys":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrends(path); err == nil {
		t.Error("版本不符应报错（宁可从零积累，也不猜旧结构）")
	}
}

func TestSaveTrendsNilSafe(t *testing.T) {
	if err := SaveTrends(filepath.Join(t.TempDir(), "x.json"), nil); err == nil {
		t.Error("nil 文件应报错而不是写出空文件")
	}
}

func f64(v float64) *float64 { return &v }
