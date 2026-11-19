package distill

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ModelConfig 是 ~/.v2mem/model.json：判定器与蒸馏共用的一家模型配置。
//
// 为什么要有文件，而不只认环境变量：定时任务与宿主钩子不保证继承交互 shell 的环境，
// 而 model.json 是确定性输入；密钥写在文件里也不会进 shell history。
// 环境变量仍然优先于文件，方便临时换模型试一次。
type ModelConfig struct {
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
	return c, nil
}
