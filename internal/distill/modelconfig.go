package distill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ModelConfig 是 ~/.v2mem/model.json：判定器与蒸馏共用的一家模型配置。
//
// 为什么要有文件，而不只认环境变量：定时任务与宿主钩子不保证继承交互 shell 的环境，
// 而 model.json 是确定性输入；密钥写在文件里也不会进 shell history。
// 环境变量仍然优先于文件，方便临时换模型试一次。
//
// Mode 用来区分两条互斥的来源：api（走 provider 解析）与 cli（走本机已装的 CLI）。
// 老文件没有 mode 字段，一律按 api，行为与加这个字段之前完全一致。
type ModelConfig struct {
	Mode     string `json:"mode,omitempty"`
	CLI      string `json:"cli,omitempty"`
	ModelTag string `json:"model_tag,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

// DefaultModelConfigPath 返回 ~/.v2mem/model.json。
func DefaultModelConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "model.json"
	}
	return filepath.Join(home, ".v2mem", "model.json")
}

// LoadModelConfig 读配置。文件不存在返回零值而不是错误 —— 没配模型是常态，
// 调用方据此退回 provider 解析（env / none），不该因此让命令失败。
func LoadModelConfig(path string) (ModelConfig, error) {
	if path == "" {
		path = DefaultModelConfigPath()
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ModelConfig{}, nil
	}
	if err != nil {
		return ModelConfig{}, err
	}
	var c ModelConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return ModelConfig{}, err
	}
	return normalizeModelConfig(c)
}

// normalizeModelConfig 把 Mode 收敛到 api|cli|off，并且只在这三种取值上放行。
// 未知 mode 不静默退化成 api：用户显式写错时静默走 API 会真的把数据发给远端，
// 报错让人重跑 mem setup 更安全。
func normalizeModelConfig(c ModelConfig) (ModelConfig, error) {
	if c.Mode == "" {
		c.Mode = "api"
		return c, nil
	}
	switch c.Mode {
	case "api", "off":
		return c, nil
	case "cli":
		if strings.TrimSpace(c.CLI) == "" || strings.TrimSpace(c.ModelTag) == "" {
			return ModelConfig{}, fmt.Errorf("model.json: mode=cli 需要同时配置 cli 与 model_tag，请重跑 mem setup")
		}
		return c, nil
	default:
		return ModelConfig{}, fmt.Errorf("model.json: 未知 mode %q（可用 api|cli|off）", c.Mode)
	}
}

// SaveModelConfig 原子写入配置：同目录临时文件 + rename，权限固定 0600。
// 写前留一份 <path>.v2mem.bak，供 mem setup --undo 回退；已有备份不覆盖，
// 这样连续多次写盘后 --undo 仍能回到最初的状态。
func SaveModelConfig(path string, c ModelConfig) error {
	if path == "" {
		path = DefaultModelConfigPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		bak := path + ".v2mem.bak"
		// 已有备份不覆盖：保留最早的状态，--undo 才能一直回到最初。
		if _, statErr := os.Stat(bak); errors.Is(statErr, fs.ErrNotExist) {
			if err := os.WriteFile(bak, old, 0o600); err != nil {
				return err
			}
		} else if statErr != nil {
			// 备份状态不可判定就别写：不能让用户丢配置又没得回退。
			return statErr
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".model.json.tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是空操作
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ResolveOptions 是「flag > model.json > 环境变量 > auto」的唯一实现点。
// 调用方只管把 flag 值原样传进来，不要各自判断 flag 是否显式给过 —— 5 处构造点
// 各判一次必然漂移。resolve() 本身只看 Options.CLI 是否非空，不重复判优先级。
func ResolveOptions(cfg ModelConfig, flagProvider, flagModel string) Options {
	// flag 显式给了 provider：按 flag 走，并且清空 CLI 让 resolve() 不进 CLI 分支。
	if flagProvider != "" {
		model := flagModel
		if model == "" {
			model = cfg.Model
		}
		return Options{Provider: flagProvider, Model: model, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL}
	}
	switch cfg.Mode {
	case "cli":
		return Options{CLI: cfg.CLI, ModelTag: cfg.ModelTag}
	case "off":
		return Options{Provider: "none"}
	default:
		return Options{Provider: cfg.Provider, Model: cfg.Model, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL}
	}
}
