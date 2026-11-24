package cli

import "strings"

const examplesAndExitCodes = `示例:
  arkjmp status                            # 控制器概况
  arkjmp groups --json                     # 分组列表（JSON）
  arkjmp group Manual --json               # 分组内节点 + 延迟
  arkjmp switch Manual 'HK-01'             # 切换节点
  arkjmp switch 'HK-01'                    # 只有一个分组时省掉分组名
  arkjmp switch Manual --index 3           # 按下标切换
  arkjmp test --group Manual               # 整组测速
  arkjmp delay 'HK-01' --json              # 单节点测速
  arkjmp current --json                    # 当前选择
  arkjmp tui                               # 交互界面（默认命令）

退出码: 0 成功 | 1 连接/API 错误 | 2 参数错误 | 3 目标不存在 | 4 操作不适用`

const agentWorkflow = `Agent 操作流程
  从查询结果复制完整的分组名和节点名，替换示例里的 Manual 和 HK-01。

  arkjmp groups --json
    从 groups[].name 获取手动分组名。
  arkjmp group 'Manual' --json
    从 nodes[].name 获取节点名。
  arkjmp switch 'Manual' 'HK-01' --dry-run --json
    核对返回的 group 和 node。would_change 为 false 时已选中目标。
  arkjmp switch 'Manual' 'HK-01' --json
    确认预览目标后提交切换。
  arkjmp current --group 'Manual' --json
    核对 current[].group 和 current[].now，确认当前选择。

  解析 stdout 的 JSON，并检查退出码和 ok。命令失败时查看 error.message。`

const globalOptions = `options:
  -h, --help           显示帮助并退出
  --api API            external-controller 完整地址，如 http://127.0.0.1:9090（默认取 .env）
  --secret SECRET      控制器密码（默认取 .env 的 ARKJMP_SECRET）
  --env-file ENV_FILE  指定 .env 路径（默认找当前目录/配置目录/.配置目录的 .env）
  --test-url TEST_URL  测速用 URL（默认 https://cp.cloudflare.com/generate_204）
  --timeout TIMEOUT    单次请求超时秒数（默认 5）
  --config CONFIG      从 mihomo config.json 读取地址/密码（.env 缺项时兜底）
  --json               输出 JSON（Agent 用）
  -q, --quiet          不打印进度/提示
  --no-color           关闭 ANSI 颜色
  --version            显示版本并退出
  --list               等同于 list 命令（兼容旧版）`

func rootHelp(prog string) string {
	return "usage: " + prog + ` [-h] [--api API] [--secret SECRET] [--env-file ENV_FILE]
                     [--test-url TEST_URL] [--timeout TIMEOUT] [--config CONFIG]
                     [--json] [-q] [--no-color] [--version] [--list]
                     <命令> ...

mihomo 节点选择器：可脚本化/可被 Agent 驱动的 CLI + 交互式 TUI

positional arguments:
  <命令>
    tui (ui)            交互式界面（默认）
    status              控制器版本/模式/分组概况
    list                打印所有分组和节点（兼容旧版 --list）
    groups              列出手动分组（Selector）
    group               查看单个分组的节点（只有一个分组时可省略）
    nodes               列出节点（默认合并所有分组）
    current             查看当前选中节点
    chain               查看分组的选择链（只有一个分组时可省略）
    switch (select, use)
                        切换节点
    delay (ping)        测单个节点延迟
    test                整组测速（机场走 provider 批量 healthcheck）

` + globalOptions + "\n\n" + agentWorkflow + "\n\n" + examplesAndExitCodes + "\n"
}

// commandHelp 是单个子命令的 --help 输出。
func commandHelp(prog, command string) string {
	type spec struct {
		usage string
		desc  string
		args  [][2]string
		flags [][2]string
	}
	specs := map[string]spec{
		"status": {
			usage: "[options]",
			desc:  "控制器版本/模式/分组概况",
		},
		"groups": {
			usage: "[options]",
			desc:  "列出分组（GLOBAL 默认隐藏）",
			flags: [][2]string{{"--show-global", "显示 GLOBAL 分组"}},
		},
		"list": {
			usage: "[options]",
			desc:  "打印所有分组和节点（兼容旧版 --list）",
			flags: [][2]string{{"--show-global", "显示 GLOBAL 分组"}},
		},
		"group": {
			usage: "[options] [分组]",
			desc:  "查看单个分组的节点（只有一个分组时可省略）",
			args:  [][2]string{{"分组", "分组名（只有一个分组时可省略）"}},
		},
		"nodes": {
			usage: "[options]",
			desc:  "列出节点（默认合并所有分组）",
			flags: [][2]string{
				{"--group", "只看这个分组"},
				{"--show-global", "包含 GLOBAL 分组的成员"},
			},
		},
		"current": {
			usage: "[options]",
			desc:  "查看当前选中节点",
			flags: [][2]string{
				{"--group", "只看这个分组"},
				{"--show-global", "包含 GLOBAL 分组"},
			},
		},
		"chain": {
			usage: "[options] [分组]",
			desc:  "查看分组的选择链（分组套分组时是 A → B → 节点）",
			args:  [][2]string{{"分组", "分组名（只有一个分组时可省略）"}},
		},
		"switch": {
			usage: "[options] [分组] [节点]",
			desc:  "切换节点（别名 select / use）",
			args: [][2]string{
				{"分组", "分组名（只有一个分组时可省略，此时第一个位置参数就是节点名）"},
				{"节点", "节点名（支持唯一前缀）"},
			},
			flags: [][2]string{
				{"--index", "用序号指定节点（0 起）"},
				{"--dry-run", "只看会切到哪，不提交"},
			},
		},
		"delay": {
			usage: "[options] 节点",
			desc:  "测单个节点延迟（别名 ping）",
			args:  [][2]string{{"节点", "节点名（支持唯一前缀）"}},
			flags: [][2]string{{"--timeout-ms", "测速超时毫秒（默认 3000，上限 5000）"}},
		},
		"test": {
			usage: "[options]",
			desc:  "整组测速（机场走 provider 批量 healthcheck）",
			flags: [][2]string{
				{"--group", "分组名（只有一个分组时可省略）"},
				{"--wait", "等待 provider 结果的最长秒数（默认 120）"},
				{"--show-global", "分组选择时包含 GLOBAL"},
			},
		},
		"tui": {
			usage: "",
			desc:  "交互式界面（别名 ui）",
		},
	}

	item, ok := specs[command]
	if !ok {
		return rootHelp(prog)
	}
	var b strings.Builder
	b.WriteString("usage: " + prog + " " + command)
	if item.usage != "" {
		b.WriteString(" " + item.usage)
	}
	b.WriteString("\n\n" + item.desc + "\n")
	if len(item.args) > 0 {
		b.WriteString("\npositional arguments:\n")
		for _, arg := range item.args {
			b.WriteString("  " + padRight(arg[0], 20) + arg[1] + "\n")
		}
	}
	b.WriteString("\n" + globalOptions + "\n")
	if len(item.flags) > 0 {
		b.WriteString("\n子命令选项:\n")
		for _, flag := range item.flags {
			b.WriteString("  " + padRight(flag[0], 20) + flag[1] + "\n")
		}
	}
	if command == "switch" {
		b.WriteString("\n" + agentWorkflow + "\n")
	}
	b.WriteString("\n" + examplesAndExitCodes + "\n")
	return b.String()
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-len(s))
}

// helpText 按命令返回帮助文本（命令为空时是总帮助）。
func helpText(command string) string {
	prog := progName()
	if command == "" {
		return rootHelp(prog)
	}
	return commandHelp(prog, command)
}
