package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
	"github.com/zjyl1994/arkjmp/internal/core"
)

// fakeAPI 是 core.API 的最小实现，供解析/推断逻辑的离线测试用。
type fakeAPI struct{}

func (fakeAPI) Base() string                                                    { return "http://127.0.0.1:9090" }
func (fakeAPI) Version(context.Context) (string, error)                         { return "v1.19.34", nil }
func (fakeAPI) Mode(context.Context) (string, error)                            { return "rule", nil }
func (fakeAPI) Select(_ context.Context, group, name string) error              { return nil }
func (fakeAPI) ProviderHealthcheck(context.Context, string) error               { return nil }
func (fakeAPI) Delay(context.Context, string, string, string, int) (int, error) { return -1, nil }
func (fakeAPI) Provider(context.Context, string) (jsontext.Value, error)        { return nil, nil }

func (fakeAPI) Providers(context.Context) (*client.Object, error) {
	return &client.Object{Items: map[string]jsontext.Value{}}, nil
}

func (fakeAPI) Proxies(context.Context) (*client.Object, error) {
	return fakeProxies(map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual"}},
		"Manual": map[string]any{"type": "Selector", "now": "HK-01", "all": []string{"HK-01", "SG-01"}},
		"Auto":   map[string]any{"type": "URLTest", "now": "SG-01", "all": []string{"SG-01"}},
		"HK-01":  map[string]any{"type": "Vless", "history": []map[string]any{{"delay": 120}}},
		"SG-01":  map[string]any{"type": "Vless", "history": []map[string]any{}},
	})
}

func fakeProxies(items map[string]any) (*client.Object, error) {
	obj := &client.Object{Items: map[string]jsontext.Value{}}
	// 固定顺序，保证 tests 结果稳定
	for _, name := range []string{"GLOBAL", "Manual", "Auto", "HK-01", "SG-01"} {
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

// singleGroupAPI 只有一个分组。
type singleGroupAPI struct{ fakeAPI }

func (singleGroupAPI) Proxies(context.Context) (*client.Object, error) {
	return fakeProxies(map[string]any{
		"GLOBAL": map[string]any{"type": "Selector", "now": "Manual", "all": []string{"Manual"}},
		"Manual": map[string]any{"type": "Selector", "now": "HK-01", "all": []string{"HK-01", "SG-01"}},
		"HK-01":  map[string]any{"type": "Vless"},
		"SG-01":  map[string]any{"type": "Vless"},
	})
}

func snapshotWith(api core.API) *core.Snapshot {
	snap := core.New(api, "https://cp.cloudflare.com/generate_204")
	if err := snap.Refresh(context.Background(), core.FullRefresh); err != nil {
		panic(err)
	}
	return snap
}

func TestParseFlagsBeforeAndAfterCommand(t *testing.T) {
	before, err := Parse([]string{"--json", "groups"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := Parse([]string{"groups", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !before.JSON || !after.JSON {
		t.Fatal("--json 写在前后都该生效")
	}
	if before.Command != "groups" || after.Command != "groups" {
		t.Fatalf("command = %q / %q", before.Command, after.Command)
	}
}

func TestParseAliasesAndLegacyList(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"ui"}, "tui"},
		{[]string{"use", "HK-01"}, "switch"},
		{[]string{"select", "HK-01"}, "switch"},
		{[]string{"ping", "HK-01"}, "delay"},
	}
	for _, c := range cases {
		o, err := Parse(c.argv)
		if err != nil {
			t.Fatalf("%v: %v", c.argv, err)
		}
		if o.Command != c.want {
			t.Errorf("%v -> %q, 期望 %q", c.argv, o.Command, c.want)
		}
	}
	o, err := Parse([]string{"--list"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "list" {
		t.Fatalf("--list -> %q", o.Command)
	}
	o, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "tui" {
		t.Fatalf("无参数 -> %q", o.Command)
	}
}

func TestParseSwitchPositionals(t *testing.T) {
	o, err := Parse([]string{"switch", "Manual", "HK-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Args) != 2 || o.Args[0] != "Manual" || o.Args[1] != "HK-01" {
		t.Fatalf("args = %v", o.Args)
	}

	o, err = Parse([]string{"switch", "--index", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.IndexSet || o.Index != 3 {
		t.Fatalf("index = %d,%v", o.Index, o.IndexSet)
	}

	o, err = Parse([]string{"nodes", "--group", "Manual", "--show-global"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Group != "Manual" || !o.ShowGlobal {
		t.Fatalf("nodes 参数 = %+v", o)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := [][]string{
		{"--nope"},
		{"nosuchcommand"},
		{"switch", "a", "b", "c"},
		{"delay"},
		{"groups", "extra"},
		{"--timeout", "abc"},
		{"--timeout", "NaN"},
		{"--timeout", "1e20"},
		{"test", "--wait", "-1"},
		{"test", "--wait", "+Inf"},
		{"delay", "HK-01", "--timeout-ms", "0"},
		{"switch", "--index", "1.5"},
	}
	for _, argv := range cases {
		if _, err := Parse(argv); err == nil {
			t.Errorf("%v 应该报错", argv)
		}
	}
}

func TestParseHelpAndVersion(t *testing.T) {
	o, err := Parse([]string{"--help"})
	if err != nil || !o.Help {
		t.Fatalf("help = %+v, %v", o, err)
	}
	o, err = Parse([]string{"switch", "--help"})
	if err != nil || !o.Help || o.Command != "switch" {
		t.Fatalf("switch --help = %+v, %v", o, err)
	}
	o, err = Parse([]string{"--version"})
	if err != nil || !o.Version {
		t.Fatalf("version = %+v, %v", o, err)
	}
}

func TestGroupOmittedWhenSingleGroup(t *testing.T) {
	snap := snapshotWith(singleGroupAPI{})

	group, node, err := groupAndNode(snap, &Options{Args: []string{"SG-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "Manual" || node != "SG-01" {
		t.Fatalf("group,node = %q,%q", group.Name, node)
	}

	// 显式分组依旧可用
	group, node, err = groupAndNode(snap, &Options{Args: []string{"Manual", "SG-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "Manual" || node != "SG-01" {
		t.Fatalf("group,node = %q,%q", group.Name, node)
	}

	// 只给分组名 = 缺节点
	group, node, err = groupAndNode(snap, &Options{Args: []string{"Manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "Manual" || node != "" {
		t.Fatalf("group,node = %q,%q", group.Name, node)
	}

	// 只靠 --index
	group, node, err = groupAndNode(snap, &Options{Args: nil, IndexSet: true, Index: 1})
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "Manual" || node != "" || group.Members[1] != "SG-01" {
		t.Fatalf("group,node = %q,%q", group.Name, node)
	}
}

func TestGroupGuessedFromUniqueOwner(t *testing.T) {
	snap := snapshotWith(fakeAPI{})
	group, err := guessGroup(snap, "HK-01", false)
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "Manual" {
		t.Fatalf("分组 = %q", group.Name)
	}
}

func TestAmbiguousNodeAcrossGroups(t *testing.T) {
	snap := snapshotWith(fakeAPI{})
	_, err := guessGroup(snap, "SG-01", false)
	var cmdErr *Error
	if !errors.As(err, &cmdErr) || cmdErr.Code != ExitUsage {
		t.Fatalf("错误 = %v", err)
	}
	if !strings.Contains(cmdErr.Msg, "Manual") {
		t.Fatalf("错误信息 = %q", cmdErr.Msg)
	}
}

func TestMultipleGroupsStillRequireGroup(t *testing.T) {
	snap := snapshotWith(fakeAPI{})
	_, err := guessGroup(snap, "", false)
	var cmdErr *Error
	if !errors.As(err, &cmdErr) || cmdErr.Code != ExitUsage {
		t.Fatalf("错误 = %v", err)
	}
	if !strings.Contains(cmdErr.Msg, "Auto") {
		t.Fatalf("错误信息 = %q", cmdErr.Msg)
	}
}

func TestSwitchPayloadJSON(t *testing.T) {
	// dry-run：would_change 在 changed 之后，now 不出现
	wouldChange := true
	previous := "SG-01"
	payload := switchPayload{
		Group:       "Manual",
		Node:        "HK-01",
		Previous:    &previous,
		DryRun:      true,
		WouldChange: &wouldChange,
		OK:          true,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"group":"Manual","node":"HK-01","previous":"SG-01","dry_run":true,"changed":false,"would_change":true,"ok":true}`
	if string(raw) != want {
		t.Fatalf("JSON = %s\n期望 %s", raw, want)
	}

	// 真正切换后：now 出现，would_change 不出现
	now := "HK-01"
	payload = switchPayload{Group: "Manual", Node: "HK-01", Previous: &previous, Changed: true, Now: &now, OK: true}
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"group":"Manual","node":"HK-01","previous":"SG-01","dry_run":false,"changed":true,"now":"HK-01","ok":true}`
	if string(raw) != want {
		t.Fatalf("JSON = %s\n期望 %s", raw, want)
	}
}

func TestPayloadsKeepNullAndEmptyShapes(t *testing.T) {
	// previous/provider/now 为 nil 时输出 null；空 map/切片输出 {} / []
	raw, err := json.Marshal(delayPayload{Node: "HK-01", Delay: -1, Timeout: true, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"node":"HK-01","provider":null,"delay":-1,"timeout":true,"ok":true}`; string(raw) != want {
		t.Fatalf("JSON = %s\n期望 %s", raw, want)
	}

	raw, err = json.Marshal(testPayload{Failures: map[string]string{}, Nodes: []core.NodeRow{}, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"failures":{}`) || !strings.Contains(string(raw), `"nodes":[]`) {
		t.Fatalf("空集合输出不对: %s", raw)
	}

	// 无 env_file 时是 null，而不是省略
	raw, err = json.Marshal(statusPayload{API: "http://x", OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"env_file":null`) {
		t.Fatalf("env_file 应该是 null: %s", raw)
	}
}

func TestJSONOutputIsIndentedAndOrdered(t *testing.T) {
	var sb strings.Builder
	err := json.MarshalWrite(&sb, groupsPayload{
		Groups: []groupSummary{{Name: "Manual", Type: "Selector", Chain: []string{"Manual"}}},
		OK:     true,
	}, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "groups": [
    {
      "name": "Manual",
      "type": "Selector",
      "now": null,
      "members": 0,
      "chain": [
        "Manual"
      ]
    }
  ],
  "ok": true
}`
	if sb.String() != want {
		t.Fatalf("输出 =\n%s\n期望 =\n%s", sb.String(), want)
	}
}

func TestJSONDoesNotEscapeHTML(t *testing.T) {
	raw, err := json.Marshal(core.NodeRow{Name: "a<b&c"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "a<b&c") {
		t.Fatalf("不该做 HTML 转义: %s", raw)
	}
}

func TestHelpTextMentionsCommands(t *testing.T) {
	for _, command := range []string{"", "switch", "test", "delay"} {
		text := helpText(command)
		if !strings.Contains(text, "usage:") {
			t.Errorf("命令 %q 的帮助缺少 usage", command)
		}
	}
	if !strings.Contains(helpText(""), "group") {
		t.Error("总帮助里应该有 group 命令")
	}
}
