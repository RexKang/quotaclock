package persist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/RexKang/quotaclock/internal/config"
)

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// Template 生成首启模板配置（version 3、默认监听/采集参数、admin + 默认密码 hash、空 providers）。
// 默认密码 hash 在模板内直接写入：首启一次完成「模板 → key.bin → 默认密码」且无无密码窗口（D-15）。
func Template() (*config.File, error) {
	hash, err := config.HashPassword(config.DefaultPassword)
	if err != nil {
		return nil, fmt.Errorf("生成默认密码 hash 失败: %w", err)
	}
	return &config.File{
		Version:   config.CurrentVersion,
		Listen:    config.DefaultListen(),
		Collector: config.DefaultCollector(),
		Auth:      config.FileAuth{Mode: config.AuthModeAdmin, PasswordHash: hash},
		Providers: []config.FileProvider{},
	}, nil
}

// MarshalFile 序列化配置为落盘 JSON（缩进两格，便于人工检视；不含任何明文 token）。
func MarshalFile(f *config.File) ([]byte, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
