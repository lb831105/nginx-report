package report

import (
	"fmt"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/pdf"
)

// PDF 生成适合打印/发邮件的 A4 报告。
// 版式与 HTML 报告保持一致的信息层次，但因为 PDF 是线性的，
// 把「按受众分区」改成了「先结论、再明细」的阅读顺序。
func PDF(in Input) ([]byte, error) {
	s := buildSnapshot(in)
	d := pdf.New()
	r := &pdfRenderer{d: d, s: s}
	r.render()
	return d.Bytes(), nil
}

type pdfRenderer struct {
	d         *pdf.Doc
	s         *snapshot
	y         float64
	pageNo    int
	firstPage bool
}

var (
	colAccent = [3]float64{0.25, 0.34, 0.60} // 主色（靛蓝）
	colGreen  = [3]float64{0.26, 0.63, 0.28}
	colAmber  = [3]float64{0.98, 0.55, 0.09}
	colRed    = [3]float64{0.90, 0.22, 0.21}
	colGray   = [3]float64{0.62, 0.66, 0.72}
)

func (r *pdfRenderer) page() *pdf.Page { return r.d.Current() }

func (r *pdfRenderer) newPage() {
	r.d.NewPage()
	r.pageNo++
	r.y = r.d.H - r.d.Margin
}

// ensure 保证还有 h 的空间，不够就换页
func (r *pdfRenderer) ensure(h float64) {
	if r.y-h < r.d.Margin+18 {
		r.newPage()
	}
}

func (r *pdfRenderer) render() {
	r.pageNo = 1
	r.y = r.d.H - r.d.Margin
	r.cover()
	r.summaryCards()
	r.siteSection()
	r.issuesSection()
	r.trendSection()
	r.dayTable()
	r.statusSection()
	r.upstreamSection()
	r.clientSection()
	r.apiSection()
	r.timeDistSection()
	r.footnote()
	r.footers()
}

// ---------------------------------------------------------------- 封面/标题

func (r *pdfRenderer) cover() {
	d := r.d
	p := r.page()
	p.Rect(0, d.H-96, d.W, 96, colAccent[0], colAccent[1], colAccent[2])
	p.Text(d.Margin, d.H-46, 20, pdf.Style{Bold: true, Gray: 0}, r.title())
	period := fmt.Sprintf("%s ~ %s", r.s.StartDate, r.s.EndDate)
	if r.s.StartDate == r.s.EndDate {
		period = r.s.StartDate
	}
	p.Text(d.Margin, d.H-70, 11, pdf.Style{Gray: 0.92}, period+"　·　共 "+itoa(int(r.s.DayCount))+" 天")
	// 健康度徽章
	if r.s.Grade != "" {
		grade := fmt.Sprintf("健康度 %s（%.0f 分/%s）", r.s.Grade, r.s.Score, r.s.GradeText)
		p.TextRight(d.W-d.Margin, d.H-46, 13, pdf.Style{Bold: true, Gray: 0}, grade)
	}
	r.y = d.H - 96 - 22
}

func (r *pdfRenderer) summaryCards() {
	s := r.s
	d := r.d
	r.sectionTitle("一、整体情况")
	// 两列布局的关键指标
	cards := [][2]string{
		{"总访问量", timesStr(s.Total)},
		{"总流量", humanBytes(float64(s.SizeTotal))},
		{"平均 QPS", fmt.Sprintf("%.2f", s.AvgQPS)},
		{"峰值 QPS", fmt.Sprintf("%d（%s）", s.PeakQPS, peakTimeStr(s))},
		{"错误数", fmt.Sprintf("%s（%.2f%%）", humanNum(s.ErrorCount), s.ErrorRate)},
		{"慢请求(>" + s.SlowLabel + ")", fmt.Sprintf("%s（%.2f%%）", humanNum(s.SlowCount), s.SlowRate)},
		{"平均响应", fmt.Sprintf("%.0f ms", s.AvgMs)},
		{"P95 / P99", fmt.Sprintf("%.0f / %.0f ms", s.P95, s.P99)},
		{"独立客户端", humanNum(int64(s.ClientNum)) + " 个"},
		{"接口数量", humanNum(int64(len(s.TopAPIs))) + " 个（Top 榜）"},
	}
	if s.WeekdayN > 0 && s.WeekendN > 0 {
		cards = append(cards, [2]string{"工作日日均", humanNum(s.WeekdayAvg) + " 次"})
		cards = append(cards, [2]string{"周末日均", humanNum(s.WeekendAvg) + " 次"})
	}
	if s.Prev != nil && s.Prev.Total > 0 {
		diff := float64(s.Total-s.Prev.Total) / float64(s.Prev.Total) * 100
		cards = append(cards, [2]string{"环比上周", fmt.Sprintf("%+.1f%%", diff)})
	}

	colW := d.ContentWidth() / 2
	const cardH = 30.0
	lineH := cardH + 6
	for i := 0; i < len(cards); i += 2 {
		r.ensure(lineH)
		for j := 0; j < 2 && i+j < len(cards); j++ {
			x := d.Margin + float64(j)*colW
			r.metricCard(x, r.y-cardH, colW-10, cardH, cards[i+j][0], cards[i+j][1])
		}
		r.y -= lineH
	}
	r.y -= 8
}

func (r *pdfRenderer) metricCard(x, y, w, h float64, label, value string) {
	p := r.page()
	p.Gray(x, y, w, h, 0.965)
	p.Gray(x, y, 2, h, 0.85)
	p.Text(x+8, y+h-9, 8.5, pdf.Muted, pdf.Truncate(label, 8.5, w-14))
	p.Text(x+8, y+7, 11.5, pdf.Bold, pdf.Truncate(value, 11.5, w-14))
}

func (r *pdfRenderer) sectionTitle(t string) {
	// 预留标题 + 表头 + 两行数据的高度，避免标题孤零零留在页尾
	r.ensure(78)
	p := r.page()
	r.y -= 6
	p.Text(r.d.Margin, r.y, 13, pdf.Bold, t)
	r.y -= 5
	p.Line(r.d.Margin, r.y, r.d.W-r.d.Margin, r.y, 0.8, 0.75)
	r.y -= 13
}

func (r *pdfRenderer) subTitle(t string) {
	// 同样留出「小标题 + 表头 + 两行」的空间，避免小标题和表格被分页切开
	r.ensure(64)
	r.y -= 4
	r.page().Text(r.d.Margin, r.y, 10.5, pdf.Bold, t)
	r.y -= 12
}

// ---------------------------------------------------------------- 表格

type pdfCol struct {
	title string
	w     float64 // 0 表示按剩余均分
	right bool    // 右对齐（数字列）
}

// table 画表格，rows 里的字符串已格式化好
func (r *pdfRenderer) table(cols []pdfCol, rows [][]string) {
	d := r.d
	// 计算列宽
	fixed := 0.0
	zero := 0
	for _, c := range cols {
		if c.w <= 0 {
			zero++
		} else {
			fixed += c.w
		}
	}
	rest := d.ContentWidth() - fixed
	if zero > 0 {
		rest /= float64(zero)
	}
	widths := make([]float64, len(cols))
	for i, c := range cols {
		if c.w > 0 {
			widths[i] = c.w
		} else {
			widths[i] = rest
		}
	}

	const size = 8.5
	const pad = 3.0
	drawHeader := func() {
		p := r.page()
		p.Gray(d.Margin, r.y-13, d.ContentWidth(), 13, 0.93)
		x := d.Margin
		for i, c := range cols {
			if c.right {
				p.TextRight(x+widths[i]-pad, r.y-9, size, pdf.Bold, c.title)
			} else {
				p.Text(x+pad, r.y-9, size, pdf.Bold, c.title)
			}
			x += widths[i]
		}
		r.y -= 13
	}

	// 表头 + 两行数据必须在一起，否则表头会孤零零留在页尾
	r.ensure(13 + 2*16)
	drawHeader()
	for _, row := range rows {
		// 先算这一行需要几行文字
		lines := 1
		wrapped := make([][]string, len(cols))
		for i := range cols {
			text := ""
			if i < len(row) {
				text = row[i]
			}
			wrapped[i] = pdf.Wrap(text, size, widths[i]-2*pad)
			if len(wrapped[i]) > lines {
				lines = len(wrapped[i])
			}
		}
		rowH := float64(lines)*11 + 5
		if r.y-rowH < d.Margin+18 {
			r.newPage()
			drawHeader()
		}
		p := r.page()
		x := d.Margin
		for i := range cols {
			for li, ln := range wrapped[i] {
				yy := r.y - 11 - float64(li)*11
				if cols[i].right {
					p.TextRight(x+widths[i]-pad, yy, size, pdf.Default, ln)
				} else {
					p.Text(x+pad, yy, size, pdf.Default, ln)
				}
			}
			x += widths[i]
		}
		r.y -= rowH
		p.Line(d.Margin, r.y, d.W-d.Margin, r.y, 0.4, 0.88)
	}
	r.y -= 8
}

// ---------------------------------------------------------------- 各章节

func (r *pdfRenderer) siteSection() {
	if len(r.s.SiteRows) == 0 {
		return
	}
	r.sectionTitle("站点对比")
	rows := make([][]string, 0, len(r.s.SiteRows))
	for _, x := range r.s.SiteRows {
		rows = append(rows, []string{siteKindLabel(x.Kind), x.Name, humanNum(x.Total),
			humanBytes(float64(x.Size)), fmt.Sprintf("%.2f%%", x.ErrorRate), humanNum(x.SlowCount),
			fmt.Sprintf("%.0fms", x.P95)})
	}
	r.table([]pdfCol{{title: "类型", w: 42}, {title: "站点", w: 0}, {title: "PV", w: 62, right: true},
		{title: "流量", w: 62, right: true}, {title: "错误率", w: 52, right: true},
		{title: "慢请求", w: 48, right: true}, {title: "P95", w: 52, right: true}}, rows)
}

func (r *pdfRenderer) issuesSection() {
	if len(r.s.Issues) == 0 {
		return
	}
	r.sectionTitle("二、问题与建议")
	rows := make([][]string, 0, len(r.s.Issues))
	for _, is := range r.s.Issues {
		level := "提示"
		if is.Level == "error" {
			level = "严重"
		} else if is.Level == "warning" {
			level = "警告"
		}
		rows = append(rows, []string{level, is.Title, is.Desc})
	}
	r.table([]pdfCol{{title: "级别", w: 34}, {title: "问题", w: 120}, {title: "说明", w: 0}}, rows)
}

func (r *pdfRenderer) trendSection() {
	s := r.s
	if s.Total == 0 {
		return
	}
	r.sectionTitle("三、业务趋势")

	multi := len(s.Series) > 1
	if multi {
		r.subTitle("QPS 曲线（5 分钟粒度，每天一条）")
	} else if s.SlotPeakVal > 0 {
		r.subTitle(fmt.Sprintf("QPS 曲线（5 分钟粒度）　峰值 %s %s · %s ≈ %s req/s",
			shortDay(s.SlotPeakDay), s.SlotPeakAt, timesStr(s.SlotPeakVal),
			rateStr(float64(s.SlotPeakVal)/float64(analyzer.SlotMinutes*60))))
	} else {
		r.subTitle("QPS 曲线（5 分钟粒度）")
	}
	if multi {
		maxSlot := int64(1)
		for _, row := range s.Series {
			if v := maxOf(row.Slots); v > maxSlot {
				maxSlot = v
			}
		}
		for _, row := range s.Series {
			if len(row.Slots) == 0 {
				continue
			}
			r.sparkRow(row.Day, row.Slots, analyzer.SlotMinutes, 22, maxSlot)
		}
		r.note("每行一天，横向覆盖 00:00–24:00（每格 5 分钟），纵轴按全周最大值归一化。")
	} else {
		r.barChart(s.Slots, analyzer.SlotMinutes, "00:00", "24:00", colAccent, 62)
	}

	if multi {
		r.subTitle("24 小时流量分布（每天一行）")
		var maxHour int64
		for _, row := range s.Series {
			if v := maxOf(row.Hours[:]); v > maxHour {
				maxHour = v
			}
		}
		for _, row := range s.Series {
			r.sparkRow(row.Day, row.Hours[:], 60, 18, maxHour)
		}
	} else {
		r.subTitle("24 小时流量分布")
		hours := make([]int64, 24)
		for h := 0; h < 24; h++ {
			hours[h] = s.Hours[h]
		}
		r.barChart(hours, 60, "00:00", "24:00", colGray, 52)
	}

	if len(s.Recent) > 0 || s.Prev != nil {
		r.subTitle("最近数周访问量")
		rows := make([][]string, 0, len(s.Recent)+2)
		for _, h := range s.Recent {
			rows = append(rows, []string{h.Start + " ~ " + h.End, humanNum(h.Total), humanBytes(float64(h.Size)), humanNum(h.ErrorCount)})
		}
		if s.Prev != nil {
			rows = append(rows, []string{s.Prev.Start + " ~ " + s.Prev.End, humanNum(s.Prev.Total), humanBytes(float64(s.Prev.Size)), humanNum(s.Prev.ErrorCount)})
		}
		rows = append(rows, []string{s.StartDate + " ~ " + s.EndDate, humanNum(s.Total), humanBytes(float64(s.SizeTotal)), humanNum(s.ErrorCount)})
		r.table([]pdfCol{{title: "周", w: 0}, {title: "访问量", w: 80, right: true}, {title: "流量", w: 80, right: true}, {title: "错误数", w: 60, right: true}}, rows)
	}
}

// sparkRow 一行迷你柱状图：日期标签 + 柱子 + 峰值说明
func (r *pdfRenderer) sparkRow(day string, vals []int64, step int, h float64, globalMax int64) {
	if len(vals) == 0 {
		return
	}
	r.ensure(h + 10)
	p := r.page()
	const labelW = 76.0
	const peakW = 152.0
	x0 := r.d.Margin + labelW
	bw := (r.d.ContentWidth() - labelW - peakW) / float64(len(vals))
	if bw <= 0 {
		return
	}
	if globalMax < 1 {
		globalMax = 1
	}
	base := r.y - h
	for i, v := range vals {
		bh := float64(v) / float64(globalMax) * h
		if bh < 0.5 && v > 0 {
			bh = 0.5
		}
		p.Rect(x0+float64(i)*bw, base, bw*0.95, bh, colAccent[0], colAccent[1], colAccent[2])
	}
	p.Line(x0, base, r.d.W-r.d.Margin-peakW, base, 0.3, 0.9)
	p.Text(r.d.Margin, base+3, 7.5, pdf.Muted, shortDay(day)+" "+weekdayName(day))
	peak, at := peakOf(vals, step)
	if peak > 0 {
		note := fmt.Sprintf("峰值 %s · %s", at, timesStr(peak))
		if step < 60 {
			note += " ≈" + rateStr(float64(peak)/float64(step*60)) + " req/s"
		}
		p.TextRight(r.d.W-r.d.Margin, base+3, 7.5, pdf.Muted, note)
	}
	r.y = base - 8
}

// barChart 画柱状图（每个桶一根柱）
func (r *pdfRenderer) barChart(vals []int64, step int, left, right string, color [3]float64, h float64) {
	if len(vals) == 0 {
		return
	}
	r.ensure(h + 26)
	p := r.page()
	maxV := int64(1)
	for _, v := range vals {
		if v > maxV {
			maxV = v
		}
	}
	w := r.d.ContentWidth()
	bw := w / float64(len(vals))
	base := r.y - h
	for i, v := range vals {
		bh := float64(v) / float64(maxV) * h
		if bh < 0.6 && v > 0 {
			bh = 0.6
		}
		if v == maxV {
			p.Rect(r.d.Margin+float64(i)*bw, base, bw*0.92, bh, colRed[0], colRed[1], colRed[2])
		} else {
			p.Rect(r.d.Margin+float64(i)*bw, base, bw*0.92, bh, color[0], color[1], color[2])
		}
	}
	p.Line(r.d.Margin, base, r.d.Margin+w, base, 0.5, 0.8)
	// 横轴刻度（左右各一个）
	p.Text(r.d.Margin, base-9, 7.5, pdf.Muted, left)
	p.TextRight(r.d.Margin+w, base-9, 7.5, pdf.Muted, right)
	// 峰值说明单独一行，避免和右侧刻度叠在一起
	peak, at := peakOf(vals, step)
	if peak > 0 {
		extra := ""
		if step < 60 {
			extra = fmt.Sprintf("（≈%s req/s）", rateStr(float64(peak)/float64(step*60)))
		}
		p.Text(r.d.Margin, base-19, 8, pdf.Default, fmt.Sprintf("峰值 %s · %s", at, timesStr(peak))+extra)
	}
	r.y = base - 26
}

func (r *pdfRenderer) dayTable() {
	s := r.s
	if len(s.Days) == 0 {
		return
	}
	r.sectionTitle("四、每日明细")
	rows := make([][]string, 0, len(s.Days))
	for _, dy := range s.Days {
		rows = append(rows, []string{
			dy.Day, weekdayName(dy.Day), humanNum(dy.Total), itoa(int(dy.PeakCnt)),
			humanBytes(float64(dy.Size)), fmt.Sprintf("%.0fms", dy.AvgTime*1000),
			humanNum(dy.ErrorCount), fmt.Sprintf("%.2f%%", pct(dy.ErrorCount, dy.Total)),
			humanNum(dy.SlowCount),
		})
	}
	r.table([]pdfCol{
		{title: "日期", w: 62}, {title: "星期", w: 34}, {title: "访问量", w: 0, right: true},
		{title: "峰值QPS", w: 50, right: true}, {title: "流量", w: 66, right: true},
		{title: "平均响应", w: 52, right: true}, {title: "错误", w: 44, right: true},
		{title: "错误率", w: 48, right: true}, {title: "慢请求", w: 46, right: true},
	}, rows)
}

func (r *pdfRenderer) statusSection() {
	s := r.s
	if len(s.StatusGroups) == 0 {
		return
	}
	r.sectionTitle("五、响应状态")
	r.subTitle("状态分组")
	rows := make([][]string, 0, len(s.StatusGroups))
	for _, g := range s.StatusGroups {
		rows = append(rows, []string{g.Name, humanNum(g.Count), fmt.Sprintf("%.2f%%", pct(g.Count, s.Total))})
	}
	r.table([]pdfCol{{title: "分组", w: 0}, {title: "请求数", w: 100, right: true}, {title: "占比", w: 80, right: true}}, rows)

	if errs := errorStatusList(r.s.Stats); len(errs) > 0 {
		r.subTitle("错误状态码分布")
		rows = rows[:0]
		for _, e := range errs {
			rows = append(rows, []string{e.Key, humanNum(e.Value), fmt.Sprintf("%.2f%%", pct(e.Value, s.Total))})
		}
		r.table([]pdfCol{{title: "状态码", w: 0}, {title: "请求数", w: 100, right: true}, {title: "占比", w: 80, right: true}}, rows)
	}
}

func (r *pdfRenderer) upstreamSection() {
	s := r.s
	if len(s.Ups) == 0 {
		return
	}
	r.sectionTitle("六、上游服务健康度")
	rows := make([][]string, 0, len(s.Ups))
	for _, u := range s.Ups {
		rows = append(rows, []string{
			displayURI(u.Addr), humanNum(u.Calls), fmt.Sprintf("%.2f%%", pct(u.Calls, s.Total)),
			fmt.Sprintf("%.0fms", u.AvgMs), humanNum(u.Err), fmt.Sprintf("%.2f%%", pct(u.Err, u.Calls)),
		})
	}
	r.table([]pdfCol{
		{title: "上游地址", w: 0}, {title: "调用次数", w: 70, right: true}, {title: "占比", w: 48, right: true},
		{title: "平均耗时", w: 58, right: true}, {title: "上游错误", w: 60, right: true}, {title: "错误率", w: 50, right: true},
	}, rows)
	r.note("上游错误率基于 upstream_status，可区分「网关问题」与「后端问题」。")
}

func (r *pdfRenderer) clientSection() {
	s := r.s
	if len(s.Clients) == 0 {
		return
	}
	r.sectionTitle("七、客户端来源 Top 15")
	rows := make([][]string, 0, len(s.Clients))
	for _, c := range s.Clients {
		key := c.Key
		if key == "" {
			key = "(未记录)"
		}
		rows = append(rows, []string{key, humanNum(c.Value), fmt.Sprintf("%.2f%%", pct(c.Value, s.Total))})
	}
	r.table([]pdfCol{{title: "来源 IP", w: 0}, {title: "请求数", w: 110, right: true}, {title: "占比", w: 80, right: true}}, rows)
}

func (r *pdfRenderer) apiSection() {
	s := r.s
	if len(s.TopAPIs) == 0 && s.Static.Calls == 0 {
		return
	}
	r.sectionTitle("八、接口健康度")
	r.subTitle("核心接口 Top 15（调用量 / 耗时 / 错误 / 慢请求）")
	rows := make([][]string, 0, len(s.TopAPIs))
	for i, u := range s.TopAPIs {
		if i >= 15 {
			break
		}
		rows = append(rows, []string{
			displayURI(u.URI), humanNum(u.Calls), fmt.Sprintf("%.2f%%", pct(u.Calls, s.Total)),
			fmt.Sprintf("%.0fms", u.AvgMs), humanNum(u.Slow), humanNum(u.Err),
			fmt.Sprintf("%.2f%%", pct(u.Err, u.Calls)),
		})
	}
	if s.Static.Calls > 0 {
		rows = append(rows, []string{
			fmt.Sprintf("(静态资源 %d 个)", s.Static.Files), humanNum(s.Static.Calls),
			fmt.Sprintf("%.2f%%", pct(s.Static.Calls, s.Total)), fmt.Sprintf("%.0fms", s.Static.AvgMs),
			humanNum(s.Static.Slows), humanNum(s.Static.Errs), fmt.Sprintf("%.2f%%", pct(s.Static.Errs, s.Static.Calls)),
		})
	}
	r.table([]pdfCol{
		{title: "接口", w: 0}, {title: "调用次数", w: 56, right: true}, {title: "占比", w: 44, right: true},
		{title: "平均耗时", w: 52, right: true}, {title: "慢请求", w: 44, right: true},
		{title: "错误数", w: 44, right: true}, {title: "错误率", w: 46, right: true},
	}, rows)

	if len(s.TopErr) > 0 {
		r.subTitle("错误次数最多的接口 Top 10")
		rows = rows[:0]
		for _, u := range s.TopErr {
			rows = append(rows, []string{displayURI(u.URI), humanNum(u.Err),
				fmt.Sprintf("%.2f%%", pct(u.Err, s.ErrorCount)), fmt.Sprintf("%.2f%%", pct(u.Err, u.Calls)), humanNum(u.Calls)})
		}
		r.table([]pdfCol{
			{title: "接口", w: 0}, {title: "错误数", w: 60, right: true}, {title: "占全部错误", w: 76, right: true},
			{title: "该接口错误率", w: 84, right: true}, {title: "调用次数", w: 66, right: true},
		}, rows)
	}
	if len(s.SlowTop) > 0 {
		r.subTitle("慢请求最多的接口 Top 10")
		rows = rows[:0]
		for _, u := range s.SlowTop {
			rows = append(rows, []string{displayURI(u.URI), humanNum(u.Slow), humanNum(u.Calls),
				fmt.Sprintf("%.0fms", u.AvgMs), fmt.Sprintf("%.1f%%", pct(u.Slow, u.Calls))})
		}
		r.table([]pdfCol{
			{title: "接口", w: 0}, {title: "慢请求", w: 60, right: true}, {title: "调用次数", w: 66, right: true},
			{title: "平均耗时", w: 62, right: true}, {title: "慢请求占比", w: 74, right: true},
		}, rows)
	}
}

func (r *pdfRenderer) timeDistSection() {
	s := r.s
	if len(s.TimeDist) == 0 {
		return
	}
	r.sectionTitle("九、响应时间分布")
	rows := make([][]string, 0, len(s.TimeDist))
	for _, t := range s.TimeDist {
		rows = append(rows, []string{t.Key, humanNum(t.Value), fmt.Sprintf("%.2f%%", pct(t.Value, s.Total))})
	}
	r.table([]pdfCol{{title: "区间", w: 0}, {title: "请求数", w: 110, right: true}, {title: "占比", w: 80, right: true}}, rows)
	if s.TimedCount != s.Total && s.Total > 0 {
		r.note(fmt.Sprintf("口径：平均响应与 P50~P99 只统计有 upstream 计时的 %s 次请求（占 %.1f%%），其余 %s 次无耗时记录（nginx 直接返回）不计入。",
			humanNum(s.TimedCount), s.TimedShare, humanNum(s.Total-s.TimedCount)))
	}
	if len(s.Methods) > 0 {
		r.subTitle("请求方法分布")
		rows = rows[:0]
		for _, m := range s.Methods {
			key := m.Key
			if key == "" {
				key = "(未记录)"
			}
			rows = append(rows, []string{key, humanNum(m.Value), fmt.Sprintf("%.2f%%", pct(m.Value, s.Total))})
		}
		r.table([]pdfCol{{title: "方法", w: 0}, {title: "请求数", w: 110, right: true}, {title: "占比", w: 80, right: true}}, rows)
	}
}

func (r *pdfRenderer) footnote() {
	r.sectionTitle("十、口径说明")
	for _, line := range []string{
		"· 数据来源：nginx JSON 访问日志（按 @timestamp 归日，跨天文件按行内时间切分）。",
		"· 错误判定：HTTP 状态码 ≥400；慢请求判定：响应耗时 > " + r.s.SlowLabel + "。",
		fmt.Sprintf("· 响应时间：平均与 P50~P99 只统计有 upstream 计时的 %s 次请求（占 %.1f%%），无耗时记录（nginx 直接返回）不计入。",
			humanNum(r.s.TimedCount), r.s.TimedShare),
		"· 成功率 = 2xx+3xx 占比；服务端错误率 = 5xx 占比（4xx 属客户端问题，不并入可用性）。",
		"· 响应时间口径：" + timeSourceText(r.s.TimeField) + "；上游状态取最后一次（重试成功计为成功）。",
		"· 接口统计默认排除静态资源（css/js/图片/字体），单独汇总一行。",
		"· 健康度评分：100 分制，错误率、慢请求占比、P99、平均响应四项扣分，详见 HTML 报告。",
	} {
		r.ensure(13)
		r.page().Text(r.d.Margin, r.y, 8.5, pdf.Muted, line)
		r.y -= 11.5
	}
}

func (r *pdfRenderer) note(t string) {
	r.ensure(14)
	r.page().Text(r.d.Margin, r.y, 8, pdf.Muted, t)
	r.y -= 12
}

// footers 给每页加页码
func (r *pdfRenderer) footers() {
	total := r.d.PageCount()
	for i, page := range r.d.Pages() {
		page.Line(r.d.Margin, r.d.Margin-4, r.d.W-r.d.Margin, r.d.Margin-4, 0.4, 0.85)
		page.Text(r.d.Margin, r.d.Margin-14, 7.5, pdf.Muted,
			fmt.Sprintf("%s　%s ~ %s", r.title(), r.s.StartDate, r.s.EndDate))
		page.TextRight(r.d.W-r.d.Margin, r.d.Margin-14, 7.5, pdf.Muted,
			fmt.Sprintf("第 %d / %d 页", i+1, total))
	}
}

// ---------------------------------------------------------------- 小工具

// title 报告标题（与 HTML 一致，来源 cfg.ReportTitle）
func (r *pdfRenderer) title() string {
	if r.s.Title != "" {
		return r.s.Title
	}
	return "Nginx 访问日志统计报告"
}

func peakTimeStr(s *snapshot) string {
	if s.PeakTime.IsZero() {
		return "-"
	}
	return s.PeakTime.Format("01-02 15:04:05")
}

func slotLabelAt(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("01-02 15:04")
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }
