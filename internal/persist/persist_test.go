package persist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/crypto"
)

func TestAtomicWriteNormal(t *testing.T) { // C-per-01
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(target, []byte(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	if string(b) != `{"new":true}` {
		t.Fatalf("目标内容错误: %q", b)
	}
	if _, err := os.Stat(target + TmpSuffix); !os.IsNotExist(err) {
		t.Fatal("存在 tmp 残留")
	}
}

func TestAtomicWriteInterrupted(t *testing.T) { // C-per-02 rename 前失败：目标完整（旧值），tmp 残留
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("old-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := atomicWriteWith(target, []byte("new-value"), func(o, n string) error { return os.ErrPermission })
	if err == nil {
		t.Fatal("注入失败未返回错误")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "old-value" {
		t.Fatalf("目标被破坏: %q", b)
	}
	if _, err := os.Stat(target + TmpSuffix); err != nil {
		t.Fatal("tmp 应残留（下次启动清理）")
	}
}

func TestCleanTmp(t *testing.T) { // C-per-03
	dir := t.TempDir()
	junk := []string{"config.json.tmp", "a.tmp", "b.tmp.tmp"}
	for _, j := range junk {
		if err := os.WriteFile(filepath.Join(dir, j), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanTmp(dir); err != nil {
		t.Fatal(err)
	}
	for _, j := range junk {
		if _, err := os.Stat(filepath.Join(dir, j)); !os.IsNotExist(err) {
			t.Fatalf("%s 未清理", j)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.json")); err != nil {
		t.Fatal("非 tmp 文件被误删")
	}
}

func TestTemplate(t *testing.T) { // C-per-04
	tpl, err := Template()
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Version != config.CurrentVersion {
		t.Fatalf("version = %d", tpl.Version)
	}
	if tpl.Listen != config.DefaultListen() || tpl.Collector != config.DefaultCollector() {
		t.Fatal("listen/collector 应为默认值")
	}
	if tpl.Auth.Mode != config.AuthModeAdmin || len(tpl.Providers) != 0 {
		t.Fatal("模板应为 admin + 空 providers")
	}
	if !config.IsDefaultPassword(tpl.Auth.PasswordHash) {
		t.Fatal("模板应含默认密码 hash")
	}
}

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileFutureVersionRejected(t *testing.T) { // C-per-05
	dir := t.TempDir()
	p := writeConfig(t, dir, `{"version":6,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"admin"},"providers":[]}`)
	_, err := LoadFile(p)
	if err == nil {
		t.Fatal("version=6（高于当前）应拒绝")
	}
	if !strings.Contains(err.Error(), "更新版本程序") || !strings.Contains(err.Error(), p) {
		t.Fatalf("错误应含指定文案与路径: %v", err)
	}
}

func TestLoadFileBadConfigs(t *testing.T) { // C-per-06
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"JSON 断裂", `{"version":5,`, "JSON 解析失败"},
		{"缺 version", `{"listen":{"host":"127.0.0.1","port":1},"auth":{"mode":"admin"}}`, "缺少 version"},
		{"version 1", `{"version":1}`, "不受支持"},
		{"未知字段", `{"version":5,"foo":1}`, "未知字段"},
		{"R4 违例", `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{"interval_base_s":10},"auth":{"mode":"admin"},"providers":[]}`, "interval_base_s"},
		{"R9 违例", `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"su"},"providers":[]}`, "auth.mode"},
		{"R11 平台白名单", `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"admin"},"providers":[{"platform":"not-a-platform","access_keys":[{"id":"k1"}]}]}`, "platform"},
		{"R12 空凭据列表", `{"version":5,"listen":{"host":"127.0.0.1","port":1},"collector":{},"auth":{"mode":"admin"},"providers":[{"platform":"deepseek","access_keys":[]}]}`, "access_keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeConfig(t, dir, tc.content)
			_, err := LoadFile(p)
			if err == nil {
				t.Fatal("坏配置未拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应含 %q: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), p) {
				t.Fatalf("错误应含文件路径: %v", err)
			}
		})
	}
}

func TestLoadFileMissingCreatesTemplate(t *testing.T) { // 首启模板（唯一不退出例外）
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	res, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateCreatedTemplate {
		t.Fatalf("state = %v", res.State)
	}
	// 落盘可读回
	res2, err := LoadFile(p)
	if err != nil || res2.State != StateOK {
		t.Fatalf("模板不可读回: %v %v", res2, err)
	}
}

func TestLoadFileNeedsMigration(t *testing.T) {
	dir := t.TempDir()
	// v0.1（version 2）→ 走 MigrateV1
	p := writeConfig(t, dir, `{"version":2,"providers":[]}`)
	res, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateNeedsMigration || len(res.Raw) == 0 {
		t.Fatalf("state = %v", res.State)
	}
	// v0.2.x（version 3）→ 走 MigrateV3
	p3 := writeConfig(t, dir, `{"version":3,"listen":{"host":"127.0.0.1","port":8787},"collector":{},"auth":{"mode":"admin"},"providers":[]}`)
	res3, err := LoadFile(p3)
	if err != nil {
		t.Fatal(err)
	}
	if res3.State != StateNeedsMigrationV3 || len(res3.Raw) == 0 {
		t.Fatalf("state = %v", res3.State)
	}
}

func TestWriteWhitelist(t *testing.T) { // C-per-07（进程内近似审计：完整生命周期仅触碰白名单文件）
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// 1) 首启模板
	res, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// 2) key.bin
	key, err := crypto.EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 3) 保存配置（PUT 等价路径）
	f := res.File
	f.Providers = append(f.Providers, config.FileProvider{Platform: "opencode", AccessKeys: []config.AccessKey{{ID: "k1", Name: "A1"}}})
	cipher, _ := crypto.SealToken(key, "sk-test")
	f.Providers[0].AccessKeys[0].TokenCipher = cipher
	if err := SaveConfig(cfgPath, f); err != nil {
		t.Fatal(err)
	}
	// 4) 锁文件
	lockPath := filepath.Join(dir, LockNameOf(cfgPath))
	if err := os.WriteFile(lockPath, []byte(`{"pid":1,"start":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		ok := name == ConfigName || name == KeyName || IsLockFile(name) || strings.HasSuffix(name, TmpSuffix)
		if !ok {
			t.Fatalf("出现白名单外文件: %s", name)
		}
	}
}

func TestMigrationFullChain(t *testing.T) { // C-per-08：FX-2 → 迁移 → 落盘 → 读回
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	v1 := `{"version":2,"providers":[
      {"id":"kimi","name":"Kimi Code","baseURL":"http://127.0.0.1:8787/kimi","token":"sk-kimi-test","endpoints":[{"method":"GET","path":"/usages"}]},
      {"id":"zhipu","name":"智谱","baseURL":"http://127.0.0.1:9000/kimi","token":"sk-z","endpoints":[{"method":"GET","path":"/x"}]}
    ]}`
	if err := os.WriteFile(cfgPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := LoadFile(cfgPath)
	if err != nil || res.State != StateNeedsMigration {
		t.Fatalf("state = %v err = %v", res, err)
	}
	key, err := crypto.EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	migrated, logs, err := config.MigrateV1(res.Raw, func(plain string) (string, error) { return crypto.SealToken(key, plain) })
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("迁移应有日志（含兜底 WARN）")
	}
	if err := SaveConfig(cfgPath, migrated); err != nil {
		t.Fatal(err)
	}
	// 读回
	b, _ := os.ReadFile(cfgPath)
	s := string(b)
	if strings.Contains(s, "sk-kimi-test") || strings.Contains(s, "sk-z") {
		t.Fatal("config.json 含明文 token")
	}
	if !strings.Contains(s, `"version": 5`) || !strings.Contains(s, `"platform": "kimi-code"`) {
		t.Fatalf("迁移结果不完整: %s", s)
	}
	// 兜底 provider（9000 端口）已跳过，启动可用
	res2, err := LoadFile(cfgPath)
	if err != nil || res2.State != StateOK || len(res2.File.Providers) != 1 {
		t.Fatalf("迁移后启动失败: %v %v %+v", res2, err, res2)
	}
}

func TestLockNameOf(t *testing.T) { // C-lock-05 前置：锁名 = sha256(绝对路径)[:16]
	n1 := LockNameOf(`C:\data\config.json`)
	n2 := LockNameOf(`C:\data\config2.json`)
	n3 := LockNameOf(`C:\data\config.json`)
	if n1 == n2 {
		t.Fatal("不同配置应产生不同锁名")
	}
	if n1 != n3 {
		t.Fatal("同配置锁名应稳定")
	}
	if !strings.HasPrefix(n1, "quotaclock-") || len(n1) != len("quotaclock-")+16+len(".lock") {
		t.Fatalf("锁名格式错误: %s", n1)
	}
}

// TestLoadFileV4NeedsMigration（v0.3.0）：v0.2.5 的 version 4 配置进入 V4 迁移态，
// OldVersion 用于备份命名（config.json.v4.bak）。
func TestLoadFileV4NeedsMigration(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, `{"version":4,"listen":{"host":"127.0.0.1","port":8787},`+
		`"collector":{"interval_base_s":300,"jitter_min_s":5,"jitter_max_s":25,"stagger_min_s":1,"stagger_max_s":5,"backoff_multiplier":2,"backoff_max_s":1800},`+
		`"auth":{"mode":"admin"},"providers":[{"platform":"deepseek","access_keys":[{"id":"k1","token_cipher":"CIPHER"}]}]}`)
	res, err := LoadFile(p)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.State != StateNeedsMigrationV4 || res.OldVersion != 4 {
		t.Fatalf("应为 V4 迁移态且记住旧版本: %+v", res)
	}
	if res.File != nil {
		t.Fatal("迁移态不应给 File")
	}
	// 迁移后能被当作当前版本正常加载
	f, logs, err := config.MigrateV4(res.Raw)
	if err != nil || len(logs) != 1 {
		t.Fatalf("迁移失败: %v %v", err, logs)
	}
	if details := config.ValidateFile(f); len(details) > 0 {
		t.Fatalf("迁移结果应通过文件校验: %+v", details)
	}
}
