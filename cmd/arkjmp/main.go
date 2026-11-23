// Command arkjmp 是 mihomo 节点选择器：CLI + 交互式 TUI。
package main

import (
	"os"

	"github.com/zjyl1994/arkjmp/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
