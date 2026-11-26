package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	json "encoding/json/v2"
)

// newFakeController 起一个最小的 mihomo 控制器，顺便验证鉴权头。
func newFakeController(t *testing.T, groupType string, keepPin bool) *httptest.Server {
	t.Helper()

	proxies := map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual"}},
		"Manual": map[string]any{"type": groupType, "now": "HK-01", "all": []string{"HK-01", "SG-01", "DIRECT"}},
		"HK-01":  map[string]any{"type": "Vless", "history": []map[string]any{{"delay": 120}}},
		"SG-01":  map[string]any{"type": "Vless", "history": []map[string]any{}},
		"DIRECT": map[string]any{"type": "Direct"},
	}
	var mu sync.Mutex
	writeJSON := func(w http.ResponseWriter, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("marshal: %v", err)
			http.Error(w, "marshal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": "v1.19.34"})
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"mode": "rule"})
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, map[string]any{"proxies": proxies})
	})
	mux.HandleFunc("/providers/proxies", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"providers": map[string]any{}})
	})
	mux.HandleFunc("/proxies/HK-01/delay", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"delay": 88})
	})
	mux.HandleFunc("/proxies/SG-01/delay", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"delay": 46})
	})
	mux.HandleFunc("/proxies/Manual", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(readBody(t, r)), &body); err != nil || body.Name == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		group := proxies["Manual"].(map[string]any)
		group["now"] = body.Name
		if groupType == "Fallback" {
			if keepPin {
				group["fixed"] = body.Name
			} else {
				group["fixed"] = ""
			}
		}
		mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// readBody 读请求体；测试里的请求都很小，直接读完。
func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("读请求体: %v", err)
		return ""
	}
	return string(data)
}

// setupEnv 把配置全部固定成测试值，避免读到本机真实的 .env / mihomo config。
func setupEnv(t testing.TB, api string) {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKJMP_API", api)
	t.Setenv("ARKJMP_SECRET", "test-secret")
	t.Setenv("ARKJMP_TEST_URL", "https://example.com/204")
	t.Setenv("ARKJMP_TIMEOUT", "2")
	t.Setenv("ARKJMP_ENV_FILE", envFile)
	t.Setenv("ARKJMP_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ARKJMP_NO_PROC", "1")
}

// runMain 跑 Main 并把 stdout 抓回来（--json 只写 stdout）；stderr 也接走，免得测试输出变脏。
func runMain(t *testing.T, argv ...string) (string, int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := Main(argv)
	os.Stdout, os.Stderr = oldOut, oldErr
	if err := outW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errW.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(outR)
	outR.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, errR); err != nil {
		t.Fatal(err)
	}
	errR.Close()
	return string(data), code
}

func decodeJSON[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("JSON 解析失败: %v\n输出:\n%s", err, raw)
	}
	return value
}

func TestMainEndToEnd(t *testing.T) {
	server := newFakeController(t, "Selector", false)
	setupEnv(t, server.URL)

	t.Run("status", func(t *testing.T) {
		raw, code := runMain(t, "status", "--json")
		if code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[statusPayload](t, raw)
		if !got.OK || got.Version != "v1.19.34" || got.Mode != "rule" || got.Groups != 1 || !got.Auth {
			t.Fatalf("status = %+v", got)
		}
	})

	t.Run("group", func(t *testing.T) {
		raw, code := runMain(t, "group", "Manual", "--json")
		if code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[groupPayload](t, raw)
		if !got.OK || len(got.Nodes) != 3 {
			t.Fatalf("group = %+v", got)
		}
		if got.Nodes[0].Name != "HK-01" || got.Nodes[0].Delay == nil || *got.Nodes[0].Delay != 120 {
			t.Fatalf("第一个节点 = %+v", got.Nodes[0])
		}
	})

	t.Run("switch dry-run", func(t *testing.T) {
		raw, code := runMain(t, "switch", "Manual", "SG-01", "--dry-run", "--json")
		if code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[switchPayload](t, raw)
		if !got.OK || got.Node != "SG-01" || got.Changed || got.WouldChange == nil || !*got.WouldChange {
			t.Fatalf("switch = %+v", got)
		}
	})

	t.Run("switch for real", func(t *testing.T) {
		raw, code := runMain(t, "switch", "Manual", "SG-01", "--json")
		if code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[switchPayload](t, raw)
		if !got.OK || !got.Changed || got.Now == nil || *got.Now != "SG-01" {
			t.Fatalf("switch = %+v", got)
		}
	})

	t.Run("delay", func(t *testing.T) {
		raw, code := runMain(t, "delay", "HK-01", "--json")
		if code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[delayPayload](t, raw)
		if !got.OK || got.Delay != 88 || got.Timeout {
			t.Fatalf("delay = %+v", got)
		}
	})

	t.Run("missing node is exit 3", func(t *testing.T) {
		raw, code := runMain(t, "switch", "Manual", "no-such-node", "--json")
		if code != ExitNotFound {
			t.Fatalf("exit = %d\n%s", code, raw)
		}
		got := decodeJSON[errorPayload](t, raw)
		if got.OK || got.Error.ExitCode != ExitNotFound || !strings.Contains(got.Error.Message, "no-such-node") {
			t.Fatalf("error = %+v", got)
		}
	})

	t.Run("bad usage is exit 2", func(t *testing.T) {
		if _, code := runMain(t, "no-such-command"); code != ExitUsage {
			t.Fatalf("exit = %d", code)
		}
	})

	t.Run("tui rejects json", func(t *testing.T) {
		if _, code := runMain(t, "tui", "--json"); code != ExitUsage {
			t.Fatalf("exit = %d", code)
		}
	})
}

func TestMainSwitchPinsCurrentFallbackNode(t *testing.T) {
	server := newFakeController(t, "Fallback", true)
	setupEnv(t, server.URL)

	preview := func(wantChange bool) {
		t.Helper()
		raw, code := runMain(t, "switch", "Manual", "HK-01", "--dry-run", "--json")
		got := decodeJSON[switchPayload](t, raw)
		if code != ExitOK || got.WouldChange == nil || *got.WouldChange != wantChange {
			t.Fatalf("exit = %d, preview = %s", code, raw)
		}
	}
	preview(true)

	raw, code := runMain(t, "switch", "Manual", "HK-01", "--json")
	got := decodeJSON[switchPayload](t, raw)
	if code != ExitOK || !got.Changed || got.Now == nil || *got.Now != "HK-01" {
		t.Fatalf("exit = %d, switch = %s", code, raw)
	}
	preview(false)
}

func TestMainSwitchReportsLostFallbackPin(t *testing.T) {
	server := newFakeController(t, "Fallback", false)
	setupEnv(t, server.URL)

	raw, code := runMain(t, "switch", "Manual", "HK-01", "--json")
	got := decodeJSON[errorPayload](t, raw)
	if code != ExitError || got.OK || !strings.Contains(got.Error.Message, "未固定节点") {
		t.Fatalf("exit = %d, switch = %s", code, raw)
	}
}

func TestJSONUsageErrors(t *testing.T) {
	for _, argv := range [][]string{
		{"invalid-command", "--json"},
		{"--json", "groups", "--invalid-option"},
		{"delay", "--json"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			raw, code := runMain(t, argv...)
			got := decodeJSON[errorPayload](t, raw)
			if code != ExitUsage || got.OK || got.Error.ExitCode != ExitUsage || got.Error.Message == "" {
				t.Fatalf("exit = %d, output = %s", code, raw)
			}
		})
	}

	if raw, code := runMain(t, "groups", "--api", "--json", "unexpected"); code != ExitUsage || raw != "" {
		t.Fatalf("选项值不应被当作 --json，exit = %d, output = %q", code, raw)
	}
}

func TestGroupCommandsOnlyRequestProxies(t *testing.T) {
	var requests []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"proxies":{"Manual":{"type":"Selector","now":"HK","all":["HK"]}}}`)
	}))
	t.Cleanup(server.Close)
	setupEnv(t, server.URL)

	for _, command := range []string{"groups", "current", "chain"} {
		if raw, code := runMain(t, command, "--json"); code != ExitOK {
			t.Fatalf("%s: exit = %d, output = %s", command, code, raw)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/proxies", "/proxies", "/proxies"}; !slices.Equal(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestGroupTestJSONDoesNotShowStaleDelays(t *testing.T) {
	const provider = `{"proxies":[{"name":"HK","type":"Vless","history":[{"delay":123}]}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /proxies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"proxies":{"Manual":{"type":"Selector","now":"HK","all":["HK"]}}}`)
	})
	mux.HandleFunc("GET /providers/proxies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"providers":{"airport":`+provider+`}}`)
	})
	mux.HandleFunc("GET /providers/proxies/airport", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, provider)
	})
	mux.HandleFunc("GET /providers/proxies/airport/healthcheck", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	setupEnv(t, server.URL)

	raw, code := runMain(t, "test", "--json", "--quiet")
	got := decodeJSON[testPayload](t, raw)
	if code != ExitOK || got.Total != 1 || got.Available != 0 || !got.Partial ||
		len(got.Nodes) != 1 || got.Nodes[0].Delay != nil || got.Nodes[0].Timeout {
		t.Fatalf("exit = %d, output = %s", code, raw)
	}
}
