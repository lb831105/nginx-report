package report

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
)

// pdfPlainText 把 PDF 内容流里的文本还原成 UTF-8：
// 中文是 UTF-16BE 十六进制（<4E2D> Tj），ASCII 是括号字符串（(abc) Tj）。
func pdfPlainText(data []byte) string {
	var b strings.Builder
	for _, m := range regexp.MustCompile(`<([0-9A-F]+)> Tj`).FindAllSubmatch(data, -1) {
		hexStr := string(m[1])
		for i := 0; i+4 <= len(hexStr); i += 4 {
			v, err := strconv.ParseUint(hexStr[i:i+4], 16, 32)
			if err != nil {
				continue
			}
			b.WriteRune(rune(v))
		}
		b.WriteString("\n")
	}
	ascii := regexp.MustCompile(`\(([A-Za-z0-9 .,:%/+\-]{1,80})\) Tj`)
	for _, m := range ascii.FindAllSubmatch(data, -1) {
		b.Write(m[1])
		b.WriteString("\n")
	}
	return b.String()
}

// sampleInput 造一份覆盖各种字段的小数据
func sampleInput(t *testing.T) Input {
	t.Helper()
	st := analyzer.NewStats()
	st.Agg.Total = 1000
	st.Agg.SizeTotal = 5 << 20
	st.Agg.ErrorCount = 60
	st.Agg.SlowCount = 12
	st.Agg.StatusCnt = map[string]int64{"200": 900, "404": 40, "502": 20}
	st.Agg.MethodCnt = map[string]int64{"GET": 950, "POST": 50}
	st.Agg.ClientCnt = map[string]int64{"10.0.0.1": 600, "10.0.0.2": 400}
	st.Agg.TimeBuckets[1] = 200
	st.Agg.TimeBuckets[30] = 700
	st.Agg.TimeBuckets[120] = 100
	st.Agg.SlotCnt[100] = 30
	st.Agg.HourCnt[9] = 400
	st.Agg.HourCnt[15] = 600
	st.Agg.UriStat = map[string]*analyzer.UriStat{
		"/api/a":            {Calls: 700, TimeS: 70, Errors: 5, Slows: 3},
		"/api/b":            {Calls: 300, TimeS: 300, Errors: 20, Slows: 9},
		"/pro/assets/x.svg": {Calls: 4000, TimeS: 4},
	}
	st.Agg.UpstreamStat = map[string]*analyzer.UpstreamStat{
		"10.0.0.9:80": {Calls: 990, TimeS: 99, TimeC: 990, Errors: 10},
	}
	dayAgg := analyzer.NewAgg()
	dayAgg.MergeFrom(&st.Agg)
	st.MergeDay("2026-10-26", dayAgg)
	return Input{
		Cfg:       config.Default(),
		Stats:     st,
		Days:      []analyzer.DaySummary{{Day: "2026-10-26", Total: 1000, PeakCnt: 30, Size: 5 << 20, AvgTime: 0.1}},
		StartDate: "2026-10-26",
		EndDate:   "2026-10-26",
	}
}

// PDF 必须包含全部章节标题，中文要能还原，静态资源不进接口榜
func TestPDFSections(t *testing.T) {
	data, err := PDF(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out := pdfPlainText(data)
	for _, want := range []string{"整体情况", "每日明细", "接口健康度", "口径说明"} {
		if !strings.Contains(out, want) {
			t.Errorf("PDF 缺少章节: %s", want)
		}
	}
	if !strings.Contains(string(data), "6574") {
		t.Error("中文未按 UTF-16BE 写入")
	}
	if strings.Contains(out, "assets/x.svg") {
		t.Error("静态资源不应出现在 PDF 接口 Top 里")
	}
	if !strings.Contains(out, "静态资源") {
		t.Error("PDF 应有静态资源汇总行")
	}
}

// 没有数据时三种格式都不能 panic
func TestOutputsEmptyData(t *testing.T) {
	in := Input{Cfg: config.Default(), Stats: analyzer.NewStats(), StartDate: "2026-11-02", EndDate: "2026-11-02"}
	if _, err := PDF(in); err != nil {
		t.Fatalf("PDF 空数据失败: %v", err)
	}
	if _, err := XLSX(in); err != nil {
		t.Fatalf("XLSX 空数据失败: %v", err)
	}
	if s := HTML(in); !strings.Contains(s, "Nginx") {
		t.Error("HTML 空数据异常")
	}
}

// XLSX 必须是合法 zip，包含约定工作表，且百分比是 0-1 小数
func TestXLSXSheets(t *testing.T) {
	data, err := XLSX(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		parts[f.Name] = string(b)
	}
	wb := parts["xl/workbook.xml"]
	for _, want := range []string{"概览", "每日明细", "接口Top", "上游健康度", "状态码", "客户端Top", "时段分布"} {
		if !strings.Contains(wb, want) {
			t.Errorf("缺少工作表: %s", want)
		}
	}
	s1 := parts["xl/worksheets/sheet1.xml"]
	if !strings.Contains(s1, "<v>1000</v>") {
		t.Error("概览缺少总访问量数值")
	}
	// 错误率 60/1000 = 6% → 单元格必须是 0.06 而不是 6
	if !strings.Contains(s1, "<v>0.06</v>") {
		t.Error("百分比单元格应是 0-1 小数（0.06），不能是 6")
	}
}
