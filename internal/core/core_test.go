package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
)

// fakeClient 只实现 Snapshot 用到的接口，行为尽量贴近真实控制器。
type fakeClient struct {
	mu           sync.Mutex
	proxies      map[string]any
	order        []string
	providers    map[string]any
	provOrder    []string
	delays       map[string]int
	selected     [][2]string
	healthcheck  []string
	version      string
	mode         string
	modeCalls    int
	versionCalls int
}

func newFake(proxies, providers map[string]any) *fakeClient {
	f := &fakeClient{
		proxies:   proxies,
		providers: providers,
		delays:    map[string]int{},
		version:   "v1.19.34",
		mode:      "rule",
	}
	for name := range proxies {
		f.order = append(f.order, name)
	}
	sort.Strings(f.order)
	for name := range providers {
		f.provOrder = append(f.provOrder, name)
	}
	sort.Strings(f.provOrder)
	return f
}

func (f *fakeClient) Base() string { return "http://127.0.0.1:9090" }

func (f *fakeClient) Version(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versionCalls++
	return f.version, nil
}

func (f *fakeClient) Mode(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modeCalls++
	return f.mode, nil
}

func toObject(order []string, items map[string]any) *client.Object {
	obj := &client.Object{Keys: append([]string(nil), order...), Items: map[string]jsontext.Value{}}
	for _, name := range order {
		raw, err := json.Marshal(items[name])
		if err != nil {
			panic(err)
		}
		obj.Items[name] = raw
	}
	return obj
}

func (f *fakeClient) Proxies(context.Context) (*client.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return toObject(f.order, f.proxies), nil
}

func (f *fakeClient) Providers(context.Context) (*client.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return toObject(f.provOrder, f.providers), nil
}

func (f *fakeClient) Select(_ context.Context, group, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected = append(f.selected, [2]string{group, name})
	if info, ok := f.proxies[group].(map[string]any); ok {
		info["now"] = name
	}
	return nil
}

func (f *fakeClient) Delay(_ context.Context, name, provider, testURL string, timeoutMs int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if delay, ok := f.delays[name]; ok {
		return delay, nil
	}
	return -1, nil
}

func (f *fakeClient) ProviderHealthcheck(_ context.Context, provider string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthcheck = append(f.healthcheck, provider)
	items, _ := f.providers[provider].(map[string]any)
	nodes, _ := items["proxies"].([]map[string]any)
	// 真实接口会把新结果写进各节点的 history；这里也要在锁内改，别和 Refresh 抢
	for _, node := range nodes {
		history, _ := node["history"].([]map[string]any)
		node["history"] = append(history, map[string]any{"delay": 200 + len(history)})
	}
	return nil
}

func (f *fakeClient) Provider(_ context.Context, name string) (jsontext.Value, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.Marshal(f.providers[name])
}

func proxyMap(fields map[string]any) map[string]any { return fields }

func makeClient() (*fakeClient, *Snapshot) {
	proxies := map[string]any{
		"GLOBAL": proxyMap(map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual", "DIRECT", "REJECT"}}),
		"Manual": proxyMap(map[string]any{"type": "Selector", "now": "HK-01", "all": []string{"HK-01", "SG-01", "DIRECT"}}),
		"Auto":   proxyMap(map[string]any{"type": "URLTest", "now": "SG-01", "all": []string{"SG-01"}}),
		"HK-01":  proxyMap(map[string]any{"type": "Vless", "history": []map[string]any{{"delay": 0}, {"delay": 123}}}),
		"DIRECT": proxyMap(map[string]any{"type": "Direct", "history": []map[string]any{}}),
		"REJECT": proxyMap(map[string]any{"type": "Reject", "history": []map[string]any{}}),
	}
	providers := map[string]any{
		"airport": map[string]any{"proxies": []map[string]any{
			{"name": "HK-01", "type": "Vless", "provider-name": "airport", "history": []map[string]any{{"delay": 123}}},
			{"name": "SG-01", "type": "Vless", "provider-name": "airport", "history": []map[string]any{}},
		}},
		"default": map[string]any{"proxies": []map[string]any{
			{"name": "DIRECT", "type": "Direct", "provider-name": ""},
		}},
	}
	fake := newFake(proxies, providers)
	snap := New(fake, "https://cp.cloudflare.com/generate_204")
	if err := snap.Refresh(context.Background(), FullRefresh); err != nil {
		panic(err)
	}
	return fake, snap
}

func groupNames(groups []Group) []string {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, group.Name)
	}
	return names
}

func TestGroupsHideGlobalByDefault(t *testing.T) {
	_, snap := makeClient()
	got := groupNames(snap.Groups(false))
	want := []string{"Manual"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("分组 = %v, 期望 %v", got, want)
	}
	all := groupNames(snap.Groups(true))
	if !slices.Equal(all, []string{"Manual", GLOBAL}) {
		t.Fatalf("手动分组（含 GLOBAL）= %v", all)
	}
}

func TestGlobalStillShownWhenItIsTheOnlyGroup(t *testing.T) {
	fake := newFake(map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "DIRECT", "all": []string{"DIRECT"}},
	}, map[string]any{})
	snap := New(fake, "")
	if err := snap.Refresh(t.Context(), FullRefresh); err != nil {
		t.Fatal(err)
	}
	if got := groupNames(snap.Groups(false)); len(got) != 1 || got[0] != GLOBAL {
		t.Fatalf("分组 = %v, 期望只剩 GLOBAL", got)
	}
}

func TestProviderOfSkipsDefaultProvider(t *testing.T) {
	_, snap := makeClient()
	if owner, ok := snap.Owner("HK-01"); !ok || owner != "airport" {
		t.Fatalf("HK-01 owner = %q,%v", owner, ok)
	}
	if owner, ok := snap.Owner("DIRECT"); ok {
		t.Fatalf("DIRECT 不该有 provider，实际 %q", owner)
	}
}

func TestLatencyPrefersMeasuredThenHistory(t *testing.T) {
	_, snap := makeClient()
	if delay, ok := snap.Latency("HK-01"); !ok || delay != 123 {
		t.Fatalf("HK-01 延迟 = %d,%v", delay, ok)
	}
	if _, ok := snap.Latency("SG-01"); ok {
		t.Fatal("SG-01 没有历史，不该有延迟")
	}
	if _, ok := snap.Latency("DIRECT"); ok {
		t.Fatal("DIRECT 不可测")
	}
	snap.setDelay("HK-01", 321)
	if delay, _ := snap.Latency("HK-01"); delay != 321 {
		t.Fatalf("HK-01 延迟 = %d, 期望 321", delay)
	}
}

func TestChainFollowsNestedGroups(t *testing.T) {
	proxies := map[string]any{
		"Outer": map[string]any{"type": "Selector", "now": "Inner", "all": []string{"Inner", "HK-01"}},
		"Inner": map[string]any{"type": "Selector", "now": "HK-01", "all": []string{"HK-01"}},
		"HK-01": map[string]any{"type": "Vless"},
	}
	snap := New(newFake(proxies, map[string]any{}), "")
	if err := snap.Refresh(t.Context(), FullRefresh); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(snap.Chain("Outer"), " → ")
	if got != "Outer → Inner → HK-01" {
		t.Fatalf("chain = %q", got)
	}
}

func TestResolveNode(t *testing.T) {
	_, snap := makeClient()
	if node, err := snap.ResolveNode("hk-01", nil); err != nil || node != "HK-01" {
		t.Fatalf("忽略大小写解析失败: %q %v", node, err)
	}
	if node, err := snap.ResolveNode("hk", nil); err != nil || node != "HK-01" {
		t.Fatalf("前缀解析失败: %q %v", node, err)
	}
	if _, err := snap.ResolveNode("nope", nil); err == nil {
		t.Fatal("不存在的节点应该报错")
	}
}

func TestResolveGroupAndAmbiguity(t *testing.T) {
	_, snap := makeClient()
	if group, err := snap.ResolveGroup("man"); err != nil || group.Name != "Manual" {
		t.Fatalf("分组解析失败: %v %v", group, err)
	}
	_, err := snap.ResolveGroup("al")
	if err == nil {
		t.Fatal("al 同时是 Manual 和 GLOBAL 的片段，应该报错")
	}
	lookup, ok := err.(*LookupError)
	if !ok || len(lookup.Near) == 0 {
		t.Fatalf("错误类型 = %T", err)
	}
	if _, err := snap.ResolveNode("0", nil); err == nil {
		t.Fatal("0 不该命中任何节点")
	}
}

func TestSelectReturnsPrevious(t *testing.T) {
	fake, snap := makeClient()
	previous, hasPrevious, err := snap.Select(t.Context(), "Manual", "SG-01")
	if err != nil {
		t.Fatal(err)
	}
	if !hasPrevious || previous != "HK-01" {
		t.Fatalf("previous = %q,%v", previous, hasPrevious)
	}
	if len(fake.selected) != 1 || fake.selected[0] != [2]string{"Manual", "SG-01"} {
		t.Fatalf("selected = %v", fake.selected)
	}
}

func TestOnlySelectorAndFallbackGroupsCanBeChanged(t *testing.T) {
	proxies := map[string]any{
		"Auto":     map[string]any{"type": "URLTest", "now": "HK", "all": []string{"HK"}},
		"Backup":   map[string]any{"type": "Fallback", "now": "HK", "all": []string{"HK"}},
		"Balanced": map[string]any{"type": "LoadBalance", "all": []string{"HK"}},
		"Manual":   map[string]any{"type": "Selector", "now": "Auto", "all": []string{"Auto", "HK"}},
		"HK":       map[string]any{"type": "Vless"},
	}
	api := newFake(proxies, nil)
	snap := New(api, "")
	if err := snap.Refresh(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if got := groupNames(snap.AllGroups()); !slices.Equal(got, []string{"Backup", "Manual"}) {
		t.Fatalf("可切换的分组 = %v", got)
	}
	if got := snap.Chain("Manual"); !slices.Equal(got, []string{"Manual", "Auto", "HK"}) {
		t.Fatalf("选择链 = %v", got)
	}
	for _, name := range []string{"Auto", "Balanced"} {
		if _, _, err := snap.Select(t.Context(), name, "HK"); err == nil {
			t.Errorf("允许直接切换自动分组 %s", name)
		}
	}
	if len(api.selected) != 0 {
		t.Fatalf("向控制器发送了自动组切换请求: %v", api.selected)
	}
	if _, _, err := snap.Select(t.Context(), "Backup", "HK"); err != nil {
		t.Fatal(err)
	}
	if len(api.selected) != 1 || api.selected[0] != [2]string{"Backup", "HK"} {
		t.Fatalf("Fallback 切换请求 = %v", api.selected)
	}
}

func TestMeasureRecordsDelay(t *testing.T) {
	fake, snap := makeClient()
	fake.delays["HK-01"] = 55
	delay, err := snap.Measure(t.Context(), "HK-01", 1000)
	if err != nil || delay != 55 {
		t.Fatalf("delay = %d,%v", delay, err)
	}
	if got, ok := snap.Latency("HK-01"); !ok || got != 55 {
		t.Fatalf("latency = %d,%v", got, ok)
	}
}

func TestMeasureRejectsDelaysOverFiveSeconds(t *testing.T) {
	fake, snap := makeClient()
	fake.delays["HK-01"] = 5001
	delay, err := snap.Measure(t.Context(), "HK-01", 6000)
	if err != nil || delay != -1 {
		t.Fatalf("delay = %d, err = %v", delay, err)
	}
	if got, ok := snap.Latency("HK-01"); !ok || got != -1 {
		t.Fatalf("latency = %d,%v", got, ok)
	}
}

func TestTestableNodesFiltersGroupsAndBuiltin(t *testing.T) {
	_, snap := makeClient()
	nodes := snap.TestableNodes([]string{"HK-01", "SG-01", "DIRECT", "REJECT", "Manual", "GLOBAL", "Auto", "SG-01"})
	if strings.Join(nodes, ",") != "HK-01,SG-01" {
		t.Fatalf("可测节点 = %v", nodes)
	}
}

func TestTestGroupRunsProviderHealthcheck(t *testing.T) {
	// 虚拟时间下仍应在 provider 完成后立即返回。
	synctest.Test(t, func(t *testing.T) {
		fake, snap := makeClient()
		group, ok := snap.FindGroup("Manual", true)
		if !ok {
			t.Fatal("找不到 Manual")
		}
		result, err := snap.TestGroup(context.Background(), *group, nil, 3*time.Second, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(fake.healthcheck) != 1 || fake.healthcheck[0] != "airport" {
			t.Fatalf("healthcheck = %v", fake.healthcheck)
		}
		if result.Total() != 2 {
			t.Fatalf("total = %d", result.Total())
		}
		if result.Nodes["HK-01"] != 201 {
			t.Fatalf("HK-01 delay = %d", result.Nodes["HK-01"])
		}
		if !snap.IsGroup("Manual") {
			t.Fatal("Manual 应该是分组")
		}
		// 测速只读各 provider 的结果。
		fake.mu.Lock()
		modeCalls, versionCalls := fake.modeCalls, fake.versionCalls
		fake.mu.Unlock()
		if modeCalls != 1 || versionCalls != 1 {
			t.Fatalf("mode/version 调用次数 = %d/%d，期望各 1 次（初次）", modeCalls, versionCalls)
		}
	})
}

type controlledHealthcheck struct {
	*fakeClient
	run func(context.Context, string) error
}

func (f controlledHealthcheck) ProviderHealthcheck(ctx context.Context, name string) error {
	return f.run(ctx, name)
}

func TestTestGroupDistinguishesFreshAndStaleHistory(t *testing.T) {
	old := []map[string]any{{"time": "2026-11-23T00:00:00Z", "delay": 123}}
	fresh := []map[string]any{{"time": "2026-11-23T00:00:01Z", "delay": 123}}
	node := map[string]any{"name": "HK", "type": "Vless", "history": old}
	fake := newFake(nil, map[string]any{"airport": map[string]any{"proxies": []map[string]any{node}}})
	snap := New(controlledHealthcheck{fake, func(context.Context, string) error {
		node["history"] = fresh
		return nil
	}}, "")
	if err := snap.Refresh(t.Context(), WithProviders); err != nil {
		t.Fatal(err)
	}

	group := Group{Members: []string{"HK"}}
	result, err := snap.TestGroup(t.Context(), group, nil, time.Second, 1000)
	if err != nil || result.Partial || result.Total() != 1 || result.Nodes["HK"] != 123 {
		t.Fatalf("本轮样本 = %+v, %v", result, err)
	}
	result, err = snap.TestGroup(t.Context(), group, nil, time.Second, 1000)
	if err != nil || !result.Partial || result.Total() != 1 || len(result.Nodes) != 0 {
		t.Fatalf("旧样本被计入新一轮 = %+v, %v", result, err)
	}
	if delay, ok := snap.Latency("HK"); !ok || delay != 123 {
		t.Fatalf("原有显示延迟 = %d, %v", delay, ok)
	}
}

func TestTestGroupPreservesOtherProvidersAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		providers := make(map[string]any)
		for _, name := range []string{"fast", "failed", "slow"} {
			providers[name] = map[string]any{"proxies": []map[string]any{
				{"name": name, "type": "Vless", "history": []map[string]any{{"delay": 123}}},
			}}
		}
		fake := newFake(nil, providers)
		stopped := make(chan struct{})
		snap := New(controlledHealthcheck{fake, func(ctx context.Context, name string) error {
			switch name {
			case "failed":
				return errors.New("healthcheck failed")
			case "slow":
				<-ctx.Done()
				close(stopped)
				return ctx.Err()
			default:
				return fake.ProviderHealthcheck(ctx, name)
			}
		}}, "")
		if err := snap.Refresh(t.Context(), WithProviders); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		group := Group{Members: []string{"fast", "failed", "slow"}}
		result, err := snap.TestGroup(t.Context(), group, nil, time.Second, 1000)
		if err != nil || time.Since(start) != time.Second {
			t.Fatalf("测速结束时间 = %s, err = %v", time.Since(start), err)
		}
		if result.Total() != 3 || result.OK() != 1 || len(result.Nodes) != 1 || result.Nodes["fast"] != 201 ||
			!result.Partial || result.Failures["failed"] != "healthcheck failed" || result.Failures["slow"] != context.DeadlineExceeded.Error() {
			t.Fatalf("部分结果 = %+v", result)
		}
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("慢 provider 收不到超时取消")
		}
	})
}

func TestTestGroupCancelsProviderResultRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake, snap := makeClient()
		api := &slowProviderRead{fakeClient: fake, started: make(chan struct{}), stopped: make(chan struct{})}
		snap.Client = api
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-api.started
			cancel()
		}()
		group, _ := snap.FindGroup("Manual", true)
		if _, err := snap.TestGroup(ctx, *group, nil, time.Minute, 1000); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		synctest.Wait()
		select {
		case <-api.stopped:
		default:
			t.Fatal("读取 provider 结果收不到取消")
		}
	})
}

type slowProviderRead struct {
	*fakeClient
	started chan struct{}
	stopped chan struct{}
}

func (f *slowProviderRead) Provider(ctx context.Context, name string) (jsontext.Value, error) {
	if len(f.healthcheck) == 0 {
		return f.fakeClient.Provider(ctx, name)
	}
	close(f.started)
	<-ctx.Done()
	close(f.stopped)
	return nil, ctx.Err()
}

func TestNodeRowShape(t *testing.T) {
	_, snap := makeClient()
	row := snap.NodeRow("HK-01", true)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "type", "provider", "group", "current", "delay", "timeout", "alive"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("缺少字段 %s: %s", key, raw)
		}
	}
	if fields["provider"] != "airport" {
		t.Fatalf("provider = %v", fields["provider"])
	}
	if fields["current"] != true {
		t.Fatalf("current = %v", fields["current"])
	}
}

func ExampleSnapshot_Groups() {
	_, snap := makeClient()
	fmt.Println(strings.Join(groupNames(snap.Groups(false)), ","))
	// Output: Manual
}
