package lock

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/RexKang/quotaclock/internal/persist"
	"strings"
	"testing"
)

func writeStaleLock(t *testing.T, dir, cfgName string, info Info) string {
	t.Helper()
	cfgPath := filepath.Join(dir, cfgName)
	lockPath := filepath.Join(dir, persist.LockNameOf(cfgPath))
	b, _ := json.Marshal(info)
	if err := os.WriteFile(lockPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestAcquireConflictAlive(t *testing.T) { // C-lock-01
	dir := t.TempDir()
	cfg := writeStaleLock(t, dir, "config.json", Info{PID: 4242, Start: 1757050000})
	probe := func(pid int, start int64) (bool, error) { return true, nil }
	_, err := Acquire(cfg, probe)
	if err == nil {
		t.Fatal("存活实例冲突未报错")
	}
	var already *AlreadyRunningError
	if !asAlready(err, &already) || already.PID != 4242 {
		t.Fatalf("错误应含 PID: %v", err)
	}
	if !strings.Contains(err.Error(), "已在运行 (PID 4242)") {
		t.Fatalf("文案错误: %v", err)
	}
}

func TestAcquireStaleTakeover(t *testing.T) { // C-lock-02
	dir := t.TempDir()
	cfg := writeStaleLock(t, dir, "config.json", Info{PID: 4242, Start: 1757050000})
	probe := func(pid int, start int64) (bool, error) { return false, nil } // 报告已死
	h, err := Acquire(cfg, probe)
	if err != nil {
		t.Fatalf("stale 应接管: %v", err)
	}
	// 接管后锁内容 = 本进程
	b, _ := os.ReadFile(h.Path())
	var cur Info
	_ = json.Unmarshal(b, &cur)
	if cur.PID != os.Getpid() {
		t.Fatalf("锁内 PID = %d", cur.PID)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Path()); !os.IsNotExist(err) {
		t.Fatal("释放后锁文件仍在")
	}
}

func TestAcquirePIDReuse(t *testing.T) { // C-lock-03：进程活但创建时间不符 → stale
	dir := t.TempDir()
	cfg := writeStaleLock(t, dir, "config.json", Info{PID: os.Getpid(), Start: 1111111111})
	probe := func(pid int, start int64) (bool, error) {
		if pid == os.Getpid() && start != selfStartUnix() {
			return false, nil // 创建时间不匹配 → 视为 stale
		}
		return true, nil
	}
	if _, err := Acquire(cfg, probe); err != nil {
		t.Fatalf("PID 复用应接管: %v", err)
	}
}

func TestAcquireProbeErrorConservative(t *testing.T) { // C-lock-04：探活报错 → 保守按存活
	dir := t.TempDir()
	cfg := writeStaleLock(t, dir, "config.json", Info{PID: 4242, Start: 1})
	probe := func(pid int, start int64) (bool, error) { return false, os.ErrPermission }
	_, err := Acquire(cfg, probe)
	if err == nil {
		t.Fatal("探活报错应保守按存活（拒绝启动）")
	}
	var already *AlreadyRunningError
	if !asAlready(err, &already) {
		t.Fatalf("应归为已在运行: %v", err)
	}
}

func TestAcquireCorruptLock(t *testing.T) { // 内容损坏 → stale 接管
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	lockPath := filepath.Join(dir, persist.LockNameOf(cfgPath))
	if err := os.WriteFile(lockPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(cfgPath, nil); err != nil {
		t.Fatalf("损坏锁应接管: %v", err)
	}
}

func TestTwoConfigsCoexist(t *testing.T) { // C-lock-05：两套配置 → 两个锁名，先后均成功
	dir := t.TempDir()
	probe := func(pid int, start int64) (bool, error) { return false, nil }
	h1, err := Acquire(filepath.Join(dir, "config.json"), probe)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Acquire(filepath.Join(dir, "config2.json"), probe)
	if err != nil {
		t.Fatal(err)
	}
	if h1.Path() == h2.Path() {
		t.Fatal("两套配置锁名应不同")
	}
	_ = h1.Release()
	_ = h2.Release()
}

func TestRelease(t *testing.T) { // C-lock-06
	dir := t.TempDir()
	h, err := Acquire(filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Path()); !os.IsNotExist(err) {
		t.Fatal("锁文件未删除")
	}
	// 幂等
	if err := h.Release(); err != nil {
		t.Fatalf("重复释放应幂等: %v", err)
	}
}

func TestCrashResidueTakeover(t *testing.T) { // C-lock-07：崩溃残留（不调释放）→ 重跑接管
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	h1, err := Acquire(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：句柄丢弃不释放
	_ = h1
	// 本进程还活着 → 第二次 Acquire 应报已在运行（同 PID 同 start）
	_, err = Acquire(cfgPath, nil)
	if err == nil {
		t.Fatal("同进程二次加锁应被拒")
	}
	// 模拟崩溃后重启：注入 stale 探活 → 接管
	probe := func(pid int, start int64) (bool, error) { return false, nil }
	h2, err := Acquire(cfgPath, probe)
	if err != nil {
		t.Fatalf("崩溃残留应接管: %v", err)
	}
	_ = h2.Release()
}

func TestRealProbeSelf(t *testing.T) { // 真实探活自检（Windows/Linux）：自己必然存活且时间匹配
	if runtimeGOOS != "windows" && runtimeGOOS != "linux" {
		t.Skip("仅 Windows/Linux 有创建时间比对")
	}
	start := selfStartUnix()
	if start <= 0 {
		t.Fatal("selfStartUnix 应返回有效值")
	}
	alive, err := probeAlive(os.Getpid(), start)
	if err != nil || !alive {
		t.Fatalf("自探活失败: alive=%v err=%v", alive, err)
	}
	// 时间不匹配（模拟 PID 复用）→ stale
	alive, err = probeAlive(os.Getpid(), start+12345)
	if err != nil || alive {
		t.Fatalf("时间不匹配应判 stale: alive=%v err=%v", alive, err)
	}
	// 不存在的 PID → 已死
	alive, err = probeAlive(4000000000, start)
	if err != nil || alive {
		t.Fatalf("不存在 PID 应判死: alive=%v err=%v", alive, err)
	}
}
