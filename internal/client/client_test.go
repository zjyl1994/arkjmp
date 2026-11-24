package client

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecodeObjectKeepsOrderAndValues(t *testing.T) {
	// 每个值都足够长，确保解码器复用缓冲区时不会串数据
	long := strings.Repeat("节点", 200)
	data := []byte(`{"z":"` + long + `","a":{"now":"x","all":["a","b"]},"m":["` + long + `"]}`)

	obj, err := DecodeObject(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(obj.Keys, ","); got != "z,a,m" {
		t.Fatalf("键顺序 = %s", got)
	}
	var z string
	if err := json.Unmarshal(obj.Items["z"], &z); err != nil {
		t.Fatal(err)
	}
	if z != long {
		t.Fatalf("z 被解码器的缓冲区覆盖了（长度 %d，期望 %d）", len(z), len(long))
	}
	var m []string
	if err := json.Unmarshal(obj.Items["m"], &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[0] != long {
		t.Fatalf("m 被覆盖了: %d 项", len(m))
	}
	var a struct {
		Now string   `json:"now"`
		All []string `json:"all"`
	}
	if err := json.Unmarshal(obj.Items["a"], &a); err != nil {
		t.Fatal(err)
	}
	if a.Now != "x" || len(a.All) != 2 {
		t.Fatalf("a = %+v", a)
	}
}

func TestDecodeObjectRejectsNonObject(t *testing.T) {
	if _, err := DecodeObject([]byte(`[1,2]`)); err == nil {
		t.Fatal("数组应该报错")
	}
}

// newServer 起一个假控制器，handler 按路径返回。
func newServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := routes[r.URL.Path]; ok {
			handler(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAuthHeaderAndVersion(t *testing.T) {
	var seenAuth string
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/version": func(w http.ResponseWriter, r *http.Request) {
			seenAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"version":"v1.19.34"}`)
		},
	})
	c := New(server.URL+"/", "s3cret", 5)
	defer c.Close()

	version, err := c.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1.19.34" {
		t.Fatalf("version = %q", version)
	}
	if seenAuth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", seenAuth)
	}
	if c.Base() != server.URL {
		t.Fatalf("base 没去掉尾部斜杠: %q", c.Base())
	}
}

func TestNoAuthHeaderWhenSecretEmpty(t *testing.T) {
	for _, secret := range []string{"", "   "} {
		var seen []string
		server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
			"/version": func(w http.ResponseWriter, r *http.Request) {
				if values, ok := r.Header["Authorization"]; ok {
					seen = values
				}
				io.WriteString(w, `{"version":"v1"}`)
			},
		})
		c := New(server.URL, secret, 5)
		if _, err := c.Version(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.Close()
		if len(seen) != 0 {
			t.Fatalf("secret=%q 时不该带 Authorization: %v", secret, seen)
		}
	}
}

func TestProxiesAndMode(t *testing.T) {
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/proxies": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"proxies":{"GLOBAL":{"type":"Selector","now":"Manual","all":["Manual"]},
				"Manual":{"type":"Selector","now":"HK-01","all":["HK-01"]},
				"HK-01":{"type":"Vless","history":[{"delay":123}]}}}`)
		},
		"/configs": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"mode":"rule","port":7890}`)
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	proxies, err := c.Proxies(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(proxies.Keys, ","); got != "GLOBAL,Manual,HK-01" {
		t.Fatalf("键顺序 = %s", got)
	}
	mode, err := c.Mode(t.Context())
	if err != nil || mode != "rule" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
}

func TestSelectSendsEscapedPut(t *testing.T) {
	var method, body, path string
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/proxies/HK 一": func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.EscapedPath()
			raw, _ := io.ReadAll(r.Body)
			body = string(raw)
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	if err := c.Select(t.Context(), "HK 一", "SG-01"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut {
		t.Fatalf("method = %s", method)
	}
	if path != "/proxies/HK%20%E4%B8%80" {
		t.Fatalf("path = %s", path)
	}
	if body != `{"name":"SG-01"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestDelayUsesProviderPathAndTranslatesResult(t *testing.T) {
	var paths []string
	var query string
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/providers/proxies/airport/HK-01/healthcheck": func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			query = r.URL.RawQuery
			io.WriteString(w, `{"delay":123}`)
		},
		"/proxies/DIRECT/delay": func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			io.WriteString(w, `{"delay":0}`)
		},
		"/proxies/slow/delay": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
		},
		"/providers/proxies/airport/healthcheck": func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			io.WriteString(w, `{}`)
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	delay, err := c.Delay(t.Context(), "HK-01", "airport", "https://cp.cloudflare.com/generate_204", 1000)
	if err != nil || delay != 123 {
		t.Fatalf("delay = %d, %v", delay, err)
	}
	if paths[0] != "/providers/proxies/airport/HK-01/healthcheck" {
		t.Fatalf("provider 节点该走 healthcheck: %v", paths)
	}
	if !strings.Contains(query, "url=https%3A%2F%2Fcp.cloudflare.com") || !strings.Contains(query, "timeout=1000") {
		t.Fatalf("query = %s", query)
	}

	// delay<=0 统一成 -1（超时/无结果）
	if delay, err = c.Delay(t.Context(), "DIRECT", "", "u", 1000); err != nil || delay != -1 {
		t.Fatalf("DIRECT delay = %d, %v", delay, err)
	}
	if delay, err = c.Delay(t.Context(), "slow", "", "u", 1000); err != nil || delay != -1 {
		t.Fatalf("slow delay = %d, %v", delay, err)
	}
	if err := c.ProviderHealthcheck(t.Context(), "airport"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorBecomesAPIError(t *testing.T) {
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/proxies": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "{\"message\":\"Unauthorized\"}\n")
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	_, err := c.Proxies(t.Context())
	if err == nil {
		t.Fatal("401 应该报错")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T", err)
	}
	if apiErr.Error() != `HTTP 401 {"message":"Unauthorized"}` {
		t.Fatalf("错误信息 = %q", apiErr.Error())
	}
}

func TestUnauthorizedWithoutSecretGetsTypedError(t *testing.T) {
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/proxies": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"Unauthorized"}`)
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	_, err := c.Proxies(t.Context())
	if _, ok := errors.AsType[*AuthRequiredError](err); !ok {
		t.Fatalf("应该返回 AuthRequiredError，实际 %T: %v", err, err)
	}
	// 仍然要能当成普通 APIError 处理
	if _, ok := errors.AsType[*APIError](err); !ok {
		t.Fatal("AuthRequiredError 应该能被 As 成 APIError")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("错误信息 = %q", err.Error())
	}

	// 配了密码的 401 就是普通 APIError（密码不对），不该提示"没读到密码"
	c2 := New(server.URL, "wrong", 5)
	defer c2.Close()
	_, err = c2.Proxies(t.Context())
	if _, ok := errors.AsType[*AuthRequiredError](err); ok {
		t.Fatal("配了密码还报 AuthRequiredError 就不对了")
	}
}

func TestNonJSONResponseBecomesAPIError(t *testing.T) {
	server := newServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/version": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<html>nope</html>")
		},
	})
	c := New(server.URL, "", 5)
	defer c.Close()

	_, err := c.Version(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "响应不是 JSON") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestConnectionRefusedBecomesAPIError(t *testing.T) {
	c := New("http://127.0.0.1:1", "", 1)
	defer c.Close()

	_, err := c.Version(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "连不上") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestObjectItemsSurviveJSONRoundTrip(t *testing.T) {
	obj, err := DecodeObject([]byte(`{"a":{"x":[1,2,3]},"b":"中"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(obj.Items["a"])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"x":[1,2,3]}` {
		t.Fatalf("raw = %s", raw)
	}
	if !jsontext.Value(obj.Items["b"]).IsValid() {
		t.Fatal("值应该是合法 JSON")
	}
}

func TestProviderHealthcheckCancelsHTTPRequest(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "", 1)
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.ProviderHealthcheck(ctx, "airport") }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("请求未到达测试控制器")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后客户端未返回")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 HTTP 请求仍在执行")
	}
}
