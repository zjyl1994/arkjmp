// Package cli 是命令行入口：所有能力都能被 Agent 以非交互方式调用。
//
// 约定：
//   - 每个命令都支持 --json，输出单个 JSON 对象，且一定带 "ok" 字段；
//   - 退出码见 Exit* 常量，脚本/Agent 可以据此判断，不必解析文本；
//   - 进度、提示走 stderr，stdout 只留结果。
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
	"github.com/zjyl1994/arkjmp/internal/core"
	"github.com/zjyl1994/arkjmp/internal/settings"
	"github.com/zjyl1994/arkjmp/internal/text"
	"github.com/zjyl1994/arkjmp/internal/tui"
)

// 退出码。
const (
	ExitOK          = 0
	ExitError       = 1 // 连不上控制器 / API 报错
	ExitUsage       = 2 // 参数用错
	ExitNotFound    = 3 // 分组或节点不存在（含歧义）
	ExitUnsupported = 4 // 操作不适用（如给 DIRECT 测延迟）
)

// Version 由 Makefile 通过 -ldflags -X 覆盖。
var Version = "3.0.0"

// Error 是带退出码的命令错误。
type Error struct {
	Msg  string
	Code int
}

func (e *Error) Error() string { return e.Msg }

func usageErr(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Code: ExitUsage}
}

func notFoundErr(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Code: ExitNotFound}
}

func unsupportedErr(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Code: ExitUnsupported}
}

func apiErr(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Code: ExitError}
}

// --------------------------------------------------------------------------
// 参数解析
// --------------------------------------------------------------------------

// Options 是解析后的命令行参数。
type Options struct {
	JSON     bool
	Quiet    bool
	NoColor  bool
	Help     bool
	Version  bool
	API      string
	Secret   string
	EnvFile  string
	TestURL  string
	Config   string
	Timeout  float64
	TimeoutS bool

	ShowGlobal bool
	Group      string
	Index      int
	IndexSet   bool
	DryRun     bool
	Wait       float64
	TimeoutMs  int

	Command  string
	Args     []string
	listSeen bool
}

type flagKind struct {
	takesValue bool
	apply      func(o *Options, value string) error
}

func stringFlag(dst func(*Options) *string) flagKind {
	return flagKind{takesValue: true, apply: func(o *Options, value string) error {
		*dst(o) = value
		return nil
	}}
}

func boolFlag(dst func(*Options) *bool) flagKind {
	return flagKind{apply: func(o *Options, _ string) error {
		*dst(o) = true
		return nil
	}}
}

var globalFlags = map[string]flagKind{
	"--json":     boolFlag(func(o *Options) *bool { return &o.JSON }),
	"--quiet":    boolFlag(func(o *Options) *bool { return &o.Quiet }),
	"-q":         boolFlag(func(o *Options) *bool { return &o.Quiet }),
	"--no-color": boolFlag(func(o *Options) *bool { return &o.NoColor }),
	"--help":     boolFlag(func(o *Options) *bool { return &o.Help }),
	"-h":         boolFlag(func(o *Options) *bool { return &o.Help }),
	"--version":  boolFlag(func(o *Options) *bool { return &o.Version }),
	"--list":     boolFlag(func(o *Options) *bool { return &o.listSeen }),
	"--api":      stringFlag(func(o *Options) *string { return &o.API }),
	"--secret":   stringFlag(func(o *Options) *string { return &o.Secret }),
	"--env-file": stringFlag(func(o *Options) *string { return &o.EnvFile }),
	"--test-url": stringFlag(func(o *Options) *string { return &o.TestURL }),
	"--config":   stringFlag(func(o *Options) *string { return &o.Config }),
	"--timeout": {takesValue: true, apply: func(o *Options, value string) error {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || !settings.ValidTimeout(parsed) {
			return usageErr("--timeout 需要有效的正秒数，收到 %q", value)
		}
		o.Timeout, o.TimeoutS = parsed, true
		return nil
	}},
}

var commandFlags = map[string]map[string]flagKind{
	"groups": {
		"--show-global": boolFlag(func(o *Options) *bool { return &o.ShowGlobal }),
	},
	"list": {
		"--show-global": boolFlag(func(o *Options) *bool { return &o.ShowGlobal }),
	},
	"nodes": {
		"--show-global": boolFlag(func(o *Options) *bool { return &o.ShowGlobal }),
		"--group":       stringFlag(func(o *Options) *string { return &o.Group }),
	},
	"current": {
		"--show-global": boolFlag(func(o *Options) *bool { return &o.ShowGlobal }),
		"--group":       stringFlag(func(o *Options) *string { return &o.Group }),
	},
	"test": {
		"--show-global": boolFlag(func(o *Options) *bool { return &o.ShowGlobal }),
		"--group":       stringFlag(func(o *Options) *string { return &o.Group }),
		"--wait": {takesValue: true, apply: func(o *Options, value string) error {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || (parsed != 0 && !settings.ValidTimeout(parsed)) {
				return usageErr("--wait 需要有效的非负秒数，收到 %q", value)
			}
			o.Wait = parsed
			return nil
		}},
	},
	"switch": {
		"--index": {takesValue: true, apply: func(o *Options, value string) error {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return usageErr("--index 需要一个整数，收到 %q", value)
			}
			o.Index, o.IndexSet = parsed, true
			return nil
		}},
		"--dry-run": boolFlag(func(o *Options) *bool { return &o.DryRun }),
	},
	"delay": {
		"--timeout-ms": {takesValue: true, apply: func(o *Options, value string) error {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed <= 0 || parsed > core.MaxDelayTimeoutMs {
				return usageErr("--timeout-ms 需要 1–%d 之间的整数，收到 %q", core.MaxDelayTimeoutMs, value)
			}
			o.TimeoutMs = parsed
			return nil
		}},
	},
}

var commands = map[string]string{
	"tui": "tui", "ui": "tui",
	"status":  "status",
	"list":    "list",
	"groups":  "groups",
	"group":   "group",
	"nodes":   "nodes",
	"current": "current",
	"chain":   "chain",
	"switch":  "switch", "select": "switch", "use": "switch",
	"delay": "delay", "ping": "delay",
	"test": "test",
}

// Parse 解析参数（全局选项写在子命令前后都可以）。
func Parse(argv []string) (*Options, error) {
	o := &Options{Wait: 120, TimeoutMs: core.DefaultDelayTimeoutMs}
	positionalOnly := false
	var parseErr error

	for i := 0; i < len(argv); i++ {
		token := argv[i]
		switch {
		case positionalOnly:
			if err := o.addPositional(token); err != nil && parseErr == nil {
				parseErr = err
			}
		case token == "--":
			positionalOnly = true
		case strings.HasPrefix(token, "-") && token != "-":
			name, inline, hasInline := strings.Cut(token, "=")
			kind, ok := globalFlags[name]
			if !ok && o.Command != "" {
				kind, ok = commandFlags[o.Command][name]
			}
			if !ok {
				if parseErr == nil {
					parseErr = usageErr("无法识别的参数 %s", name)
				}
				continue
			}
			value := ""
			if kind.takesValue {
				if hasInline {
					value = inline
				} else {
					if i+1 >= len(argv) {
						if parseErr == nil {
							parseErr = usageErr("参数 %s 需要一个值", name)
						}
						continue
					}
					i++
					value = argv[i]
				}
			} else if hasInline {
				if parseErr == nil {
					parseErr = usageErr("参数 %s 不接受值", name)
				}
				continue
			}
			if err := kind.apply(o, value); err != nil && parseErr == nil {
				parseErr = err
			}
		default:
			if err := o.addPositional(token); err != nil && parseErr == nil {
				parseErr = err
			}
		}
	}
	if parseErr != nil {
		return o, parseErr
	}
	return o, o.validate()
}

func (o *Options) addPositional(token string) error {
	if o.Command == "" {
		canonical, ok := commands[token]
		if !ok {
			return usageErr("无法识别的命令 '%s'（用 --help 看看有哪些）", token)
		}
		o.Command = canonical
		return nil
	}
	o.Args = append(o.Args, token)
	return nil
}

func (o *Options) validate() error {
	if o.Help || o.Version {
		return nil
	}
	if o.Command == "" {
		if o.listSeen {
			o.Command = "list"
		} else {
			o.Command = "tui"
		}
	}
	switch o.Command {
	case "switch":
		if len(o.Args) > 2 {
			return usageErr("switch 最多两个位置参数（分组、节点），收到 %d 个", len(o.Args))
		}
	case "chain", "group":
		if len(o.Args) > 1 {
			return usageErr("%s 最多一个位置参数，收到 %d 个", o.Command, len(o.Args))
		}
	case "delay":
		if len(o.Args) != 1 {
			return usageErr("delay 需要一个节点名")
		}
	case "tui", "status", "list", "groups", "nodes", "current", "test":
		if len(o.Args) > 0 {
			return usageErr("%s 不接受位置参数，收到 '%s'", o.Command, o.Args[0])
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// 输出
// --------------------------------------------------------------------------

type out struct {
	jsonOut bool
	quiet   bool
	style   text.Style
}

func newOut(o *Options) *out {
	enabled := !o.JSON && !o.NoColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	return &out{jsonOut: o.JSON, quiet: o.Quiet, style: text.Style{Enabled: enabled}}
}

// emit 成功时输出 payload（JSON 或文本行）。JSON 用 encoding/json/v2：
// 结构体按声明顺序输出，nil map/slice 变成 {} 和 []，字符串不做 HTML 转义。
func (o *out) emit(payload any, lines []string) error {
	if o.jsonOut {
		if err := json.MarshalWrite(os.Stdout, payload,
			json.Deterministic(true), jsontext.WithIndent("  ")); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout)
		return err
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

func (o *out) note(message string) {
	if o.quiet {
		return
	}
	fmt.Fprintln(os.Stderr, message)
}

func (o *out) emitError(err error, code int) {
	if !o.jsonOut {
		return
	}
	payload := errorPayload{OK: false, Error: errorInfo{Message: err.Error(), ExitCode: code}}
	if encodeErr := o.emit(payload, nil); encodeErr != nil {
		fmt.Fprintln(os.Stderr, "错误: "+encodeErr.Error())
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// progressPrinter 测速进度：终端里原地刷新，重定向时按行输出。
type progressPrinter struct {
	out  *out
	tty  bool
	last string
}

func newProgress(o *out) *progressPrinter {
	return &progressPrinter{out: o, tty: isTerminal(os.Stderr)}
}

func (p *progressPrinter) progress(stage string, done, total int, label string) {
	if p.out.quiet {
		return
	}
	var message string
	if stage == "node" {
		message = "测速 " + label + " …"
	} else {
		message = fmt.Sprintf("测速进度 … %d/%d 个 provider 已处理（%s）", done, total, label)
	}
	if message == p.last {
		return
	}
	p.last = message
	if p.tty {
		fmt.Fprint(os.Stderr, "\r\x1b[2K"+message)
		return
	}
	fmt.Fprintln(os.Stderr, message)
}

func (p *progressPrinter) clear() {
	if p.tty && !p.out.quiet {
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
	}
}

// --------------------------------------------------------------------------
// 公共准备
// --------------------------------------------------------------------------

func buildSnapshot(ctx context.Context, o *Options, parts core.RefreshParts) (*core.Snapshot, *client.Client, settings.Settings, error) {
	cfg := settings.Load(settings.Overrides{
		API:        o.API,
		Secret:     o.Secret,
		TestURL:    o.TestURL,
		ConfigPath: o.Config,
		EnvFile:    o.EnvFile,
		Timeout:    o.Timeout,
		TimeoutSet: o.TimeoutS,
	}, nil, nil)
	c := client.New(cfg.API, cfg.Secret, cfg.Timeout)
	snap := core.New(c, cfg.TestURL)
	if err := snap.Refresh(ctx, parts); err != nil {
		c.Close()
		return nil, nil, cfg, err
	}
	return snap, c, cfg, nil
}

func groupTable(groups []core.Group, style text.Style) []string {
	rows := make([][]text.Cell, 0, len(groups))
	for _, group := range groups {
		now := text.Cell{Text: "-", Color: "dim"}
		if group.HasNow {
			now = text.Cell{Text: group.Now, Color: "cyan"}
		}
		rows = append(rows, []text.Cell{
			{Text: group.Name, Color: "bold"},
			{Text: group.Type},
			now,
			{Text: strconv.Itoa(group.Count())},
		})
	}
	return text.RenderTable(
		[]string{"分组", "类型", "当前节点", "节点数"},
		rows,
		[]string{"left", "left", "left", "right"},
		style, "  ",
	)
}

func nodeTable(nodes []core.NodeRow, style text.Style) []string {
	rows := make([][]text.Cell, 0, len(nodes))
	for _, node := range nodes {
		var mark text.Cell
		switch {
		case node.Current:
			mark = text.Cell{Text: "●", Color: "green"}
		case node.Group:
			mark = text.Cell{Text: "◎", Color: "cyan"}
		default:
			mark = text.Cell{Text: "○", Color: "dim"}
		}
		label := text.Cell{Text: node.Name}
		if node.Current {
			label.Color = "bold"
		}
		nodeType, provider := "-", "-"
		if node.Type != nil {
			nodeType = *node.Type
		}
		if node.Provider != nil {
			provider = *node.Provider
		}
		var delay int
		if node.Delay != nil {
			delay = *node.Delay
		}
		rows = append(rows, []text.Cell{
			mark,
			label,
			{Text: nodeType},
			{Text: provider},
			text.LatencyCell(delay, node.Delay != nil),
		})
	}

	return text.RenderTable(
		[]string{"", "节点", "类型", "机场", "延迟"},
		rows,
		[]string{"left", "left", "left", "left", "right"},
		style, "  ",
	)
}

// --------------------------------------------------------------------------
// 名字解析（分组可省略）
// --------------------------------------------------------------------------

func pickGroup(snap *core.Snapshot, name string) (core.Group, error) {
	group, err := snap.ResolveGroup(name)
	if err == nil {
		return *group, nil
	}
	if lookup, ok := errors.AsType[*core.LookupError](err); ok {
		if len(lookup.Near) > 0 {
			return core.Group{}, notFoundErr("无法唯一确定分组 '%s'，候选: %s", lookup.Want, strings.Join(lookup.Near, ", "))
		}
		return core.Group{}, notFoundErr("找不到分组 '%s'", lookup.Want)
	}
	return core.Group{}, notFoundErr("找不到分组 '%s'", name)
}

func pickNode(snap *core.Snapshot, name string, members []string) (string, error) {
	node, err := snap.ResolveNode(name, members)
	if err == nil {
		return node, nil
	}
	if lookup, ok := errors.AsType[*core.LookupError](err); ok {
		if len(lookup.Near) > 0 {
			return "", notFoundErr("无法唯一确定节点 '%s'，候选: %s", lookup.Want, strings.Join(lookup.Near, ", "))
		}
		return "", notFoundErr("找不到节点 '%s'", lookup.Want)
	}
	return "", notFoundErr("找不到节点 '%s'", name)
}

func nodeInGroup(snap *core.Snapshot, token string, members []string) bool {
	_, err := snap.ResolveNode(token, members)
	return err == nil
}

// guessGroup 省略分组时的推断：token 只属于一个分组就用它，否则回落到唯一分组。
func guessGroup(snap *core.Snapshot, token string, includeGlobal bool) (core.Group, error) {
	groups := snap.Groups(includeGlobal)
	if token != "" {
		owners := make([]core.Group, 0, 2)
		for _, group := range groups {
			if nodeInGroup(snap, token, group.Members) {
				owners = append(owners, group)
			}
		}
		switch len(owners) {
		case 1:
			return owners[0], nil
		case 0:
			// 不是节点、但看起来是分组（用户忘了给节点名）
			if group, err := snap.ResolveGroup(token); err == nil {
				return core.Group{}, usageErr("[%s] 需要节点名，或用 --index 指定序号", group.Name)
			}
		default:
			names := make([]string, 0, len(owners))
			for _, group := range owners {
				names = append(names, "["+group.Name+"]")
			}
			return core.Group{}, usageErr("节点 '%s' 属于多个分组，请指定分组：%s", token, strings.Join(names, ", "))
		}
	}

	if len(groups) == 0 {
		return core.Group{}, usageErr("控制器里没有分组")
	}
	if len(groups) == 1 {
		return groups[0], nil
	}
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, "["+group.Name+"]")
	}
	return core.Group{}, usageErr("有多个分组，请指定分组：%s", strings.Join(names, ", "))
}

// groupAndNode 给 switch / chain 用：分组可省略。
//
// 给了两个位置参数就是 <分组> <节点>；只给一个时，先看是不是精确的分组名，
// 不是就当节点名并自动推断分组。
func groupAndNode(snap *core.Snapshot, o *Options) (core.Group, string, error) {
	var first, second string
	if len(o.Args) > 0 {
		first = o.Args[0]
	}
	if len(o.Args) > 1 {
		second = o.Args[1]
	}
	if first != "" && second != "" {
		group, err := pickGroup(snap, first)
		return group, second, err
	}
	if first == "" {
		group, err := guessGroup(snap, "", false)
		return group, "", err
	}
	if group, ok := snap.FindGroup(first, true); ok {
		return *group, "", nil
	}
	group, err := guessGroup(snap, first, false)
	if err != nil {
		return core.Group{}, "", err
	}
	return group, first, nil
}

// --------------------------------------------------------------------------
// 命令
// --------------------------------------------------------------------------

func cmdStatus(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, cfg, err := buildSnapshot(ctx, o, core.FullRefresh)
	if err != nil {
		return err
	}
	defer conn.Close()

	groups := snap.Groups(true)
	visible := make([]core.Group, 0, len(groups))
	for _, group := range groups {
		if group.Name != core.GLOBAL {
			visible = append(visible, group)
		}
	}
	providers := snap.ProviderNames()
	sort.Strings(providers)

	var envFile, configFile *string
	if cfg.EnvFile != "" {
		envFile = &cfg.EnvFile
	}
	if cfg.ConfigPath != "" {
		configFile = &cfg.ConfigPath
	}
	payload := statusPayload{
		API:         cfg.API,
		EnvFile:     envFile,
		ConfigFile:  configFile,
		Auth:        cfg.Secret != "",
		Version:     snap.Version(),
		Mode:        snap.Mode(),
		Groups:      len(visible),
		Providers:   providers,
		TestURL:     cfg.TestURL,
		ToolVersion: Version,
		OK:          true,
	}
	envLabel := cfg.EnvFile
	if envLabel == "" {
		envLabel = ".env 未找到（用默认值/环境变量）"
	}
	authLabel := "未配置（不发送 Authorization）"
	if cfg.Secret != "" {
		authLabel = "已配置（发送 Authorization: Bearer …）"
	}
	configLabel := cfg.ConfigPath
	if configLabel == "" {
		configLabel = "未找到（端口默认 9090、不带密码）"
	}
	providerLabel := "-"
	if len(providers) > 0 {
		providerLabel = strings.Join(providers, ", ")
	}
	lines := []string{
		"控制器  " + cfg.API,
		"配置    " + envLabel,
		"mihomo配置 " + configLabel,
		"密码    " + authLabel,
		"版本    " + snap.Version(),
		"模式    " + snap.Mode(),
		fmt.Sprintf("分组    %d（含 GLOBAL 共 %d）", len(visible), len(groups)),
		"机场    " + providerLabel,
		"测速    " + cfg.TestURL,
	}
	return oo.emit(payload, lines)
}

func cmdGroups(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	groups := snap.Groups(o.ShowGlobal)
	if !oo.jsonOut {
		return oo.emit(nil, groupTable(groups, oo.style))
	}
	summary := make([]groupSummary, 0, len(groups))
	for _, group := range groups {
		chain := []string{group.Name}
		if group.HasNow {
			chain = snap.Chain(group.Name)
		}
		summary = append(summary, groupSummary{
			Name:    group.Name,
			Type:    group.Type,
			Now:     nullableString(group.Now, group.HasNow),
			Members: group.Count(),
			Chain:   chain,
		})
	}
	return oo.emit(groupsPayload{Groups: summary, OK: true}, nil)
}

func cmdGroup(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, core.WithProviders)
	if err != nil {
		return err
	}
	defer conn.Close()

	var group core.Group
	if len(o.Args) > 0 {
		group, err = pickGroup(snap, o.Args[0])
	} else {
		group, err = guessGroup(snap, "", false)
	}
	if err != nil {
		return err
	}
	chain := snap.Chain(group.Name)
	nodes := snap.NodeRows(group.Members, group.Now, group.HasNow)
	if oo.jsonOut {
		return oo.emit(groupPayload{
			Group: group.Name,
			Type:  group.Type,
			Now:   nullableString(group.Now, group.HasNow),
			Chain: chain,
			Nodes: nodes,
			OK:    true,
		}, nil)
	}
	tail := "-"
	if len(chain) > 1 {
		tail = strings.Join(chain[1:], " → ")
	}
	header := fmt.Sprintf("[%s] %s   当前: %s   共 %d 个", group.Type, group.Name, tail, group.Count())
	lines := append([]string{header, ""}, nodeTable(nodes, oo.style)...)
	return oo.emit(nil, lines)
}

func cmdNodes(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, core.WithProviders)
	if err != nil {
		return err
	}
	defer conn.Close()

	var groups []core.Group
	var nodes []string
	if o.Group != "" {
		group, err := pickGroup(snap, o.Group)
		if err != nil {
			return err
		}
		groups = []core.Group{group}
		nodes = group.Members
	} else {
		groups = snap.Groups(o.ShowGlobal)
		nodes = snap.AllNodes(o.ShowGlobal)
	}

	var groupName *string
	now, hasNow := "", false
	if len(groups) == 1 {
		groupName = &groups[0].Name
		now, hasNow = groups[0].Now, groups[0].HasNow
	}
	nodeRows := snap.NodeRows(nodes, now, hasNow)
	if oo.jsonOut {
		rows := make([]core.NodeRowWithGroups, 0, len(nodes))
		for _, node := range nodeRows {
			rows = append(rows, core.NodeRowWithGroups{NodeRow: node, Groups: snap.GroupsOf(node.Name, o.ShowGlobal)})
		}
		return oo.emit(nodesPayload{Group: groupName, Nodes: rows, OK: true}, nil)
	}

	lines := nodeTable(nodeRows, oo.style)
	if o.Group == "" {
		lines = append([]string{fmt.Sprintf("全部分组共 %d 个节点", len(nodes)), ""}, lines...)
	}
	return oo.emit(nil, lines)
}

func cmdCurrent(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	var groups []core.Group
	if o.Group != "" {
		group, err := pickGroup(snap, o.Group)
		if err != nil {
			return err
		}
		groups = []core.Group{group}
	} else {
		groups = snap.Groups(o.ShowGlobal)
	}

	var current []currentRow
	var rows [][]text.Cell
	if oo.jsonOut {
		current = make([]currentRow, 0, len(groups))
	} else {
		rows = make([][]text.Cell, 0, len(groups))
	}
	for _, group := range groups {
		chain := []string{group.Name}
		if group.HasNow {
			chain = snap.Chain(group.Name)
		}
		if oo.jsonOut {
			current = append(current, currentRow{
				Group: group.Name,
				Type:  group.Type,
				Now:   nullableString(group.Now, group.HasNow),
				Chain: chain,
			})
			continue
		}
		nowCell := text.Cell{Text: "-", Color: "dim"}
		if group.HasNow {
			nowCell = text.Cell{Text: group.Now, Color: "green"}
		}
		tail := "-"
		if len(chain) > 1 {
			tail = strings.Join(chain[1:], " → ")
		}
		rows = append(rows, []text.Cell{
			{Text: group.Name, Color: "bold"},
			nowCell,
			{Text: tail, Color: "dim"},
		})
	}
	if oo.jsonOut {
		return oo.emit(currentPayload{Current: current, OK: true}, nil)
	}
	lines := text.RenderTable(
		[]string{"分组", "当前节点", "选择链"},
		rows,
		[]string{"left", "left", "left"},
		oo.style, "  ",
	)
	return oo.emit(nil, lines)
}

func cmdChain(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	group, _, err := groupAndNode(snap, o)
	if err != nil {
		return err
	}
	chain := snap.Chain(group.Name)
	payload := chainPayload{
		Group: group.Name,
		Chain: chain,
		Leaf:  chain[len(chain)-1],
		OK:    true,
	}
	return oo.emit(payload, []string{group.Name + ": " + strings.Join(chain, " → ")})
}

func cmdSwitch(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	group, node, err := groupAndNode(snap, o)
	if err != nil {
		return err
	}

	if node != "" && o.IndexSet {
		return usageErr("节点名和 --index 只能给一个")
	}
	switch {
	case o.IndexSet:
		if o.Index < 0 || o.Index >= group.Count() {
			return notFoundErr("索引 %d 越界，%s 只有 %d 个节点", o.Index, group.Name, group.Count())
		}
		node = group.Members[o.Index]
	case node != "":
		node, err = pickNode(snap, node, group.Members)
		if err != nil {
			return err
		}
	default:
		return usageErr("需要节点名，或用 --index 指定序号")
	}

	previous := group.Now
	if !group.HasNow {
		previous = ""
	}
	payload := switchPayload{
		Group:    group.Name,
		Node:     node,
		Previous: nullableString(previous, group.HasNow),
		DryRun:   o.DryRun,
	}

	if o.DryRun {
		wouldChange := !group.IsSelected(node)
		payload.WouldChange = &wouldChange
		payload.OK = true
		return oo.emit(payload, []string{fmt.Sprintf("[%s] %s → %s（dry-run，未提交）", group.Name, orDash(group.Now, group.HasNow), node)})
	}

	if group.IsSelected(node) {
		payload.OK = true
		return oo.emit(payload, []string{fmt.Sprintf("[%s] 当前已经是 %s", group.Name, node)})
	}

	if _, _, err := snap.Select(ctx, group.Name, node); err != nil {
		return err
	}
	if err := snap.Refresh(ctx, 0); err != nil {
		return err
	}
	refreshed, ok := snap.FindGroup(group.Name, true)
	if !ok || !refreshed.HasNow {
		return apiErr("切换已提交，但控制器未返回当前节点")
	}
	if refreshed.Now != node {
		return apiErr("切换已提交，但控制器返回的当前节点是 '%s'", refreshed.Now)
	}
	if !refreshed.IsSelected(node) {
		return apiErr("切换已提交，但控制器未固定节点 '%s'", node)
	}
	payload.Changed = true
	payload.Now = &refreshed.Now
	payload.OK = true
	return oo.emit(payload, []string{fmt.Sprintf("已切换 [%s] %s → %s", group.Name, orDash(previous, group.HasNow), node)})
}

func cmdDelay(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, core.WithProviders)
	if err != nil {
		return err
	}
	defer conn.Close()

	node, err := pickNode(snap, o.Args[0], nil)
	if err != nil {
		return err
	}
	if !snap.IsTestable(node) {
		info := snap.Info(node)
		nodeType := info.Type
		if nodeType == "" {
			nodeType = "未知"
		}
		return unsupportedErr("%s 没有延迟可测（类型 %s）", node, nodeType)
	}

	oo.note("正在测试 " + node + " …")
	delay, err := snap.Measure(ctx, node, o.TimeoutMs)
	if err != nil {
		return err
	}
	var provider *string
	if owner, ok := snap.Owner(node); ok && owner != "" {
		provider = &owner
	}
	payload := delayPayload{
		Node:     node,
		Provider: provider,
		Delay:    delay,
		Timeout:  delay < 0,
		OK:       true,
	}
	label := strconv.Itoa(delay) + "ms"
	color := "green"
	if delay < 0 {
		label, color = "超时", "red"
	}
	return oo.emit(payload, []string{oo.style.Paint(node+": "+label, color)})
}

func cmdTest(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, core.WithProviders)
	if err != nil {
		return err
	}
	defer conn.Close()

	var group core.Group
	if o.Group != "" {
		group, err = pickGroup(snap, o.Group)
	} else {
		group, err = guessGroup(snap, "", o.ShowGlobal)
	}
	if err != nil {
		return err
	}
	if len(snap.TestableNodes(group.Members)) == 0 {
		return unsupportedErr("[%s] 没有可测节点", group.Name)
	}

	progress := newProgress(oo)
	result, err := snap.TestGroup(ctx, group,
		progress.progress, time.Duration(o.Wait*float64(time.Second)), o.TimeoutMs)
	progress.clear()
	if err != nil {
		return err
	}

	nodes := snap.NodeRows(group.Members, group.Now, group.HasNow)
	for i := range nodes {
		nodes[i].Delay = nil
		nodes[i].Timeout = false
		if delay, ok := result.Nodes[nodes[i].Name]; ok {
			nodes[i].Delay = &delay
			nodes[i].Timeout = delay < 0
		}
	}
	if oo.jsonOut {
		return oo.emit(testPayload{
			Group:     group.Name,
			Total:     result.Total(),
			Available: result.OK(),
			Failed:    result.Failed(),
			Partial:   result.Partial,
			Failures:  result.Failures,
			Nodes:     nodes,
			OK:        true,
		}, nil)
	}
	headline := fmt.Sprintf("[%s] %d/%d 可用", group.Name, result.OK(), result.Total())
	if result.Partial {
		headline += "（部分 provider 节点未取得本轮结果）"
	}
	lines := append([]string{headline, ""}, nodeTable(nodes, oo.style)...)
	if detail := result.SortedFailures(); len(detail) > 0 {
		lines = append(lines, "", "错误: "+strings.Join(detail, "; "))
	}
	return oo.emit(nil, lines)
}

func cmdList(ctx context.Context, o *Options) error {
	oo := newOut(o)
	snap, conn, _, err := buildSnapshot(ctx, o, core.FullRefresh)
	if err != nil {
		return err
	}
	defer conn.Close()

	groups := snap.Groups(o.ShowGlobal)
	if oo.jsonOut {
		summary := make([]listGroup, 0, len(groups))
		for _, group := range groups {
			summary = append(summary, listGroup{
				Name:  group.Name,
				Type:  group.Type,
				Now:   nullableString(group.Now, group.HasNow),
				Nodes: snap.NodeRows(group.Members, group.Now, group.HasNow),
			})
		}
		return oo.emit(listPayload{API: conn.Base(), Version: snap.Version(), Mode: snap.Mode(), Groups: summary, OK: true}, nil)
	}
	lines := []string{fmt.Sprintf("# %s  %s  mode=%s", conn.Base(), snap.Version(), snap.Mode()), ""}
	for _, group := range groups {
		lines = append(lines, fmt.Sprintf("[%s] %s  当前: %s", group.Type, group.Name, orDash(group.Now, group.HasNow)))
		table := nodeTable(snap.NodeRows(group.Members, group.Now, group.HasNow), oo.style)
		for _, line := range table[1:] {
			lines = append(lines, "   "+line)
		}
		lines = append(lines, "")
	}
	return oo.emit(nil, lines)
}

func cmdTUI(ctx context.Context, o *Options) error {
	if o.JSON {
		return usageErr("tui 命令不支持 --json")
	}
	snap, conn, _, err := buildSnapshot(ctx, o, core.FullRefresh)
	if err != nil {
		return err
	}
	defer conn.Close()
	return tui.Run(ctx, snap)
}

func nullableString(value string, ok bool) *string {
	if !ok {
		return nil
	}
	return &value
}

func orDash(value string, ok bool) string {
	if !ok || value == "" {
		return "-"
	}
	return value
}

// --------------------------------------------------------------------------
// 入口
// --------------------------------------------------------------------------

// Main 是命令行主函数，返回进程退出码。
func Main(argv []string) int {
	opts, err := Parse(argv)
	if err != nil {
		if opts.JSON {
			newOut(opts).emitError(err, ExitUsage)
		} else {
			fmt.Fprintln(os.Stderr, "错误: "+err.Error())
			fmt.Fprintf(os.Stderr, "用法: %s --help\n", progName())
		}
		return ExitUsage
	}
	if opts.Version {
		fmt.Println("arkjmp " + Version)
		return ExitOK
	}
	if opts.Help {
		fmt.Println(helpText(opts.Command))
		return ExitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = dispatch(ctx, opts)
	if err == nil {
		return ExitOK
	}

	oo := newOut(opts)
	if cmdErr, ok := errors.AsType[*Error](err); ok {
		oo.emitError(err, cmdErr.Code)
		if !opts.JSON {
			fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		}
		return cmdErr.Code
	}
	_, isAPIError := errors.AsType[*client.APIError](err)
	_, isTimeout := errors.AsType[*client.TimeoutError](err)
	if isAPIError || isTimeout {
		oo.emitError(err, ExitError)
		if !opts.JSON {
			fmt.Fprintln(os.Stderr, "连接失败: "+err.Error())
			if _, needsAuth := errors.AsType[*client.AuthRequiredError](err); needsAuth {
				fmt.Fprintln(os.Stderr, "提示: 控制器要求鉴权，但没读到密码")
				fmt.Fprintln(os.Stderr, "      在 .env 里设置 ARKJMP_SECRET，或让程序能找到 mihomo 的 config.json（会读它的 secret）")
			} else {
				fmt.Fprintln(os.Stderr, "提示: 确认 mihomo 已启动，且 external-controller / secret 配置正确")
				fmt.Fprintln(os.Stderr, "      端口默认 9090，可在 .env 设置 ARKJMP_HOST / ARKJMP_PORT / ARKJMP_SECRET")
			}
		}
		return ExitError
	}
	oo.emitError(err, ExitError)
	if !opts.JSON {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
	}
	return ExitError
}

func dispatch(ctx context.Context, o *Options) error {
	switch o.Command {
	case "status":
		return cmdStatus(ctx, o)
	case "groups":
		return cmdGroups(ctx, o)
	case "group":
		return cmdGroup(ctx, o)
	case "nodes":
		return cmdNodes(ctx, o)
	case "current":
		return cmdCurrent(ctx, o)
	case "chain":
		return cmdChain(ctx, o)
	case "switch":
		return cmdSwitch(ctx, o)
	case "delay":
		return cmdDelay(ctx, o)
	case "test":
		return cmdTest(ctx, o)
	case "list":
		return cmdList(ctx, o)
	case "tui":
		return cmdTUI(ctx, o)
	default:
		return usageErr("无法识别的命令 '%s'", o.Command)
	}
}

func progName() string {
	if len(os.Args) == 0 {
		return "arkjmp"
	}
	return filepath.Base(os.Args[0])
}
