package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RexKang/quotaclock/internal/config"
	"github.com/RexKang/quotaclock/internal/logx"
)

// LoadState 是加载配置后的处置类别。
type LoadState int

const (
	// StateCreatedTemplate 首次启动无配置，已生成模板文件。
	StateCreatedTemplate LoadState = iota
	// StateOK 正常加载当前版本配置。
	StateOK
	// StateNeedsMigration 检测到 version 2（v0.1），Raw 保留原文待迁移。
	StateNeedsMigration
	// StateNeedsMigrationV3 检测到 version 3（v0.2.0~v0.2.4），Raw 保留原文待迁移到 v0.2.5 结构。
	StateNeedsMigrationV3
	// StateNeedsMigrationV4 检测到 version 4（v0.2.5），Raw 保留原文待迁移到 v0.3.0 结构
	//（只补 balance 段，其余原样）。
	StateNeedsMigrationV4
)

// LoadResult 配置加载结果。
type LoadResult struct {
	State LoadState
	File  *config.File // StateOK / StateCreatedTemplate 时非 nil
	Raw   []byte       // StateNeedsMigration* 时的原文
	// OldVersion 检测到的旧配置版本号（仅迁移态有意义）：迁移前备份文件按它命名
	OldVersion int
}

// BadConfigError 坏配置：启动必须打印具体错误与文件路径后退出（不静默降级，PRD §4.5）。
type BadConfigError struct {
	Path    string
	Details []config.ValidationError
	Msg     string
}

func (e *BadConfigError) Error() string {
	msg := "配置文件非法: " + e.Path + ": " + e.Msg
	for _, d := range e.Details {
		msg += "\n  - " + d.String()
	}
	return msg
}

// LoadFile 加载并校验配置文件（不含迁移执行——迁移需要 key.bin，由 main 编排）。
//   - 文件不存在 → 生成模板、原子写、返回 StateCreatedTemplate（唯一不退出的例外）
//   - version < 3 → 返回 StateNeedsMigration（version 2 才可迁移，其余视为坏配置）
//   - JSON 损坏 / 未知字段 / 校验失败 / version 缺失 / version > 3 → BadConfigError（调用方 exit 1）
func LoadFile(path string) (*LoadResult, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		tpl, terr := Template()
		if terr != nil {
			return nil, terr
		}
		data, terr := MarshalFile(tpl)
		if terr != nil {
			return nil, terr
		}
		if terr := AtomicWrite(path, data); terr != nil {
			return nil, fmt.Errorf("生成模板配置失败: %w", terr)
		}
		logx.Infof("已生成模板配置 %s（默认密码 %s，请尽快修改）", path, config.DefaultPassword)
		return &LoadResult{State: StateCreatedTemplate, File: tpl}, nil
	} else if err != nil {
		return nil, &BadConfigError{Path: path, Msg: "读取失败: " + err.Error()}
	}

	version, hasVersion, perr := config.ProbeVersion(raw)
	if perr != nil {
		return nil, &BadConfigError{Path: path, Msg: "JSON 解析失败: " + perr.Error()}
	}
	if version < config.CurrentVersion {
		if !hasVersion {
			return nil, &BadConfigError{Path: path, Msg: "缺少 version 字段"}
		}
		switch version {
		case 2:
			return &LoadResult{State: StateNeedsMigration, Raw: raw, OldVersion: version}, nil
		case 3:
			return &LoadResult{State: StateNeedsMigrationV3, Raw: raw, OldVersion: version}, nil
		case 4:
			// v0.2.5 → v0.3.0：只补 balance 段
			return &LoadResult{State: StateNeedsMigrationV4, Raw: raw, OldVersion: version}, nil
		default:
			return nil, &BadConfigError{Path: path, Msg: fmt.Sprintf("version %d 不受支持（支持 2=v0.1、3=v0.2.x、4=v0.2.5 迁移，或 %d）", version, config.CurrentVersion)}
		}
	}
	if version > config.CurrentVersion {
		return nil, &BadConfigError{Path: path, Msg: "配置来自更新版本程序，请升级 QuotaClock"}
	}

	f, err := config.ParseFile(raw)
	if err != nil {
		return nil, &BadConfigError{Path: path, Msg: err.Error()}
	}
	if details := config.ValidateFile(f); len(details) > 0 {
		return nil, &BadConfigError{Path: path, Msg: "校验失败", Details: details}
	}
	return &LoadResult{State: StateOK, File: f}, nil
}

// SaveConfig 原子写配置文件（配置保存/迁移/模板/-admin-password 全部走此口）。
func SaveConfig(path string, f *config.File) error {
	data, err := MarshalFile(f)
	if err != nil {
		return err
	}
	return AtomicWrite(path, data)
}

// DirOf 返回配置文件所在目录（绝对路径化）。
func DirOf(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}
