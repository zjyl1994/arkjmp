// Package text 提供终端文本工具：中文按双宽计算、表格对齐、ANSI 上色。
package text

import (
	"strconv"
	"strings"

	"github.com/rivo/uniseg"
)

// colors 颜色名 -> ANSI SGR。
var colors = map[string]string{
	"dim":     "2",
	"bold":    "1",
	"red":     "31",
	"green":   "32",
	"yellow":  "33",
	"blue":    "34",
	"magenta": "35",
	"cyan":    "36",
	"gray":    "90",
}

// asciiWidth 全是可打印 ASCII 时宽度就等于字节数，省掉 uniseg 的分词开销。
func asciiWidth(s string) (int, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return 0, false
		}
	}
	return len(s), true
}

// DispWidth 返回字符串占几列（中文算 2，组合符算 0）。
//
// 用 uniseg 按字素簇计算，结果不随 LANG/LC_ALL 变化：
// 中文等宽字符算 2 列，省略号这类 East Asian Ambiguous 字符算 1 列。
func DispWidth(s string) int {
	if width, ok := asciiWidth(s); ok {
		return width
	}
	return uniseg.StringWidth(s)
}

// Fit 把 text 截断/补齐到 width 个终端列，避免撑破布局。
func Fit(content string, width int, pad bool) string {
	if width <= 0 {
		return ""
	}
	type cluster struct {
		text string
		cols int
	}
	// 快路径：足够窄就不必按字素簇再走一遍（绝大多数单元格都走这里）
	if used := DispWidth(content); used <= width {
		if pad && used < width {
			return content + strings.Repeat(" ", width-used)
		}
		return content
	}

	graphemes := uniseg.NewGraphemes(content)
	clusters := make([]cluster, 0, len(content))
	used := 0
	cut := false
	for graphemes.Next() {
		cols := graphemes.Width()
		if used+cols > width {
			cut = true
			break
		}
		clusters = append(clusters, cluster{text: graphemes.Str(), cols: cols})
		used += cols
	}
	if cut {
		for len(clusters) > 0 && used+1 > width {
			used -= clusters[len(clusters)-1].cols
			clusters = clusters[:len(clusters)-1]
		}
	}
	var b strings.Builder
	for _, item := range clusters {
		b.WriteString(item.text)
	}
	out := b.String()
	if cut {
		out += "…"
		used = DispWidth(out)
	}
	if pad && used < width {
		out += strings.Repeat(" ", width-used)
	}
	return out
}

// PadTo 返回 (左填充, 右填充)，让调用方可以把颜色码放在中间。
func PadTo(s string, width int, align string) (string, string) {
	gap := width - DispWidth(s)
	if gap < 0 {
		gap = 0
	}
	switch align {
	case "right":
		return strings.Repeat(" ", gap), ""
	case "center":
		left := gap / 2
		return strings.Repeat(" ", left), strings.Repeat(" ", gap-left)
	default:
		return "", strings.Repeat(" ", gap)
	}
}

// Style 颜色开关；关闭时 Paint 原样返回，方便重定向到文件。
type Style struct {
	Enabled bool
}

// Paint 给文本包一层颜色。
func (s Style) Paint(text, color string) string {
	if !s.Enabled || color == "" {
		return text
	}
	code, ok := colors[color]
	if !ok {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Cell 是表格里的一个单元格。
type Cell struct {
	Text  string
	Color string
}

// RenderTable 按显示宽度渲染表格，单元格可带颜色。返回未加换行的行列表。
func RenderTable(headers []string, rows [][]Cell, aligns []string, style Style, gap string) []string {
	if aligns == nil {
		aligns = make([]string, len(headers))
	}
	if gap == "" {
		gap = "  "
	}
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = DispWidth(header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && DispWidth(cell.Text) > widths[i] {
				widths[i] = DispWidth(cell.Text)
			}
		}
	}

	lines := make([]string, 0, len(rows)+1)
	head := make([]string, 0, len(headers))
	for i, header := range headers {
		left, right := PadTo(header, widths[i], alignAt(aligns, i))
		head = append(head, left+style.Paint(header, "dim")+right)
	}
	lines = append(lines, strings.TrimRight(strings.Join(head, gap), " "))

	for _, row := range rows {
		cols := make([]string, 0, len(row))
		for i, cell := range row {
			left, right := PadTo(cell.Text, widths[i], alignAt(aligns, i))
			cols = append(cols, left+style.Paint(cell.Text, cell.Color)+right)
		}
		lines = append(lines, strings.TrimRight(strings.Join(cols, gap), " "))
	}
	return lines
}

func alignAt(aligns []string, i int) string {
	if i < len(aligns) && aligns[i] != "" {
		return aligns[i]
	}
	return "left"
}

// LatencyCell 按延迟上色：无记录暗色，超时红色，越快越绿。
func LatencyCell(delay int, has bool) Cell {
	if !has {
		return Cell{Text: "-", Color: "dim"}
	}
	if delay < 0 {
		return Cell{Text: "超时", Color: "red"}
	}
	label := strconv.Itoa(delay) + "ms"
	switch {
	case delay < 300:
		return Cell{Text: label, Color: "green"}
	case delay < 700:
		return Cell{Text: label, Color: "yellow"}
	default:
		return Cell{Text: label, Color: "red"}
	}
}
