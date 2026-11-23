package text

import (
	"strings"
	"testing"
)

func TestDispWidthCountsCJKAsTwo(t *testing.T) {
	cases := map[string]int{
		"abc":      3,
		"中文":       4,
		"a中b":      4,
		"[XX][HK]": 8,
		"":         0,
	}
	for input, want := range cases {
		if got := DispWidth(input); got != want {
			t.Errorf("DispWidth(%q) = %d, 期望 %d", input, got, want)
		}
	}
}

func TestFitTruncatesWithEllipsis(t *testing.T) {
	if got := Fit("abcdef", 4, false); got != "abc…" {
		t.Fatalf("Fit = %q", got)
	}
	if got := Fit("abcdef", 3, false); got != "ab…" {
		t.Fatalf("Fit = %q", got)
	}
	if got := Fit("中文中文", 5, false); DispWidth(got) > 5 {
		t.Fatalf("Fit 超宽: %q (%d)", got, DispWidth(got))
	}
	if got := Fit("ab", 4, true); got != "ab  " {
		t.Fatalf("补齐 = %q", got)
	}
	if got := Fit("ab", 0, true); got != "" {
		t.Fatalf("宽度 0 = %q", got)
	}
}

func TestRenderTableAlignsByDisplayWidth(t *testing.T) {
	lines := RenderTable(
		[]string{"分组", "节点数"},
		[][]Cell{{{Text: "Manual"}, {Text: "85"}}, {{Text: "中文组"}, {Text: "7"}}},
		[]string{"left", "right"},
		Style{},
		"  ",
	)
	if len(lines) != 3 {
		t.Fatalf("行数 = %d", len(lines))
	}
	if lines[1] != "Manual      85" {
		t.Fatalf("第 1 行 = %q", lines[1])
	}
	// 列宽按显示宽度算：中文组=6，节点数字段右对齐到 6 列
	if want := "中文组" + strings.Repeat(" ", 2+5) + "7"; lines[2] != want {
		t.Fatalf("第 2 行 = %q, 期望 %q", lines[2], want)
	}
}

func TestStylePaintsOnlyWhenEnabled(t *testing.T) {
	plain := Style{}
	if got := plain.Paint("abc", "red"); got != "abc" {
		t.Fatalf("关闭时不该上色: %q", got)
	}
	color := Style{Enabled: true}
	if got := color.Paint("abc", "red"); got != "\x1b[31mabc\x1b[0m" {
		t.Fatalf("上色结果 = %q", got)
	}
	if got := color.Paint("abc", "nope"); got != "abc" {
		t.Fatalf("未知颜色 = %q", got)
	}
}

func TestLatencyCellColours(t *testing.T) {
	cases := []struct {
		delay int
		has   bool
		text  string
		color string
	}{
		{0, false, "-", "dim"},
		{-1, true, "超时", "red"},
		{100, true, "100ms", "green"},
		{500, true, "500ms", "yellow"},
		{900, true, "900ms", "red"},
	}
	for _, c := range cases {
		got := LatencyCell(c.delay, c.has)
		if got.Text != c.text || got.Color != c.color {
			t.Errorf("LatencyCell(%d,%v) = %+v", c.delay, c.has, got)
		}
	}
}

func TestPadTo(t *testing.T) {
	left, right := PadTo("中", 4, "left")
	if left != "" || right != strings.Repeat(" ", 2) {
		t.Fatalf("左对齐 = %q,%q", left, right)
	}
	left, right = PadTo("中", 4, "right")
	if left != "  " || right != "" {
		t.Fatalf("右对齐 = %q,%q", left, right)
	}
}
