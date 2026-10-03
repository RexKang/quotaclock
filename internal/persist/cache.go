package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RexKang/quotaclock/internal/logx"
)

// CacheName 上次成功采集数据快照文件（v0.2.4 新增白名单项，写盘白名单第 4 类）。
// 只存展示用数据（data + last_success_at），**不含任何 token/密文**。
const CacheName = "cache.json"

// CacheVersion 缓存 schema 版本（与 config version 独立演进：不匹配即忽略，不阻塞启动）。
const CacheVersion = 1

// cacheDebounce 一次采集 tick 内多平台相继成功时的合并窗口（合并为一次落盘）。
const cacheDebounce = 200 * time.Millisecond

// Cache 缓存文件落盘形态。
type Cache struct {
	Version   int          `json:"version"`
	WrittenAt string       `json:"written_at"` // RFC3339 UTC 秒级
	Providers []CacheEntry `json:"providers"`
}

// CacheEntry 单平台「上次成功」结果。
type CacheEntry struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Data          json.RawMessage `json:"data"`
	LastSuccessAt string          `json:"last_success_at"` // RFC3339 UTC 秒级
}

// CachePathOf 返回目录下的 cache.json 路径。
func CachePathOf(dir string) string { return filepath.Join(dir, CacheName) }

// LoadCache 读取缓存文件：
//   - 不存在 → (nil, nil)（首次启动的正常情形）
//   - 解析失败 / 版本不符 → (nil, err)：调用方 WARN 后忽略即可，坏缓存**不得**阻塞启动
func LoadCache(path string) (*Cache, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取缓存失败: %s: %w", path, err)
	}
	var c Cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("缓存 JSON 解析失败: %s: %w", path, err)
	}
	if c.Version != CacheVersion {
		return nil, fmt.Errorf("缓存版本 %d 不受支持（当前 %d）: %s", c.Version, CacheVersion, path)
	}
	return &c, nil
}

// SaveCache 原子写缓存文件（0600，与配置同权限约定）。
func SaveCache(path string, c *Cache) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("缓存序列化失败: %w", err)
	}
	return AtomicWrite(path, data)
}

// CacheWriter 缓存写入器：Save 非阻塞投递最新缓存，后台单 goroutine 合并落盘。
// 采集 tick 内不做磁盘 IO；同一窗口内多次成功只落最后一次（内容本就是全量）。
type CacheWriter struct {
	path string

	mu      sync.Mutex
	pending *Cache
	lastErr error

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// StartCacheWriter 创建并启动后台写入器（调用方负责 Close）。
func StartCacheWriter(path string) *CacheWriter {
	w := &CacheWriter{
		path: path,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// Save 投递一份缓存（覆盖上一份未落盘的内容）；不阻塞采集流程。
func (w *CacheWriter) Save(c *Cache) {
	if w == nil || c == nil {
		return
	}
	w.mu.Lock()
	w.pending = c
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default: // 已有待处理唤醒，无需重复
	}
}

// Close 关停后台写入器，并把最后一份待写内容落盘（关停序列中调用）。
func (w *CacheWriter) Close() {
	if w == nil {
		return
	}
	select {
	case <-w.stop:
		<-w.done // 已关停过
		return
	default:
	}
	close(w.stop)
	<-w.done
}

// LastError 返回最近一次写盘错误（nil = 一路正常）。
func (w *CacheWriter) LastError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

func (w *CacheWriter) loop() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			w.flush()
			return
		case <-w.wake:
			select {
			case <-time.After(cacheDebounce):
			case <-w.stop:
				w.flush()
				return
			}
			w.flush()
		}
	}
}

func (w *CacheWriter) flush() {
	w.mu.Lock()
	c := w.pending
	w.pending = nil
	w.mu.Unlock()
	if c == nil {
		return
	}
	if err := SaveCache(w.path, c); err != nil {
		w.mu.Lock()
		w.lastErr = err
		w.mu.Unlock()
		logx.Warnf("缓存写盘失败（下次成功采集会重写）: %v", err)
		return
	}
	logx.Debugf("已缓存 %d 个平台的上次成功数据 → %s", len(c.Providers), w.path)
}
