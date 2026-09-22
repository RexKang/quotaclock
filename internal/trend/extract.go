package trend

import (
	"encoding/json"
	"strconv"
)

// 提取器：从平台应答里挖出「要记进趋势的那一个数」。
//
// 为什么不复用前端的通用 walker：那个 walker 是为「卡片尽量显示出额度」写的宽容探索，
// 而趋势要的是**同一口径逐日可比**的序列——宽容探索会把不同字段混进同一条曲线。
// 这里改为**显式平台映射**（4 家一一列出，与 web/index.html 的取值口径对齐）：

// 口径变更时，请同步 web/index.html 的 extractQuotaItems（前端卡片口径）。

// 值提取结果：两者都可为 nil（该平台该字段这次没给）。
type Extracted struct {
	Pct     *float64    // 5 小时额度使用百分比（0~100+）
	Balance *BalanceVal // 账户余额（当前只有 DeepSeek）
}

// BalanceVal 余额快照（金额 + 币种）。
type BalanceVal struct {
	Amount   float64
	Currency string
}

// Extract 按平台取数；platform 认不出时返回空（不猜字段，宁可不记）。
func Extract(platform string, data json.RawMessage) Extracted {
	if len(data) == 0 {
		return Extracted{}
	}
	switch platform {
	case "zhipu-glm":
		return ExtractZhipu(data)
	case "deepseek":
		return ExtractDeepSeek(data)
	case "kimi-code":
		return ExtractKimi(data)
	case "opencode":
		return ExtractOpenCode(data)
	}
	return Extracted{}
}

// KindOf 该平台的指标口径：余额型只有 DeepSeek，其余都是 5 小时额度百分比。
func KindOf(platform string) string {
	if platform == "deepseek" {
		return "balance"
	}
	return "percent"
}

// ExtractZhipu 智谱：data.data.limits[] 里 unit=3（5 小时，见前端 zhipuUnitLabel）那条的 percentage。
// 实例应答：{"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":0}, ...]}}
// 5 小时额度**是整数百分比**（token 口径）——照实记录，不放大成假小数。
func ExtractZhipu(data json.RawMessage) Extracted {
	var body struct {
		Data struct {
			Limits []struct {
				Type       string `json:"type"`
				Unit       any    `json:"unit"`
				Number     any    `json:"number"`
				Percentage any    `json:"percentage"`
			} `json:"limits"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &body) != nil {
		return Extracted{}
	}
	var found *float64
	for i := range body.Data.Limits {
		l := &body.Data.Limits[i]
		if int(toNum(l.Unit)) != 3 {
			continue
		}
		p := toNumPtr(l.Percentage)
		if p == nil {
			continue
		}
		// number=5 的那条优先（unit=3 的语义就是「5 小时」，number 是冗余确认）
		if int(toNum(l.Number)) == 5 {
			v := *p
			found = &v
			break
		}
		if found == nil {
			found = p
		}
	}
	return Extracted{Pct: found}
}

// ExtractDeepSeek 深度求索：balance_infos[] 取**数值最大**的一笔当总余额
// （total = granted + topped_up，与前端 balanceOf 的取法一致）。
func ExtractDeepSeek(data json.RawMessage) Extracted {
	var body struct {
		BalanceInfos []struct {
			Currency       string `json:"currency"`
			TotalBalance   any    `json:"total_balance"`
			GrantedBalance any    `json:"granted_balance"`
			ToppedUpBal    any    `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if json.Unmarshal(data, &body) != nil {
		return Extracted{}
	}
	var best *BalanceVal
	for i := range body.BalanceInfos {
		bi := &body.BalanceInfos[i]
		v := toNumPtr(bi.TotalBalance)
		if v == nil {
			// 只有分项时按 total = granted + topped 口径兜底
			g, t := toNumPtr(bi.GrantedBalance), toNumPtr(bi.ToppedUpBal)
			if g == nil && t == nil {
				continue
			}
			sum := 0.0
			if g != nil {
				sum += *g
			}
			if t != nil {
				sum += *t
			}
			v = &sum
		}
		if best == nil || *v > best.Amount {
			best = &BalanceVal{Amount: *v, Currency: bi.Currency}
		}
	}
	return Extracted{Balance: best}
}

// ExtractKimi Kimi Code：limits[] 里 window.duration=300 MINUTE（= 5 小时）那条，
// 用 (limit − remaining) ÷ limit 求百分比——**不用 usages.*.used_ratio**（用户 260922 拍板不接，
// 接了会把卡片口径也带成小数，与其余平台不一致）。
func ExtractKimi(data json.RawMessage) Extracted {
	var body struct {
		Limits []struct {
			Window struct {
				Duration any    `json:"duration"`
				TimeUnit string `json:"timeUnit"`
			} `json:"window"`
			Detail struct {
				Limit     any `json:"limit"`
				Remaining any `json:"remaining"`
			} `json:"detail"`
		} `json:"limits"`
	}
	if json.Unmarshal(data, &body) != nil {
		return Extracted{}
	}
	for i := range body.Limits {
		l := &body.Limits[i]
		if !is5hWindow(toNum(l.Window.Duration), l.Window.TimeUnit) {
			continue
		}
		limit, remaining := toNumPtr(l.Detail.Limit), toNumPtr(l.Detail.Remaining)
		if limit == nil || *limit <= 0 || remaining == nil {
			continue
		}
		v := (*limit - *remaining) / *limit * 100
		if v < 0 {
			v = 0
		}
		return Extracted{Pct: &v}
	}
	return Extracted{}
}

func is5hWindow(duration float64, unit string) bool {
	u := unit
	if len(u) >= 6 && u[len(u)-6:] == "MINUTE" {
		return duration == 300
	}
	if len(u) >= 4 && u[len(u)-4:] == "HOUR" {
		return duration == 5
	}
	return false
}

// ExtractOpenCode：usage.rolling.percent（rolling = 5 小时滚动窗口，见前端 periodOf 的 rolling 映射）。
func ExtractOpenCode(data json.RawMessage) Extracted {
	var body struct {
		Usage struct {
			Rolling struct {
				Percent any `json:"percent"`
			} `json:"rolling"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &body) != nil {
		return Extracted{}
	}
	return Extracted{Pct: toNumPtr(body.Usage.Rolling.Percent)}
}

// ---------- 数值宽松转换（对应前端 toN：接口时而给数字时而给字符串） ----------

func toNum(v any) float64 {
	if f := toNumPtr(v); f != nil {
		return *f
	}
	return 0
}

func toNumPtr(v any) *float64 {
	switch x := v.(type) {
	case nil:
		return nil
	case float64:
		return &x
	case int:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	case string:
		s := x
		if s == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return &f
		}
	}
	return nil
}
