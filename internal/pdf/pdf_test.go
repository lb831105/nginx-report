package pdf

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// PDF 结构必须自洽：xref 偏移能对上每个对象，页数与 Kids 一致
func TestBytesStructure(t *testing.T) {
	d := New()
	d.pages[0].Text(50, 700, 14, Bold, "Nginx 周报 Weekly Report")
	d.pages[0].Line(50, 690, 500, 690, 0.5, 0.8)
	d.pages[0].Rect(50, 600, 40, 30, 0.4, 0.5, 0.9)
	d.NewPage()
	d.pages[1].Text(50, 700, 11, Default, "第二页 page two")
	data := d.Bytes()

	if !bytes.HasPrefix(data, []byte("%PDF-1.4")) {
		t.Fatal("缺少 PDF 头")
	}
	if !bytes.HasSuffix(data, []byte("%%EOF\n")) {
		t.Fatal("缺少 EOF")
	}
	xi := bytes.LastIndex(data, []byte("\nxref\n"))
	if xi < 0 {
		t.Fatal("找不到 xref")
	}
	var startxref int
	if _, err := fmt.Sscanf(string(data[bytes.LastIndex(data, []byte("startxref")):]), "startxref\n%d", &startxref); err != nil {
		t.Fatalf("startxref 解析失败: %v", err)
	}
	if startxref != xi+1 {
		t.Fatalf("startxref=%d 与 xref 实际位置 %d 不一致", startxref, xi+1)
	}
	// lines: 0="" 1="xref" 2="0 N" 3..="偏移 00000 n"
	lines := strings.Split(string(data[xi:]), "\n")
	if len(lines) < 4 {
		t.Fatalf("xref 段落太短: %q", lines)
	}
	count, err := strconv.Atoi(strings.Fields(lines[2])[1])
	if err != nil {
		t.Fatalf("xref 头部解析失败: %q", lines[2])
	}
	for i := 4; i < 4+count-1; i++ { // lines[3] 是对象 0 的空闲条目

		f := strings.Fields(lines[i])
		if len(f) != 3 {
			t.Fatalf("xref 第 %d 行格式异常: %q", i, lines[i])
		}
		off, err := strconv.Atoi(f[0])
		if err != nil {
			t.Fatalf("xref 偏移非法: %q", f[0])
		}
		objNum := i - 3
		want := fmt.Sprintf("%d 0 obj", objNum)
		if off <= 0 || off >= len(data) || !bytes.HasPrefix(data[off:], []byte(want)) {
			t.Fatalf("对象 %d 的 xref 偏移 %d 指向错误内容", objNum, off)
		}
	}
	if n := bytes.Count(data, []byte("/Type /Page ")); n != 2 {
		t.Fatalf("页对象数 %d, want 2", n)
	}
	if m := regexp.MustCompile(`/Type /Pages /Count (\d+)`).FindSubmatch(data); m == nil || string(m[1]) != "2" {
		t.Fatalf("Pages Count 不对: %s", m)
	}
	if !bytes.Contains(data, []byte("/BaseFont /STSong-Light")) {
		t.Error("中文预定义字体缺失")
	}
	// 内容流长度声明必须和真实数据一致
	for _, m := range regexp.MustCompile(`/Length (\d+) >>\nstream\n`).FindAllSubmatch(data, -1) {
		n, _ := strconv.Atoi(string(m[1]))
		at := bytes.Index(data, m[0]) + len(m[0])
		if at+n > len(data) || !bytes.HasPrefix(data[at+n:], []byte("\nendstream")) {
			t.Fatalf("流长度 %d 与实际不符", n)
		}
	}
}

// 零宽矩形不输出；中英文混排都要切成正确的字体 run
func TestDrawEdgeCases(t *testing.T) {
	d := New()
	p := d.pages[0]
	p.Text(10, 10, 10, Default, "")
	p.Rect(10, 10, 0, 10, 0, 0, 0)
	p.Text(10, 20, 10, Default, "混排 mixed 文本 123")
	out := string(d.Bytes())
	if strings.Contains(out, " 0 10 re ") {
		t.Error("零宽矩形不应输出")
	}
	if !strings.Contains(out, "/F1") || !strings.Contains(out, "/F3") {
		t.Error("中英文混排应分别使用 Helvetica 与 STSong")
	}
	// 中文以 UTF-16BE 十六进制输出：混
	if !strings.Contains(out, "6DF7") {
		t.Error("中文未按 UTF-16BE 编码")
	}
	if d.ContentWidth() < 400 {
		t.Errorf("内容宽度异常: %v", d.ContentWidth())
	}
}

func TestMeasureAndWrap(t *testing.T) {
	if w := Measure("M", 10, Default); w < 8.3 || w > 8.34 {
		t.Errorf("Measure(M,10)=%v, want ≈8.33", w)
	}
	if w := Measure("中文", 10, Default); w != 20 {
		t.Errorf("Measure(中文,10)=%v, want 20", w)
	}
	if w := Measure("a中", 10, Default); w < 15.5 || w > 15.6 {
		t.Errorf("Measure(a中,10)=%v, want ≈15.56", w)
	}
	lines := Wrap("这是一段需要换行的中文文本，宽度很窄所以会折成很多行，用来验证按字断行。", 10, 60)
	if len(lines) < 3 {
		t.Fatalf("窄宽度应折成多行，实际 %d 行: %v", len(lines), lines)
	}
	for _, ln := range lines {
		if w := Measure(ln, 10, Default); w > 61 {
			t.Errorf("行宽 %v 超过限制: %q", w, ln)
		}
	}
	for _, ln := range Wrap("alpha beta gamma delta epsilon", 10, 60) {
		if strings.HasSuffix(ln, "gam") {
			t.Errorf("英文单词被切开: %q", ln)
		}
	}
	if n := len(Wrap("a\nb\nc", 10, 200)); n != 3 {
		t.Errorf("显式换行应得到 3 行，实际 %d", n)
	}
	if got := Truncate("abcdefghij", 10, 20); !strings.HasSuffix(got, "…") {
		t.Errorf("超宽应加省略号: %q", got)
	}
	if got := Truncate("abc", 10, 200); got != "abc" {
		t.Errorf("未超宽不应截断: %q", got)
	}
}

func TestNumRejectsNonFinite(t *testing.T) {
	if got := num(math.NaN()); got != "0" {
		t.Fatalf("num(NaN)=%q", got)
	}
	if got := num(math.Inf(1)); got != "0" {
		t.Fatalf("num(Inf)=%q", got)
	}
}
