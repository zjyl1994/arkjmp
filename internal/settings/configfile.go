package settings

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fileConfig 是从 mihomo 配置文件里读到的、我们能用的字段。
type fileConfig struct {
	ExternalController string
	Secret             string
	TestURL            string
	Timeout            string // 原始文本，解析失败就当没写
}

// configFileNames 是一个目录下按优先级尝试的文件名。
var configFileNames = []string{"config.yaml", "config.yml", "config.json"}

// configCandidates 返回自动发现的候选配置文件（按优先级）。
//
// 这是变量而不是函数，测试里可以替换掉，避免读到本机真实的 mihomo 配置。
var configCandidates = defaultConfigCandidates

// defaultConfigCandidates 的优先级：
//
//  1. 正在运行的 mihomo/clash 进程的命令行（-f / -d）
//  2. 当前目录
//  3. 用户配置目录（$XDG_CONFIG_HOME 或 ~/.config，macOS 是 ~/Library/Application Support）
//  4. 系统/发行版常见位置（/etc/mihomo、OpenWrt 的 /var/etc/mihomo 等）
//  5. 可执行文件同目录
func defaultConfigCandidates() []string {
	var candidates []string
	candidates = append(candidates, processConfigPaths()...)

	candidates = append(candidates, configFileNames...)
	candidates = append(candidates, filepath.Join("conf", "config.yaml"), filepath.Join("conf", "config.json"))

	if userDir, err := os.UserConfigDir(); err == nil {
		for _, app := range []string{"mihomo", "clash"} {
			for _, name := range configFileNames {
				candidates = append(candidates, filepath.Join(userDir, app, name))
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, app := range []string{"mihomo", "clash"} {
			for _, name := range configFileNames {
				candidates = append(candidates, filepath.Join(home, ".config", app, name))
				// macOS GUI 客户端常把配置放这里
				candidates = append(candidates,
					filepath.Join(home, "Library", "Application Support", app, name))
			}
		}
		for _, name := range configFileNames {
			candidates = append(candidates, filepath.Join(home, ".local", "share", "mihomo", name))
		}
	}

	for _, dir := range []string{
		"/etc/mihomo", "/etc/clash",
		"/usr/local/etc/mihomo", "/usr/local/etc/clash",
		"/opt/etc/mihomo", // Entware
		"/var/etc/mihomo", // OpenWrt
		"/var/lib/mihomo", // -d 的工作目录
	} {
		for _, name := range configFileNames {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}

	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		for _, name := range configFileNames {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	return dedupePaths(candidates)
}

// loadConfig 读配置文件：显式路径优先；没给（或给了读不到）才自动发现。
//
// 返回实际用到的路径（可能为空）和内容。
func loadConfig(explicit string) (string, *fileConfig) {
	if explicit != "" {
		// 显式指定就算权威：读不到就当作"没有可用的兜底配置"，不去猜别的文件
		return explicit, readConfig(explicit)
	}
	for _, candidate := range configCandidates() {
		if cfg := readConfig(candidate); cfg != nil {
			return candidate, cfg
		}
	}
	return "", nil
}

// readConfig 按内容判断格式：先看是不是 JSON（{ 开头），否则按 YAML 的单行标量读。
// 这样扩展名写错（.yaml 里放 JSON）也能读。
func readConfig(path string) *fileConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\ufeff")))
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		return parseJSONConfig(trimmed)
	}
	return parseSimpleYAML(string(trimmed))
}

// parseSimpleYAML 只认顶层 `key: value` 标量。
//
// mihomo 的配置动辄几千行（规则、proxy-groups），但我们只需要 external-controller
// 和 secret 两个顶层标量，所以不引入 YAML 依赖：
//   - 只看不带缩进的行，所以 proxy-groups/rules 里的同名键不会被误读
//   - 复用 .env 的引号/行尾注释处理
//   - 块标量（| / >）、锚点这类值直接跳过（读不到就走默认值）
func parseSimpleYAML(text string) *fileConfig {
	cfg := &fileConfig{}
	found := false
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			continue // 缩进行是嵌套内容，不碰
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))
		if value == "" || value == "~" || value == "null" || value == "|" || value == ">" {
			continue
		}
		switch key {
		case "external-controller":
			cfg.ExternalController = value
			found = true
		case "external-controller-tls":
			if cfg.ExternalController == "" {
				cfg.ExternalController = tlsController(value)
			}
			found = true
		case "secret":
			cfg.Secret = value
			found = true
		}
	}
	if !found {
		return nil
	}
	return cfg
}

// parseJSONConfig 读 mihomo 的 config.json，顺带支持 test-url / timeout 这两个自造键。
func parseJSONConfig(data []byte) *fileConfig {
	var fields map[string]any
	if err := jsonUnmarshal(data, &fields); err != nil {
		return nil
	}
	cfg := &fileConfig{}
	if value, ok := fields["external-controller"].(string); ok {
		cfg.ExternalController = value
	}
	if value, ok := fields["api"].(string); ok && cfg.ExternalController == "" {
		cfg.ExternalController = value
	}
	if value, ok := fields["external-controller-tls"].(string); ok && cfg.ExternalController == "" {
		cfg.ExternalController = tlsController(value)
	}
	if value, ok := fields["secret"].(string); ok {
		cfg.Secret = value
	}
	if value, ok := fields["test-url"].(string); ok {
		cfg.TestURL = value
	}
	switch value := fields["timeout"].(type) {
	case float64:
		cfg.Timeout = strconv.FormatFloat(value, 'f', -1, 64)
	case string:
		cfg.Timeout = value
	}
	return cfg
}

func tlsController(address string) string {
	address = strings.TrimSpace(address)
	if address != "" && !strings.Contains(address, "://") {
		return "https://" + address
	}
	return address
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		cleaned := filepath.Clean(path)
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	return out
}
