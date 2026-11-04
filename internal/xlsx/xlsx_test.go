package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"math"
	"regexp"
	"strings"
	"testing"
)

func readZip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("不是合法 zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f.Name, err)
		}
		out[f.Name] = string(b)
	}
	return out
}

// 必须包含 OOXML 规定的所有必需部件，且每个部件都是合法 XML
func TestBytesParts(t *testing.T) {
	wb := New()
	s1 := wb.AddSheet("概览")
	s1.AddRow(Text("指标"), Text("数值"))
	s1.AddRow(Text("总请求"), Int(12345))
	s2 := wb.AddSheet("接口[Top]/1") // 含非法字符，应被替换
	s2.AddRow(Text("/api/x"), Percent(0.0123))

	data, err := wb.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	parts := readZip(t, data)
	for _, need := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/styles.xml",
		"xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml",
	} {
		if _, ok := parts[need]; !ok {
			t.Fatalf("缺少部件 %s（实际: %v）", need, keys(parts))
		}
	}
	for name, body := range parts {
		if strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels") {
			dec := xml.NewDecoder(strings.NewReader(body))
			for {
				_, err := dec.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("%s 不是合法 XML: %v", name, err)
				}
			}
		}
	}
	// 非法字符被替换
	if strings.Contains(parts["xl/workbook.xml"], "接口[Top]") {
		t.Error("工作表名里的非法字符应被替换")
	}
	if !strings.Contains(parts["xl/workbook.xml"], "接口_Top_") {
		t.Errorf("工作表名未按预期清理: %s", regexp.MustCompile(`name="[^"]*"`).FindString(parts["xl/workbook.xml"]))
	}
	// 数值单元格与百分比
	if !strings.Contains(parts["xl/worksheets/sheet1.xml"], "<v>12345</v>") {
		t.Error("整数单元格缺失")
	}
	if !strings.Contains(parts["xl/worksheets/sheet2.xml"], "<v>0.0123</v>") {
		t.Error("百分比单元格缺失")
	}
}

// 样式号必须都在 cellXfs 范围内（否则 Excel 报「文件已损坏」）
func TestStylesInRange(t *testing.T) {
	wb := New()
	s := wb.AddSheet("t")
	s.AddRowStyle(StyleBold, Text("a"), Float1(1.5))
	s.AddRowStyle(StyleNote, Text("note"))
	s.AddRow(Int(1), Float2(2.25), Percent(0.5), Cell{Text: "mono", Style: StyleMono})
	data, _ := wb.Bytes()
	parts := readZip(t, data)

	cellXfs := regexp.MustCompile(`<cellXfs count="(\d+)">`).FindStringSubmatch(parts["xl/styles.xml"])
	if cellXfs == nil {
		t.Fatal("styles.xml 缺 cellXfs")
	}
	n := 0
	_, _ = fmtSscan(cellXfs[1], &n)
	for _, m := range regexp.MustCompile(`<c r="[A-Z]+\d+" s="(\d+)"`).FindAllStringSubmatch(parts["xl/worksheets/sheet1.xml"], -1) {
		var idx int
		_, _ = fmtSscan(m[1], &idx)
		if idx >= n {
			t.Fatalf("单元格样式号 %d 超出 cellXfs 范围 %d", idx, n)
		}
	}
}

// XML 特殊字符必须转义，否则 Excel 打不开
func TestEscape(t *testing.T) {
	wb := New()
	s := wb.AddSheet("x")
	s.AddRow(Text(`/api/a?x=1&y=<2>`), Text(`"quote" & <tag>`))
	data, _ := wb.Bytes()
	parts := readZip(t, data)
	sheet := parts["xl/worksheets/sheet1.xml"]
	if strings.Contains(sheet, "<tag>") {
		t.Error("尖括号未转义")
	}
	if !strings.Contains(sheet, "&amp;") || !strings.Contains(sheet, "&lt;") {
		t.Error("& 或 < 未转义")
	}
}

// 列名换算与冻结窗格、自动筛选
func TestColNameAndFeatures(t *testing.T) {
	cases := map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"}
	for i, want := range cases {
		if got := colName(i); got != want {
			t.Errorf("colName(%d)=%q, want %q", i, got, want)
		}
	}
	wb := New()
	s := wb.AddSheet("s")
	s.Widths = []float64{30, 12, 12}
	s.Freeze = 1
	s.AutoFilter = true
	s.AddRow(Text("h1"), Text("h2"), Text("h3"))
	s.AddRow(Text("a"), Int(1), Int(2))
	s.MergeCell("A3:C3")
	data, _ := wb.Bytes()
	parts := readZip(t, data)
	sheet := parts["xl/worksheets/sheet1.xml"]
	for _, want := range []string{
		`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`,
		`<autoFilter ref="A1:C2"/>`,
		`<mergeCells count="1"><mergeCell ref="A3:C3"/></mergeCells>`,
		`<col min="1" max="1" width="30" customWidth="1"/>`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("缺少 %q", want)
		}
	}
	// mergeCells 必须在 sheetData 之后
	if strings.Index(sheet, "mergeCells") < strings.Index(sheet, "</sheetData>") {
		t.Error("mergeCells 位置不符合 OOXML 顺序要求")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func fmtSscan(s string, v *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, nil
		}
		n = n*10 + int(r-'0')
	}
	*v = n
	return 1, nil
}

// 样式编号必须和 styles.xml 里 cellXfs 的顺序一致：错位会让 41.09 显示成 4108.80%
func TestStyleIndexMapping(t *testing.T) {
	wants := map[int]string{
		StyleDefault: "0",   // numFmtId 0
		StyleBold:    "0",   // 只是加粗
		StyleHeader:  "0",   // 只是底色
		StyleInt:     "164", // #,##0
		StyleFloat1:  "165", // 0.0
		StyleFloat2:  "166", // 0.00
		StylePercent: "167", // 0.00%
		StyleMono:    "0",
		StyleNote:    "0",
	}
	body := stylesXML[strings.Index(stylesXML, "<cellXfs"):]
	body = body[:strings.Index(body, "</cellXfs>")]
	xf := regexp.MustCompile(`<xf numFmtId="(\d+)"`).FindAllStringSubmatch(body, -1)
	if len(xf) != 9 {
		t.Fatalf("cellXfs 条目数 %d, want 9", len(xf))
	}
	for style, wantFmt := range wants {
		if xf[style][1] != wantFmt {
			t.Errorf("样式 %d 的 numFmtId = %s, want %s（编号与 cellXfs 顺序错位）", style, xf[style][1], wantFmt)
		}
	}
	// 百分比与两位小数必须是两个不同的格式，且百分比带 %
	if !strings.Contains(stylesXML, `<numFmt numFmtId="167" formatCode="0.00%"/>`) {
		t.Error("缺少百分比格式定义")
	}
	if !strings.Contains(stylesXML, `<numFmt numFmtId="166" formatCode="0.00"/>`) {
		t.Error("缺少两位小数格式定义")
	}
}

// 打印设置要放在 worksheet 末尾（在 mergeCells 之后）
func TestFitToWidth(t *testing.T) {
	wb := New()
	s := wb.AddSheet("s")
	s.FitToWidth = true
	s.Add(Text("a"))
	data, _ := wb.Bytes()
	sheet := readZip(t, data)["xl/worksheets/sheet1.xml"]
	if !strings.Contains(sheet, `<pageSetUpPr fitToPage="1"/>`) {
		t.Error("缺少 fitToPage")
	}
	if !strings.Contains(sheet, `orientation="landscape"`) {
		t.Error("缺少横向打印设置")
	}
	if strings.Index(sheet, "pageSetup") < strings.Index(sheet, "sheetData") {
		t.Error("pageSetup 必须在 sheetData 之后")
	}
	// OOXML 规定 pageMargins 在 pageSetup 之前
	if strings.Index(sheet, "pageMargins") > strings.Index(sheet, "pageSetup") {
		t.Error("pageMargins 必须排在 pageSetup 之前")
	}
	if !strings.Contains(sheet, "<sheetPr><pageSetUpPr") || strings.Index(sheet, "sheetPr") > strings.Index(sheet, "sheetData") {
		t.Error("sheetPr 必须在最前面")
	}
}

func TestFnumRejectsNonFinite(t *testing.T) {
	if got := fnum(math.NaN()); got != "0" {
		t.Fatalf("fnum(NaN)=%q", got)
	}
	if got := fnum(math.Inf(1)); got != "0" {
		t.Fatalf("fnum(Inf)=%q", got)
	}
}
