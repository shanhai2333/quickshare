package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// launchConfig 是启动配置，**存在数据目录之外**。
//
// 为什么不能存进数据目录：数据目录本身是可配置的，把它存进去就成了鸡生蛋——
// 下次启动得先知道数据目录在哪，才能读到"数据目录在哪"这句话。
// 所以固定放在用户配置目录下（Windows 是 %AppData%，Linux 是 ~/.config）。
type launchConfig struct {
	DataDir string `json:"dataDir"`
}

// configPath 返回启动配置文件的位置。
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "quickshare", "config.json"), nil
}

// loadLaunchConfig 读启动配置。读不到、读坏了都退回零值，
// 让调用方用默认值继续——配置文件坏了不该导致服务起不来。
func loadLaunchConfig() launchConfig {
	var c launchConfig
	p, err := configPath()
	if err != nil {
		return c
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveLaunchConfig(c launchConfig) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}
