package tui

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
	"github.com/zjyl1994/arkjmp/internal/core"
)

type fakeAPI struct {
	delay      int
	proxyCalls int
	proxiesErr error
	chosen     [][2]string
	checkin    []string
}

func (f *fakeAPI) Base() string                            { return "http://127.0.0.1:9090" }
func (f *fakeAPI) Version(context.Context) (string, error) { return "v1.19.34", nil }
func (f *fakeAPI) Mode(context.Context) (string, error)    { return "rule", nil }

func (f *fakeAPI) Proxies(context.Context) (*client.Object, error) {
	f.proxyCalls++
	if f.proxiesErr != nil {
		return nil, f.proxiesErr
	}
	now := "HK-01"
	if len(f.chosen) > 0 {
		now = f.chosen[len(f.chosen)-1][1]
	}
	return fakeObject(map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual"}},
		"Manual": map[string]any{"type": "Selector", "now": now, "all": []string{"HK-01", "SG-01"}},
		"HK-01":  map[string]any{"type": "Vless", "history": []map[string]any{{"delay": 120}}},
		"SG-01":  map[string]any{"type": "Vless", "history": []map[string]any{}},
	})
}

func (*fakeAPI) Provider(context.Context, string) (jsontext.Value, error) { return nil, nil }

func (f *fakeAPI) Providers(context.Context) (*client.Object, error) { return fakeObject(nil) }

func (f *fakeAPI) Select(_ context.Context, group, name string) error {
	f.chosen = append(f.chosen, [2]string{group, name})
	return nil
}

func (f *fakeAPI) Delay(_ context.Context, name, provider, testURL string, timeoutMs int) (int, error) {
	return f.delay, nil
}

func (f *fakeAPI) ProviderHealthcheck(_ context.Context, provider string) error {
	f.checkin = append(f.checkin, provider)
	return nil
}

func fakeObject(items map[string]any) (*client.Object, error) {
	obj := &client.Object{Items: map[string]jsontext.Value{}}
	for _, name := range []string{"GLOBAL", "Auto", "Manual", "Media", "HK-01", "SG-01"} {
		value, ok := items[name]
		if !ok {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		obj.Keys = append(obj.Keys, name)
		obj.Items[name] = raw
	}
	return obj, nil
}

func newSnapshot(t *testing.T) (*fakeAPI, *core.Snapshot) {
	t.Helper()
	api := &fakeAPI{delay: 42}
	snap := core.New(api, "https://cp.cloudflare.com/generate_204")
	if err := snap.Refresh(t.Context(), core.FullRefresh); err != nil {
		t.Fatal(err)
	}
	return api, snap
}

func newTestModel(t *testing.T) (*model, *fakeAPI) {
	t.Helper()
	api, snap := newSnapshot(t)
	m := newModel(t.Context(), snap)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, api
}

// key 构造一个 KeyPressMsg，方便测试按字母/特殊键。
func key(name string) tea.KeyPressMsg {
	special := map[string]rune{
		"enter":  tea.KeyEnter,
		"esc":    tea.KeyEscape,
		"tab":    tea.KeyTab,
		"left":   tea.KeyLeft,
		"right":  tea.KeyRight,
		"up":     tea.KeyUp,
		"down":   tea.KeyDown,
		"pgup":   tea.KeyPgUp,
		"pgdown": tea.KeyPgDown,
		"home":   tea.KeyHome,
		"end":    tea.KeyEnd,
	}
	if code, ok := special[name]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	if letter, ok := strings.CutPrefix(name, "ctrl+"); ok {
		return tea.KeyPressMsg{Code: rune(letter[0]), Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: rune(name[0]), Text: name}
}

func press(m *model, keys ...string) tea.Cmd {
	var last tea.Cmd
	for _, name := range keys {
		_, last = m.Update(key(name))
	}
	return last
}

func finishAction(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("预期返回异步命令")
	}
	m.Update(cmd())
}

func TestModelDrivesTheInterface(t *testing.T) {
	m, api := newTestModel(t)

	if view := m.render(); !strings.Contains(view, "arkjmp  mihomo v1.19.34") {
		t.Fatalf("标题没画出来:\n%s", view)
	}

	// 下移一格到 SG-01 并测延迟
	finishAction(t, m, press(m, "down", "ctrl+d"))
	if view := m.render(); !strings.Contains(view, "SG-01: 42ms") {
		t.Fatalf("测延迟结果没显示:\n%s", view)
	}

	// GLOBAL 开关
	press(m, "ctrl+g")
	if view := m.render(); !strings.Contains(view, "显示 GLOBAL") {
		t.Fatalf("GLOBAL 提示没显示:\n%s", view)
	}

	// 刷新
	finishAction(t, m, press(m, "ctrl+r"))
	if view := m.render(); !strings.Contains(view, "已刷新") {
		t.Fatalf("刷新提示没显示:\n%s", view)
	}

	// 回车切换选中节点
	finishAction(t, m, press(m, "enter"))
	if len(api.chosen) != 1 || api.chosen[0] != [2]string{"Manual", "SG-01"} {
		t.Fatalf("切换请求 = %v", api.chosen)
	}
	if view := m.render(); !strings.Contains(view, "已切换") {
		t.Fatalf("切换提示没显示:\n%s", view)
	}

	// Esc 退出（没过滤词时）
	if cmd := press(m, "esc"); cmd == nil {
		t.Fatal("按 Esc 应该返回 tea.Quit")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("按 Esc 返回的不是 QuitMsg")
	}
}

func TestModelQuitsOnEscape(t *testing.T) {
	m, _ := newTestModel(t)
	cmd := press(m, "esc")
	if cmd == nil {
		t.Fatal("按 Esc 应该返回 tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("按 Esc 返回的不是 QuitMsg")
	}
}

func TestModelScrollsAndKeepsCursorVisible(t *testing.T) {
	_, snap := newSnapshot(t)
	m := newModel(t.Context(), snap)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 8}) // 只有 4 行节点区
	for i := 0; i < 2; i++ {
		press(m, "down")
	}
	view := m.render()
	if m.off != 0 {
		t.Fatalf("不该滚动，off = %d", m.off)
	}
	if !strings.Contains(view, "SG-01") {
		t.Fatalf("光标行不在视野里:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines != 8 {
		t.Fatalf("View 行数 = %d，期望 8", lines)
	}
}

type tabsAPI struct{ *fakeAPI }

func (tabsAPI) Proxies(context.Context) (*client.Object, error) {
	return fakeObject(map[string]any{
		"Auto":   map[string]any{"type": "URLTest", "all": []string{"HK-01"}},
		"Manual": map[string]any{"type": "Selector", "all": []string{"Auto", "HK-01", "🇭🇰 香港 👩🏽‍💻"}},
		"Media":  map[string]any{"type": "Selector", "all": []string{"SG-01"}},
	})
}

func TestModelOpensFirstSelectorTab(t *testing.T) {
	snap := core.New(tabsAPI{&fakeAPI{}}, "")
	if err := snap.Refresh(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	m := newModel(t.Context(), snap)
	for _, step := range []struct {
		key  string
		tabs string
		node string
	}{
		{"", styleSel.Render("[Manual]") + " Media", "Auto"},
		{"right", "Manual " + styleSel.Render("[Media]"), "SG-01"},
		{"left", styleSel.Render("[Manual]") + " Media", "Auto"},
	} {
		if step.key != "" {
			press(m, step.key)
		}
		if got := m.tabLine(80); got != step.tabs || m.nodes()[0] != step.node {
			t.Fatalf("标签页 = %q，节点 = %v", got, m.nodes())
		}
	}
}

func TestModelRunsGroupTestInBackground(t *testing.T) {
	m, _ := newTestModel(t)
	cmd := press(m, "ctrl+t")
	if cmd == nil || !m.busy {
		t.Fatal("按 Ctrl+T 应该开始测速")
	}

	for cmd != nil {
		_, cmd = m.Update(cmd())
	}
	view := m.render()
	if !strings.Contains(view, "测速完成 [Manual]：2/2 可用") {
		t.Fatalf("测速完成提示不对:\n%s", view)
	}
}

// emptyAPI 是没有任何分组的控制器。
type emptyAPI struct{}

func (emptyAPI) Base() string                                                    { return "http://127.0.0.1:9090" }
func (emptyAPI) Version(context.Context) (string, error)                         { return "v1.19.34", nil }
func (emptyAPI) Mode(context.Context) (string, error)                            { return "rule", nil }
func (emptyAPI) Select(context.Context, string, string) error                    { return nil }
func (emptyAPI) Delay(context.Context, string, string, string, int) (int, error) { return -1, nil }
func (emptyAPI) ProviderHealthcheck(context.Context, string) error               { return nil }
func (emptyAPI) Provider(context.Context, string) (jsontext.Value, error)        { return nil, nil }

func (emptyAPI) Providers(context.Context) (*client.Object, error) {
	return &client.Object{Items: map[string]jsontext.Value{}}, nil
}
func (emptyAPI) Proxies(context.Context) (*client.Object, error) {
	return &client.Object{Items: map[string]jsontext.Value{}}, nil
}

func TestModelIgnoresZeroWindowSize(t *testing.T) {
	m, _ := newTestModel(t) // 100x30
	m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	if m.width != 100 || m.height != 30 {
		t.Fatalf("0x0 的窗口尺寸不该覆盖默认值: %dx%d", m.width, m.height)
	}
	if view := m.render(); !strings.Contains(view, "mihomo") {
		t.Fatalf("0x0 时不该显示\"终端太小\":\n%s", view)
	}
}

// reversedAPI 的 Manual 分组里 SG-01 在前，用来验证排序真的会重排。
type reversedAPI struct{ *fakeAPI }

func (r reversedAPI) Proxies(context.Context) (*client.Object, error) {
	return fakeObject(map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual"}},
		"Manual": map[string]any{"type": "Selector", "now": "HK-01", "all": []string{"SG-01", "HK-01"}},
		"HK-01":  map[string]any{"type": "Vless", "history": []map[string]any{{"delay": 120}}},
		"SG-01":  map[string]any{"type": "Vless", "history": []map[string]any{}},
	})
}

func TestModelSortsByDelay(t *testing.T) {
	api := &fakeAPI{delay: 42}
	snap := core.New(reversedAPI{api}, "https://cp.cloudflare.com/generate_204")
	if err := snap.Refresh(t.Context(), core.FullRefresh); err != nil {
		t.Fatal(err)
	}
	m := newModel(t.Context(), snap)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	if got := m.nodes(); got[0] != "SG-01" || got[1] != "HK-01" {
		t.Fatalf("初始顺序 = %v", got)
	}
	press(m, "up") // 光标从当前节点 HK-01 移到 SG-01

	press(m, "ctrl+o")
	if !strings.Contains(m.render(), "⇅") {
		t.Fatal("排序状态没显示")
	}
	if got := m.nodes(); got[0] != "HK-01" || got[1] != "SG-01" {
		t.Fatalf("排序后顺序 = %v，未测过的应该排在测过的之后", got)
	}
	// 光标留在原来选中的 SG-01 上
	if m.index() != 1 {
		t.Fatalf("排序后光标 = %d，期望 1", m.index())
	}
	finishAction(t, m, press(m, "enter"))
	if len(api.chosen) != 1 || api.chosen[0] != [2]string{"Manual", "SG-01"} {
		t.Fatalf("排序后切换请求 = %v", api.chosen)
	}

	// 再按一次恢复原始顺序
	press(m, "ctrl+o")
	if got := m.nodes(); got[0] != "SG-01" || got[1] != "HK-01" {
		t.Fatalf("恢复后顺序 = %v", got)
	}
	if m.index() != 0 {
		t.Fatalf("恢复后光标 = %d，期望 0", m.index())
	}
}

func TestModelKeepsSelectionWhenDelaysReorder(t *testing.T) {
	api := &fakeAPI{delay: 42}
	snap := core.New(reversedAPI{api}, "https://cp.cloudflare.com/generate_204")
	if err := snap.Refresh(t.Context(), core.FullRefresh); err != nil {
		t.Fatal(err)
	}
	m := newModel(t.Context(), snap)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// reversedAPI 的成员是 [SG-01 HK-01]，当前节点 HK-01(120ms)；把光标移到 SG-01
	press(m, "up")
	press(m, "ctrl+o")
	if got := m.nodes(); got[0] != "HK-01" || m.index() != 1 {
		t.Fatalf("排序后 nodes=%v index=%d", got, m.index())
	}
	if m.anchor["Manual"] != "SG-01" {
		t.Fatalf("anchor = %q", m.anchor["Manual"])
	}

	// SG-01 测出更低延迟，排序会把它顶到第一行；光标要跟着它，而不是留在下标 1
	if _, err := snap.Measure(t.Context(), "SG-01", 1000); err != nil {
		t.Fatal(err)
	}
	m.testing = true
	m.Update(progressMsg{id: m.actionID})
	if got := m.nodes(); got[0] != "SG-01" {
		t.Fatalf("重排后 nodes=%v", got)
	}
	if m.index() != 0 || m.anchor["Manual"] != "SG-01" {
		t.Fatalf("重排后 index=%d anchor=%q", m.index(), m.anchor["Manual"])
	}
}

func TestModelFilterAndSortTogether(t *testing.T) {
	m, _ := newTestModel(t)

	press(m, "ctrl+o")
	press(m, "s", "g")
	if got := m.nodes(); len(got) != 1 || got[0] != "SG-01" {
		t.Fatalf("过滤 + 排序后 nodes = %v", got)
	}
	if m.index() != 0 {
		t.Fatalf("index = %d", m.index())
	}
	if view := m.render(); !strings.Contains(view, "⇅") || !strings.Contains(view, "过滤: sg") {
		t.Fatalf("排序/过滤状态没显示:\n%s", view)
	}

	// Esc 只清过滤，排序状态保留
	press(m, "esc")
	if m.filter != "" || !m.sortDelay {
		t.Fatalf("Esc 后 filter=%q sort=%v", m.filter, m.sortDelay)
	}
}

func TestModelFiltersByName(t *testing.T) {
	m, api := newTestModel(t)

	// 直接敲字母就是过滤，不会当成命令
	if cmd := press(m, "s", "g"); cmd != nil {
		t.Fatal("敲字母不应该触发命令")
	}
	if m.filter != "sg" {
		t.Fatalf("过滤词 = %q", m.filter)
	}
	if got := m.nodes(); len(got) != 1 || got[0] != "SG-01" {
		t.Fatalf("过滤结果 = %v", got)
	}
	if view := m.render(); !strings.Contains(view, "过滤: sg") {
		t.Fatalf("过滤状态没显示:\n%s", view)
	}

	// Enter 直接切换过滤出的那一个
	finishAction(t, m, press(m, "enter"))
	if len(api.chosen) != 1 || api.chosen[0] != [2]string{"Manual", "SG-01"} {
		t.Fatalf("过滤后切换请求 = %v", api.chosen)
	}

	// Backspace 删字
	press(m, "backspace")
	if m.filter != "s" {
		t.Fatalf("退格后过滤词 = %q", m.filter)
	}

	// Esc 先清过滤，不会退出
	if cmd := press(m, "esc"); cmd != nil {
		t.Fatal("有过滤词时 Esc 不该退出")
	}
	if m.filter != "" {
		t.Fatalf("Esc 后过滤词 = %q", m.filter)
	}
	if got := m.nodes(); len(got) != 2 {
		t.Fatalf("清过滤后节点 = %v", got)
	}

	// q 现在也只是过滤词
	if cmd := press(m, "q"); cmd != nil {
		t.Fatal("按 q 不应该退出")
	}
	if m.filter != "q" {
		t.Fatalf("过滤词 = %q", m.filter)
	}
}

func TestModelFiltersPastedEmojiAndDeletesWholeGraphemes(t *testing.T) {
	snap := core.New(tabsAPI{&fakeAPI{}}, "")
	if err := snap.Refresh(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	m := newModel(t.Context(), snap)
	const name = "🇭🇰 香港 👩🏽‍💻"
	_, cmd := m.Update(tea.PasteMsg{Content: name + "\r\n"})
	if cmd != nil || m.filter != name || len(m.nodes()) != 1 || m.nodes()[0] != name {
		t.Fatalf("粘贴后过滤词 = %q，节点 = %v", m.filter, m.nodes())
	}
	for _, want := range []string{"🇭🇰 香港 ", "🇭🇰 香港", "🇭🇰 香", "🇭🇰 ", "🇭🇰", ""} {
		press(m, "backspace")
		if m.filter != want {
			t.Fatalf("退格后过滤词 = %q，期望 %q", m.filter, want)
		}
	}
}

func TestModelShowsErrorWhenNoGroups(t *testing.T) {
	snap := core.New(emptyAPI{}, "")
	m := newModel(t.Context(), snap)
	if view := m.render(); !strings.Contains(view, "没有可切换的 Selector") {
		t.Fatalf("没有任何分组时应该提示:\n%s", view)
	}
}

func TestModelRefreshFailurePreservesList(t *testing.T) {
	m, api := newTestModel(t)
	if api.proxyCalls != 1 {
		t.Fatalf("启动时 /proxies 请求次数 = %d", api.proxyCalls)
	}
	press(m, "ctrl+g", "ctrl+g")
	if api.proxyCalls != 1 {
		t.Fatalf("切换 GLOBAL 后 /proxies 请求次数 = %d", api.proxyCalls)
	}
	api.proxiesErr = errors.New("controller unreachable")
	finishAction(t, m, press(m, "ctrl+r"))
	if len(m.nodes()) != 2 || m.kind != "err" || !strings.Contains(m.msg, "controller unreachable") {
		t.Fatalf("刷新失败后 nodes=%v, kind=%s, msg=%s", m.nodes(), m.kind, m.msg)
	}
}

type blockingDelay struct {
	*fakeAPI
	started chan struct{}
}

func (f blockingDelay) Delay(ctx context.Context, _, _, _ string, _ int) (int, error) {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestModelNavigatesAndQuitsDuringSingleTest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := blockingDelay{fakeAPI: &fakeAPI{}, started: make(chan struct{})}
		snap := core.New(api, "")
		if err := snap.Refresh(t.Context(), core.FullRefresh); err != nil {
			t.Fatal(err)
		}
		m := newModel(t.Context(), snap)
		cmd := press(m, "ctrl+d")
		if cmd == nil || !m.busy {
			t.Fatal("预期延迟测试在后台执行")
		}
		result := make(chan tea.Msg, 1)
		go func() { result <- cmd() }()
		<-api.started

		press(m, "down")
		if m.index() != 1 {
			t.Fatal("测速期间应可移动光标")
		}
		if cmd := press(m, "ctrl+c"); cmd == nil {
			t.Fatal("测速期间应可退出")
		}
		msg := (<-result).(actionMsg)
		if !strings.Contains(msg.message, context.Canceled.Error()) {
			t.Fatalf("退出后测速结果 = %+v", msg)
		}
	})
}

func TestModelCanSwitchImmediatelyAfterCancelingTest(t *testing.T) {
	for _, shortcut := range []string{"ctrl+d", "ctrl+t"} {
		t.Run(shortcut, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := blockingDelay{fakeAPI: &fakeAPI{}, started: make(chan struct{}, 2)}
				snap := core.New(api, "")
				if err := snap.Refresh(t.Context(), 0); err != nil {
					t.Fatal(err)
				}
				m := newModel(t.Context(), snap)
				cmd := press(m, shortcut)
				result := make(chan tea.Msg, 1)
				go func() { result <- cmd() }()
				<-api.started
				if shortcut == "ctrl+t" {
					<-api.started // 两个节点并发测速
				}
				press(m, "SG")
				if press(m, "esc") != nil || !m.testing || m.filter != "" {
					t.Fatal("第一次 Esc 应该清除过滤词，继续测速")
				}
				if press(m, "esc") != nil || m.busy || m.msg != "已取消测速" {
					t.Fatal("第二次 Esc 应该取消测速并保留界面")
				}
				switchCmd := press(m, "down", "enter")
				m.Update(<-result) // 旧命令返回时切换已经开始。
				finishAction(t, m, switchCmd)
				if len(api.chosen) != 1 || api.chosen[0] != [2]string{"Manual", "SG-01"} ||
					!strings.Contains(m.msg, "已切换") {
					t.Fatalf("切换请求 = %v，状态 = %s", api.chosen, m.msg)
				}
			})
		})
	}
}

func TestModelSingleTestTimesOutAfterThreeSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := blockingDelay{fakeAPI: &fakeAPI{}, started: make(chan struct{}, 1)}
		snap := core.New(api, "")
		if err := snap.Refresh(t.Context(), 0); err != nil {
			t.Fatal(err)
		}
		m := newModel(t.Context(), snap)
		start := time.Now()
		finishAction(t, m, press(m, "ctrl+d"))
		if elapsed := time.Since(start); elapsed != 3*time.Second || m.msg != "HK-01: 超时" {
			t.Fatalf("耗时 = %v，状态 = %s", elapsed, m.msg)
		}
	})
}
