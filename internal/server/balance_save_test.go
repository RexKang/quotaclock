package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/config"
)

// TestSaveConfigKeepsBalance（v0.3.0 回归）：PUT 带 balance 必须落盘。
// 起因：mergeForSave 重建 File 时漏了 Balance 字段，保存一次就把用户的
// 满额/阈值写回 0（GET 视图因 Normalize 看不出问题，只有读文件才发现）。
func TestSaveConfigKeepsBalance(t *testing.T) {
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)

	body := validPutBody(env)
	body["balance"] = map[string]any{"full": 200, "green_pct": 60, "warn_pct": 25}
	code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != http.StatusOK {
		t.Fatalf("PUT 失败: %d", code)
	}

	// 1) 落盘文件里必须有（本用例重点）
	raw, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatalf("读配置文件失败: %v", err)
	}
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("配置文件解析失败: %v", err)
	}
	if f.Balance.Full != 200 || f.Balance.GreenPct != 60 || f.Balance.WarnPct != 25 {
		t.Errorf("balance 未落盘：%+v（期望 200/60/25）", f.Balance)
	}
	if f.Version != config.CurrentVersion {
		t.Errorf("落盘版本 = %d，期望 %d", f.Version, config.CurrentVersion)
	}

	// 2) 视图也必须一致
	code, view, _ := env.doJSON("GET", "/api/config", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/config 失败: %d", code)
	}
	bal, _ := view["balance"].(map[string]any)
	if bal == nil || bal["full"] != float64(200) || bal["green_pct"] != float64(60) || bal["warn_pct"] != float64(25) {
		t.Errorf("视图 balance = %v（期望 200/60/25）", view["balance"])
	}
}

// TestSaveConfigBalanceContract：余额设置的校验与规范化契约（R13）。
//   - full=0 视为「未设置」→ 不报错、整体回落默认（100/50/20）
//   - warn == green 允许（那就是两档）
//   - 越界/倒挂一律 400
func TestSaveConfigBalanceContract(t *testing.T) {
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)

	cases := []struct {
		name string
		bal  map[string]any
		want int
	}{
		{"未设置（0）回落默认", map[string]any{"full": 0, "green_pct": 0, "warn_pct": 0}, http.StatusOK},
		{"两档（warn == green）", map[string]any{"full": 100, "green_pct": 60, "warn_pct": 60}, http.StatusOK},
		{"绿档越界", map[string]any{"full": 100, "green_pct": 120, "warn_pct": 20}, http.StatusBadRequest},
		{"黄档为负", map[string]any{"full": 100, "green_pct": 50, "warn_pct": -1}, http.StatusBadRequest},
		{"黄高于绿", map[string]any{"full": 100, "green_pct": 20, "warn_pct": 80}, http.StatusBadRequest},
		{"满额为负", map[string]any{"full": -5, "green_pct": 50, "warn_pct": 20}, http.StatusOK}, // 负数同样按未设置处理
	}
	for _, c := range cases {
		body := validPutBody(env)
		body["balance"] = c.bal
		code, _, _ := env.doJSON("PUT", "/api/config", body, authHdr(ck))
		if code != c.want {
			t.Errorf("%s：期望 %d，实际 %d", c.name, c.want, code)
			continue
		}
		if code != http.StatusOK {
			continue
		}
		raw, err := os.ReadFile(env.configPath)
		if err != nil {
			t.Fatalf("读配置文件失败: %v", err)
		}
		var f config.File
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("配置文件解析失败: %v", err)
		}
		if f.Balance.Full <= 0 {
			t.Errorf("%s：落盘后 full 仍为 %v（应已规范化）", c.name, f.Balance.Full)
		}
	}
}

// TestPutOlderVersionAccepted（r5）：跨机器搬运的核心路径——
// 源机器可能是旧版，导出文件里带的是那台机器的版本号（如 v0.2.5 = 4），
// 目标机器必须直接吃下：按「旧版配置导入」处理，补默认值 + 覆盖前备份，落盘升到当前版本。
func TestPutOlderVersionAccepted(t *testing.T) {
	env := newEnv(t, nil)
	ck := env.login(config.DefaultPassword)
	if config.CurrentVersion <= 1 {
		t.Skip("当前版本已是最低，无非当前版本可测")
	}
	bakPath := fmt.Sprintf("%s.v%d.bak", env.configPath, config.CurrentVersion)

	body := validPutBody(env)
	body["version"] = config.CurrentVersion - 1
	delete(body, "balance") // 旧版不会有 balance 段
	code, _, resp := env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != http.StatusOK {
		t.Fatalf("旧版本 PUT 应被接受，实际 %d：%v", code, resp)
	}
	raw, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.Version != config.CurrentVersion {
		t.Errorf("落盘版本 = %d，期望升级为 %d", f.Version, config.CurrentVersion)
	}
	if f.Balance != config.DefaultBalance() {
		t.Errorf("旧版没有 balance 段 → 应补默认 %+v，实际 %+v", config.DefaultBalance(), f.Balance)
	}
	if _, err := os.Stat(bakPath); err != nil {
		t.Errorf("覆盖前应备份到 %s：%v", bakPath, err)
	}
	if !strings.Contains(env.logBuf.String(), "旧版配置导入") {
		t.Errorf("应有「旧版配置导入」日志，实际日志尾部：%s", tailLines(env.logBuf.String(), 3))
	}

	// 高于当前版本依旧拒绝
	body["version"] = config.CurrentVersion + 1
	code, _, _ = env.doJSON("PUT", "/api/config", body, authHdr(ck))
	if code != http.StatusBadRequest {
		t.Errorf("未来版本应被拒，实际 %d", code)
	}
}

// tailLines 取日志最后 n 行（失败信息里给点上下文）。
func tailLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " / ")
}
