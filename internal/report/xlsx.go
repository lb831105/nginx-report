package report

import (
	"fmt"

	"nginx-report/internal/xlsx"
)

// XLSX 生成多工作表 Excel 报告：概览、每日明细、接口、上游、状态码、客户端、
// 时段分布、问题清单。每个 sheet 首行冻结并带筛选，方便直接排序透视。
func XLSX(in Input) ([]byte, error) {
	s := buildSnapshot(in)
	wb := xlsx.New()

	overviewSheet(wb, s)
	siteSheet(wb, s)
	daySheet(wb, s)
	apiSheet(wb, s)
	upstreamSheet(wb, s)
	statusSheet(wb, s)
	clientSheet(wb, s)
	trendSheet(wb, s)
	issueSheet(wb, s)

	return wb.Bytes()
}

func header(cells ...xlsx.Cell) xlsx.Row {
	for i := range cells {
		cells[i].Style = xlsx.StyleHeader
	}
	return xlsx.Row{Cells: cells}
}

func row(cells ...xlsx.Cell) xlsx.Row { return xlsx.Row{Cells: cells} }

func txt(s string) xlsx.Cell { return xlsx.Cell{Text: s} }

func overviewSheet(wb *xlsx.Workbook, s *snapshot) {
	sh := wb.AddSheet("概览")
	sh.Widths = []float64{24, 22, 52}
	sh.FitToWidth = true
	sh.Freeze = 1

	period := s.StartDate + " ~ " + s.EndDate
	if s.StartDate == s.EndDate {
		period = s.StartDate
	}
	sh.AddRowStyle(xlsx.StyleBold, txt(s.Title), txt(period), txt(fmt.Sprintf("共 %d 天", s.DayCount)))
	sh.AddBlank()

	sh.Freeze = 3
	sh.AddCells(header(txt("指标"), txt("数值"), txt("说明")))
	rows := [][3]xlsx.Cell{
		{txt("总访问量"), xlsx.Int(s.Total), txt("区间内请求总数")},
		{txt("总流量"), xlsx.Float2(float64(s.SizeTotal) / (1 << 30)), txt("单位：GB")},
		{txt("平均 QPS"), xlsx.Float2(s.AvgQPS), txt("总请求 / 天数 / 86400")},
		{txt("峰值 QPS"), xlsx.Int(s.PeakQPS), txt("逐秒峰值，时刻 " + peakTimeStr(s))},
		{txt("错误数"), xlsx.Int(s.ErrorCount), txt("HTTP 状态码 >= 400")},
		{txt("错误率"), xlsx.Percent(s.ErrorRate / 100), txt("错误数 / 总请求")},
		{txt("慢请求数"), xlsx.Int(s.SlowCount), txt("响应耗时 > " + s.SlowLabel)},
		{txt("慢请求占比"), xlsx.Percent(s.SlowRate / 100), txt("慢请求 / 总请求")},
		{txt("平均响应"), xlsx.Int(int64(s.AvgMs)), txt("单位：毫秒")},
		{txt("P50"), xlsx.Int(int64(s.P50)), txt("单位：毫秒")},
		{txt("P90"), xlsx.Int(int64(s.P90)), txt("单位：毫秒")},
		{txt("P95"), xlsx.Int(int64(s.P95)), txt("单位：毫秒")},
		{txt("P99"), xlsx.Int(int64(s.P99)), txt("单位：毫秒")},
		{txt("独立客户端"), xlsx.Int(int64(s.ClientNum)), txt("去重来源 IP 数")},
		{txt("有耗时请求"), xlsx.Int(s.TimedCount), txt(fmt.Sprintf("占 %.1f%%，平均响应/分位数的分母", s.TimedShare))},
		{txt("健康度"), txt(fmt.Sprintf("%.0f 分 / %s / %s", s.Score, s.Grade, s.GradeText)), txt("100 分制，四项扣分")},
	}
	if s.WeekdayN > 0 {
		rows = append(rows, [3]xlsx.Cell{txt("工作日日均"), xlsx.Int(s.WeekdayAvg), txt("周一至周五平均")})
	}
	if s.WeekendN > 0 {
		rows = append(rows, [3]xlsx.Cell{txt("周末日均"), xlsx.Int(s.WeekendAvg), txt("周六、周日平均")})
	}
	if s.Prev != nil && s.Prev.Total > 0 {
		diff := float64(s.Total-s.Prev.Total) / float64(s.Prev.Total)
		rows = append(rows, [3]xlsx.Cell{txt("环比上周"), xlsx.Percent(diff), txt("上周 " + s.Prev.Start + " ~ " + s.Prev.End)})
	}
	for _, r := range rows {
		sh.AddCells(row(r[0], r[1], r[2]))
	}
	sh.AddBlank()
	sh.AddCells(xlsx.Row{Cells: []xlsx.Cell{txt("口径：日志按 @timestamp 归日；错误=状态码>=400；慢请求=响应耗时>" + s.SlowLabel + "；接口统计默认排除静态资源。")}, Style: xlsx.StyleNote})
}

func siteSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.SiteRows) == 0 {
		return
	}
	sh := wb.AddSheet("站点对比")
	sh.FitToWidth = true
	sh.Widths = []float64{10, 34, 14, 12, 12, 12, 10, 10}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("类型"), txt("站点"), txt("PV"), txt("流量GB"), txt("错误率"), txt("慢请求"), txt("P95ms"), txt("P99ms")))
	for _, r := range s.SiteRows {
		sh.Add(row(txt(siteKindLabel(r.Kind)), txt(r.Name), xlsx.Int(r.Total),
			xlsx.Float2(float64(r.Size)/(1<<30)), xlsx.Percent(r.ErrorRate/100),
			xlsx.Int(r.SlowCount), xlsx.Int(int64(r.P95)), xlsx.Int(int64(r.P99))))
	}
}

func daySheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.Days) == 0 {
		return
	}
	sh := wb.AddSheet("每日明细")
	sh.FitToWidth = true
	sh.Widths = []float64{12, 8, 14, 11, 14, 11, 11, 10, 11}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("日期"), txt("星期"), txt("访问量"), txt("峰值QPS"), txt("流量GB"),
		txt("平均响应ms"), txt("错误数"), txt("错误率"), txt("慢请求")))
	for _, d := range s.Days {
		sh.Add(row(
			txt(d.Day), txt(weekdayName(d.Day)), xlsx.Int(d.Total), xlsx.Int(d.PeakCnt),
			xlsx.Float2(float64(d.Size)/(1<<30)), xlsx.Int(int64(d.AvgTime*1000)),
			xlsx.Int(d.ErrorCount), xlsx.Percent(pct(d.ErrorCount, d.Total)/100), xlsx.Int(d.SlowCount),
		))
	}
	// 合计行
	var total, errs, slows, size int64
	for _, d := range s.Days {
		total += d.Total
		errs += d.ErrorCount
		slows += d.SlowCount
		size += d.Size
	}
	sh.AddRowStyle(xlsx.StyleBold,
		txt("合计"), txt(""), xlsx.Int(total), txt(""), xlsx.Float2(float64(size)/(1<<30)),
		txt(""), xlsx.Int(errs), xlsx.Percent(pct(errs, total)/100), xlsx.Int(slows))
}

func apiSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.TopAPIs) == 0 && s.Static.Calls == 0 {
		return
	}
	sh := wb.AddSheet("接口Top")
	sh.FitToWidth = true
	sh.Widths = []float64{52, 14, 10, 11, 11, 11, 10, 10}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("接口"), txt("调用次数"), txt("占比"), txt("平均耗时ms"),
		txt("慢请求"), txt("错误数"), txt("错误率"), txt("标记")))
	for _, u := range s.TopAPIs {
		mark := ""
		if u.AvgMs > 500 {
			mark = "慢接口"
		}
		if pct(u.Err, u.Calls) > 1 {
			if mark != "" {
				mark += "+"
			}
			mark += "高错误"
		}
		sh.Add(row(
			xlsx.Cell{Text: displayURI(u.URI), Style: xlsx.StyleMono}, xlsx.Int(u.Calls),
			xlsx.Percent(pct(u.Calls, s.Total)/100), xlsx.Int(int64(u.AvgMs)),
			xlsx.Int(u.Slow), xlsx.Int(u.Err), xlsx.Percent(pct(u.Err, u.Calls)/100), txt(mark),
		))
	}
	if s.Static.Calls > 0 {
		sh.AddRowStyle(xlsx.StyleNote,
			txt(fmt.Sprintf("(静态资源 %d 个：js/css/图片/字体)", s.Static.Files)), xlsx.Int(s.Static.Calls),
			xlsx.Percent(pct(s.Static.Calls, s.Total)/100), xlsx.Int(int64(s.Static.AvgMs)),
			xlsx.Int(s.Static.Slows), xlsx.Int(s.Static.Errs), xlsx.Percent(pct(s.Static.Errs, s.Static.Calls)/100), txt("已排除"))
	}

	if len(s.TopErr) > 0 {
		sh.AddBlank()
		sh.AddRowStyle(xlsx.StyleBold, txt("错误次数最多的接口 Top 10"))
		sh.AddCells(header(txt("接口"), txt("错误数"), txt("占全部错误"), txt("该接口错误率"), txt("调用次数")))
		for _, u := range s.TopErr {
			sh.Add(row(
				xlsx.Cell{Text: displayURI(u.URI), Style: xlsx.StyleMono}, xlsx.Int(u.Err),
				xlsx.Percent(pct(u.Err, s.ErrorCount)/100), xlsx.Percent(pct(u.Err, u.Calls)/100), xlsx.Int(u.Calls),
			))
		}
	}
	if len(s.SlowTop) > 0 {
		sh.AddBlank()
		sh.AddRowStyle(xlsx.StyleBold, txt("慢请求最多的接口 Top 10"))
		sh.AddCells(header(txt("接口"), txt("慢请求"), txt("调用次数"), txt("平均耗时ms"), txt("慢请求占比")))
		for _, u := range s.SlowTop {
			sh.Add(row(
				xlsx.Cell{Text: displayURI(u.URI), Style: xlsx.StyleMono}, xlsx.Int(u.Slow), xlsx.Int(u.Calls),
				xlsx.Int(int64(u.AvgMs)), xlsx.Percent(pct(u.Slow, u.Calls)/100),
			))
		}
	}
}

func upstreamSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.Ups) == 0 {
		return
	}
	sh := wb.AddSheet("上游健康度")
	sh.FitToWidth = true
	sh.Widths = []float64{26, 14, 10, 12, 12, 10}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("上游地址"), txt("调用次数"), txt("占比"), txt("平均耗时ms"), txt("上游错误"), txt("错误率")))
	for _, u := range s.Ups {
		sh.Add(row(
			xlsx.Cell{Text: displayURI(u.Addr), Style: xlsx.StyleMono}, xlsx.Int(u.Calls),
			xlsx.Percent(pct(u.Calls, s.Total)/100), xlsx.Int(int64(u.AvgMs)),
			xlsx.Int(u.Err), xlsx.Percent(pct(u.Err, u.Calls)/100),
		))
	}
	sh.AddBlank()
	sh.AddCells(xlsx.Row{Cells: []xlsx.Cell{txt("上游错误率基于 upstream_status，可区分「网关问题」与「后端问题」。")}, Style: xlsx.StyleNote})
}

func statusSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.StatusCodes) == 0 {
		return
	}
	sh := wb.AddSheet("状态码")
	sh.FitToWidth = true
	sh.Widths = []float64{14, 16, 10, 12}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("状态码"), txt("请求数"), txt("占比"), txt("分组")))
	for _, c := range s.StatusCodes {
		sh.AddCells(row(txt(c.Key), xlsx.Int(c.Value), xlsx.Percent(pct(c.Value, s.Total)/100), txt(statusClass(c.Key))))
	}
}

func clientSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.Clients) == 0 {
		return
	}
	sh := wb.AddSheet("客户端Top")
	sh.FitToWidth = true
	sh.Widths = []float64{22, 16, 10}
	sh.Freeze = 1
	sh.AutoFilter = true
	sh.AddCells(header(txt("来源 IP"), txt("请求数"), txt("占比")))
	for _, c := range s.Clients {
		key := c.Key
		if key == "" {
			key = "(未记录)"
		}
		sh.AddCells(row(txt(key), xlsx.Int(c.Value), xlsx.Percent(pct(c.Value, s.Total)/100)))
	}
}

func trendSheet(wb *xlsx.Workbook, s *snapshot) {
	sh := wb.AddSheet("时段分布")
	sh.FitToWidth = true
	sh.Widths = []float64{14, 14, 10}

	sh.AddRowStyle(xlsx.StyleBold, txt("24 小时流量分布"))
	sh.AddCells(header(txt("小时"), txt("请求数"), txt("占比")))
	for h := 0; h < 24; h++ {
		cnt := s.Hours[h]
		if cnt == 0 {
			continue
		}
		sh.AddCells(row(txt(fmt.Sprintf("%02d:00-%02d:00", h, h+1)), xlsx.Int(cnt), xlsx.Percent(pct(cnt, s.Total)/100)))
	}

	sh.AddBlank()
	sh.AddRowStyle(xlsx.StyleBold, txt("5 分钟粒度曲线"))
	sh.AddCells(header(txt("时段"), txt("请求数"), txt("折合 QPS")))
	for i, c := range s.Slots {
		if c == 0 {
			continue
		}
		sh.AddCells(row(txt(bucketLabel(i, 5)), xlsx.Int(c), xlsx.Float2(float64(c)/300)))
	}
}

func issueSheet(wb *xlsx.Workbook, s *snapshot) {
	if len(s.Issues) == 0 {
		return
	}
	sh := wb.AddSheet("问题清单")
	sh.FitToWidth = true
	sh.Widths = []float64{10, 40, 80}
	sh.Freeze = 1
	sh.AddCells(header(txt("级别"), txt("问题"), txt("说明")))
	for _, is := range s.Issues {
		level := "提示"
		switch is.Level {
		case "error":
			level = "严重"
		case "warning":
			level = "警告"
		}
		sh.AddCells(row(txt(level), txt(is.Title), txt(is.Desc)))
	}
}
