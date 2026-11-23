package settings

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// proxyProcessNames 是常见内核可执行文件名（小写，去掉 .exe）。
var proxyProcessNames = map[string]bool{
	"mihomo": true, "mihomo-core": true, "mihomo-alpha": true, "mihomo-meta": true,
	"clash": true, "clash-meta": true, "clash.meta": true, "clash-premium": true,
	"verge-mihomo": true, "clash-verge": true, "clash-nyanpasu": true,
}

// processConfigPaths 从正在运行的 mihomo/clash 进程命令行推断配置文件位置。
//
// 这是最准的一种自动发现：不管是 systemd、Docker、OpenWrt 还是 GUI 客户端拉起来的
// 内核，只要命令行里带 -f <file> 或 -d <dir> 就能找到。
func processConfigPaths() []string {
	if runtime.GOOS != "linux" || os.Getenv("ARKJMP_NO_PROC") != "" {
		return nil
	}

	// 不用 os.ReadDir：它会排序并给每个条目建 DirEntry，几百个进程下不值得
	dir, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil
	}

	// 复用同一个缓冲区读 cmdline：每个进程一次 open+read+close，不再单独分配
	buffer := make([]byte, 4096)
	var paths []string
	for _, entry := range entries {
		pid := entry.Name()
		if !isAllDigits(pid) {
			continue
		}
		base := "/proc/" + pid
		size, err := readSmallFile(base+"/cmdline", buffer)
		if err != nil || size == 0 {
			continue
		}
		raw := bytes.TrimSuffix(buffer[:size], []byte{0})
		if !looksLikeProxy(string(firstNulField(raw))) {
			continue // 绝大多数进程在这里就被排除了，不做任何字符串拆分
		}
		args := splitNul(raw)
		cwd, _ := os.Readlink(base + "/cwd") // root 进程可能读不到，忽略
		paths = append(paths, configsFromArgs(args, cwd)...)
	}
	return dedupePaths(paths)
}

// readSmallFile 用调用方给的缓冲区读一次小文件（/proc 里的文件 size 都是 0，ReadFile 会反复扩容）。
func readSmallFile(path string, buffer []byte) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.Read(buffer)
}

func firstNulField(raw []byte) []byte {
	if index := bytes.IndexByte(raw, 0); index >= 0 {
		return raw[:index]
	}
	return raw
}

// looksLikeProxy 判断可执行文件是不是代理内核。
//
// 只认已知名字或 `mihomo-*` / `clash-*`，排除名称含 select 的选择器：
// 误判也没什么损失（只看 -f/-d 参数），但没必要。
func looksLikeProxy(executable string) bool {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(executable), ".exe"))
	if base == "" || strings.Contains(base, "select") {
		return false
	}
	if proxyProcessNames[base] {
		return true
	}
	for _, prefix := range []string{"mihomo-", "clash-", "clash."} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// configsFromArgs 解析 mihomo 的命令行参数：
//
//	-f / --config <file>   显式配置文件
//	-d / --dir / -home <dir>  工作目录（配置默认是 <dir>/config.yaml）
//
// 相对路径依次按 -d 目录、进程 cwd 解析。
func configsFromArgs(args []string, cwd string) []string {
	var files, dirs []string
	for i := 1; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		next := func() string {
			if hasInline {
				return inline
			}
			if i+1 < len(args) {
				i++
				return strings.TrimSpace(args[i])
			}
			return ""
		}
		switch strings.TrimLeft(name, "-") {
		case "f", "config":
			if value := next(); value != "" {
				files = append(files, value)
			}
		case "d", "dir", "home", "directory":
			if value := next(); value != "" {
				dirs = append(dirs, value)
			}
		}
	}
	if len(files) == 0 && len(dirs) == 0 {
		return nil
	}

	var paths []string
	for _, file := range files {
		paths = append(paths, absoluteFrom(file, dirs, cwd))
	}
	for _, dir := range dirs {
		base := absoluteFrom(dir, nil, cwd)
		for _, name := range configFileNames {
			paths = append(paths, filepath.Join(base, name))
		}
	}
	return paths
}

// absoluteFrom 把相对路径先按工作目录（-d）再按进程 cwd 展开。
func absoluteFrom(path string, dirs []string, cwd string) string {
	if filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, path[2:])
			}
		}
		return path
	}
	if len(dirs) > 0 && dirs[len(dirs)-1] != "" {
		return filepath.Join(dirs[len(dirs)-1], path)
	}
	if cwd != "" {
		return filepath.Join(cwd, path)
	}
	return path
}

func splitNul(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
