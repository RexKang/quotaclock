package server

import (
	"net/http"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/crypto"
	"github.com/RexKang/quotaclock/internal/logx"
)

// handleExportConfig GET /api/config/export（v0.3.0）
//
// 导出可读配置（**含明文 token**）：token 是用本机 key.bin 加密的，换台机器解不开，
// 所以跨机器搬运只能给明文，由目标机器的导入流程重新加密落盘。
// 因此这个端点按「敏感」对待：admin 模式需登录（requireWrite）、no-store、并记一条 INFO 日志
// （导出即意味着明文出过机，日志里留痕便于日后回溯）。
func (s *Server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	f := s.file.Load()
	if f == nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "配置尚未就绪")
		return
	}
	ex, warnings := config.BuildExport(f, func(cipher string) (string, error) {
		return crypto.OpenToken(s.key, cipher)
	})
	body, err := config.MarshalExport(ex)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "导出序列化失败")
		return
	}
	total, withToken := 0, 0
	for _, p := range ex.Providers {
		for _, k := range p.AccessKeys {
			total++
			if k.Token != "" {
				withToken++
			}
		}
	}
	logx.Infof("已导出配置（%d 个平台 / %d 个 Key，其中 %d 个带明文 token）：文件含明文凭据，请尽快导入目标机器后删除",
		len(ex.Providers), total, withToken)
	for _, warn := range warnings {
		logx.Warnf("导出：%s", warn)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename(time.Now())+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// exportFilename 形如 quotaclock-config-20260912.json（日期用本地时区，与页面一致）。
func exportFilename(now time.Time) string {
	return "quotaclock-config-" + now.Format("20060102") + ".json"
}
