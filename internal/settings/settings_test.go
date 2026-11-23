package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// disableDiscovery 让测试不去读本机真实的 mihomo 配置，保证离线可重复。
func disableDiscovery(t *testing.T) {
	t.Helper()
	original := configCandidates
	configCandidates = func() []string { return nil }
	t.Cleanup(func() { configCandidates = original })
}

func writeConfig(t *testing.T, dir string, payload map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseDotenv(t *testing.T) {
	parsed := ParseDotenv(`
# 注释
export A=1
B="two words"
C='hash#inside'  # 行尾注释
D = plain value` + "  " + `
E=
F=ab#cd
`)
	want := map[string]string{
		"A": "1",
		"B": "two words",
		"C": "hash#inside",
		"D": "plain value",
		"E": "",
		"F": "ab#cd",
	}
	for key, value := range want {
		if parsed[key] != value {
			t.Errorf("%s = %q, 期望 %q", key, parsed[key], value)
		}
	}
	if len(parsed) != len(want) {
		t.Errorf("解析出 %d 项，期望 %d 项: %v", len(parsed), len(want), parsed)
	}
}

func TestPortAndSecretComeFromDotenv(t *testing.T) {
	disableDiscovery(t)
	got := Load(Overrides{}, map[string]string{}, map[string]string{
		"ARKJMP_HOST":   "10.0.0.2",
		"ARKJMP_PORT":   "7890",
		"ARKJMP_SECRET": "s3",
	})
	if got.API != "http://10.0.0.2:7890" {
		t.Fatalf("api = %q", got.API)
	}
	if got.Secret != "s3" {
		t.Fatalf("secret = %q", got.Secret)
	}
}

func TestRealEnvBeatsDotenv(t *testing.T) {
	disableDiscovery(t)
	got := Load(Overrides{}, map[string]string{"ARKJMP_PORT": "8888"}, map[string]string{
		"ARKJMP_PORT":   "7890",
		"ARKJMP_SECRET": "dot",
	})
	if got.API != "http://127.0.0.1:8888" {
		t.Fatalf("api = %q", got.API)
	}
	if got.Secret != "dot" {
		t.Fatalf("secret = %q", got.Secret)
	}
}

func TestCLIArgsBeatEverything(t *testing.T) {
	disableDiscovery(t)
	got := Load(Overrides{
		API:        "http://1.2.3.4:80",
		Secret:     "cli",
		Timeout:    9,
		TimeoutSet: true,
	}, map[string]string{"ARKJMP_SECRET": "env"}, map[string]string{"ARKJMP_PORT": "7890"})
	if got.API != "http://1.2.3.4:80" || got.Secret != "cli" {
		t.Fatalf("settings = %+v", got)
	}
	if got.Timeout != 9 {
		t.Fatalf("timeout = %v", got.Timeout)
	}
}

func TestConfigJSONIsFallbackOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, map[string]any{
		"external-controller": "127.0.0.1:1234",
		"secret":              "cfg-secret",
		"timeout":             7,
	})

	got := Load(Overrides{ConfigPath: path}, map[string]string{}, map[string]string{})
	if got.API != "http://127.0.0.1:1234" || got.Secret != "cfg-secret" {
		t.Fatalf("settings = %+v", got)
	}
	if got.Timeout != 7 {
		t.Fatalf("timeout = %v", got.Timeout)
	}

	// .env 的端口压过 config.json，缺项仍可兜底
	got = Load(Overrides{ConfigPath: path}, map[string]string{}, map[string]string{"ARKJMP_PORT": "7000"})
	if got.API != "http://127.0.0.1:7000" {
		t.Fatalf("api = %q", got.API)
	}
	if got.Secret != "cfg-secret" {
		t.Fatalf("secret = %q", got.Secret)
	}
}

func TestNoHardcodedSecret(t *testing.T) {
	got := Load(
		Overrides{ConfigPath: filepath.Join(t.TempDir(), "missing.json")},
		map[string]string{}, map[string]string{},
	)
	if got.Secret != "" {
		t.Fatalf("没有配置时 secret 应该为空，实际 %q", got.Secret)
	}
	if got.API != buildAPI("", "", "") {
		t.Fatalf("api = %q", got.API)
	}
}

func TestEnvFileDiscovery(t *testing.T) {
	disableDiscovery(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(path, []byte("ARKJMP_PORT=4321\nARKJMP_SECRET=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := Load(Overrides{EnvFile: path}, map[string]string{}, nil)
	if got.API != "http://127.0.0.1:4321" || got.Secret != "abc" {
		t.Fatalf("settings = %+v", got)
	}
	if got.EnvFile != path {
		t.Fatalf("env_file = %q", got.EnvFile)
	}
}

func TestBuildAPI(t *testing.T) {
	cases := []struct {
		api, host, port, want string
	}{
		// 什么都不配：默认回环 + 9090
		{"", "", "", "http://127.0.0.1:9090"},
		// 没写端口就补 9090（各种写法）
		{"", "127.0.0.1", "", "http://127.0.0.1:9090"},
		{"127.0.0.1", "", "", "http://127.0.0.1:9090"},
		{"http://127.0.0.1", "", "", "http://127.0.0.1:9090"},
		{"http://127.0.0.1/", "", "", "http://127.0.0.1:9090"},
		{"localhost", "", "", "http://localhost:9090"},
		{"https://example.com", "", "", "https://example.com:9090"},
		{"http://example.com/base", "", "", "http://example.com:9090/base"},
		// 写了端口就用写的
		{"", "127.0.0.1", "8080", "http://127.0.0.1:8080"},
		{"", "example.com:1234", "", "http://example.com:1234"},
		{"", "https://example.com", "443", "https://example.com:443"},
		{"127.0.0.1:9091", "", "", "http://127.0.0.1:9091"},
		{"http://a.b:1/", "", "", "http://a.b:1"},
		{"127.0.0.1:", "", "", "http://127.0.0.1:9090"},
		// IPv6
		{"", "[::1]", "", "http://[::1]:9090"},
		{"", "::1", "", "http://[::1]:9090"},
		{"", "[::1]:9091", "", "http://[::1]:9091"},
		{"http://[::1]", "", "", "http://[::1]:9090"},
		// 监听在通配地址时连回环
		{"", "0.0.0.0", "1234", "http://127.0.0.1:1234"},
		{"0.0.0.0:9090", "", "", "http://127.0.0.1:9090"},
		{"", "::", "", "http://[::1]:9090"},
	}
	for _, c := range cases {
		if got := buildAPI(c.api, c.host, c.port); got != c.want {
			t.Errorf("buildAPI(%q,%q,%q) = %q, 期望 %q", c.api, c.host, c.port, got, c.want)
		}
	}
}

func TestPortDefaultsTo9090(t *testing.T) {
	disableDiscovery(t)
	// 只给 host（或端口写空）都要落到 9090
	for _, dotenv := range []map[string]string{
		{"ARKJMP_HOST": "127.0.0.1"},
		{"ARKJMP_HOST": "127.0.0.1", "ARKJMP_PORT": ""},
		{"ARKJMP_PORT": ""},
		{},
	} {
		got := Load(Overrides{}, map[string]string{}, dotenv)
		if got.API != "http://127.0.0.1:9090" {
			t.Errorf("dotenv=%v -> %q", dotenv, got.API)
		}
	}

	// --api 不给端口也一样补 9090
	got := Load(Overrides{API: "http://192.0.2.10"}, map[string]string{}, map[string]string{})
	if got.API != "http://192.0.2.10:9090" {
		t.Fatalf("api = %q", got.API)
	}
}

func TestEmptySecretMeansNoAuth(t *testing.T) {
	disableDiscovery(t)
	// 没配密码：Secret 为空，客户端就不会带 Authorization
	for _, dotenv := range []map[string]string{{}, {"ARKJMP_SECRET": ""}, {"ARKJMP_SECRET": "   "}} {
		got := Load(
			Overrides{ConfigPath: filepath.Join(t.TempDir(), "missing.json")},
			map[string]string{}, dotenv,
		)
		if got.Secret != "" {
			t.Errorf("dotenv=%v -> secret %q，应该为空", dotenv, got.Secret)
		}
	}

	// 配了才用
	got := Load(Overrides{}, map[string]string{}, map[string]string{"ARKJMP_SECRET": " s3 "})
	if got.Secret != "s3" {
		t.Fatalf("secret = %q（应该去掉首尾空格）", got.Secret)
	}
}

func TestSecretAutoDetectedFromConfigJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, map[string]any{
		"external-controller": "0.0.0.0:9090",
		"secret":              "from-config",
	})

	// .env 没写密码 -> 从 mihomo config.json 自动补上
	got := Load(Overrides{ConfigPath: path}, map[string]string{}, map[string]string{"ARKJMP_SECRET": ""})
	if got.Secret != "from-config" {
		t.Fatalf("secret = %q", got.Secret)
	}
	// 通配监听地址也要换成回环
	if got.API != "http://127.0.0.1:9090" {
		t.Fatalf("api = %q", got.API)
	}

	// 显式给了密码就以显式的为准
	got = Load(Overrides{ConfigPath: path, Secret: "cli"}, map[string]string{}, map[string]string{})
	if got.Secret != "cli" {
		t.Fatalf("secret = %q", got.Secret)
	}
}

func TestParseSimpleYAML(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		control string
		secret  string
		wantNil bool
	}{
		{
			name:    "基本写法",
			input:   "mixed-port: 7890\nexternal-controller: 127.0.0.1:9090\nsecret: abc123\n",
			control: "127.0.0.1:9090",
			secret:  "abc123",
		},
		{
			name:    "引号与行尾注释",
			input:   "# 头部注释\nexternal-controller: \"0.0.0.0:9091\"   # 控制器\nsecret: 'x#y'\n",
			control: "0.0.0.0:9091",
			secret:  "x#y",
		},
		{
			name:    "空密码不覆盖",
			input:   "external-controller: 127.0.0.1:9090\nsecret: \"\"\n",
			control: "127.0.0.1:9090",
		},
		{
			name:    "嵌套同名键不算",
			input:   "proxy-groups:\n  - name: a\n    external-controller: 1.2.3.4:1\n    secret: nested\nexternal-controller: 127.0.0.1:9090\n",
			control: "127.0.0.1:9090",
		},
		{
			name:    "TLS 控制器",
			input:   "external-controller-tls: 127.0.0.1:9443\n",
			control: "https://127.0.0.1:9443",
		},
		{
			name:    "CRLF",
			input:   "external-controller: 127.0.0.1:9090\r\nsecret: s\r\n",
			control: "127.0.0.1:9090",
			secret:  "s",
		},
		{
			name:    "没有我们认识的键",
			input:   "mixed-port: 7890\nmode: rule\n",
			wantNil: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := parseSimpleYAML(c.input)
			if c.wantNil {
				if cfg != nil {
					t.Fatalf("应该返回 nil，实际 %+v", cfg)
				}
				return
			}
			if cfg == nil {
				t.Fatal("不该返回 nil")
			}
			if cfg.ExternalController != c.control {
				t.Errorf("controller = %q, 期望 %q", cfg.ExternalController, c.control)
			}
			if cfg.Secret != c.secret {
				t.Errorf("secret = %q, 期望 %q", cfg.Secret, c.secret)
			}
		})
	}
}

func TestReadConfigDetectsFormatByContent(t *testing.T) {
	dir := t.TempDir()

	jsonAsYAML := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(jsonAsYAML, []byte(`{"external-controller":"127.0.0.1:9092","secret":"j"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(jsonAsYAML)
	if cfg == nil || cfg.ExternalController != "127.0.0.1:9092" || cfg.Secret != "j" {
		t.Fatalf("按内容识别 JSON 失败: %+v", cfg)
	}

	noExt := filepath.Join(dir, "config")
	if err := os.WriteFile(noExt, []byte("external-controller: 127.0.0.1:9093\nsecret: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = readConfig(noExt)
	if cfg == nil || cfg.ExternalController != "127.0.0.1:9093" || cfg.Secret != "y" {
		t.Fatalf("无扩展名的 YAML 失败: %+v", cfg)
	}

	if readConfig(filepath.Join(dir, "nope.yaml")) != nil {
		t.Fatal("不存在的文件应该返回 nil")
	}
}

func TestLooksLikeProxy(t *testing.T) {
	for _, name := range []string{"/usr/local/bin/mihomo", "mihomo", "/opt/clash/clash-meta", "/usr/bin/clash"} {
		if !looksLikeProxy(name) {
			t.Errorf("%s 应该被认为是内核", name)
		}
	}
	for _, name := range []string{"/bin/bash", "curl", "arkjmp"} {
		if looksLikeProxy(name) {
			t.Errorf("%s 不该被当成内核", name)
		}
	}
}

func TestConfigsFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cwd  string
		want []string
	}{
		{
			name: "systemd 风格 -d + -f",
			args: []string{"/usr/local/bin/mihomo", "-d", "/var/lib/mihomo", "-f", "/etc/mihomo/config.json"},
			want: []string{"/etc/mihomo/config.json", "/var/lib/mihomo/config.yaml", "/var/lib/mihomo/config.yml", "/var/lib/mihomo/config.json"},
		},
		{
			name: "等号写法与双横线",
			args: []string{"mihomo", "--config=/etc/mihomo/config.yaml"},
			want: []string{"/etc/mihomo/config.yaml"},
		},
		{
			name: "只有 -d，配置取目录下的默认名",
			args: []string{"mihomo", "-d", "/etc/mihomo"},
			want: []string{"/etc/mihomo/config.yaml", "/etc/mihomo/config.yml", "/etc/mihomo/config.json"},
		},
		{
			name: "相对路径按 cwd 展开",
			args: []string{"mihomo", "-f", "config.yaml"},
			cwd:  "/home/me/mihomo",
			want: []string{"/home/me/mihomo/config.yaml"},
		},
		{
			name: "没带 -f/-d 就没有候选",
			args: []string{"mihomo"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := configsFromArgs(c.args, c.cwd)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("got %v, 期望 %v", got, c.want)
			}
		})
	}
}

func TestAutoDiscoveryUsesFirstReadableCandidate(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("external-controller: 127.0.0.1:9095\nsecret: auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := configCandidates
	configCandidates = func() []string { return []string{missing, yamlPath} }
	t.Cleanup(func() { configCandidates = original })

	got := Load(Overrides{}, map[string]string{}, map[string]string{})
	if got.API != "http://127.0.0.1:9095" || got.Secret != "auto" {
		t.Fatalf("settings = %+v", got)
	}
	if got.ConfigPath != yamlPath {
		t.Fatalf("config_path = %q", got.ConfigPath)
	}
}

func TestExplicitConfigIsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "fallback.yaml")
	if err := os.WriteFile(fallback, []byte("external-controller: 127.0.0.1:9099\nsecret: fallback\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := configCandidates
	configCandidates = func() []string { return []string{fallback} }
	t.Cleanup(func() { configCandidates = original })

	missing := filepath.Join(dir, "typo.yaml")
	got := Load(Overrides{ConfigPath: missing}, map[string]string{}, map[string]string{})
	if got.Secret != "" {
		t.Fatalf("显式路径读不到时不该去猜别的文件，secret = %q", got.Secret)
	}
	if got.API != "http://127.0.0.1:9090" {
		t.Fatalf("api = %q", got.API)
	}
	if got.ConfigPath != missing {
		t.Fatalf("config_path = %q", got.ConfigPath)
	}
}

func TestYAMLConfigSuppliesControllerAndSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "mixed-port: 7890\nexternal-controller: 0.0.0.0:9090\nsecret: yaml-secret\n" +
		"proxy-groups:\n  - name: Manual\n    secret: nested\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := Load(Overrides{ConfigPath: path}, map[string]string{}, map[string]string{})
	if got.Secret != "yaml-secret" {
		t.Fatalf("secret = %q", got.Secret)
	}
	// 0.0.0.0 要换成回环
	if got.API != "http://127.0.0.1:9090" {
		t.Fatalf("api = %q", got.API)
	}
}

func TestProcessScanCanBeDisabled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("只有 Linux 才有 /proc")
	}
	t.Setenv("ARKJMP_NO_PROC", "1")
	if paths := processConfigPaths(); paths != nil {
		t.Fatalf("ARKJMP_NO_PROC 生效时不该扫进程，实际 %v", paths)
	}
}

func TestIsAllDigits(t *testing.T) {
	for _, value := range []string{"1", "506037"} {
		if !isAllDigits(value) {
			t.Errorf("%q 应该全是数字", value)
		}
	}
	for _, value := range []string{"", "12a", "/proc", " 1", "1 "} {
		if isAllDigits(value) {
			t.Errorf("%q 不该被当成 pid", value)
		}
	}
}

func TestFirstNulField(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/mihomo\x00-d\x00/etc/mihomo\x00": "/usr/local/bin/mihomo",
		"mihomo": "mihomo",
		"":       "",
	}
	for input, want := range cases {
		if got := string(firstNulField([]byte(input))); got != want {
			t.Errorf("firstNulField(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestAddressSourcePrecedence(t *testing.T) {
	dotenv := map[string]string{"ARKJMP_API": "http://dotenv:9000", "ARKJMP_PORT": "9091"}
	for _, test := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"environment host", map[string]string{"ARKJMP_HOST": "env"}, "http://env:9091"},
		{"environment port", map[string]string{"ARKJMP_PORT": "9001"}, "http://127.0.0.1:9001"},
		{"environment API", map[string]string{"ARKJMP_API": "http://env:9002", "ARKJMP_HOST": "ignored"}, "http://env:9002"},
		{"dotenv API", map[string]string{}, "http://dotenv:9000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Load(Overrides{ConfigPath: filepath.Join(t.TempDir(), "not-present")}, test.env, dotenv)
			if got.API != test.want {
				t.Fatalf("API = %q, want %q", got.API, test.want)
			}
		})
	}
}

func TestLoadUsesHTTPSForTLSController(t *testing.T) {
	for _, content := range []string{
		"external-controller-tls: 0.0.0.0:9443\nsecret: example\n",
		`{"external-controller-tls":"0.0.0.0:9443","secret":"example"}`,
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got := Load(Overrides{ConfigPath: path}, map[string]string{}, map[string]string{})
		if got.API != "https://127.0.0.1:9443" || got.Secret != "example" {
			t.Fatalf("加载 TLS 地址失败，API = %q", got.API)
		}
	}
}
