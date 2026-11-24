# arkjmp

mihomo 节点选择器：CLI + 全屏 TUI。非交互命令支持 `--json`，方便脚本/Agent 调用。

## 安装

需要 Go ≥ 1.27：

```bash
go install github.com/zjyl1994/arkjmp/cmd/arkjmp@latest

# 或从源码
git clone https://github.com/zjyl1994/arkjmp.git
cd arkjmp
make install     # -> ~/.local/bin/arkjmp
```

控制器密码放在 `~/.config/arkjmp/.env`（模板见 `.env.example`）：

```bash
ARKJMP_PORT=9090
ARKJMP_SECRET=
```

## 默认行为

- 不带子命令时进入 TUI，打开第一个手动分组。多个手动分组显示为标签页。
- 支持切换 `Selector` 手动分组，手动组内的自动组仍可作为候选项。
- 控制器默认 `http://127.0.0.1:9090`；地址和密码依次从命令行参数、环境变量、`.env`、
  mihomo 的 `config.json`/`config.yaml` 获取，找不到密码就不带鉴权。
- `GLOBAL` 分组默认隐藏（`--show-global` 或 TUI 的 `Ctrl+G` 打开）。
- 测速默认用 `https://cp.cloudflare.com/generate_204`，单节点请求默认超时 3 秒，上限 5 秒。超过 5 秒的延迟视为超时。
  TUI 在后台并发测当前组内的节点。CLI 的 `test` 对机场节点使用 provider 批量 healthcheck，`--wait` 限制 provider 阶段的等待时间（默认 120 秒）。
  整组报告只统计本轮结果，缺少新样本的 provider 节点在 JSON 中显示为 `delay: null`。
- 结果输出到 stdout，进度/提示输出到 stderr；`--json` 时 stdout 只有一个 JSON 对象，
  退出码稳定（0 成功 / 1 连接或 API 错误 / 2 参数错误 / 3 目标不存在 / 4 操作不适用）。
- 分组只有一个时，`group` / `chain` / `switch` / `test` 可以省略分组名；
  节点名支持唯一的大小写不敏感前缀/子串匹配。

## 命令

| 命令 | 作用 |
| --- | --- |
| `status` | 控制器版本/模式/分组概况 |
| `groups` / `group [分组]` | 分组列表 / 分组内节点和延迟 |
| `nodes` / `current` / `chain` | 节点、当前选择、选择链 |
| `switch [分组] 节点` | 切换节点（`--dry-run` 只预览） |
| `delay 节点` / `test --group 分组` | 单节点测延迟 / 整组测速 |
| `tui` | 全屏界面（默认） |

## TUI 按键

| 键 | 作用 |
| --- | --- |
| 直接输入或粘贴 | 按名字过滤，支持 emoji |
| `↑`/`↓`、`PgUp`/`PgDn`、`Home`/`End` | 移动 / 翻页 / 首尾 |
| `Enter` | 切换为选中节点 |
| `Tab` / `←`/`→` | 切换分组标签页 |
| `Ctrl+D` / `Ctrl+T` | 测单节点 / 整组测速 |
| `Ctrl+O` / `Ctrl+R` | 按延迟排序 / 刷新 |
| `Ctrl+G` | 显示/隐藏 GLOBAL |
| `Esc` | 有过滤词时清除过滤，测速中取消测速，空闲时退出（`Ctrl+C` 始终退出） |

## 开发

```bash
make check       # gofmt + vet + test
make test-race
make lint        # staticcheck + deadcode
```

## License

[MIT](LICENSE)
