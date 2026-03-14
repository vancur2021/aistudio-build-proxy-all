package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

type GlobalSettings struct {
	Headless string  `json:"headless" yaml:"headless"`
	Proxy    *string `json:"proxy,omitempty" yaml:"proxy,omitempty"`
	URL      *string `json:"url,omitempty" yaml:"url,omitempty"`
}

type InstanceConfig struct {
	CookieFile string  `json:"cookie_file" yaml:"cookie_file"`
	URL        string  `json:"url,omitempty" yaml:"url,omitempty"`
	Headless   *string `json:"headless,omitempty" yaml:"headless,omitempty"`
	Proxy      *string `json:"proxy,omitempty" yaml:"proxy,omitempty"`
}

type AppConfig struct {
	GlobalSettings GlobalSettings   `json:"global_settings" yaml:"global_settings"`
	Instances      []InstanceConfig `json:"instances" yaml:"instances"`
}

var (
	configMu   sync.RWMutex
	AppCfg     AppConfig
	ConfigPath = "config.yaml" // 生产环境通常为 /app/config.yaml 或由环境变量指定
	CookiesDir = "cookies"     // 生产环境通常为 /app/cookies 或由环境变量指定
)

func initPath() {
	// 判断 ConfigPath
	if cp := os.Getenv("CONFIG_PATH"); cp != "" {
		ConfigPath = cp
	} else if _, err := os.Stat("camoufox-py/config.yaml.example"); err == nil {
		ConfigPath = "camoufox-py/config.yaml" // 在根目录运行时
	} else if _, err := os.Stat("../camoufox-py/config.yaml.example"); err == nil {
		ConfigPath = "../camoufox-py/config.yaml" // 在 golang 目录下运行时
	}

	// 判断 CookiesDir
	if cd := os.Getenv("COOKIES_DIR"); cd != "" {
		CookiesDir = cd
	} else if _, err := os.Stat("camoufox-py/cookies"); err == nil {
		CookiesDir = "camoufox-py/cookies" // 在根目录运行时
	} else if _, err := os.Stat("../camoufox-py/cookies"); err == nil {
		CookiesDir = "../camoufox-py/cookies" // 在 golang 目录下运行时
	}

	os.MkdirAll(CookiesDir, 0755)
	fmt.Printf("Initialised ConfigPath: %s\n", ConfigPath)
	fmt.Printf("Initialised CookiesDir: %s\n", CookiesDir)
}

// LoadConfig 从文件加载配置
func LoadConfig() error {
	configMu.Lock()
	defer configMu.Unlock()

	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在，返回默认配置
			AppCfg = AppConfig{
				GlobalSettings: GlobalSettings{
					Headless: "virtual",
				},
				Instances: []InstanceConfig{},
			}
			return nil
		}
		return err
	}

	err = yaml.Unmarshal(data, &AppCfg)
	if err != nil {
		return fmt.Errorf("解析 YAML 失败: %w", err)
	}
	return nil
}

// SaveConfig 保存配置到文件
func SaveConfig() error {
	configMu.RLock()
	defer configMu.RUnlock()

	data, err := yaml.Marshal(&AppCfg)
	if err != nil {
		return fmt.Errorf("序列化 YAML 失败: %w", err)
	}

	err = os.WriteFile(ConfigPath, data, 0644)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}

// GetConfig 获取当前配置副本
func GetConfig() AppConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return AppCfg
}

// UpdateConfig 更新并保存配置
func UpdateConfig(newCfg AppConfig) error {
	configMu.Lock()
	AppCfg = newCfg
	configMu.Unlock()
	return SaveConfig()
}

// --- Cookie 文件管理 ---

// ListCookies 列出所有 cookie 文件
func ListCookies() ([]string, error) {
	entries, err := os.ReadDir(CookiesDir)
	if err != nil {
		return nil, err
	}

	var cookies []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			cookies = append(cookies, entry.Name())
		}
	}
	return cookies, nil
}

// SaveCookieFile 保存（上传）cookie 文件
func SaveCookieFile(filename string, content []byte) error {
	filePath := filepath.Join(CookiesDir, filename)
	return os.WriteFile(filePath, content, 0644)
}

// DeleteCookieFile 删除 cookie 文件
func DeleteCookieFile(filename string) error {
	filePath := filepath.Join(CookiesDir, filename)
	return os.Remove(filePath)
}
