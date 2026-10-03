// Package lock 实现单实例锁：同一配置文件只允许一个进程（PRD §5.1）。
//
// 锁文件 = 配置同目录 quotaclock-<sha256(配置绝对路径) 前 16 位 hex>.lock，
// 内容 JSON {"pid", "start"}（start = 进程创建时刻 unix 秒，用于 PID 复用防护 D-9）。
// 冲突时读 PID 探活：存活 → AlreadyRunningError；stale（已死或创建时间不匹配）→ 接管。
package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RexKang/quotaclock/internal/persist"
)

// Info 锁文件内容。
type Info struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"` // 进程创建时刻 unix 秒
}

// AlreadyRunningError 同配置已有存活实例。
type AlreadyRunningError struct{ PID int }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("已在运行 (PID %d)，同一配置文件只允许一个进程", e.PID)
}

// ProbeFunc 探活注入点：返回 (目标进程是否存活, 探活实现自身错误)。
// 探活报错时调用方保守按存活处理（不误杀，C-lock-04）。
type ProbeFunc func(pid int, start int64) (alive bool, err error)

// maxTakeoverAttempts stale 接管重试上限（防竞态死循环，设计 §4.1）。
const maxTakeoverAttempts = 3

// Acquire 获取单实例锁；成功返回句柄（进程退出前须 Release）。
func Acquire(configPath string, probe ProbeFunc) (*Handle, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("解析配置路径失败: %w", err)
	}
	lockPath := filepath.Join(filepath.Dir(abs), persist.LockNameOf(abs))
	if probe == nil {
		probe = probeAlive
	}
	self := Info{PID: os.Getpid(), Start: selfStartUnix()}
	data, err := json.Marshal(self)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxTakeoverAttempts; attempt++ {
		f, cerr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if cerr == nil {
			if _, werr := f.Write(data); werr != nil {
				f.Close()
				os.Remove(lockPath)
				return nil, fmt.Errorf("写入锁文件失败: %s: %w", lockPath, werr)
			}
			f.Close()
			return &Handle{path: lockPath}, nil
		}
		if !os.IsExist(cerr) {
			return nil, fmt.Errorf("创建锁文件失败: %s: %w", lockPath, cerr)
		}
		// 已存在：读出 PID 探活
		raw, rerr := os.ReadFile(lockPath)
		if rerr != nil {
			continue // 读不到（竞态/权限）→ 重试
		}
		var cur Info
		if jerr := json.Unmarshal(raw, &cur); jerr != nil || cur.PID <= 0 {
			// 内容损坏 → 视为 stale，接管
			os.Remove(lockPath)
			continue
		}
		alive, perr := probe(cur.PID, cur.Start)
		if perr != nil || alive {
			// 探活报错保守按存活（不误杀）
			return nil, &AlreadyRunningError{PID: cur.PID}
		}
		// stale：删除旧锁接管
		os.Remove(lockPath)
	}
	return nil, fmt.Errorf("多次尝试接管锁文件失败: %s", lockPath)
}

// Handle 持有的锁；Release 删除锁文件（进程正常退出时调用；崩溃残留由下次启动 stale 接管兜底）。
type Handle struct{ path string }

func (h *Handle) Release() error {
	if h == nil || h.path == "" {
		return nil
	}
	if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	h.path = ""
	return nil
}

// Path 返回锁文件路径（测试用）。
func (h *Handle) Path() string { return h.path }
