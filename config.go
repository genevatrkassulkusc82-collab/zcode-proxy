package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ---- JSON 文件配置（config/config.json，热重载）----
// 运行期可变项（策略/代理/指纹/密码/APIKey）存 SQLite settings 表；
// 这里只放部署级配置：监听地址、上游端点、模型清单、客户端版本号。

// UpstreamURLs 上游端点（可被 config.json 覆盖，便于离线测试）
type UpstreamURLs struct {
	Zai         string `json:"zai"`          // zcode.z.ai JWT 免费通道
	ZaiFallback string `json:"zai_fallback"` // api.z.ai API Key 通道
	Bigmodel    string `json:"bigmodel"`     // open.bigmodel.cn
}

// FileConfig config.json 结构
type FileConfig struct {
	ListenAddr string       `json:"listen_addr"`
	AppVersion string       `json:"app_version"` // ZCode 客户端伪装版本号，空=自动探测注册表
	Models     []string     `json:"models"`      // /v1/models 公布的模型清单
	Upstream   UpstreamURLs `json:"upstream"`

	configDir string
	mu        sync.RWMutex
}

// ConfigDir 配置目录（-doctor 报告用）
func (c *FileConfig) ConfigDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.configDir
}

// DefaultUpstream 与 zcode2api settings.py 一致的默认端点
var DefaultUpstream = UpstreamURLs{
	Zai:         "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages",
	ZaiFallback: "https://api.z.ai/api/anthropic/v1/messages",
	Bigmodel:    "https://open.bigmodel.cn/api/anthropic/v1/messages",
}

// DefaultModels 默认模型清单（上游大小写敏感，这里存官方名）
var DefaultModels = []string{
	"GLM-5.3", "GLM-5.3-Flash",
}

// LoadFileConfig 加载配置目录；文件不存在时用默认值并落盘一份
func LoadFileConfig(configDir string) (*FileConfig, error) {
	c := &FileConfig{configDir: configDir}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *FileConfig) reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := filepath.Join(c.configDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：写默认配置
			c.ListenAddr = "127.0.0.1:8687"
			c.AppVersion = ""
			c.Models = DefaultModels
			c.Upstream = DefaultUpstream
			c.writeDefaultLocked(path)
			return nil
		}
		return fmt.Errorf("read config.json: %w", err)
	}
	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	c.ListenAddr = fc.ListenAddr
	c.AppVersion = fc.AppVersion
	c.Models = fc.Models
	c.Upstream = fc.Upstream
	// 补默认值
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8687"
	}
	if len(c.Models) == 0 {
		c.Models = DefaultModels
	}
	if c.Upstream.Zai == "" {
		c.Upstream.Zai = DefaultUpstream.Zai
	}
	if c.Upstream.ZaiFallback == "" {
		c.Upstream.ZaiFallback = DefaultUpstream.ZaiFallback
	}
	if c.Upstream.Bigmodel == "" {
		c.Upstream.Bigmodel = DefaultUpstream.Bigmodel
	}
	return nil
}

func (c *FileConfig) writeDefaultLocked(path string) {
	os.MkdirAll(filepath.Dir(path), 0755)
	// 独立结构体序列化，避免复制 FileConfig 内的互斥锁
	out := struct {
		ListenAddr string       `json:"listen_addr"`
		AppVersion string       `json:"app_version"`
		Models     []string     `json:"models"`
		Upstream   UpstreamURLs `json:"upstream"`
	}{
		ListenAddr: c.ListenAddr,
		AppVersion: c.AppVersion,
		Models:     c.Models,
		Upstream:   c.Upstream,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[config] write default config.json: %v", err)
	}
}

// StartHotReload 定时热加载
// StartHotReload 返回停止函数：停机时调用，否则 ticker 与 goroutine 随进程存活
func (c *FileConfig) StartHotReload(interval time.Duration) (stop func()) {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := c.reload(); err != nil {
					log.Printf("[config] hot reload failed: %v", err)
				}
			}
		}
	}()
	return func() { close(done) }
}

// GetListenAddr 线程安全读取
func (c *FileConfig) GetListenAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ListenAddr
}

// GetUpstream 线程安全读取
func (c *FileConfig) GetUpstream() UpstreamURLs {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Upstream
}

// GetModels 线程安全读取
func (c *FileConfig) GetModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string{}, c.Models...)
}

// GetAppVersion 配置的版本号（空则调用方走自动探测）
func (c *FileConfig) GetAppVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AppVersion
}
