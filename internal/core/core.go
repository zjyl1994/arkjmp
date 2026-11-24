// Package core 是数据与操作层：CLI 和 TUI 共用，避免两份逻辑跑偏。
//
// Snapshot 是控制器状态的一份快照（/proxies + /providers/proxies），
// 所有"读"操作都尽量走快照，只有 Select / healthcheck 才真正发写请求。
package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
)

const (
	DefaultDelayTimeoutMs = 3000
	MaxDelayTimeoutMs     = 5000
	selectorType          = "Selector"
)

// 这些"节点"根本没有延迟可测。
var noDelayTypes = map[string]bool{
	"Direct":     true,
	"Reject":     true,
	"RejectDrop": true,
	"Compatible": true,
	"Pass":       true,
	"PassRule":   true,
	"Dns":        true,
}

// GLOBAL mihomo 自动生成的全局分组。
const GLOBAL = "GLOBAL"

// API 是 Snapshot 依赖的控制器能力，测试里可以换成假实现。
type API interface {
	Base() string
	Version(ctx context.Context) (string, error)
	Mode(ctx context.Context) (string, error)
	Proxies(ctx context.Context) (*client.Object, error)
	Providers(ctx context.Context) (*client.Object, error)
	Select(ctx context.Context, group, name string) error
	Delay(ctx context.Context, name, provider, testURL string, timeoutMs int) (int, error)
	ProviderHealthcheck(ctx context.Context, provider string) error
	Provider(ctx context.Context, name string) (jsontext.Value, error)
}

// HistoryRecord 是节点测速历史的一条记录。
type HistoryRecord struct {
	Delay int    `json:"delay"`
	Time  string `json:"time"`
}

// Proxy 是 /proxies 或 provider 里的一个节点/分组。
type Proxy struct {
	Name         string
	Type         string
	Now          string
	HasNow       bool
	All          []string
	HasAll       bool
	History      []HistoryRecord
	Alive        bool
	HasAlive     bool
	ProviderName string
}

func decodeProxy(raw jsontext.Value) *Proxy {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return &Proxy{}
	}
	p := &Proxy{}
	if v, ok := fields["name"]; ok {
		_ = json.Unmarshal(v, &p.Name)
	}
	if v, ok := fields["type"]; ok {
		_ = json.Unmarshal(v, &p.Type)
	}
	if v, ok := fields["now"]; ok && !isNull(v) {
		p.HasNow = true
		_ = json.Unmarshal(v, &p.Now)
	}
	if v, ok := fields["all"]; ok && !isNull(v) {
		p.HasAll = true
		_ = json.Unmarshal(v, &p.All)
	}
	if v, ok := fields["history"]; ok && !isNull(v) {
		_ = json.Unmarshal(v, &p.History)
	}
	if v, ok := fields["alive"]; ok && !isNull(v) {
		p.HasAlive = true
		_ = json.Unmarshal(v, &p.Alive)
	}
	if v, ok := fields["provider-name"]; ok && !isNull(v) {
		_ = json.Unmarshal(v, &p.ProviderName)
	}
	return p
}

func isNull(raw jsontext.Value) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func decodeProvider(raw jsontext.Value) ([]*Proxy, error) {
	var fields struct {
		Proxies []jsontext.Value `json:"proxies"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields.Proxies == nil {
		return nil, fmt.Errorf("provider 响应缺少 proxies 列表")
	}
	out := make([]*Proxy, 0, len(fields.Proxies))
	for _, item := range fields.Proxies {
		out = append(out, decodeProxy(item))
	}
	return out, nil
}

// Group 是一个可选分组。
type Group struct {
	Name    string
	Type    string
	Now     string
	HasNow  bool
	Members []string
}

// Count 返回成员数。
func (g Group) Count() int { return len(g.Members) }

// Current 返回当前选中的节点名（没有就是空串）。
func (g Group) Current() string { return g.Now }

// HistoryDelay 是"只看 mihomo 记录的历史延迟"。
func (p *Proxy) HistoryDelay() (int, bool) {
	if noDelayTypes[p.Type] || p.IsGroup() {
		return 0, false
	}
	if len(p.History) == 0 {
		return 0, false
	}
	delay := p.History[len(p.History)-1].Delay
	if delay <= 0 {
		return 0, false
	}
	return checkedDelay(delay, MaxDelayTimeoutMs), true
}

// IsGroup 包含嵌套选择链中的手动和自动分组。
func (p *Proxy) IsGroup() bool { return p.HasAll }

// TestResult 是整组测速的结果。
type TestResult struct {
	Nodes    map[string]int // 有本轮结果的节点（-1 表示测速失败）
	total    int
	Failures map[string]string // provider 或节点 -> 错误信息
	Partial  bool              // 有 provider 节点缺少本轮结果
}

// Total 参与测速的节点数。
func (r *TestResult) Total() int { return r.total }

// OK 有结果的节点数。
func (r *TestResult) OK() int {
	n := 0
	for _, delay := range r.Nodes {
		if delay > 0 {
			n++
		}
	}
	return n
}

// Failed 超时/失败的节点数。
func (r *TestResult) Failed() int { return r.Total() - r.OK() }

// SortedFailures 稳定的失败信息输出。
func (r *TestResult) SortedFailures() []string {
	keys := make([]string, 0, len(r.Failures))
	for key := range r.Failures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+": "+r.Failures[key])
	}
	return out
}

// Progress 是测速进度回调：stage 为 "node" 时 (done, total, 节点名)，
// "poll" 时 (fresh, total, "")。
type Progress func(stage string, done, total int, label string)

// Snapshot 是控制器状态的一份快照。
type Snapshot struct {
	Client  API
	TestURL string

	mu            sync.RWMutex
	infoMap       map[string]*Proxy
	groups        []Group             // 所有分组（GLOBAL 最后）
	groupsVisible []Group             // 不含 GLOBAL（只剩 GLOBAL 时就是全部）
	nodeGroups    map[string][]string // 节点 -> 所在分组（含 GLOBAL）
	providers     providerInventory
	providerErr   error
	delays        map[string]int
	mode          string
	version       string
}

// New 创建快照。
func New(c API, testURL string) *Snapshot {
	return &Snapshot{
		Client:  c,
		TestURL: testURL,
		infoMap: make(map[string]*Proxy),
		delays:  make(map[string]int),
		mode:    "?",
		version: "?",
	}
}

// RefreshParts 指定除 /proxies 外还要请求哪些数据。
type RefreshParts uint8

const (
	WithProviders RefreshParts = 1 << iota
	WithMetadata
	FullRefresh = WithProviders | WithMetadata
)

// Refresh 拉取节点，可选刷新 provider 数据、模式和版本。
func (s *Snapshot) Refresh(ctx context.Context, parts RefreshParts) error {
	proxies, err := s.Client.Proxies(ctx)
	if err != nil {
		return err
	}
	s.mu.RLock()
	providers, providerErr, mode, version := s.providers, s.providerErr, s.mode, s.version
	s.mu.RUnlock()
	if parts&WithProviders != 0 {
		if fetched, err := s.Client.Providers(ctx); err == nil {
			providers = indexProviders(fetched)
			providerErr = nil
		} else {
			providerErr = err
		}
	}
	if parts&WithMetadata != 0 {
		if m, err := s.Client.Mode(ctx); err == nil {
			mode = m
		}
		if v, err := s.Client.Version(ctx); err == nil {
			version = v
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info := make(map[string]*Proxy, len(proxies.Keys)+len(providers.nodes))
	for _, name := range proxies.Keys {
		info[name] = decodeProxy(proxies.Items[name])
	}
	for name, node := range providers.nodes {
		if _, exists := info[name]; !exists {
			info[name] = node
		}
	}

	// 分组列表和"节点 -> 分组"索引在两次刷新之间不变，算好缓存起来，
	// CLI/TUI 每帧就不用重新分配和遍历了。
	groups := make([]Group, 0, 8)
	nodeGroups := make(map[string][]string)
	for _, name := range proxies.Keys {
		proxy := info[name]
		if proxy == nil || !proxy.IsGroup() || proxy.Type != selectorType {
			continue
		}
		group := Group{
			Name:    name,
			Type:    proxy.Type,
			Now:     proxy.Now,
			HasNow:  proxy.HasNow,
			Members: append([]string{}, proxy.All...),
		}
		groups = append(groups, group)
		for _, member := range group.Members {
			nodeGroups[member] = append(nodeGroups[member], name)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].Name != GLOBAL && groups[j].Name == GLOBAL
	})
	visible := make([]Group, 0, len(groups))
	for _, group := range groups {
		if group.Name != GLOBAL {
			visible = append(visible, group)
		}
	}
	if len(visible) == 0 {
		visible = groups
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.infoMap = info
	s.groups = groups
	s.groupsVisible = visible
	s.nodeGroups = nodeGroups
	s.providers = providers
	s.providerErr = providerErr
	s.mode = mode
	s.version = version
	return nil
}

type providerInventory struct {
	nodes  map[string]*Proxy
	owners map[string]string
	names  []string
}

func indexProviders(providers *client.Object) providerInventory {
	inventory := providerInventory{
		nodes:  make(map[string]*Proxy),
		owners: make(map[string]string),
		names:  providers.Keys,
	}
	for _, name := range providers.Keys {
		nodes, err := decodeProvider(providers.Items[name])
		if err != nil {
			continue
		}
		for _, node := range nodes {
			if node.Name == "" {
				continue
			}
			if _, exists := inventory.nodes[node.Name]; !exists {
				inventory.nodes[node.Name] = node
			}
			owner := node.ProviderName
			if owner == "" && name != "default" {
				owner = name
			}
			if owner != "" {
				inventory.owners[node.Name] = owner
			}
		}
	}
	return inventory
}

// ProviderError 返回最近一次 provider 列表刷新失败的原因。
func (s *Snapshot) ProviderError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerErr
}

// Mode 返回运行模式。
func (s *Snapshot) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// Version 返回控制器版本。
func (s *Snapshot) Version() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// ProviderNames 返回所有 provider 名（按控制器给出的顺序）。
func (s *Snapshot) ProviderNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.providers.names...)
}

// Info 返回节点详情（不存在时返回空结构）。
func (s *Snapshot) Info(name string) *Proxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.infoMap[name]; ok {
		return p
	}
	return &Proxy{}
}

// Owner 返回节点所属 provider。
func (s *Snapshot) Owner(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, ok := s.providers.owners[name]
	return owner, ok
}

// IsGroup 判断某个名字是不是可选分组。
func (s *Snapshot) IsGroup(name string) bool { return s.Info(name).IsGroup() }

// AllGroups 返回所有可选分组（含 GLOBAL，GLOBAL 排最后）。
//
// 返回的是快照内部切片，调用方只读，不要修改。
func (s *Snapshot) AllGroups() []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groups
}

// Groups 给界面/CLI 用的分组列表；GLOBAL 默认藏起来，只剩它时才显示。
func (s *Snapshot) Groups(includeGlobal bool) []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if includeGlobal {
		return s.groups
	}
	return s.groupsVisible
}

// FindGroup 按名字精确查找分组。
func (s *Snapshot) FindGroup(name string, includeGlobal bool) (*Group, bool) {
	groups := s.Groups(includeGlobal)
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], true
		}
	}
	return nil, false
}

// Chain 分组嵌套（分组里选分组）时，返回从 name 开始的选择链。
func (s *Snapshot) Chain(name string) []string {
	out := []string{name}
	seen := map[string]bool{name: true}
	current := name
	for i := 0; i < 4; i++ {
		info := s.Info(current)
		if !info.IsGroup() || !info.HasNow || info.Now == "" || seen[info.Now] {
			break
		}
		out = append(out, info.Now)
		seen[info.Now] = true
		current = info.Now
	}
	return out
}

// HistoryDelay 只看 mihomo 记录的历史延迟。
func (s *Snapshot) HistoryDelay(name string) (int, bool) { return s.Info(name).HistoryDelay() }

// Latency 本次测过就用本次的，否则用 history。
func (s *Snapshot) Latency(name string) (int, bool) {
	s.mu.RLock()
	delay, ok := s.delays[name]
	s.mu.RUnlock()
	if ok {
		return delay, true
	}
	return s.HistoryDelay(name)
}

func (s *Snapshot) setDelay(name string, delay int) {
	s.mu.Lock()
	s.delays[name] = delay
	s.mu.Unlock()
}

// ClearDelays 丢掉本次会话测出来的延迟。
func (s *Snapshot) ClearDelays() {
	s.mu.Lock()
	s.delays = make(map[string]int)
	s.mu.Unlock()
}

// IsTestable 判断节点能不能测延迟。
func (s *Snapshot) IsTestable(name string) bool {
	return s.Info(name).isTestable()
}

func (p *Proxy) isTestable() bool {
	return (p.Type != "" || p.HasAll) && !p.IsGroup() && !noDelayTypes[p.Type]
}

// TestableNodes 过滤出可测节点。
func (s *Snapshot) TestableNodes(members []string) []string {
	out := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, name := range members {
		if !seen[name] && s.IsTestable(name) {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

// AllNodes 所有分组里出现过的节点，按首次出现顺序去重。
func (s *Snapshot) AllNodes(includeGlobal bool) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 64)
	for _, group := range s.Groups(includeGlobal) {
		for _, name := range group.Members {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// GroupsOf 返回包含某个节点的分组名。
func (s *Snapshot) GroupsOf(node string, includeGlobal bool) []string {
	s.mu.RLock()
	names := s.nodeGroups[node]
	s.mu.RUnlock()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if includeGlobal || name != GLOBAL {
			out = append(out, name)
		}
	}
	return out
}

// LookupError 名字找不到或有歧义。
type LookupError struct {
	Kind string // "group" 或 "node"
	Want string
	Near []string
}

func (e *LookupError) Error() string {
	if len(e.Near) > 0 {
		return "无法唯一确定 " + e.Kind + " " + e.Want
	}
	return "找不到 " + e.Kind + " " + e.Want
}

// match 精确名 > 忽略大小写 > 前缀 > 子串，只在唯一命中时才接受。
func match(candidates []string, want string) (string, []string) {
	names := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range names {
		if name == want {
			return want, nil
		}
	}
	lowered := strings.ToLower(want)
	stages := []func(string) bool{
		func(name string) bool { return strings.ToLower(name) == lowered },
		func(name string) bool { return strings.HasPrefix(strings.ToLower(name), lowered) },
		func(name string) bool { return strings.Contains(strings.ToLower(name), lowered) },
	}
	for _, stage := range stages {
		hits := make([]string, 0, 4)
		for _, name := range names {
			if stage(name) {
				hits = append(hits, name)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
		if len(hits) > 1 {
			if len(hits) > 20 {
				hits = hits[:20]
			}
			return "", hits
		}
	}
	return "", nil
}

// ResolveGroup 解析分组名（精确 > 忽略大小写 > 前缀 > 子串）。
func (s *Snapshot) ResolveGroup(want string) (*Group, error) {
	if group, ok := s.FindGroup(want, true); ok {
		return group, nil
	}
	if s.Info(want).IsGroup() {
		return nil, &LookupError{Kind: "group", Want: want}
	}
	groups := s.AllGroups()
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, group.Name)
	}
	name, near := match(names, want)
	if name != "" {
		if group, ok := s.FindGroup(name, true); ok {
			return group, nil
		}
	}
	return nil, &LookupError{Kind: "group", Want: want, Near: near}
}

// ResolveNode 解析节点名；members 为 nil 时在全部分组里找。
func (s *Snapshot) ResolveNode(want string, members []string) (string, error) {
	pool := members
	if pool == nil {
		pool = s.AllNodes(true)
	}
	name, near := match(pool, want)
	if name != "" {
		return name, nil
	}
	return "", &LookupError{Kind: "node", Want: want, Near: near}
}

// Select 切换并返回切换前的节点。
func (s *Snapshot) Select(ctx context.Context, group, node string) (string, bool, error) {
	g, ok := s.FindGroup(group, true)
	if !ok {
		return "", false, &LookupError{Kind: "group", Want: group}
	}
	return g.Now, g.HasNow, s.Client.Select(ctx, group, node)
}

// Measure 测单个节点，结果写进 s.delays。
func (s *Snapshot) Measure(ctx context.Context, node string, timeoutMs int) (int, error) {
	if timeoutMs <= 0 {
		timeoutMs = DefaultDelayTimeoutMs
	}
	timeoutMs = min(timeoutMs, MaxDelayTimeoutMs)
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	provider, _ := s.Owner(node)
	delay, err := s.Client.Delay(probeCtx, node, provider, s.TestURL, timeoutMs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if probeCtx.Err() != nil {
		delay = -1
	} else if err != nil {
		return 0, err
	}
	delay = checkedDelay(delay, timeoutMs)
	s.delays[node] = delay
	return delay, nil
}

func checkedDelay(delay, limit int) int {
	if delay <= 0 || delay > limit {
		return -1
	}
	return delay
}

// TestGroup 整组测速。
func (s *Snapshot) TestGroup(ctx context.Context, group Group, progress Progress, wait time.Duration, timeoutMs int) (*TestResult, error) {
	return s.TestNodes(ctx, group.Members, progress, wait, timeoutMs)
}

// TestNodes 整组测速：机场节点走 provider 的批量 healthcheck（服务端跑完），
// 其余内置节点逐个测。
func (s *Snapshot) TestNodes(ctx context.Context, members []string, progress Progress, wait time.Duration, timeoutMs int) (*TestResult, error) {
	result := &TestResult{Nodes: map[string]int{}, Failures: map[string]string{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	targets := make(map[string][]string)
	var builtins []string
	s.mu.RLock()
	providerErr := s.providerErr
	seen := make(map[string]bool, len(members))
	for _, name := range members {
		proxy := s.infoMap[name]
		if seen[name] || proxy == nil || !proxy.isTestable() {
			continue
		}
		seen[name] = true
		if provider := s.providers.owners[name]; provider != "" {
			targets[provider] = append(targets[provider], name)
		} else {
			builtins = append(builtins, name)
		}
	}
	s.mu.RUnlock()
	if providerErr != nil {
		result.Partial = true
		result.Failures["/providers/proxies"] = providerErr.Error()
	}
	result.total = len(seen)

	type providerResult struct {
		name   string
		delays map[string]int
		err    error
	}
	providerCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	completed := make(chan providerResult, len(targets))
	for provider, names := range targets {
		go func() {
			delays, err := s.measureProvider(providerCtx, provider, names)
			completed <- providerResult{name: provider, delays: delays, err: err}
		}()
	}

	for i, name := range builtins {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if progress != nil {
			progress("node", i, len(builtins), name)
		}
		delay, err := s.Measure(ctx, name, timeoutMs)
		if err != nil {
			result.Failures[name] = err.Error()
			delay = -1
		}
		result.Nodes[name] = delay
	}

	totalProviders := len(targets)
	recordOutcome := func(outcome providerResult) {
		if outcome.err != nil {
			result.Failures[outcome.name] = outcome.err.Error()
		}
		if len(outcome.delays) < len(targets[outcome.name]) {
			result.Partial = true
		}
		for name, delay := range outcome.delays {
			result.Nodes[name] = delay
			s.setDelay(name, delay)
		}
		delete(targets, outcome.name)
		if progress != nil {
			progress("provider", totalProviders-len(targets), totalProviders, outcome.name)
		}
	}

	for len(targets) > 0 {
		select {
		case <-providerCtx.Done():
			if err := ctx.Err(); err != nil {
				return result, err
			}
			for len(completed) > 0 {
				recordOutcome(<-completed)
			}
			for provider := range targets {
				result.Failures[provider] = providerCtx.Err().Error()
				result.Partial = true
			}
			return result, nil
		case outcome := <-completed:
			if err := ctx.Err(); err != nil {
				return result, err
			}
			recordOutcome(outcome)
		}
	}
	return result, ctx.Err()
}

type historySample struct {
	record HistoryRecord
	length int
}

func latestSample(proxy *Proxy) historySample {
	if proxy == nil || len(proxy.History) == 0 {
		return historySample{}
	}
	length := len(proxy.History)
	return historySample{record: proxy.History[length-1], length: length}
}

func (sample historySample) newerThan(previous historySample) bool {
	if sample.length == 0 {
		return false
	}
	if sample.record.Time != "" && previous.record.Time != "" {
		currentTime, currentErr := time.Parse(time.RFC3339Nano, sample.record.Time)
		previousTime, previousErr := time.Parse(time.RFC3339Nano, previous.record.Time)
		if currentErr == nil && previousErr == nil {
			return currentTime.After(previousTime)
		}
	}
	return sample.length > previous.length || sample.record != previous.record
}

func (s *Snapshot) measureProvider(ctx context.Context, provider string, names []string) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := s.providerSamples(ctx, provider)
	if err != nil {
		return nil, err
	}
	if err := s.Client.ProviderHealthcheck(ctx, provider); err != nil {
		return nil, err
	}
	after, err := s.providerSamples(ctx, provider)
	if err != nil {
		return nil, err
	}
	delays := make(map[string]int, len(names))
	for _, name := range names {
		previous, known := before[name]
		sample := after[name]
		if known && sample.newerThan(previous) {
			delays[name] = checkedDelay(sample.record.Delay, MaxDelayTimeoutMs)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return delays, nil
}

func (s *Snapshot) providerSamples(ctx context.Context, name string) (map[string]historySample, error) {
	raw, err := s.Client.Provider(ctx, name)
	if err != nil {
		return nil, err
	}
	nodes, err := decodeProvider(raw)
	if err != nil {
		return nil, err
	}
	samples := make(map[string]historySample, len(nodes))
	for _, node := range nodes {
		samples[node.Name] = latestSample(node)
	}
	return samples, nil
}

// NodeRow 是给 CLI/TUI 用的节点摘要（JSON 字段顺序与旧版一致）。
type NodeRow struct {
	Name     string  `json:"name"`
	Type     *string `json:"type"`
	Provider *string `json:"provider"`
	Group    bool    `json:"group"`
	Current  bool    `json:"current"`
	Delay    *int    `json:"delay"`
	Timeout  bool    `json:"timeout"`
	Alive    *bool   `json:"alive"`
}

// NodeRowWithGroups 是 nodes 命令的输出，多一个所在分组列表。
type NodeRowWithGroups struct {
	NodeRow
	Groups []string `json:"groups"`
}

// NodeRow 组装一个节点的摘要。
func (s *Snapshot) NodeRow(node string, current bool) NodeRow {
	info := s.Info(node)
	row := NodeRow{
		Name:    node,
		Group:   info.IsGroup(),
		Current: current,
		Timeout: false,
	}
	if info.Type != "" {
		row.Type = &info.Type
	}
	if provider, ok := s.Owner(node); ok && provider != "" {
		row.Provider = &provider
	}
	if delay, ok := s.Latency(node); ok {
		row.Delay = &delay
		row.Timeout = delay < 0
	}
	if info.HasAlive {
		row.Alive = &info.Alive
	}
	return row
}

// NodeRows 批量组装。
func (s *Snapshot) NodeRows(nodes []string, now string, hasNow bool) []NodeRow {
	rows := make([]NodeRow, 0, len(nodes))
	for _, name := range nodes {
		rows = append(rows, s.NodeRow(name, hasNow && name == now))
	}
	return rows
}
