package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct{}

func NewConfig() *Config {

	return &Config{}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// controller 端使用
func (c *Config) GetSyncType() string  { return getenv("OPENLB_SYNC_TYPE", "http") }
func (c *Config) GetStoreType() string { return getenv("OPENLB_STORE_TYPE", "crd") }

// SyncHTTPTimeout 是 controller 到 agent 的单次 HTTP 请求总超时时间。
func (c *Config) SyncHTTPTimeout() time.Duration {
	return getenvDuration("OPENLB_SYNC_TIMEOUT", 10*time.Second)
}

// SyncMaxRetries 是单次同步请求首次失败后的最大重试次数，最多允许 10 次。
func (c *Config) SyncMaxRetries() int {
	n := getenvInt("OPENLB_SYNC_MAX_RETRIES", 2)
	if n < 0 {
		return 0
	}
	if n > 10 {
		return 10
	}
	return n
}

// SyncRetryBackoff 是同步请求第一次重试前的退避时间，后续按指数增长。
func (c *Config) SyncRetryBackoff() time.Duration {
	return getenvDuration("OPENLB_SYNC_RETRY_BACKOFF", 200*time.Millisecond)
}

// SyncRetryMaxBackoff 限制指数退避的最大时间。
func (c *Config) SyncRetryMaxBackoff() time.Duration {
	return getenvDuration("OPENLB_SYNC_RETRY_MAX_BACKOFF", 2*time.Second)
}

// LeaderElectionEnabled 控制是否启用 leader election（默认启用）
// 设置 OPENLB_LEADER_ELECTION=false 可禁用（单副本场景）
func (c *Config) LeaderElectionEnabled() bool {
	return getenvBool("OPENLB_LEADER_ELECTION", true)
}

// AgentPort 是 instance 注解未带端口时，推送到 agent 使用的默认端口
func (c *Config) AgentPort() string { return getenv("OPENLB_AGENT_PORT", "8080") }

// MetricsBindAddress 返回 controller metrics 监听地址（默认 :8081）
// 设置为 "0" 可禁用 metrics
func (c *Config) MetricsBindAddress() string {
	return getenv("OPENLB_METRICS_BIND_ADDRESS", ":8081")
}

// agent 端使用
func (c *Config) ServePort() string { return getenv("OPENLB_SERVE_PORT", ":8080") }

// AgentStateDir 保存端口归属及未完成操作；容器部署时需持久化此目录。
func (c *Config) AgentStateDir() string {
	return getenv("OPENLB_AGENT_STATE_DIR", "/var/lib/openlb-agent")
}

// AgentMetricsAddr 返回 agent metrics 监听地址（默认 :8082）
// 设置为 "0" 可禁用 metrics
func (c *Config) AgentMetricsAddr() string {
	return getenv("OPENLB_AGENT_METRICS_ADDRESS", ":8082")
}

// nginx agent 使用
func (c *Config) NginxHome() string { return getenv("OPENLB_NGINX_HOME", "/etc/nginx/") }
func (c *Config) NginxBin() string  { return getenv("OPENLB_NGINX_BIN", "") }

func (c *Config) NginxCommandTimeout() time.Duration {
	return getenvDuration("OPENLB_NGINX_COMMAND_TIMEOUT", 3*time.Second)
}

// LVS 默认发现本机内网 IPv4；此项用于显式覆盖监听地址。额外 VIP 的绑定和漂移由部署环境管理。
func (c *Config) LVSVIPs() string       { return getenv("OPENLB_LVS_VIPS", "") }
func (c *Config) LVSBinary() string     { return getenv("OPENLB_LVS_BIN", "ipvsadm") }
func (c *Config) LVSForwarding() string { return getenv("OPENLB_LVS_FORWARDING", "nat") }
func (c *Config) LVSScheduler() string  { return getenv("OPENLB_LVS_SCHEDULER", "rr") }
func (c *Config) LVSCommandTimeout() time.Duration {
	return getenvDuration("OPENLB_LVS_COMMAND_TIMEOUT", 3*time.Second)
}

// NginxStatusURL 返回 nginx stub_status 的采集地址（七层指标）
// 设置为 "0" 可禁用七层连接数采集
func (c *Config) NginxStatusURL() string {
	return getenv("OPENLB_NGINX_STATUS_URL", "http://127.0.0.1:8888/nginx_status")
}

// EnableL4Stats 返回是否启用四层(TCP stream)连接数采集（默认启用）
// 设置为 "false" 或 "0" 可禁用
func (c *Config) EnableL4Stats() bool {
	return getenvBool("OPENLB_ENABLE_L4_STATS", true)
}

// ConnectionStatsInterval 返回连接数采集间隔（秒），默认 15s
func (c *Config) ConnectionStatsInterval() time.Duration {
	s := getenv("OPENLB_CONNECTION_STATS_INTERVAL", "15")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		n = 15
	}
	return time.Duration(n) * time.Second
}
