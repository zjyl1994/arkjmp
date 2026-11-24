// Package client 封装 mihomo external-controller 的 REST 接口。
//
// 所有失败统一成 *APIError，方便 CLI 转成退出码。
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

// DefaultTestURL 默认测速地址。
const DefaultTestURL = "https://cp.cloudflare.com/generate_204"

// APIError 连不上控制器、HTTP 报错、响应格式不对，都用这一个错误类型。
type APIError struct {
	Msg    string
	status int
}

func (e *APIError) Error() string { return e.Msg }

// AuthRequiredError 是"控制器要求鉴权、但我们没配密码"的情况，
// 单独给个类型好让 CLI 给出更准的提示。
type AuthRequiredError struct{ *APIError }

func (e *AuthRequiredError) Unwrap() error { return e.APIError }

// TimeoutError 请求超时；测延迟时会被当成 -1 而不是错误。
type TimeoutError struct{ Path string }

func (e *TimeoutError) Error() string {
	path := e.Path
	if len(path) > 60 {
		path = path[:60]
	}
	return "请求超时（" + path + "）"
}

// Object 保留 JSON 对象的键顺序（/proxies 的插入顺序决定了分组顺序）。
type Object struct {
	Keys  []string
	Items map[string]jsontext.Value
}

// DecodeObject 按出现顺序解析 JSON 对象的键。
func DecodeObject(data []byte) (*Object, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != jsontext.KindBeginObject {
		return nil, fmt.Errorf("期望 JSON 对象")
	}
	obj := &Object{Items: make(map[string]jsontext.Value)}
	for dec.PeekKind() != jsontext.KindEndObject {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		key := name.String() // Token 会被下一次解码调用作废，先取出字符串
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		obj.Keys = append(obj.Keys, key)
		// ReadValue 返回的缓冲区下一次解码就会被覆盖，必须拷贝
		obj.Items[key] = bytes.Clone(raw)
	}
	if _, err := dec.ReadToken(); err != nil { // 收尾的 '}'
		return nil, err
	}
	return obj, nil
}

// Client 是一个 mihomo 控制器客户端。
type Client struct {
	BaseURL string
	Secret  string
	Timeout float64 // 秒
	http    *http.Client
}

// New 创建客户端；timeout<=0 时按 5 秒处理。
func New(base, secret string, timeout float64) *Client {
	if timeout <= 0 {
		timeout = 5
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConns:        32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	// 本机自己就是代理客户端，别让环境里的 HTTP_PROXY 把 127.0.0.1 的请求也带走
	transport.Proxy = nil
	return &Client{
		BaseURL: strings.TrimRight(base, "/"),
		Secret:  strings.TrimSpace(secret), // 空 = 不鉴权，绝不上 `Authorization: Bearer `
		Timeout: timeout,
		http:    &http.Client{Transport: transport},
	}
}

func (c *Client) Base() string { return c.BaseURL }

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) timeout() time.Duration {
	return time.Duration(c.Timeout * float64(time.Second))
}

// call 发请求，返回原始 JSON（空响应体返回 nil）。
func (c *Client) call(parent context.Context, method, path string, query url.Values, payload any, timeout time.Duration) (jsontext.Value, error) {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &APIError{Msg: "编码请求失败: " + err.Error()}
		}
		body = bytes.NewReader(encoded)
	}

	if timeout <= 0 {
		timeout = c.timeout()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, &APIError{Msg: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if err := parent.Err(); err != nil {
			return nil, err
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, &TimeoutError{Path: path}
		}
		return nil, &APIError{Msg: fmt.Sprintf("连不上 %s（%T: %v）", c.BaseURL, unwrapNetError(err), err)}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		if err := parent.Err(); err != nil {
			return nil, err
		}
		return nil, &APIError{Msg: fmt.Sprintf("读响应失败（%s）", err)}
	}

	if resp.StatusCode >= 400 {
		text := strings.Join(strings.Fields(string(data)), " ")
		if len(text) > 200 {
			text = text[:200]
		}
		message := strings.TrimSpace(fmt.Sprintf("HTTP %d %s", resp.StatusCode, text))
		// 没配密码却收到 401：多半是控制器开了鉴权
		if resp.StatusCode == http.StatusUnauthorized && c.Secret == "" {
			return nil, &AuthRequiredError{&APIError{Msg: message, status: resp.StatusCode}}
		}
		return nil, &APIError{Msg: message, status: resp.StatusCode}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	// 顺带校验 JSON：解析失败就走这里，免得各调用方拿到半截数据
	if err := json.Unmarshal(data, new(any)); err != nil {
		snippet := string(data)
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, &APIError{Msg: fmt.Sprintf("响应不是 JSON: %q", snippet)}
	}
	return data, nil
}

// unwrapNetError 让错误类型名更接近 Python 的 ConnectionError。
func unwrapNetError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// Version 返回控制器版本。
func (c *Client) Version(ctx context.Context) (string, error) {
	raw, err := c.call(ctx, http.MethodGet, "/version", nil, nil, 0)
	if err != nil {
		return "", err
	}
	var res struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "?", nil
	}
	if res.Version == "" {
		return "?", nil
	}
	return res.Version, nil
}

// Configs 返回 /configs 的原始内容。
func (c *Client) Configs(ctx context.Context) (map[string]jsontext.Value, error) {
	raw, err := c.call(ctx, http.MethodGet, "/configs", nil, nil, 0)
	if err != nil {
		return nil, err
	}
	var res map[string]jsontext.Value
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &APIError{Msg: "响应不是 JSON 对象"}
	}
	return res, nil
}

// Mode 返回运行模式。
func (c *Client) Mode(ctx context.Context) (string, error) {
	configs, err := c.Configs(ctx)
	if err != nil {
		return "?", err
	}
	var mode string
	if raw, ok := configs["mode"]; ok {
		_ = json.Unmarshal(raw, &mode)
	}
	if mode == "" {
		return "?", nil
	}
	return mode, nil
}

// Proxies 返回 /proxies（保留键顺序）。
func (c *Client) Proxies(ctx context.Context) (*Object, error) {
	raw, err := c.call(ctx, http.MethodGet, "/proxies", nil, nil, 0)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Proxies jsontext.Value `json:"proxies"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, &APIError{Msg: "响应不是 JSON: /proxies"}
	}
	return decodeWrapped(wrap.Proxies)
}

// Providers 返回 /providers/proxies（保留键顺序）。
func (c *Client) Providers(ctx context.Context) (*Object, error) {
	raw, err := c.call(ctx, http.MethodGet, "/providers/proxies", nil, nil, 0)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Providers jsontext.Value `json:"providers"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, &APIError{Msg: "响应不是 JSON: /providers/proxies"}
	}
	return decodeWrapped(wrap.Providers)
}

func decodeWrapped(raw jsontext.Value) (*Object, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return &Object{Items: map[string]jsontext.Value{}}, nil
	}
	obj, err := DecodeObject(trimmed)
	if err != nil {
		return nil, &APIError{Msg: "响应不是 JSON 对象: " + err.Error()}
	}
	return obj, nil
}

// Select 切换分组里的节点。
func (c *Client) Select(ctx context.Context, group, name string) error {
	_, err := c.call(ctx, http.MethodPut, "/proxies/"+quote(group), nil, map[string]string{"name": name}, 0)
	return err
}

// Delay 测单个节点延迟。
//
// 机场(provider)里的节点不在 /proxies 的顶层 map 里，
// 用 /proxies/{name}/delay 会 404，必须走
//
//	/providers/proxies/{provider}/{name}/healthcheck
func (c *Client) Delay(ctx context.Context, name, provider, testURL string, timeoutMs int) (int, error) {
	query := url.Values{}
	query.Set("url", testURL)
	query.Set("timeout", strconv.Itoa(timeoutMs))

	var path string
	if provider != "" {
		path = "/providers/proxies/" + quote(provider) + "/" + quote(name) + "/healthcheck"
	} else {
		path = "/proxies/" + quote(name) + "/delay"
	}
	raw, err := c.call(ctx, http.MethodGet, path, query, nil, time.Duration(timeoutMs)*time.Millisecond+5*time.Second)
	if err != nil {
		if _, timedOut := errors.AsType[*TimeoutError](err); timedOut {
			return -1, nil
		}
		if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.status == http.StatusGatewayTimeout {
			return -1, nil
		}
		return 0, err
	}
	var res struct {
		Delay int `json:"delay"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &res)
	}
	if res.Delay > 0 {
		return res.Delay, nil
	}
	return -1, nil
}

// ProviderHealthcheck 触发整个 provider 的批量测速。
//
// 这个接口是同步的，节点多的时候要十几秒，而且结果只写进各节点的 history，
// 所以调用方一般把它丢到后台 goroutine 里跑。
func (c *Client) ProviderHealthcheck(ctx context.Context, provider string) error {
	_, err := c.call(
		ctx,
		http.MethodGet,
		"/providers/proxies/"+quote(provider)+"/healthcheck",
		nil, nil, 180*time.Second,
	)
	return err
}

// Provider 返回单个 provider 的数据及其节点历史。
func (c *Client) Provider(ctx context.Context, name string) (jsontext.Value, error) {
	return c.call(ctx, http.MethodGet, "/providers/proxies/"+quote(name), nil, nil, 0)
}
