package text

import (
	"strings"
	"testing"
)

var benchLine = "[SS][HK] 示例节点-01"

func BenchmarkFitShort(b *testing.B) {
	for b.Loop() {
		Fit("HK-01", 40, true) // 快路径：直接返回
	}
}

func BenchmarkFitCJK(b *testing.B) {
	for b.Loop() {
		Fit(benchLine, 40, true) // 需要按字素簇算宽度
	}
}

func BenchmarkFitTruncate(b *testing.B) {
	for b.Loop() {
		Fit(benchLine, 12, true) // 需要截断 + 省略号
	}
}

func BenchmarkRenderTable(b *testing.B) {
	nodes := make([]string, 85)
	for i := range nodes {
		nodes[i] = benchLine
	}
	rows := make([][]Cell, 0, len(nodes))
	for _, name := range nodes {
		rows = append(rows, []Cell{{Text: "○"}, {Text: name}, {Text: "Shadowsocks"}, {Text: "airport"}, {Text: "123ms"}})
	}
	headers := []string{"", "节点", "类型", "机场", "延迟"}
	aligns := []string{"left", "left", "left", "left", "right"}

	b.ReportAllocs()
	for b.Loop() {
		RenderTable(headers, rows, aligns, Style{}, "  ")
	}
}

func BenchmarkDispWidthCJK(b *testing.B) {
	for b.Loop() {
		if DispWidth(benchLine) == 0 {
			b.Fatal("宽度不该是 0")
		}
	}
}

func BenchmarkDispWidthASCII(b *testing.B) {
	for b.Loop() {
		if DispWidth("Shadowsocks") == 0 {
			b.Fatal("宽度不该是 0")
		}
	}
}

func BenchmarkFitLongName(b *testing.B) {
	long := strings.Repeat("很长的节点名字", 10)
	for b.Loop() {
		Fit(long, 60, true)
	}
}
