// Package tui 是交互界面：人类手动挑节点用，逻辑全部复用 core.Snapshot。
//
// 基于 Bubble Tea（Elm 架构）：Update 处理按键/消息，View 只负责渲染当前状态。
// 整组测速在后台 goroutine 里跑，进度通过 channel 以 tea.Msg 的形式送回主循环，
// 所以测速期间界面依然跟手。
package tui

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/zjyl1994/arkjmp/internal/core"
	"github.com/zjyl1994/arkjmp/internal/text"
)

var (
	styleSel   = lipgloss.NewStyle().Background(lipgloss.Color("6")).Foreground(lipgloss.Color("0")).Bold(true)
	styleTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Reverse(true)
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Faint(true)
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
)

const helpLine = " 输入过滤  ←→ 分组  Ctrl+D 延迟  Ctrl+T 全组  Ctrl+O 排序  Ctrl+R 刷新  Esc 退出"

// progressMsg 是整组测速的进度。
type progressMsg struct {
	stage string
	done  int
	total int
	label string
}

// doneMsg 是整组测速的最终结果。
type doneMsg struct {
	group  string
	result *core.TestResult
	err    error
}

type actionMsg struct {
	message string
	kind    string
	refresh bool
	reorder bool
}

// model 是界面状态。
type model struct {
	ctx        context.Context
	snap       *core.Snapshot
	visible    []string
	groups     []core.Group
	showGlobal bool   // GLOBAL 是 mihomo 自动生成的，默认藏起来
	sortDelay  bool   // 按延迟升序显示当前分组的节点
	filter     string // 按名字快速过滤，直接敲字符即可
	gi         int
	cursor     map[string]int
	anchor     map[string]string // 每个分组里选中的节点名，列表重排后靠它找回原节点
	off        int
	width      int
	height     int
	msg        string
	kind       string

	busy   bool
	cancel context.CancelFunc
	events chan tea.Msg
}

// Run 进入交互界面，退出时返回 nil。
func Run(ctx context.Context, snap *core.Snapshot) error {
	m := newModel(ctx, snap)
	defer m.stopAction()
	program := tea.NewProgram(m, tea.WithContext(ctx), tea.WithoutSignalHandler())
	_, err := program.Run()
	return err
}

func newModel(ctx context.Context, snap *core.Snapshot) *model {
	m := &model{
		ctx:    ctx,
		snap:   snap,
		cursor: make(map[string]int),
		anchor: make(map[string]string),
		width:  80,
		height: 24,
		kind:   "info",
	}
	m.syncGroups()
	if len(m.groups) == 0 {
		m.setMsg("没读到任何分组，检查一下控制器地址/密码", "err")
	}
	return m
}

func (m *model) Init() tea.Cmd { return nil }

// ---------------- 数据 ----------------

func (m *model) setMsg(message, kind string) {
	m.msg, m.kind = message, kind
}

func (m *model) curGroup() (core.Group, bool) {
	if m.gi < 0 || m.gi >= len(m.groups) {
		return core.Group{}, false
	}
	return m.groups[m.gi], true
}

func (m *model) nodes() []string { return m.visible }

func (m *model) refreshNodes() {
	group, ok := m.curGroup()
	if !ok {
		m.visible = nil
		return
	}
	m.visible = m.filterNodes(group.Members)
	if m.sortDelay {
		m.visible = m.sortedByDelay(m.visible)
	}
}

// filterNodes 对名字做不区分大小写的包含匹配。
func (m *model) filterNodes(names []string) []string {
	if m.filter == "" {
		return names
	}
	query := strings.ToLower(m.filter)
	matched := make([]string, 0, len(names))
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), query) {
			matched = append(matched, name)
		}
	}
	return matched
}

// appendFilter 把敲下的字符接在过滤词后面。
func (m *model) appendFilter(text string) {
	m.applyFilter(m.filter + text)
}

func (m *model) backspaceFilter() {
	if m.filter == "" {
		return
	}
	// 按 rune 删，别把多字节字符切成两半
	runes := []rune(m.filter)
	m.applyFilter(string(runes[:len(runes)-1]))
}

func (m *model) clearFilter() {
	if m.filter == "" {
		return
	}
	m.applyFilter("")
}

// applyFilter 改完过滤词后把光标和滚动位置重置回列表顶部。
func (m *model) applyFilter(filter string) {
	m.filter = filter
	m.refreshNodes()
	m.setIndex(0) // 新过滤结果从第一个开始
	m.off = 0
	m.scrollIntoView()
	if m.filter == "" {
		m.setMsg("已清除过滤", "ok")
		return
	}
	m.setMsg(fmt.Sprintf("过滤 %q：%d 个匹配", m.filter, len(m.nodes())), "info")
}

// followAnchor 在列表顺序改变后把光标挪回原来选中的节点。
// 开了排序后延迟会随测速实时变化，只记下标的话高亮行会悄悄换人。
func (m *model) followAnchor() {
	group, ok := m.curGroup()
	if !ok {
		return
	}
	name := m.anchor[group.Name]
	if name == "" {
		return
	}
	for i, node := range m.nodes() {
		if node == name {
			m.setIndex(i)
			return
		}
	}
}

// sortedByDelay 按延迟升序排；没测过的排在测过的之后，超时排最后。
// 同档内保持原顺序，这样反复切 Ctrl+O 不会让列表乱跳。
func (m *model) sortedByDelay(names []string) []string {
	// 先把延迟快照下来，避免排序比较时反复加锁
	type entry struct {
		name  string
		delay int
		rank  int
	}
	entries := make([]entry, len(names))
	for i, name := range names {
		delay, ok := m.snap.Latency(name)
		entries[i] = entry{name: name, delay: delay, rank: delayRank(delay, ok)}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		if entries[i].rank == 0 {
			return entries[i].delay < entries[j].delay
		}
		return false
	})
	sorted := make([]string, len(entries))
	for i, item := range entries {
		sorted[i] = item.name
	}
	return sorted
}

// delayRank 把延迟分成三档：0 可用、1 未测、2 超时。
func delayRank(delay int, ok bool) int {
	switch {
	case !ok:
		return 1
	case delay < 0:
		return 2
	default:
		return 0
	}
}

func (m *model) index() int {
	group, ok := m.curGroup()
	if !ok {
		return 0
	}
	nodes := m.nodes()
	if len(nodes) == 0 {
		return 0
	}
	index := m.cursor[group.Name]
	if index < 0 {
		index = 0
	}
	if index > len(nodes)-1 {
		index = len(nodes) - 1
	}
	return index
}

func (m *model) setIndex(index int) {
	group, ok := m.curGroup()
	if !ok {
		return
	}
	nodes := m.nodes()
	if len(nodes) == 0 {
		m.cursor[group.Name] = 0
		delete(m.anchor, group.Name)
		return
	}
	if index < 0 {
		index = 0
	}
	if index > len(nodes)-1 {
		index = len(nodes) - 1
	}
	m.cursor[group.Name] = index
	m.anchor[group.Name] = nodes[index]
}

func (m *model) syncGroups() {
	oldName := ""
	if group, ok := m.curGroup(); ok {
		oldName = group.Name
	}
	m.groups = m.snap.Groups(m.showGlobal)
	if oldName != "" {
		for i, group := range m.groups {
			if group.Name == oldName {
				m.gi = i
				break
			}
		}
	}
	if m.gi >= len(m.groups) {
		m.gi = len(m.groups) - 1
	}
	if m.gi < 0 {
		m.gi = 0
	}

	for _, group := range m.groups {
		if _, seen := m.cursor[group.Name]; seen {
			continue
		}
		index := 0
		for i, name := range group.Members {
			if group.HasNow && name == group.Now {
				index = i
				break
			}
		}
		m.cursor[group.Name] = index
	}
	m.clampCursors()
	// 节点被删掉时锚点会失效，换回当前下标；还在就保留身份
	for _, group := range m.groups {
		name := m.anchor[group.Name]
		if name != "" && slices.Contains(group.Members, name) {
			continue
		}
		delete(m.anchor, group.Name)
		if index := m.cursor[group.Name]; index >= 0 && index < len(group.Members) {
			m.anchor[group.Name] = group.Members[index]
		}
	}
	m.refreshNodes()
	m.followAnchor()
	m.scrollIntoView()
}

func (m *model) clampCursors() {
	for _, group := range m.groups {
		n := len(group.Members)
		if n == 0 {
			m.cursor[group.Name] = 0
			continue
		}
		index := m.cursor[group.Name]
		if index < 0 {
			index = 0
		}
		if index > n-1 {
			index = n - 1
		}
		m.cursor[group.Name] = index
	}
}

func (m *model) viewHeight() int {
	height := m.height - 4 // 标题/分组头/状态/帮助各占一行
	if height < 0 {
		return 0
	}
	return height
}

func (m *model) scrollIntoView() {
	view := m.viewHeight()
	if view > 0 && m.off > len(m.nodes())-view {
		m.off = len(m.nodes()) - view
	}
	if m.off < 0 {
		m.off = 0
	}
	index := m.index()
	if index < m.off {
		m.off = index
	} else if view > 0 && index >= m.off+view {
		m.off = index - view + 1
	}
}

// nodeLine 渲染一行节点；选中行整行反色，颜色统一交给选中样式。

// ---------------- 更新 ----------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// 某些环境（script/CI 的 pty）会报 0x0，别把界面改成"终端太小"
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		m.scrollIntoView()
		return m, nil

	case actionMsg:
		m.stopAction()
		if msg.refresh {
			m.syncGroups()
		} else if msg.reorder {
			m.refreshNodes()
			m.followAnchor()
			m.scrollIntoView()
		}
		if msg.message != "" {
			m.setMsg(msg.message, msg.kind)
		}
		return m, nil

	case progressMsg:
		if msg.stage == "node" {
			m.setMsg("测速: "+msg.label, "info")
		} else {
			m.setMsg(fmt.Sprintf("测速进度 … %d/%d 个 provider 已处理（%s）", msg.done, msg.total, msg.label), "info")
		}
		if m.sortDelay {
			m.refreshNodes()
			m.followAnchor()
			m.scrollIntoView()
		}
		return m, waitForEvent(m.events)

	case doneMsg:
		m.stopAction()
		m.events = nil
		m.refreshNodes()
		m.followAnchor()
		m.scrollIntoView()
		if msg.err != nil {
			m.setMsg("测速失败: "+msg.err.Error(), "err")
			return m, nil
		}
		tail := ""
		if failed := msg.result.Failed(); failed > 0 {
			tail = fmt.Sprintf("，%d 个无可用结果", failed)
		}
		if msg.result.Partial {
			tail += "（部分 provider 节点未取得本轮结果）"
		}
		kind := "ok"
		if len(msg.result.Failures) > 0 {
			tail += "。错误: " + strings.Join(msg.result.SortedFailures(), "; ")
		}
		if msg.result.OK() == 0 {
			kind = "err"
		} else if msg.result.Partial || msg.result.Failed() > 0 {
			kind = "warn"
		}
		m.setMsg(fmt.Sprintf("测速完成 [%s]：%d/%d 可用%s", msg.group, msg.result.OK(), msg.result.Total(), tail), kind)
		// 自动分组的当前节点可能随测速变化。先显示结果，再同步分组状态。
		return m, m.startAction("", func(ctx context.Context, snap *core.Snapshot) actionMsg {
			return actionMsg{refresh: snap.Refresh(ctx, 0) == nil}
		})

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.stopAction()
		return m, tea.Quit
	case "esc":
		// 有过滤词时先清过滤，没过滤才是退出
		if m.filter != "" {
			m.clearFilter()
			return m, nil
		}
		m.stopAction()
		return m, tea.Quit
	case "up":
		m.move(-1)
	case "down":
		m.move(1)
	case "pgup":
		m.move(-10)
	case "pgdown":
		m.move(10)
	case "home":
		m.setIndex(0)
	case "end":
		m.setIndex(len(m.nodes()) - 1)
	case "left":
		m.switchGroup(-1)
	case "right", "tab":
		m.switchGroup(1)
	case "enter":
		return m, m.apply()
	case "backspace":
		m.backspaceFilter()
		return m, nil
	case "ctrl+r":
		return m, m.refresh()
	case "ctrl+d":
		return m, m.testOne()
	case "ctrl+t":
		return m.startTest()
	case "ctrl+o":
		m.toggleSort()
	case "ctrl+g":
		m.toggleGlobal()
	default:
		// 可打印字符直接进过滤词；Ctrl/Alt 组合的 Text 为空，不会误吞
		if msg.Text != "" {
			m.appendFilter(msg.Text)
			return m, nil
		}
	}
	m.scrollIntoView()
	return m, nil
}

func (m *model) move(delta int) { m.setIndex(m.index() + delta) }

func (m *model) switchGroup(delta int) {
	if len(m.groups) == 0 {
		return
	}
	m.gi = (m.gi + delta) % len(m.groups)
	if m.gi < 0 {
		m.gi += len(m.groups)
	}
	m.off = 0
	m.refreshNodes()
	if m.filter != "" {
		m.setIndex(0) // 新分组从过滤结果的第一个开始
	} else {
		m.followAnchor()
	}
	m.scrollIntoView()
}

// toggleSort 在原始顺序和按延迟升序之间切换，光标跟着原来选中的节点走。
func (m *model) toggleSort() {
	m.sortDelay = !m.sortDelay
	m.refreshNodes()
	m.followAnchor()
	m.scrollIntoView()
	if m.sortDelay {
		m.setMsg("已按延迟排序（再按 Ctrl+O 恢复原始顺序）", "ok")
	} else {
		m.setMsg("已恢复原始顺序", "ok")
	}
}

func (m *model) toggleGlobal() {
	m.showGlobal = !m.showGlobal
	m.syncGroups()
	if m.showGlobal {
		m.setMsg("显示 GLOBAL 分组（仅在全局模式下起作用）", "ok")
	} else {
		m.setMsg("已隐藏 GLOBAL 分组", "ok")
	}
}

func (m *model) apply() tea.Cmd {
	group, ok := m.curGroup()
	nodes := m.nodes()
	if !ok || len(nodes) == 0 {
		if m.filter != "" {
			m.setMsg("没有匹配的节点，Backspace 删字或 Esc 清除过滤", "err")
			return nil
		}
		m.setMsg("当前分组没有可选节点", "err")
		return nil
	}
	name := nodes[m.index()]
	if group.HasNow && name == group.Now && !m.busy {
		m.setMsg(fmt.Sprintf("[%s] 当前已经是 %s", group.Name, name), "ok")
		return nil
	}
	return m.startAction("正在切换 "+name+" …", func(ctx context.Context, snap *core.Snapshot) actionMsg {
		if _, _, err := snap.Select(ctx, group.Name, name); err != nil {
			return actionMsg{message: "切换失败: " + err.Error(), kind: "err"}
		}
		if err := snap.Refresh(ctx, 0); err != nil {
			return actionMsg{message: "切换已提交，读取当前节点失败: " + err.Error(), kind: "err"}
		}
		current, ok := snap.FindGroup(group.Name, true)
		if !ok || !current.HasNow {
			return actionMsg{message: "切换已提交，控制器未返回当前节点", kind: "err", refresh: true}
		}
		if current.Now != name {
			return actionMsg{message: fmt.Sprintf("切换已提交，当前节点为 %s", current.Now), kind: "err", refresh: true}
		}
		return actionMsg{message: fmt.Sprintf("已切换 [%s] → %s", group.Name, name), kind: "ok", refresh: true}
	})
}

func (m *model) refresh() tea.Cmd {
	return m.startAction("正在刷新 …", func(ctx context.Context, snap *core.Snapshot) actionMsg {
		if err := snap.Refresh(ctx, core.FullRefresh); err != nil {
			return actionMsg{message: "刷新失败: " + err.Error(), kind: "err"}
		}
		if err := snap.ProviderError(); err != nil {
			return actionMsg{message: "已刷新分组，provider 刷新失败: " + err.Error(), kind: "warn", refresh: true}
		}
		snap.ClearDelays()
		return actionMsg{message: "已刷新", kind: "ok", refresh: true}
	})
}

func (m *model) testOne() tea.Cmd {
	nodes := m.nodes()
	if len(nodes) == 0 {
		return nil
	}
	name := nodes[m.index()]
	if !m.snap.IsTestable(name) {
		m.setMsg(name+" 没有延迟可测", "err")
		return nil
	}
	return m.startAction("正在测试 "+name+" …", func(ctx context.Context, snap *core.Snapshot) actionMsg {
		delay, err := snap.Measure(ctx, name, 5000)
		if err != nil {
			return actionMsg{message: "测试失败: " + err.Error(), kind: "err"}
		}
		if delay < 0 {
			return actionMsg{message: name + ": 超时", kind: "err", reorder: true}
		}
		return actionMsg{message: fmt.Sprintf("%s: %dms", name, delay), kind: "ok", reorder: true}
	})
}

func (m *model) beginAction(message string) (context.Context, bool) {
	if m.busy {
		m.setMsg("操作进行中，请等待完成", "warn")
		return nil, false
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.busy, m.cancel = true, cancel
	if message != "" {
		m.setMsg(message, "info")
	}
	return ctx, true
}

func (m *model) startAction(message string, work func(context.Context, *core.Snapshot) actionMsg) tea.Cmd {
	ctx, ok := m.beginAction(message)
	if !ok {
		return nil
	}
	snap := m.snap
	return func() tea.Msg { return work(ctx, snap) }
}

// startTest 起一个后台测速 goroutine，并把第一条等待消息的 Cmd 交给 Bubble Tea。
func (m *model) startTest() (tea.Model, tea.Cmd) {
	group, ok := m.curGroup()
	if !ok {
		return m, nil
	}
	if len(m.snap.TestableNodes(group.Members)) == 0 {
		m.setMsg("当前分组没有可测节点", "err")
		return m, nil
	}

	ctx, ok := m.beginAction("正在测速 [" + group.Name + "] …")
	if !ok {
		return m, nil
	}
	events := make(chan tea.Msg, 64)
	m.events = events

	snap := m.snap
	go func() {
		defer close(events)
		result, err := snap.TestGroup(ctx, group, func(stage string, done, total int, label string) {
			select {
			case events <- progressMsg{stage: stage, done: done, total: total, label: label}:
			case <-ctx.Done():
			}
		}, 120*time.Second, 5000)
		select {
		case events <- doneMsg{group: group.Name, result: result, err: err}:
		case <-ctx.Done():
		}
	}()

	return m, waitForEvent(events)
}

func (m *model) stopAction() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.busy = false
}

// waitForEvent 把后台 goroutine 的下一条消息变成 tea.Msg。
func waitForEvent(events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if events == nil {
			return nil
		}
		msg, ok := <-events
		if !ok {
			return nil
		}
		return msg
	}
}

// ---------------- 渲染 ----------------

func (m *model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	return view
}

func (m *model) render() string {
	width, height := m.width, m.height
	if height < 6 || width < 30 {
		return styleErr.Render("终端太小了，放大一点再按任意键")
	}

	lines := make([]string, 0, height)
	title := fmt.Sprintf(" arkjmp  mihomo %s  %s  mode=%s", m.snap.Version(), m.snap.Client.Base(), m.snap.Mode())
	lines = append(lines, styleTitle.Render(text.Fit(title, width, true)))

	group, hasGroup := m.curGroup()
	header := " -"
	if hasGroup {
		header = " " + group.Name
		if len(m.groups) > 1 {
			header += fmt.Sprintf(" (%d/%d)", m.gi+1, len(m.groups))
		}
		if m.sortDelay {
			header += "   ⇅ 按延迟排序"
		}
		if m.filter != "" {
			header += "   过滤: " + m.filter
		}
		if group.HasNow {
			chain := m.snap.Chain(group.Name)
			if len(chain) > 1 {
				header += "   当前: " + strings.Join(chain[1:], " → ")
			} else {
				header += "   当前: " + group.Now
			}
		}
	}
	lines = append(lines, styleOK.Render(text.Fit(header, width, true)))

	nodes := m.nodes()
	index := m.index()
	viewHeight := m.viewHeight()
	latencyWidth := 8
	nameWidth := width - latencyWidth
	if nameWidth < 4 {
		nameWidth = 4
	}
	for row := 0; row < viewHeight; row++ {
		position := m.off + row
		if position >= len(nodes) {
			lines = append(lines, strings.Repeat(" ", width))
			continue
		}
		lines = append(lines, m.nodeLine(nodes[position], position == index, group, nameWidth, latencyWidth, width))
	}

	lines = append(lines, m.msgLine(width), styleTitle.Render(text.Fit(helpLine, width, true)))
	return strings.Join(lines, "\n")
}

func (m *model) nodeLine(name string, selected bool, group core.Group, nameWidth, latencyWidth, width int) string {
	info := m.snap.Info(name)
	mark := "○"
	switch {
	case group.HasNow && name == group.Now:
		mark = "●"
	case info.IsGroup():
		mark = "◎"
	}
	label := " " + mark + " " + name
	if info.IsGroup() {
		label += "  ↳ " + info.Now
	}
	label = text.Fit(label, nameWidth, true)

	delay, hasDelay := m.snap.Latency(name)
	latencyText, latencyStyle := "-", styleDim
	switch {
	case !hasDelay:
	case delay < 0:
		latencyText, latencyStyle = "超时", styleErr
	case delay < 300:
		latencyText, latencyStyle = strconv.Itoa(delay)+"ms", styleOK
	case delay < 700:
		latencyText, latencyStyle = strconv.Itoa(delay)+"ms", styleWarn
	default:
		latencyText, latencyStyle = strconv.Itoa(delay)+"ms", styleErr
	}
	padding := strings.Repeat(" ", max(0, latencyWidth-text.DispWidth(latencyText)))
	line := label + padding + latencyText

	if selected {
		return styleSel.Render(text.Fit(line, width, true))
	}
	return label + padding + latencyStyle.Render(latencyText)
}

func (m *model) msgLine(width int) string {
	line := text.Fit(" "+m.msg, width, true)
	switch m.kind {
	case "ok":
		return styleOK.Render(line)
	case "err":
		return styleErr.Render(line)
	case "warn":
		return styleWarn.Render(line)
	default:
		return styleDim.Render(line)
	}
}
