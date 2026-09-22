package trend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RexKang/QuotaClock/internal/persist"
)

// ---------- 真实应答形状（取自用户实例 2026-09-22 的快照，勿手改，它们是回归基准） ----------

const (
	zhipuBody    = `{"code":200,"msg":"操作成功","data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":0},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":2,"nextResetTime":1790474452983},{"type":"TIME_LIMIT","unit":5,"number":1,"usage":1000,"currentValue":10,"remaining":990,"percentage":1}],"level":"pro"},"success":true}`
	deepseekBody = `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"18.39","granted_balance":"0.00","topped_up_balance":"18.39"}]}`
	kimiBody     = `{"usage":{"limit":"100","used":"11","remaining":"89","resetTime":"2026-09-25T08:57:38.324467Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"100","resetTime":"2026-09-22T05:57:38.324467Z"}}],"usages":{"limit_5h":{"used_ratio":0,"reset_time":"2026-09-22T05:57:37Z"},"limit_7d":{"used_ratio":0.114911,"reset_time":"2026-09-25T08:57:37Z"}}}`
	opencodeBody = `{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-09-22T03:35:14.577Z"},"weekly":{"status":"ok","percent":4,"resetsAt":"2026-09-28T00:00:00.000Z"},"monthly":{"status":"ok","percent":25,"resetsAt":"2026-10-11T01:35:18.000Z"}}}`
)

func TestExtractPlatforms(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		body     string
		wantPct  *float64
		wantBal  *float64
	}{
		{"智谱取 5 小时那档（不是周 2 / 月 1）", "zhipu-glm", zhipuBody, f(0), nil},
		{"智谱 5 小时有值时照实取", "zhipu-glm",
			`{"data":{"limits":[{"unit":3,"number":5,"percentage":12},{"unit":6,"number":1,"percentage":2}]}}`, f(12), nil},
		{"智谱缺 percentage → 不记（不写 0）", "zhipu-glm",
			`{"data":{"limits":[{"unit":3,"number":5}]}}`, nil, nil},
		{"DeepSeek 取最大一笔余额", "deepseek", deepseekBody, nil, f(18.39)},
		{"Kimi 用 5 小时窗口的 limit−remaining", "kimi-code", kimiBody, f(0), nil},
		{"Kimi 剩余 89/100 → 11%", "kimi-code",
			`{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"89"}}]}`, f(11), nil},
		{"Kimi 不接 used_ratio（拍板 260922）", "kimi-code",
			`{"limits":[],"usages":{"limit_5h":{"used_ratio":0.5}}}`, nil, nil},
		{"OpenCode 取 rolling（不是周 4 / 月 25）", "opencode", opencodeBody, f(0), nil},
		{"OpenCode 有值照实取", "opencode", `{"usage":{"rolling":{"percent":59}}}`, f(59), nil},
		{"未知平台不猜字段", "future-llm", opencodeBody, nil, nil},
		{"空 body 安全", "deepseek", ``, nil, nil},
	}
	for _, c := range cases {
		got := Extract(c.platform, json.RawMessage(c.body))
		if !eqF(got.Pct, c.wantPct) {
			t.Errorf("%s：pct = %v，期望 %v", c.name, deref(got.Pct), deref(c.wantPct))
		}
		var gotBal *float64
		if got.Balance != nil {
			gotBal = &got.Balance.Amount
		}
		if !eqF(gotBal, c.wantBal) {
			t.Errorf("%s：balance = %v，期望 %v", c.name, deref(gotBal), deref(c.wantBal))
		}
	}
}

func TestKindOf(t *testing.T) {
	if KindOf("deepseek") != persist.KindBalance {
		t.Error("DeepSeek 应为余额口径")
	}
	for _, p := range []string{"zhipu-glm", "kimi-code", "opencode"} {
		if KindOf(p) != persist.KindPercent {
			t.Errorf("%s 应为百分比口径", p)
		}
	}
}

// ---------- 聚合 ----------

func TestObserveGainIsUsageNotLevel(t *testing.T) {
	s := newStore(t)
	d := day(2026, 9, 22)
	t0 := time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local)
	// 用户给的判定：11 点 4% → 12 点 7%，这段「实际使用」是 3%（不是水平值 7%）
	for i, v := range []float64{4, 7} {
		s.Observe("opencode.k1", "OC", "opencode", t0.Add(time.Duration(i*5)*time.Minute), ocBody(v))
	}
	kd := dayOf(t, s, "opencode.k1", d)
	if kd.Pct.Peak != 7 {
		t.Errorf("峰值 = %v，期望 7（水平值另存）", kd.Pct.Peak)
	}
	if kd.Pct.Gain != 3 {
		t.Errorf("用量 = %v，期望 3（= 7 − 4）", kd.Pct.Gain)
	}
}

func TestObserveGainAcrossWindowReset(t *testing.T) {
	s := newStore(t)
	d := day(2026, 9, 22)
	t0 := time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local)
	// 用户给的例子：99 → 100 → 3，这一段应为 (100−99) + 3 = 4
	for i, v := range []float64{99, 100, 3} {
		s.Observe("opencode.k1", "OC", "opencode", t0.Add(time.Duration(i*5)*time.Minute), ocBody(v))
	}
	kd := dayOf(t, s, "opencode.k1", d)
	if kd.Pct.Gain != 4 {
		t.Errorf("用量 = %v，期望 4（= (100−99) + 3）", kd.Pct.Gain)
	}
	if kd.Pct.Resets != 1 {
		t.Errorf("重置次数 = %d，期望 1", kd.Pct.Resets)
	}
	if kd.Pct.Peak != 100 || kd.Pct.Last != 3 {
		t.Errorf("峰值/末值 = %v/%v，期望 100/3", kd.Pct.Peak, kd.Pct.Last)
	}

	// 跌幅（窗口重置）要补回「重置前填满的那段」：(100 − prev) + cur
	s2 := newStore(t)
	t1 := time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local)
	s2.Observe("opencode.k2", "OC", "opencode", t1, ocBody(40))
	s2.Observe("opencode.k2", "OC", "opencode", t1.Add(5*time.Minute), ocBody(5))
	if g := dayOf(t, s2, "opencode.k2", d).Pct.Gain; g != 65 {
		t.Errorf("40 → 5（窗口重置）用量 = %v，期望 65 = (100−40) + 5", g)
	}
}

func TestObserveGainCrossesDayAndHourBoundary(t *testing.T) {
	s := newStore(t)
	// 跨天：23:58 → 00:02 这一段用量（2%）要算进新的一天，而不是被当成「当天第一条」丢掉
	s.Observe("opencode.k1", "OC", "opencode", time.Date(2026, 9, 22, 23, 58, 0, 0, time.Local), ocBody(40))
	s.Observe("opencode.k1", "OC", "opencode", time.Date(2026, 9, 23, 0, 2, 0, 0, time.Local), ocBody(42))
	nd := dayOf(t, s, "opencode.k1", day(2026, 9, 23))
	if nd.Pct.Gain != 2 {
		t.Errorf("跨天用量 = %v，期望 2", nd.Pct.Gain)
	}
	// 跨小时：小时槽也要有用量（此前小时级 gain 从未累加，悬停里恒为 0.00%）
	s.Observe("opencode.k1", "OC", "opencode", time.Date(2026, 9, 23, 0, 7, 0, 0, time.Local), ocBody(49))
	k := s.fileForTests().Keys["opencode.k1"]
	last := k.Hours[len(k.Hours)-1]
	if last.Slot != "2026-09-23T00" {
		t.Fatalf("小时槽 = %s", last.Slot)
	}
	if last.Pct == nil || last.Pct.Gain != 9 { // 42 → 49 都在 0 点这一小时内
		t.Errorf("当小时用量 = %v，期望 9", last.Pct)
	}
	if last.Pct.Peak != 49 {
		t.Errorf("当小时峰值 = %v，期望 49", last.Pct.Peak)
	}
}

func TestObserveGapAndIdempotence(t *testing.T) {
	s := newStore(t)
	d := day(2026, 9, 22)
	t0 := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
	s.Observe("opencode.k1", "OC", "opencode", t0, ocBody(10))
	s.Observe("opencode.k1", "OC", "opencode", t0, ocBody(10)) // 同一份成功结果重复投递
	s.Observe("opencode.k1", "OC", "opencode", t0.Add(-5*time.Minute), ocBody(99))
	kd := dayOf(t, s, "opencode.k1", d)
	if kd.Samples != 1 {
		t.Fatalf("重复投递与乱序样本都该被丢弃，实际 samples = %d", kd.Samples)
	}
	// 2 小时后 → 记一次缺口（进程没跑）
	s.Observe("opencode.k1", "OC", "opencode", t0.Add(2*time.Hour), ocBody(30))
	kd = dayOf(t, s, "opencode.k1", d)
	if kd.Gaps != 1 {
		t.Errorf("间隔 2 小时应记 1 次缺口，实际 %d", kd.Gaps)
	}
}

func TestBalanceSpentTopupPartialGap(t *testing.T) {
	t.Run("完整一天：首尾差即当日消费", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
		s.Observe("deepseek.ds", "DS", "deepseek", base, dsBody(22.35))
		s.Observe("deepseek.ds", "DS", "deepseek", base.Add(time.Hour), dsBody(18.39))
		b := dayOf(t, s, "deepseek.ds", day(2026, 9, 22)).Balance
		if b == nil || b.Spent == nil || round2(*b.Spent) != 3.96 {
			t.Fatalf("spent = %v，期望 3.96", b)
		}
		if b.Topup || b.Partial {
			t.Errorf("不该标充值/不完整：%+v", b)
		}
		if b.Currency != "CNY" {
			t.Errorf("币种 = %q，期望 CNY", b.Currency)
		}
	})
	t.Run("充值 → 差值转正、不出值", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
		s.Observe("deepseek.ds", "DS", "deepseek", base, dsBody(18.39))
		s.Observe("deepseek.ds", "DS", "deepseek", base.Add(time.Hour), dsBody(30.00))
		b := dayOf(t, s, "deepseek.ds", day(2026, 9, 22)).Balance
		if !b.Topup || b.Spent != nil {
			t.Errorf("充值应标 Topup 且不出值：%+v", b)
		}
	})
	t.Run("进程中途启动 → 当日不完整、不出值", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2026, 9, 22, 9, 0, 0, 0, time.Local)
		s.Observe("deepseek.ds", "DS", "deepseek", base, dsBody(22.35))
		s.Observe("deepseek.ds", "DS", "deepseek", base.Add(time.Hour), dsBody(18.39))
		b := dayOf(t, s, "deepseek.ds", day(2026, 9, 22)).Balance
		if !b.Partial || b.Spent != nil {
			t.Errorf("零点到启动之间的消费无从得知 → 应标 Partial 且不出值：%+v", b)
		}
	})
	t.Run("采样缺口 → 不出值", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
		s.Observe("deepseek.ds", "DS", "deepseek", base, dsBody(22.35))
		s.Observe("deepseek.ds", "DS", "deepseek", base.Add(4*time.Hour), dsBody(18.39))
		d := dayOf(t, s, "deepseek.ds", day(2026, 9, 22))
		if d.Gaps != 1 || d.Balance.Spent != nil {
			t.Errorf("有缺口时首尾差不等于当日消费：gaps=%d spent=%v", d.Gaps, d.Balance.Spent)
		}
	})
}

func TestDayRolloverAndHours(t *testing.T) {
	s := newStore(t)
	d1 := time.Date(2026, 9, 22, 23, 55, 0, 0, time.Local)
	s.Observe("opencode.k1", "OC", "opencode", d1, ocBody(20))
	s.Observe("opencode.k1", "OC", "opencode", time.Date(2026, 9, 23, 0, 5, 0, 0, time.Local), ocBody(30))
	s.Observe("opencode.k1", "OC", "opencode", time.Date(2026, 9, 23, 0, 10, 0, 0, time.Local), ocBody(35))

	k := s.fileForTests().Keys["opencode.k1"]
	if len(k.Days) != 2 {
		t.Fatalf("跨零点应产生 2 条日记录，实际 %d", len(k.Days))
	}
	if k.Days[0].Date != "2026-09-22" || k.Days[1].Date != "2026-09-23" {
		t.Errorf("日期 = %s / %s", k.Days[0].Date, k.Days[1].Date)
	}
	if k.Days[0].Pct.Peak != 20 || k.Days[1].Pct.Peak != 35 {
		t.Errorf("跨零点峰值串档：%v / %v", k.Days[0].Pct.Peak, k.Days[1].Pct.Peak)
	}
	if k.Days[1].Balance != nil {
		t.Error("百分比口径不该产生余额字段")
	}
	if len(k.Hours) != 2 {
		t.Fatalf("小时槽 = %d，期望 2（23 时与 0 时）", len(k.Hours))
	}
	if k.Hours[0].Slot != "2026-09-22T23" || k.Hours[1].Slot != "2026-09-23T00" {
		t.Errorf("小时槽 = %s / %s", k.Hours[0].Slot, k.Hours[1].Slot)
	}
}

func TestSnapshotContiguousSlotsWithNulls(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 9, 22, 10, 30, 0, 0, time.Local)
	s.Observe("zhipu-glm.z1", "智谱 GLM", "zhipu-glm", now.Add(-24*time.Hour), zhipuRaw(46))
	s.Observe("kimi-code.m1", "Kimi", "kimi-code", now, kimiRaw(11))

	v := s.Snapshot(now, 7)
	if len(v.Keys) != 2 {
		t.Fatalf("keys = %d，期望 2（按 id 升序：kimi-code.m1 / zhipu-glm.z1）", len(v.Keys))
	}
	if v.Today != "2026-09-22" || v.Days != 7 {
		t.Errorf("today/days = %s/%d", v.Today, v.Days)
	}
	for _, k := range v.Keys {
		if len(k.Daily) != 7 {
			t.Errorf("%s：daily 长度 = %d，期望 7（定长连续，前端不必自己对齐日期）", k.ID, len(k.Daily))
		}
		if len(k.Hours) != 48 {
			t.Errorf("%s：hours 长度 = %d，期望 48", k.ID, len(k.Hours))
		}
		if k.Daily[6] == nil && k.ID == "kimi-code.m1" {
			t.Error("今天那条不该为空")
		}
	}
	z := v.Keys[1]
	if z.FirstDay != "2026-09-21" || z.TotalDays != 1 || z.Kind != persist.KindPercent {
		t.Errorf("zhipu 视图元信息 = %+v", z)
	}
	if z.Daily[5] == nil || z.Daily[6] != nil {
		t.Error("只有 09-21 那格该有数据（09-20 更早、09-22 今天都没采到）")
	}
	if z.Hours[48-1] != nil {
		t.Error("zhipu 当前小时没有样本 → 该槽必须是 null（不是 0）")
	}
	if v.Keys[0].Kind != persist.KindPercent || len(v.Keys[0].Daily) != 7 {
		t.Errorf("kimi 视图异常：%+v", v.Keys[0])
	}
}

// ---------- 落盘 / 重启 / 清理 ----------

func TestFlushPersistsAndReloads(t *testing.T) {
	s, path := newTestStore(t)
	at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
	s.Observe("zhipu-glm.z1", "智谱 GLM", "zhipu-glm", at, zhipuRaw(12))
	if err := s.Flush(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("trends.json 未写出: %v", err)
	}
	// 重启：新 Store 读同一份文件，历史还在
	s2 := New(path)
	if s2.Loaded() != 1 {
		t.Fatalf("重启后应加载 1 个 Key，实际 %d", s2.Loaded())
	}
	k := s2.fileForTests().Keys["zhipu-glm.z1"]
	if k == nil || len(k.Days) != 1 || k.Days[0].Pct.Peak != 12 {
		t.Fatalf("重启后历史丢失：%+v", k)
	}
	if k.FirstAt == "" || k.LastAt == "" {
		t.Errorf("首末采样时间应记录：%q / %q", k.FirstAt, k.LastAt)
	}
	// 重启后的第一条样本仍按 gap 判定（不把停机时长当成正常间隔）
	s2.Observe("zhipu-glm.z1", "智谱 GLM", "zhipu-glm", at.Add(3*time.Hour), zhipuRaw(20))
	if len(s2.fileForTests().Keys["zhipu-glm.z1"].Days) != 1 {
		t.Error("同一天的第二段样本应并入当天")
	}
	if s2.fileForTests().Keys["zhipu-glm.z1"].Days[0].Gaps != 1 {
		t.Error("停机 3 小时应记为一次缺口")
	}
}

func TestFlushSkipsWhenClean(t *testing.T) {
	s, path := newTestStore(t)
	at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
	s.Flush()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("无变动时不该写文件")
	}
	s.Observe("opencode.k1", "OC", "opencode", at, ocBody(5))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("无新变动时不该重写文件（避免每小时无谓落盘）")
	}
	s.Observe("opencode.k1", "OC", "opencode", at.Add(time.Minute), ocBody(6))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("有新变动应重新落盘")
	}
}

func TestPurgeRemovesGivenKeysOnly(t *testing.T) {
	s, path := newTestStore(t)
	at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
	s.Observe("opencode.k1", "OC1", "opencode", at, ocBody(5))
	s.Observe("opencode.k2", "OC2", "opencode", at, ocBody(9))
	removed, err := s.Purge([]string{"opencode.k1", "opencode.none"})
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if len(removed) != 1 || removed[0] != "opencode.k1" {
		t.Fatalf("removed = %v，期望只含 opencode.k1", removed)
	}
	if _, ok := s.fileForTests().Keys["opencode.k1"]; ok {
		t.Error("内存里仍留有被删 Key")
	}
	f, err := persist.LoadTrends(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Keys["opencode.k1"]; ok {
		t.Error("文件里仍留有被删 Key（用户要求删除 AK 时连带清理）")
	}
	if _, ok := f.Keys["opencode.k2"]; !ok {
		t.Error("未删的 Key 不该受影响")
	}
}

func TestObserveUnknownPlatformIsDropped(t *testing.T) {
	s := newStore(t)
	s.Observe("future-llm.x", "X", "future-llm", time.Now(), json.RawMessage(`{"any":1}`))
	if s.Loaded() != 0 {
		t.Error("认不出字段的平台不该创建空趋势记录")
	}
}

// ---------- 小工具 ----------

func newStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	return s
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), persist.TrendsName)
	return New(path), path
}

func day(y int, m time.Month, d int) string {
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local).Format("2006-01-02")
}

func dayOf(t *testing.T, s *Store, id, date string) *persist.TrendDay {
	t.Helper()
	k := s.fileForTests().Keys[id]
	if k == nil {
		t.Fatalf("没有 %s 的趋势记录", id)
	}
	for i := range k.Days {
		if k.Days[i].Date == date {
			return &k.Days[i]
		}
	}
	t.Fatalf("%s 没有 %s 的记录", id, date)
	return nil
}

func ocBody(percent float64) json.RawMessage {
	return json.RawMessage(`{"usage":{"rolling":{"percent":` + fstr(percent) + `}}}`)
}

func zhipuRaw(percent float64) json.RawMessage {
	return json.RawMessage(`{"data":{"limits":[{"unit":3,"number":5,"percentage":` + fstr(percent) + `}]}}`)
}

func kimiRaw(used float64) json.RawMessage {
	return json.RawMessage(`{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"` + fstr(100-used) + `"}}]}`)
}

func dsBody(total float64) json.RawMessage {
	return json.RawMessage(`{"balance_infos":[{"currency":"CNY","total_balance":"` + fstr(total) + `"}]}`)
}

func f(x float64) *float64 { return &x }

func fstr(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func deref(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fstr(*p)
}

func eqF(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return round2(*a) == round2(*b)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5*sign(v))) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
