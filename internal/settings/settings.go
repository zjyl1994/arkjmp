// Package settings 解析配置：命令行 > 环境变量 > .env > config.json > 内置默认值。
//
// 端口和密钥不写死在代码里，由 .env（或同名环境变量）提供：
//
//	ARKJMP_API       控制器地址，优先于同来源的 HOST/PORT
//	ARKJMP_HOST      控制器主机（默认 127.0.0.1）
//	ARKJMP_PORT      控制器端口（默认 9090）
//	ARKJMP_SECRET    控制器密码（默认空，即不鉴权）
//	ARKJMP_TEST_URL  测速 URL
//	ARKJMP_TIMEOUT   单次请求超时秒数
//	ARKJMP_CONFIG    mihomo config.json 路径（读 external-controller / secret 兜底）
//
// .env 查找顺序：--env-file > $ARKJMP_ENV_FILE > ./.env >
// $XDG_CONFIG_HOME/arkjmp/.env > 可执行文件同目录 .env。
// 已经存在的真实环境变量优先于 .env 里的同名项。
package settings

import (
	"bufio"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
)

// 内置默认值（只有非敏感项）。
const (
	DefaultHost    = "127.0.0.1"
	DefaultPort    = 9090
	DefaultTimeout = 5.0

	// EnvFileVar 指定 .env 路径的环境变量名。
	EnvFileVar = "ARKJMP_ENV_FILE"
)

// jsonUnmarshal 让 configfile.go 不必直接依赖 json 包。
func jsonUnmarshal(data []byte, out any) error { return json.Unmarshal(data, out) }

// Settings 是最终生效的配置。
type Settings struct {
	API        string
	Secret     string
	TestURL    string
	Timeout    float64
	ConfigPath string
	EnvFile    string
}

// Overrides 是命令行参数（优先级最高）；字符串为空表示没给。
type Overrides struct {
	API        string
	Secret     string
	TestURL    string
	ConfigPath string
	EnvFile    string
	Timeout    float64
	TimeoutSet bool
}

var trailingComment = regexp.MustCompile(`\s+#.*$`)

// ParseDotenv 极简 .env 解析：KEY=VALUE、# 注释、export 前缀、单双引号。
func ParseDotenv(text string) map[string]string {
	data := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		data[key] = unquote(strings.TrimSpace(value))
	}
	return data
}

func unquote(value string) string {
	if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
		quote := value[:1]
		if end := strings.Index(value[1:], quote); end >= 0 {
			return value[1 : end+1]
		}
	}
	return strings.TrimRight(trailingComment.ReplaceAllString(value, ""), " \t")
}

// ReadDotenv 读 .env（读不到就当空配置）。
func ReadDotenv(path string) map[string]string {
	if path == "" {
		return map[string]string{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseDotenv(string(data))
}

// envFileCandidates 按优先级列出可能的 .env 路径。
func envFileCandidates(explicit string) []string {
	candidates := make([]string, 0, 4)
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if value := os.Getenv(EnvFileVar); value != "" {
		candidates = append(candidates, value)
	}
	candidates = append(candidates, ".env")
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "arkjmp", ".env"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	seen := make(map[string]bool, len(candidates))
	unique := candidates[:0]
	for _, candidate := range candidates {
		cleaned := filepath.Clean(candidate)
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		unique = append(unique, cleaned)
	}
	return unique
}

// FindEnvFile 返回第一个存在的 .env。
func FindEnvFile(explicit string) string {
	for _, candidate := range envFileCandidates(explicit) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// wildcardHosts 控制器监听在通配地址时，客户端要连回环地址。
var wildcardHosts = map[string]string{
	"0.0.0.0": DefaultHost,
	"::":      "::1",
	"::0":     "::1",
	"[::]":    "::1",
}

// buildAPI 把各种写法的控制器地址归一成 scheme://host:port[path]：
//
//   - 没写端口（127.0.0.1、http://host、[::1]）→ 补 DefaultPort / port 参数
//   - 没写 scheme → http
//   - 监听在 0.0.0.0 / :: → 换成回环地址
//
// api 为空时用 host，host 也为空就用 DefaultHost。
func buildAPI(api, host, port string) string {
	defaultPort := strings.TrimSpace(port)
	if defaultPort == "" {
		defaultPort = strconv.Itoa(DefaultPort)
	}

	raw := strings.TrimSpace(api)
	if raw == "" {
		raw = strings.TrimSpace(host)
	}
	if raw == "" {
		raw = DefaultHost
	}
	original := raw
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// 解析不了（比如未加括号的 IPv6）就整串当主机名处理
		hostname := strings.Trim(original, "/ ")
		if loopback, ok := wildcardHosts[strings.ToLower(hostname)]; ok {
			hostname = loopback
		}
		return "http://" + net.JoinHostPort(hostname, defaultPort)
	}

	hostname := parsed.Hostname()
	if hostname == "" {
		hostname = DefaultHost
	}
	if loopback, ok := wildcardHosts[strings.ToLower(hostname)]; ok {
		hostname = loopback
	}
	portValue := parsed.Port()
	if portValue == "" {
		portValue = defaultPort
	}
	parsed.Host = net.JoinHostPort(hostname, portValue)
	return strings.TrimRight(parsed.String(), "/")
}

// Load 按优先级算出最终配置。
//
// env 为 nil 时读真实环境变量；dotenv 为 nil 时按 envFileCandidates 查找 .env。
func Load(ov Overrides, env map[string]string, dotenv map[string]string) Settings {
	if env == nil {
		env = environ()
	}
	envFile := ""
	if dotenv == nil {
		envFile = FindEnvFile(ov.EnvFile)
		dotenv = ReadDotenv(envFile)
	} else if ov.EnvFile != "" {
		envFile = ov.EnvFile
	}

	pick := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(env[name]); value != "" {
				return value
			}
			if value := strings.TrimSpace(dotenv[name]); value != "" {
				return value
			}
		}
		return ""
	}

	host := pick("ARKJMP_HOST")
	port := pick("ARKJMP_PORT")

	api := strings.TrimSpace(ov.API)
	if api == "" {
		switch {
		case strings.TrimSpace(env["ARKJMP_API"]) != "":
			api = env["ARKJMP_API"]
		case strings.TrimSpace(env["ARKJMP_HOST"]) != "" || strings.TrimSpace(env["ARKJMP_PORT"]) != "":
			api = buildAPI("", host, port)
		case strings.TrimSpace(dotenv["ARKJMP_API"]) != "":
			api = dotenv["ARKJMP_API"]
		case host != "" || port != "":
			api = buildAPI("", host, port)
		}
	}
	secret := strings.TrimSpace(ov.Secret)
	if secret == "" {
		secret = pick("ARKJMP_SECRET")
	}
	testURL := strings.TrimSpace(ov.TestURL)
	if testURL == "" {
		testURL = pick("ARKJMP_TEST_URL")
	}
	timeout := ov.Timeout
	timeoutSet := ov.TimeoutSet
	if !timeoutSet {
		if raw := pick("ARKJMP_TIMEOUT"); raw != "" {
			if value, err := strconv.ParseFloat(raw, 64); err == nil && ValidTimeout(value) {
				timeout, timeoutSet = value, true
			}
		}
	}

	path := strings.TrimSpace(ov.ConfigPath)
	if path == "" {
		path = pick("ARKJMP_CONFIG")
	}

	configPath := path
	if api == "" || secret == "" || testURL == "" || !timeoutSet {
		resolved, cfg := loadConfig(path)
		if resolved != "" {
			configPath = resolved
		}
		if cfg != nil {
			if api == "" && cfg.ExternalController != "" {
				api = buildAPI(cfg.ExternalController, "", port)
			}
			if secret == "" && cfg.Secret != "" {
				secret = cfg.Secret
			}
			if testURL == "" && cfg.TestURL != "" {
				testURL = cfg.TestURL
			}
			if !timeoutSet && cfg.Timeout != "" {
				if value, err := strconv.ParseFloat(cfg.Timeout, 64); err == nil && ValidTimeout(value) {
					timeout, timeoutSet = value, true
				}
			}
		}
	}

	if !timeoutSet || !ValidTimeout(timeout) {
		timeout = DefaultTimeout
	}
	if testURL == "" {
		testURL = client.DefaultTestURL
	}
	return Settings{
		API:        buildAPI(api, host, port),
		Secret:     secret,
		TestURL:    testURL,
		Timeout:    timeout,
		ConfigPath: configPath,
		EnvFile:    envFile,
	}
}

// ValidTimeout 判断秒数能否表示成正的 time.Duration。
func ValidTimeout(seconds float64) bool {
	nanoseconds := seconds * float64(time.Second)
	return nanoseconds >= 1 && nanoseconds < float64(math.MaxInt64)
}

func environ() map[string]string {
	env := make(map[string]string, len(os.Environ()))
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok {
			env[key] = value
		}
	}
	return env
}
