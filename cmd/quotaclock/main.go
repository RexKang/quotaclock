// QuotaClock：多平台 LLM 配额总览 Dashboard 的 Go 单二进制服务端（v0.2）。
// 启动链与关停序列见详细设计 §3。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RexKang/quotaclock/internal/collector"
	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/crypto"
	"github.com/RexKang/quotaclock/internal/lock"
	"github.com/RexKang/quotaclock/internal/logx"
	"github.com/RexKang/quotaclock/internal/persist"
	"github.com/RexKang/quotaclock/internal/server"
	"github.com/RexKang/quotaclock/internal/trend"
	"github.com/RexKang/quotaclock/web"
)

// version 由 -ldflags -X main.version=... 编译期注入。
var version = "dev"

// httpReadTimeout / httpWriteTimeout 附录 A 超时约定。
const (
	httpReadTimeout  = 10 * time.Second
	httpWriteTimeout = 30 * time.Second
	shutdownTimeout  = 10 * time.Second
)

func main() {
	var (
		flagAddr          = flag.String("addr", "", "监听 IP（覆盖 config）")
		flagPort          = flag.Int("port", 0, "监听端口（覆盖 config）")
		flagInterval      = flag.Int("interval", 0, "采集基准间隔秒（覆盖 config）")
		flagConfig        = flag.String("config", "", "配置文件路径（默认 exe 同目录/config.json）")
		flagAdminPassword = flag.String("admin-password", "", "初始化/重置管理员密码后继续启动")
		flagVersion       = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()
	if *flagVersion {
		fmt.Println("QuotaClock " + version)
		return
	}

	// 配置路径三级解析：-config > QUOTACLOCK_CONFIG > exe 同目录/config.json（不用 cwd，规避 Windows 双击陷阱）
	cfgPath := *flagConfig
	if cfgPath == "" {
		cfgPath = os.Getenv("QUOTACLOCK_CONFIG")
	}
	if cfgPath == "" {
		exe, err := os.Executable()
		if err != nil {
			fatal("定位可执行文件失败: %v", err)
		}
		cfgPath = filepath.Join(filepath.Dir(exe), persist.ConfigName)
	}
	absPath, err := filepath.Abs(cfgPath)
	if err != nil {
		fatal("解析配置路径失败: %v", err)
	}
	dir := filepath.Dir(absPath)

	// 4. 清理配置目录 *.tmp 残留（先于任何原子写）
	if err := persist.CleanTmp(dir); err != nil {
		logx.Warnf("清理 tmp 残留失败: %v", err)
	}

	// 5. 加载配置（模板 / version 3 / version 2 待迁移；坏配置打印后 exit 1，不静默降级）
	res, err := persist.LoadFile(absPath)
	if err != nil {
		fatal("%v", err)
	}

	// 6. 确保 key.bin（缺失生成；损坏 → 提示删除重录后 exit 1）
	key, err := crypto.EnsureKey(dir)
	if err != nil {
		fatal("%v", err)
	}

	file := res.File
	switch res.State {
	case persist.StateNeedsMigration, persist.StateNeedsMigrationV3, persist.StateNeedsMigrationV4:
		var migrated *config.File
		var logs []string
		var merr error
		switch res.State {
		case persist.StateNeedsMigration:
			migrated, logs, merr = config.MigrateV1(res.Raw, func(plain string) (string, error) {
				return crypto.SealToken(key, plain)
			})
		case persist.StateNeedsMigrationV3:
			migrated, logs, merr = config.MigrateV3(res.Raw)
		default:
			migrated, logs, merr = config.MigrateV4(res.Raw)
		}
		if merr != nil {
			fatal("%v", merr)
		}
		// 迁移是不可逆的结构改写：写盘前先把原文件整份备份（备份失败即中止，不冒险丢配置）
		backup, berr := persist.BackupConfig(absPath, res.OldVersion)
		if berr != nil {
			fatal("迁移前备份配置失败，已中止迁移（原文件未改动）: %v", berr)
		}
		logx.Infof("已备份迁移前的配置 → %s", backup)
		if res.OldVersion == 2 {
			logx.Warnf("注意：该备份内含 v0.1 的明文 token，核对无误后请删除 %s", backup)
		}
		for _, line := range logs {
			logLine(line)
		}
		if err := persist.SaveConfig(absPath, migrated); err != nil {
			fatal("迁移配置写盘失败: %v", err)
		}
		file = migrated
		logx.Infof("配置迁移完成（version %d → %d），已升版 → %s", res.OldVersion, config.CurrentVersion, absPath)
	}

	// -interval / -addr / -port 覆盖（flag > config）
	overrides := server.FlagOverrides{}
	if *flagInterval > 0 {
		file.Collector.IntervalBaseS = *flagInterval
		overrides.IntervalBaseS = *flagInterval
	}
	if *flagAddr != "" {
		file.Listen.Host = *flagAddr
		overrides.Host = *flagAddr
	}
	if *flagPort > 0 {
		file.Listen.Port = *flagPort
		overrides.Port = *flagPort
	}

	// 7. 派生会话签名密钥 + 构建运行时（解密 token，失败降级 token_invalid 不 crash）
	sessionKey := crypto.SessionKey(key)
	runtime := config.BuildRuntime(file, key)
	registerSecrets(runtime)

	// 8. -admin-password 初始化/重置（写盘后继续正常启动，D-7）
	if *flagAdminPassword != "" {
		hash, herr := config.HashPassword(*flagAdminPassword)
		if herr != nil {
			fatal("生成密码 hash 失败: %v", herr)
		}
		file.Auth.PasswordHash = hash
		if err := persist.SaveConfig(absPath, file); err != nil {
			fatal("密码写盘失败: %v", err)
		}
		runtime.PasswordHash = []byte(hash)
		logx.Infof("管理员密码已重置")
	}

	// 9. 默认密码未修改检测
	passwordIsDefault := config.IsDefaultPassword(string(runtime.PasswordHash))

	// 10. 单实例锁（同配置唯一；stale 接管；同目录多配置并存）
	lockHandle, err := lock.Acquire(absPath, nil)
	if err != nil {
		fatal("%v", err)
	}

	// 11. 快照 store + 采集器
	store := collector.NewStore(version)
	sched := collector.NewScheduler(runtime, store, collector.NewClient(version))

	// 11.5 缓存上次成功数据（v0.2.4）：启动恢复 → 页面立刻有数据；成功后异步回写
	cachePath := persist.CachePathOf(dir)
	if c, cerr := persist.LoadCache(cachePath); cerr != nil {
		logx.Warnf("缓存不可用，已忽略: %v", cerr)
	} else if c != nil {
		if n := sched.SeedCached(collector.RestoreFromCache(c, runtime)); n > 0 {
			logx.Infof("已恢复 %d 个平台的上次成功数据（等待首轮采集刷新）", n)
		}
	}
	cacheWriter := persist.StartCacheWriter(cachePath)
	sched.SetCacheWriter(cacheWriter)

	// 11.6 用量趋势（v0.4.0）：每次成功采集进内存聚合，每小时 + 退出时落盘
	trendPath := persist.TrendsPathOf(dir)
	trends := trend.New(trendPath)
	if n := trends.Loaded(); n > 0 {
		logx.Infof("已加载 %d 个 API Key 的趋势数据（%s）", n, trendPath)
	}
	sched.SetSampler(trends)

	// 12. HTTP 服务
	static, err := web.Index()
	if err != nil {
		fatal("内嵌静态页缺失: %v", err)
	}
	srv := server.New(server.Options{
		Version:         version,
		ConfigPath:      absPath,
		MasterKey:       key,
		SessionKey:      sessionKey,
		Store:           store,
		Trends:          trends,
		Static:          static,
		Icons:           web.Icons(),
		OnConfigChanged: sched.Reload,
		FlagOverrides:   overrides,
	}, file, runtime)

	httpSrv := &http.Server{
		Addr:         runtime.Listen.Addr(),
		Handler:      srv.Handler(),
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
	}
	ln, err := net.Listen("tcp", runtime.Listen.Addr())
	if err != nil {
		_ = lockHandle.Release()
		fatal("监听失败 %s: %v", runtime.Listen.Addr(), err)
	}
	go func() {
		if serr := httpSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			logx.Errorf("HTTP 服务异常退出: %v", serr)
		}
	}()

	// 启动 banner（INFO，单行逐条）
	logx.Infof("QuotaClock %s 已启动 | 监听 http://%s | auth=%s | 采集间隔 %ds(+抖动 %d~%ds) | 配置 %s",
		version, runtime.Listen.Addr(), runtime.AuthMode, runtime.Collector.IntervalBaseS,
		runtime.Collector.JitterMinS, runtime.Collector.JitterMaxS, absPath)
	if passwordIsDefault {
		logx.Warnf("正在使用默认密码，请立即修改")
	}
	if runtime.Listen.Host == "0.0.0.0" {
		logx.Warnf("监听 0.0.0.0：服务暴露于所有网络接口，任何可访问者均可查看数据，请注意暴露风险")
	}

	// 自动打开浏览器：交互式启动（双击/桌面会话）时直接进看板；
	// 无图形环境（Linux SSH/systemd/容器）或 QUOTACLOCK_NO_BROWSER=1 时跳过，仅提示地址。
	pageURL := displayURL(runtime.Listen.Host, runtime.Listen.Port)
	switch {
	case os.Getenv("QUOTACLOCK_NO_BROWSER") != "":
		logx.Infof("访问地址: %s（QUOTACLOCK_NO_BROWSER=1，已禁用自动打开）", pageURL)
	case openBrowser(pageURL) != nil:
		logx.Infof("未自动打开浏览器，请手动访问 %s", pageURL)
	default:
		logx.Infof("已在浏览器打开 %s", pageURL)
	}

	// 13. 阻塞等待信号（SIGINT/SIGTERM 归一）
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() { sched.Run(ctx) }()
	// 趋势每小时落盘一次（用户要求「后台进程每小时保存」），ctx 结束时最后落一次
	trends.Start(ctx, trend.FlushInterval)

	<-sigCh
	logx.Infof("收到退出信号，开始优雅关停（停采集 ∥ HTTP drain）...")
	cancel() // 停采集器调度；在途 fetch 随 ctx 快速结束

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer scancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			logx.Warnf("HTTP drain 超时或失败: %v", err)
		}
	}()
	sched.Wait() // 等待在途采集请求结束（与 drain 并行）
	<-drainDone
	cacheWriter.Close() // 最后一份缓存落盘（在途采集已全部结束）
	trends.Flush()      // 趋势最后一份落盘（同上；错过这次就要等下一个整点）

	if err := lockHandle.Release(); err != nil {
		logx.Warnf("删除锁文件失败: %v", err)
	}
	logx.Infof("已退出")
}

// registerSecrets 把全部 token 明文/密文/掩码/Authorization 值登记进日志脱敏红线。
func registerSecrets(r *config.Runtime) {
	for _, p := range r.Providers {
		if p.Token != "" {
			logx.RegisterSecret(p.Token)
			logx.RegisterSecret("Bearer " + p.Token)
		}
		if p.TokenCipher != "" {
			logx.RegisterSecret(p.TokenCipher)
		}
		if p.TokenMasked != "" {
			logx.RegisterSecret(p.TokenMasked)
		}
	}
}

// logLine 输出迁移日志行（"WARN ..."/"INFO ..." 前缀分派）。
func logLine(line string) {
	switch {
	case strings.HasPrefix(line, "WARN "):
		logx.Warnf("%s", strings.TrimPrefix(line, "WARN "))
	case strings.HasPrefix(line, "INFO "):
		logx.Infof("%s", strings.TrimPrefix(line, "INFO "))
	default:
		logx.Infof("%s", line)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "QuotaClock: "+format+"\n", args...)
	os.Exit(1)
}
