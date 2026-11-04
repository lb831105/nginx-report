// Package pdf 是一个「够用就好」的 PDF 写入器：零依赖，只用标准库。
//
// 支持范围正好覆盖统计报告的需要：
//   - A4 页面、自动分页
//   - 文本（ASCII 用 Helvetica / Helvetica-Bold，中文用 PDF 预定义的
//     STSong-Light + UniGB-UCS2-H，不嵌字体，文件只有几十 KB）
//   - 自动换行、左右对齐、灰度
//   - 画线、填充矩形（画表格和柱状图）
//
// 中文用的是 PDF 规范里的预定义 CJK 字体（Adobe-GB1），Acrobat、Chrome/Edge、
// WPS、macOS 预览都自带；极小概率遇到没装 CJK 字体支持的阅读器会显示方块，
// 这种环境下请改用 HTML 或 Excel 输出。
package pdf

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Style 文本样式
type Style struct {
	Bold bool    // 粗体（中文没有粗体，仍用常规体）
	Gray float64 // 灰度 0=黑 1=白
}

var (
	// Default 正文样式
	Default = Style{Gray: 0.15}
	// Bold 强调样式
	Bold = Style{Bold: true, Gray: 0.05}
	// Muted 次要信息
	Muted = Style{Gray: 0.45}
)

// Page 一页
type Page struct {
	buf bytes.Buffer
}

// Doc 一个 PDF 文档
type Doc struct {
	W, H   float64
	Margin float64
	pages  []*Page
}

// A4 尺寸与默认页边距（单位 pt）
const (
	A4Width  = 595.28
	A4Height = 841.89
)

// New 新建文档，自动创建第一页
func New() *Doc {
	d := &Doc{W: A4Width, H: A4Height, Margin: 42}
	d.NewPage()
	return d
}

// NewPage 换页
func NewPage(d *Doc) *Page { return d.NewPage() }

// NewPage 换页
func (d *Doc) NewPage() *Page {
	p := &Page{}
	d.pages = append(d.pages, p)
	return p
}

// Current 当前页
func (d *Doc) Current() *Page {
	if len(d.pages) == 0 {
		d.NewPage()
	}
	return d.pages[len(d.pages)-1]
}

// Pages 所有页（用于统一加页脚）
func (d *Doc) Pages() []*Page { return d.pages }

// PageCount 页数
func (d *Doc) PageCount() int { return len(d.pages) }

// ContentWidth 可用内容宽度
func (d *Doc) ContentWidth() float64 { return d.W - 2*d.Margin }

// ---------------------------------------------------------------- 文本宽度

// helveticaWidths 是 Helvetica 标准字宽的千分之一 em（来自 AFM 度量），
// ASCII 32..126 逐个列出，其它字符按 556 处理。
var helveticaWidths = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
}

func isASCII(r rune) bool { return r >= 32 && r < 127 }

func runeWidth(r rune, size float64) float64 {
	if isASCII(r) {
		return float64(helveticaWidths[r-32]) / 1000 * size
	}
	if r == '\t' {
		return 2 * size
	}
	return size // 中日韩字符按全宽
}

// Measure 单行文本宽度（pt）
func Measure(s string, size float64, st Style) float64 {
	var w float64
	for _, r := range s {
		w += runeWidth(r, size)
	}
	return w
}

// ---------------------------------------------------------------- 绘制

// Text 在 (x, y) 处绘制单行文本，y 是基线
func (p *Page) Text(x, y, size float64, st Style, s string) {
	if s == "" {
		return
	}
	gray := st.Gray
	if gray < 0 {
		gray = 0
	}
	if gray > 1 {
		gray = 1
	}
	cur := x
	for _, run := range splitRuns(s) {
		w := Measure(run, size, st)
		font := "F1"
		if run[0] < 127 {
			if st.Bold {
				font = "F2"
			}
			fmt.Fprintf(&p.buf, "BT /%s %s Tf %s %s %s rg 1 0 0 1 %s %s Tm (%s) Tj ET\n",
				font, num(size), num(gray), num(gray), num(gray), num(cur), num(y), escapeASCII(run))
		} else {
			font = "F3" // 中文一律走 STSong-Light
			fmt.Fprintf(&p.buf, "BT /%s %s Tf %s %s %s rg 1 0 0 1 %s %s Tm <%s> Tj ET\n",
				font, num(size), num(gray), num(gray), num(gray), num(cur), num(y), hexUTF16(run))
		}
		cur += w
	}
}

// TextRight 右对齐绘制（元素右边缘固定在 xRight）
func (p *Page) TextRight(xRight, y, size float64, st Style, s string) {
	p.Text(xRight-Measure(s, size, st), y, size, st, s)
}

// TextCenter 居中绘制
func (p *Page) TextCenter(xCenter, y, size float64, st Style, s string) {
	p.Text(xCenter-Measure(s, size, st)/2, y, size, st, s)
}

// Line 画一条线
func (p *Page) Line(x1, y1, x2, y2, width, gray float64) {
	fmt.Fprintf(&p.buf, "%s %s %s RG %s w %s %s m %s %s l S\n",
		num(gray), num(gray), num(gray), num(width), num(x1), num(y1), num(x2), num(y2))
}

// Rect 填充矩形（r/g/b 取 0..1）
func (p *Page) Rect(x, y, w, h, r, g, b float64) {
	if w <= 0 || h <= 0 {
		return
	}
	fmt.Fprintf(&p.buf, "%s %s %s rg %s %s %s %s re f\n", num(r), num(g), num(b), num(x), num(y), num(w), num(h))
}

// Gray 填充灰度矩形
func (p *Page) Gray(x, y, w, h, gray float64) {
	p.Rect(x, y, w, h, gray, gray, gray)
}

// ---------------------------------------------------------------- 换行

// Wrap 按宽度换行：遇到换行符硬折行，中文按字断行，英文按空格断行
func Wrap(s string, size, maxW float64) []string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, wrapLine(para, size, maxW)...)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func wrapLine(s string, size, maxW float64) []string {
	if s == "" {
		return []string{""}
	}
	if maxW <= 0 {
		return []string{s}
	}
	var lines []string
	var cur strings.Builder
	var curW float64
	lastSpace := -1 // cur 中最后一个可断点的字节位置（空格后）
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
		curW = 0
		lastSpace = -1
	}
	for _, r := range s {
		w := runeWidth(r, size)
		if curW+w > maxW && cur.Len() > 0 {
			// 优先在最近的空格处断行，避免英文单词被切开
			if r < 127 && lastSpace > 0 {
				text := cur.String()
				head, tail := text[:lastSpace], text[lastSpace:]
				lines = append(lines, strings.TrimRight(head, " "))
				cur.Reset()
				cur.WriteString(strings.TrimLeft(tail, " "))
				curW = Measure(cur.String(), size, Default)
				lastSpace = -1
			} else {
				flush()
			}
		}
		cur.WriteRune(r)
		curW += w
		if r == ' ' {
			lastSpace = cur.Len()
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) == 0 {
		lines = append(lines, "")
	}
	return lines
}

// Truncate 按宽度截断，超出部分用省略号
func Truncate(s string, size, maxW float64) string {
	if Measure(s, size, Default) <= maxW {
		return s
	}
	ell := "…"
	ellW := Measure(ell, size, Default)
	var b strings.Builder
	var w float64
	for _, r := range s {
		rw := runeWidth(r, size)
		if w+rw+ellW > maxW {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + ell
}

// ---------------------------------------------------------------- 编码细节

// splitRuns 把字符串切成「纯 ASCII」与「非 ASCII」的连续片段，
// 这样英文可以用 Helvetica、中文用 STSong，宽度计算和显示都准确。
func splitRuns(s string) []string {
	var runs []string
	var cur []rune
	curASCII := false
	for i, r := range s {
		a := isASCII(r)
		if i == 0 {
			curASCII = a
			cur = append(cur, r)
			continue
		}
		if a != curASCII {
			runs = append(runs, string(cur))
			cur = cur[:0]
			curASCII = a
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		runs = append(runs, string(cur))
	}
	return runs
}

func escapeASCII(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', ')', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\r', '\n':
			b.WriteByte(' ')
		default:
			if c < 32 {
				b.WriteByte(' ')
			} else if c > 126 {
				b.WriteByte('?')
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// hexUTF16 非 ASCII 文本按 UTF-16BE 十六进制输出（UniGB-UCS2-H 要求）
func hexUTF16(s string) string {
	units := utf16.Encode([]rune(s))
	var b strings.Builder
	b.Grow(len(units) * 4)
	for _, u := range units {
		fmt.Fprintf(&b, "%04X", u)
	}
	return b.String()
}

func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = 0
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ---------------------------------------------------------------- 输出

// Bytes 生成 PDF 文件内容
func (d *Doc) Bytes() []byte {
	if len(d.pages) == 0 {
		d.NewPage()
	}
	// 对象编号：1=Catalog 2=Pages 3=F1 4=F2 5=Type0 6=CIDFont 7=FontDescriptor
	// 之后每页两个对象：8+2i=Page，9+2i=Contents
	const fixed = 7
	objs := make([][]byte, 0, fixed+2*len(d.pages))
	objs = append(objs,
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		nil, // Pages，稍后填
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /STSong-Light /Encoding /UniGB-UCS2-H /DescendantFonts [6 0 R] >>"),
		[]byte("<< /Type /Font /Subtype /CIDFontType0 /BaseFont /STSong-Light "+
			"/CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 2 >> "+
			"/FontDescriptor 7 0 R /DW 1000 /W [1 95 500] >>"),
		// 不嵌字体时也必须给 FontDescriptor：部分阅读器（MuPDF/Poppler）缺它会报错不显示
		[]byte("<< /Type /FontDescriptor /FontName /STSong-Light /Flags 6 "+
			"/FontBBox [-25 -254 1000 880] /ItalicAngle 0 /Ascent 880 /Descent -120 "+
			"/CapHeight 700 /StemV 80 >>"),
	)

	var kids strings.Builder
	for i := range d.pages {
		pageNum := 8 + 2*i
		contentNum := pageNum + 1
		fmt.Fprintf(&kids, "%d 0 R ", pageNum)
		page := fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] "+
			"/Resources << /Font << /F1 3 0 R /F2 4 0 R /F3 5 0 R >> >> /Contents %d 0 R >>",
			num(d.W), num(d.H), contentNum)
		objs = append(objs, []byte(page))
		stream := d.pages[i].buf.Bytes()
		head := fmt.Sprintf("<< /Length %d >>\nstream\n", len(stream))
		body := make([]byte, 0, len(head)+len(stream)+11)
		body = append(body, head...)
		body = append(body, stream...)
		body = append(body, "\nendstream"...)
		objs = append(objs, body)
	}
	objs[1] = []byte(fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(d.pages), strings.TrimSpace(kids.String())))

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(o)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(objs)+1)
	out.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}
