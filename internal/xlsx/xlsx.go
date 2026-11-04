// Package xlsx 是一个最小的 XLSX（Excel OOXML）写入器：零依赖，只用标准库
// 的 archive/zip 拼出 zip 包。
//
// 支持：多工作表、表头加粗+底色、列宽、冻结首行、数字/百分比/文本单元格、
// 自动筛选。够画统计报表，不追求公式、图表、合并单元格之外的高级特性
// （合并单元格用 MergeCell 支持）。
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 单元格样式编号，对应 styles.xml 里的 cellXfs 顺序
// 编号必须与 styles.xml 里 <cellXfs> 的顺序严格一致，错位会导致
// 数字被套上错误格式（例如 41.09 显示成 4108.80%），有测试专门盯这个。
const (
	StyleDefault = 0 // 普通文本
	StyleBold    = 1 // 加粗（标题）
	StyleHeader  = 2 // 表头：加粗 + 灰底 + 边框
	StyleInt     = 3 // 整数（千分位 #,##0）
	StyleFloat1  = 4 // 一位小数 0.0
	StyleFloat2  = 5 // 两位小数 0.00
	StylePercent = 6 // 百分比 0.00%（值按 0-1 传入）
	StyleMono    = 7 // 等宽（接口路径）
	StyleNote    = 8 // 灰色说明文字
)

// Cell 一个单元格
type Cell struct {
	Text  string  // 文本内容（Style 为数字类时忽略）
	Num   float64 // 数值内容
	IsNum bool    // 是否数值
	Style int     // 样式编号
}

// Text 文本单元格
func Text(s string) Cell { return Cell{Text: s} }

// Int 整数单元格
func Int(v int64) Cell { return Cell{Num: float64(v), IsNum: true, Style: StyleInt} }

// Float1 一位小数
func Float1(v float64) Cell { return Cell{Num: v, IsNum: true, Style: StyleFloat1} }

// Float2 两位小数
func Float2(v float64) Cell { return Cell{Num: v, IsNum: true, Style: StyleFloat2} }

// Percent 百分比（传 0-1）
func Percent(v float64) Cell { return Cell{Num: v, IsNum: true, Style: StylePercent} }

// Row 一行
type Row struct {
	Cells []Cell
	Style int // 整行默认样式（0 表示不设置）
}

// Sheet 一个工作表
type Sheet struct {
	Name       string
	Rows       []Row
	Widths     []float64 // 每列宽度（字符数），不足则用 12
	Freeze     int       // 冻结前 N 行，0 不冻结
	AutoFilter bool      // 是否给首行加自动筛选
	FitToWidth bool      // 打印时按宽度缩放到一页（A4 横向）
	merges     []string
}

// Workbook 工作簿
type Workbook struct {
	sheets []*Sheet
}

// New 新建工作簿
func New() *Workbook {
	return &Workbook{}
}

// AddSheet 追加工作表（名字里不能含 []:*?/\ 等字符，会自动替换）
func (w *Workbook) AddSheet(name string) *Sheet {
	s := &Sheet{Name: sanitizeSheetName(name)}
	w.sheets = append(w.sheets, s)
	return s
}

// Sheets 所有工作表
func (w *Workbook) Sheets() []*Sheet { return w.sheets }

// AddRow 追加一行
func (s *Sheet) AddRow(cells ...Cell) {
	s.Rows = append(s.Rows, Row{Cells: cells})
}

// Add 追加一行，可混用 Cell 与 Row（Row 用于带整行样式或空行）
func (s *Sheet) Add(items ...any) {
	var r Row
	for _, it := range items {
		switch v := it.(type) {
		case Cell:
			r.Cells = append(r.Cells, v)
		case Row:
			r = v
		case string:
			r.Cells = append(r.Cells, Cell{Text: v})
		}
	}
	s.Rows = append(s.Rows, r)
}

// AddCells 追加一行（Row 形式，可指定整行样式）
func (s *Sheet) AddCells(r Row) { s.Rows = append(s.Rows, r) }

// AddBlank 追加空行
func (s *Sheet) AddBlank() { s.Rows = append(s.Rows, Row{}) }

// AddRowStyle 追加一行并指定整行样式
func (s *Sheet) AddRowStyle(style int, cells ...Cell) {
	s.Rows = append(s.Rows, Row{Cells: cells, Style: style})
}

// MergeCell 合并单元格（如 "A1:D1"）
func (s *Sheet) MergeCell(ref string) {
	s.merges = append(s.merges, ref)
}

func sanitizeSheetName(n string) string {
	bad := `[]:*?/\`
	var b strings.Builder
	for _, r := range n {
		if strings.ContainsRune(bad, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	// Excel 限制：最多 31 个字符，不能为空
	r := []rune(out)
	if len(r) == 0 {
		return "Sheet"
	}
	if len(r) > 31 {
		return string(r[:31])
	}
	return out
}

// ---------------------------------------------------------------- 输出

// Bytes 生成 xlsx 文件内容
func (w *Workbook) Bytes() ([]byte, error) {
	if len(w.sheets) == 0 {
		w.AddSheet("Sheet1")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	files := []struct {
		name string
		body []byte
	}{
		{"[Content_Types].xml", w.contentTypes()},
		{"_rels/.rels", []byte(relsRoot)},
		{"xl/_rels/workbook.xml.rels", w.workbookRels()},
		{"xl/workbook.xml", w.workbookXML()},
		{"xl/styles.xml", []byte(stylesXML)},
	}
	for i, s := range w.sheets {
		files = append(files, struct {
			name string
			body []byte
		}{fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), w.sheetXML(s, i == 0)})
	}
	for _, f := range files {
		part, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (w *Workbook) contentTypes() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := range w.sheets {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
	}
	b.WriteString(`</Types>`)
	return []byte(b.String())
}

const relsRoot = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

func (w *Workbook) workbookRels() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := range w.sheets {
		fmt.Fprintf(&b,
			`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`,
			i+1, i+1)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, len(w.sheets)+1)
	b.WriteString(`</Relationships>`)
	return []byte(b.String())
}

func (w *Workbook) workbookXML() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" `)
	b.WriteString(`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, s := range w.sheets {
		fmt.Fprintf(&b, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, esc(s.Name), i+1, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	return []byte(b.String())
}

const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<numFmts count="4">
<numFmt numFmtId="164" formatCode="#,##0"/>
<numFmt numFmtId="165" formatCode="0.0"/>
<numFmt numFmtId="166" formatCode="0.00"/>
<numFmt numFmtId="167" formatCode="0.00%"/>
</numFmts>
<fonts count="5">
<font><sz val="11"/><name val="Calibri"/></font>
<font><b/><sz val="11"/><name val="Calibri"/></font>
<font><b/><sz val="11"/><color rgb="FF1F3864"/><name val="Calibri"/></font>
<font><sz val="11"/><name val="Consolas"/><color rgb="FF333333"/></font>
<font><sz val="10"/><name val="Calibri"/><color rgb="FF777777"/></font>
</fonts>
<fills count="4">
<fill><patternFill patternType="none"/></fill>
<fill><patternFill patternType="gray125"/></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFEDF2FA"/><bgColor indexed="64"/></patternFill></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFF5F7FB"/><bgColor indexed="64"/></patternFill></fill>
</fills>
<borders count="2">
<border><left/><right/><top/><bottom/><diagonal/></border>
<border><left style="thin"><color rgb="FFD0D7E5"/></left><right style="thin"><color rgb="FFD0D7E5"/></right><top style="thin"><color rgb="FFD0D7E5"/></top><bottom style="thin"><color rgb="FFD0D7E5"/></bottom><diagonal/></border>
</borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="9">
<xf numFmtId="0" fontId="0" fillId="0" borderId="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="1" fillId="0" borderId="1" applyFont="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="2" fillId="2" borderId="1" applyFont="1" applyFill="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" horizontal="center" wrapText="1"/></xf>
<xf numFmtId="164" fontId="0" fillId="0" borderId="1" applyNumberFormat="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" horizontal="right"/></xf>
<xf numFmtId="165" fontId="0" fillId="0" borderId="1" applyNumberFormat="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" horizontal="right"/></xf>
<xf numFmtId="166" fontId="0" fillId="0" borderId="1" applyNumberFormat="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" horizontal="right"/></xf>
<xf numFmtId="167" fontId="0" fillId="0" borderId="1" applyNumberFormat="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" horizontal="right"/></xf>
<xf numFmtId="0" fontId="3" fillId="0" borderId="1" applyFont="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="4" fillId="3" borderId="1" applyFont="1" applyFill="1" applyBorder="1" xfId="0" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
</cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`

func (w *Workbook) sheetXML(s *Sheet, first bool) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)

	if s.FitToWidth {
		b.WriteString(`<sheetPr><pageSetUpPr fitToPage="1"/></sheetPr>`)
	}

	// 冻结窗格（必须在 sheetData 之前）
	if s.Freeze > 0 {
		fmt.Fprintf(&b, `<sheetViews><sheetView workbookViewId="0" tabSelected="%d">`, b2i(first))
		fmt.Fprintf(&b, `<pane ySplit="%d" topLeftCell="A%d" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`,
			s.Freeze, s.Freeze+1)
	} else if first {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0" tabSelected="1"/></sheetViews>`)
	}

	// 列宽
	if len(s.Widths) > 0 {
		b.WriteString(`<cols>`)
		for i, wid := range s.Widths {
			if wid <= 0 {
				wid = 12
			}
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, i+1, i+1, fnum(wid))
		}
		b.WriteString(`</cols>`)
	}

	b.WriteString(`<sheetData>`)
	for ri, row := range s.Rows {
		fmt.Fprintf(&b, `<row r="%d">`, ri+1)
		for ci, c := range row.Cells {
			// 完全空的单元格不写，减小体积
			if !c.IsNum && c.Text == "" && c.Style == 0 {
				continue
			}
			style := c.Style
			if style == 0 && row.Style != 0 {
				style = row.Style
			}
			ref := colName(ci) + strconv.Itoa(ri+1)
			if c.IsNum {
				fmt.Fprintf(&b, `<c r="%s" s="%d"><v>%s</v></c>`, ref, style, fnum(c.Num))
				continue
			}
			// 内联字符串：省掉 sharedStrings 表
			fmt.Fprintf(&b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, esc(c.Text))
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData>`)

	if s.AutoFilter && len(s.Rows) > 0 {
		lastCol := 0
		for _, r := range s.Rows {
			if len(r.Cells) > lastCol {
				lastCol = len(r.Cells)
			}
		}
		if lastCol > 0 {
			fmt.Fprintf(&b, `<autoFilter ref="A1:%s%d"/>`, colName(lastCol-1), len(s.Rows))
		}
	}
	if len(s.merges) > 0 {
		fmt.Fprintf(&b, `<mergeCells count="%d">`, len(s.merges))
		for _, m := range s.merges {
			fmt.Fprintf(&b, `<mergeCell ref="%s"/>`, esc(m))
		}
		b.WriteString(`</mergeCells>`)
	}
	if s.FitToWidth {
		// 顺序必须按 OOXML 规定：pageMargins 在 pageSetup 之前，否则 Excel 会拒开
		b.WriteString(`<pageMargins left="0.4" right="0.4" top="0.5" bottom="0.5" header="0.3" footer="0.3"/>`)
		b.WriteString(`<pageSetup paperSize="9" orientation="landscape" fitToWidth="1" fitToHeight="0"/>`)
	}
	b.WriteString(`</worksheet>`)
	return []byte(b.String())
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// colName 0->A 25->Z 26->AA
func colName(i int) string {
	if i < 0 {
		i = 0
	}
	var b []byte
	for {
		b = append([]byte{byte('A' + i%26)}, b...)
		i = i/26 - 1
		if i < 0 {
			break
		}
	}
	return string(b)
}

func fnum(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func esc(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}
