package settings

import (
	"testing"
)

// BenchmarkProcessConfigPaths 量一下"遍历 /proc 找内核进程"的启动开销。
func BenchmarkProcessConfigPaths(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		processConfigPaths()
	}
}

// BenchmarkDefaultConfigCandidates 是完整候选列表（进程扫描 + 各常见路径）的开销。
func BenchmarkDefaultConfigCandidates(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		defaultConfigCandidates()
	}
}
