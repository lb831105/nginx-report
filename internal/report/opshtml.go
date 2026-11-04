package report

import (
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"nginx-report/internal/analyzer"
)

// OpsHTML 生成运维/开发版 HTML 报告（带图表）。
// 和 OpsText 用同一份统计量，内容一一对应；文件名固定带 _ops，
// 不会和客户/上报用的 single_report_*.html / weekly_report_*.html 混淆。
func OpsHTML(in Input) string {
	s := buildSnapshot(in)
	g := in.Stats
	top := opsTopN(in.Cfg)
	var b strings.Builder

	rangeText := in.StartDate
	if in.EndDate != "" && in.EndDate != in.StartDate {
		rangeText = in.StartDate + " ~ " + in.EndDate
	}

	b.WriteString("<!DOCTYPE html>\n<html lang=\"zh-CN\"><head><meta charset=\"UTF-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	fmt.Fprintf(&b, "<title>%s - 运维分析</title>", html.EscapeString(s.Title))
	b.WriteString("<style>" + opsCSS + "</style></head><body><div class=\"wrap\">\n")

	// ---------------- 封面 / 概览 ----------------
	b.WriteString(`<header class="hero"><div class="tag">OPS / DEV · 深度分析</div>`)
	fmt.Fprintf(&b, "<h1>%s</h1>", html.EscapeString(s.Title))
	b.WriteString(`<div class="sub">1 分钟粒度 · PV / 流量 / 状态码 / 接口 / 性能 / 终端 / 来源</div>`)
	fmt.Fprintf(&b, `<div class="meta">报告周期：%s　|　日志：%s　|　生成时间：%s</div>`,
		html.EscapeString(rangeText), html.EscapeString(orDash(in.Source)), time.Now().Format("2006-01-02 15:04:05"))
	if in.Cfg != nil && in.Cfg.FromClock != "" {
		fmt.Fprintf(&b, `<div class="meta">时间过滤：每天 %s 之后（对应原脚本 START_TIME）</div>`, html.EscapeString(in.Cfg.FromClock))
	}
	if in.SiteName != "" {
		fmt.Fprintf(&b, `<div class="meta">站点：%s</div>`, opsE(in.SiteName))
	}
	if in.Cfg != nil && in.Cfg.SkipStatic {
		b.WriteString(`<span class="badge ok">已排除静态资源</span>`)
	}
	if g != nil && g.StaticSkipped > 0 {
		fmt.Fprintf(&b, `<span class="badge">排除 %d 条 / %s（原始 %d 条）</span>`,
			g.StaticSkipped, humanBytes(float64(g.StaticBytes)), s.Total+g.StaticSkipped+g.BotSkipped)
	}
	if g != nil && g.BotSkipped > 0 {
		fmt.Fprintf(&b, `<span class="badge">排除爬虫/探针 %d 条</span>`, g.BotSkipped)
	}
	fmt.Fprintf(&b, `<span class="badge">Top 榜 %d 条</span></header>`, top)

	if g == nil || s.Total == 0 {
		b.WriteString(`<section><h2>没有数据</h2><p class="muted">统计区间内没有有效日志记录。</p></section></div></body></html>`)
		return b.String()
	}

	// ---------------- KPI ----------------
	b.WriteString(`<section class="kpis">`)
	kpi := func(label, value, extra string) {
		fmt.Fprintf(&b, `<div class="kpi"><div class="k-label">%s</div><div class="k-value">%s</div><div class="k-extra">%s</div></div>`,
			label, value, extra)
	}
	kpi("总 PV", humanNum(s.Total), "业务请求")
	kpi("独立访客 IP", fmt.Sprintf("%d", s.ClientNum), fmt.Sprintf("直接访问 %d（%.1f%%）", g.DirectCnt, pct(g.DirectCnt, s.Total)))
	kpi("错误请求", fmt.Sprintf("%d", s.ErrorCount), fmt.Sprintf("错误率 %.2f%%", s.ErrorRate))
	kpi("慢请求（>"+slowThresholdText(in.Cfg)+"）", fmt.Sprintf("%d", s.SlowCount), fmt.Sprintf("占比 %.2f%%", s.SlowRate))
	kpi("总流量", humanBytes(float64(s.SizeTotal)), fmt.Sprintf("平均响应 %.2f KB", float64(s.SizeTotal)/float64(s.Total)/1024))
	kpi("平均响应", fmt.Sprintf("%.0f ms", s.AvgMs), fmt.Sprintf("P95 %.0f / P99 %.0f ms", s.P95, s.P99))
	kpi("峰值 QPS", fmt.Sprintf("%d", s.PeakQPS), s.PeakTime.Format("01-02 15:04:05"))
	kpi("平均 QPS", fmt.Sprintf("%.2f", s.AvgQPS), fmt.Sprintf("统计 %d 天", s.DayCount))
	b.WriteString(`</section>`)
	if len(in.SiteRows) > 0 {
		b.WriteString(`<section><h2>站点对比</h2><table><thead><tr><th>类型</th><th>站点</th><th class="num">PV</th><th class="num">流量</th><th class="num">错误率</th><th class="num">慢请求</th><th class="num">P95</th></tr></thead><tbody>`)
		for _, r := range in.SiteRows {
			fmt.Fprintf(&b, `<tr><td>%s</td><td class="mono">%s</td><td class="num">%d</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%d</td><td class="num">%.0fms</td></tr>`,
				siteKindLabel(r.Kind), opsE(r.Name), r.Total, humanBytes(float64(r.Size)), r.ErrorRate, r.SlowCount, r.P95)
		}
		b.WriteString(`</tbody></table></section>`)
	}

	// ---------------- 一、趋势（HTML5 Canvas，可缩放） ----------------
	days := sortedDays(in)
	charts := buildOpsCharts(g, days)
	chartJSON, _ := json.Marshal(charts)

	b.WriteString(`<section><h2>一、访问趋势<span class="aud">HTML5 · 可缩放</span></h2><div class="grid2">`)
	b.WriteString(opsChartCard("pv", "PV 趋势", charts.PV.Unit, len(days) == 1))
	b.WriteString(opsChartCard("traffic", "流量趋势", charts.Traffic.Unit, len(days) == 1))
	b.WriteString(`</div><p class="muted">滚轮缩放 · 拖拽平移 · 双击复位；触屏支持单指拖动、双指缩放。</p>`)
	b.WriteString(`<noscript><p class="muted">图表需要启用 JavaScript；纯文本数据见同目录 <code>_ops.txt</code>。</p></noscript>`)
	b.WriteString(`</section>`)

	// ---------------- 二、请求结构 ----------------
	b.WriteString(`<section><h2>二、请求结构</h2><div class="grid2">`)
	b.WriteString(`<div><h3>状态码分布</h3>`)
	b.WriteString(opsStatusStack(s.StatusGroups, s.Total))
	b.WriteString(`<table><thead><tr><th>状态码</th><th class="num">次数</th><th class="num">占比</th></tr></thead><tbody>`)
	for _, code := range sortedStatusCodes(g.StatusCnt) {
		n := g.StatusCnt[code]
		fmt.Fprintf(&b, `<tr><td><span class="code %s">%s</span></td><td class="num">%d</td><td class="num">%.2f%%</td></tr>`,
			statusClass(code), html.EscapeString(code), n, pct(n, s.Total))
	}
	b.WriteString(`</tbody></table></div>`)

	b.WriteString(`<div><h3>HTTP 方法</h3>`)
	b.WriteString(opsBarsFromKV(topN(g.MethodCnt, top), s.Total, "#3949ab"))
	b.WriteString(`<h3>URL 类型分布</h3>`)
	page, other := urlTypeCounts(g)
	b.WriteString(opsBars([]opsBar{
		{Label: "业务页面", Value: float64(page), Text: fmt.Sprintf("%d（%.1f%%）", page, pct(page, s.Total))},
		{Label: "其他", Value: float64(other), Text: fmt.Sprintf("%d（%.1f%%）", other, pct(other, s.Total))},
	}, "#1e88e5"))
	b.WriteString(`</div></div></section>`)

	// ---------------- 三、Top 排行 ----------------
	b.WriteString(`<section><h2>三、Top 排行<span class="aud">` + fmt.Sprintf("前 %d 条", top) + `</span></h2>`)
	b.WriteString(`<div class="grid2"><div><h3>URL 调用量</h3>`)
	b.WriteString(opsURIBars(topURIs(g, top), s.Total))
	b.WriteString(`</div><div><h3>客户端 IP</h3>`)
	b.WriteString(opsBarsFromKV(topN(g.ClientCnt, top), s.Total, "#00897b"))
	b.WriteString(`</div></div>`)
	b.WriteString(`<h3>大流量 URL（响应字节）</h3>`)
	b.WriteString(opsBytesURIBars(topURIByBytes(g, top)))
	b.WriteString(`</section>`)

	// ---------------- 四、错误分析 ----------------
	b.WriteString(`<section><h2>四、错误分析</h2>`)
	fmt.Fprintf(&b, `<p class="muted">错误请求 %d 条，占 %.2f%%；其中 4xx %d 条、5xx %d 条。</p>`,
		s.ErrorCount, s.ErrorRate, sumClass(g, "4xx"), sumClass(g, "5xx"))
	b.WriteString(`<h3>错误状态码</h3>`)
	b.WriteString(opsErrorCodeBars(g, s.ErrorCount))
	b.WriteString(`<div class="grid3">`)
	for _, code := range []string{"404", "5xx", "400"} {
		fmt.Fprintf(&b, `<div><h3>Top %d %s URL</h3>`, top, code)
		rows := errURIList(g, code, top)
		if len(rows) == 0 {
			b.WriteString(`<p class="muted">（无）</p>`)
		} else {
			b.WriteString(`<table><thead><tr><th>URL</th><th class="num">次数</th></tr></thead><tbody>`)
			for _, kvp := range rows {
				fmt.Fprintf(&b, `<tr><td class="mono" title="%s">%s</td><td class="num">%d</td></tr>`,
					opsE(kvp.Key), opsE(displayURI(kvp.Key)), kvp.Value)
			}
			b.WriteString(`</tbody></table>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div></section>`)

	// ---------------- 五、响应性能 ----------------
	b.WriteString(`<section><h2>五、响应性能</h2>`)
	b.WriteString(`<div class="kpis small">`)
	kpi("平均响应", fmt.Sprintf("%.0f ms", s.AvgMs), fmt.Sprintf("样本 %d 条", s.TimedCount))
	kpi("最大响应", fmt.Sprintf("%.3f s", g.MaxTime), "单次最长")
	kpi("P50 / P90", fmt.Sprintf("%.0f / %.0f ms", s.P50, s.P90), "中位 / 长尾")
	kpi("P95 / P99", fmt.Sprintf("%.0f / %.0f ms", s.P95, s.P99), "尾部延迟")
	kpi("慢请求", fmt.Sprintf("%d", s.SlowCount), fmt.Sprintf(">%s，占 %.2f%%", slowThresholdText(in.Cfg), s.SlowRate))
	b.WriteString(`</div>`)
	b.WriteString(`<div class="grid2"><div><h3>响应时间分布</h3>`)
	b.WriteString(opsTimeBins(g, s.Total))
	b.WriteString(`</div><div><h3>成功率 / 错误率</h3>`)
	success, serverErr := 0.0, 0.0
	for code, n := range g.StatusCnt {
		switch statusClass(code) {
		case "2xx", "3xx":
			success += float64(n)
		case "5xx":
			serverErr += float64(n)
		}
	}
	b.WriteString(opsBars([]opsBar{
		{Label: "成功率(2xx+3xx)", Value: success, Text: fmt.Sprintf("%.2f%%", pct(int64(success), s.Total))},
		{Label: "客户端错误(4xx)", Value: float64(sumClass(g, "4xx")), Text: fmt.Sprintf("%.2f%%", pct(sumClass(g, "4xx"), s.Total))},
		{Label: "服务端错误(5xx)", Value: serverErr, Text: fmt.Sprintf("%.2f%%", pct(int64(serverErr), s.Total))},
	}, "#43a047"))
	b.WriteString(`</div></div>`)
	b.WriteString(`<h3>慢请求 URL（按慢请求平均耗时）</h3>`)
	slows := slowURIList(g, top)
	if len(slows) == 0 {
		b.WriteString(`<p class="muted">（无慢请求）</p>`)
	} else {
		rows := make([]opsBar, 0, len(slows))
		for _, r := range slows {
			rows = append(rows, opsBar{
				Label: displayURI(r.URI),
				Value: r.Avg,
				Text:  fmt.Sprintf("%.3fs × %d 次", r.Avg, r.Calls),
			})
		}
		b.WriteString(opsBars(rows, "#e53935"))
	}
	b.WriteString(`</section>`)

	// ---------------- 六、终端与来源 ----------------
	b.WriteString(`<section><h2>六、终端与来源</h2><div class="grid2">`)
	b.WriteString(`<div><h3>设备类型</h3>`)
	b.WriteString(opsBarsFromKV(topN(g.DeviceCnt, top), s.Total, "#8e24aa"))
	b.WriteString(`<h3>浏览器</h3>`)
	b.WriteString(opsBarsFromKV(topN(g.BrowserCnt, top), s.Total, "#fb8c00"))
	b.WriteString(`<h3>文件类型</h3>`)
	b.WriteString(opsBarsFromKV(fileTypeCounts(g), s.Total, "#546e7a"))
	b.WriteString(`</div><div><h3>Referer 域名</h3>`)
	b.WriteString(opsBarsFromKV(topN(g.RefererCnt, top), s.Total, "#00acc1"))
	fmt.Fprintf(&b, `<p class="muted">直接访问（无 Referer）：%d 条，占 %.1f%%</p>`,
		g.DirectCnt, pct(g.DirectCnt, s.Total))
	b.WriteString(`</div></div></section>`)

	// ---------------- 页脚 ----------------
	b.WriteString(`<footer>`)
	b.WriteString(`<div>口径：PV / 流量 / 状态码 / IP / 错误均按业务请求统计；响应时间仅统计带 upstream_response_time / responsetime 的请求。</div>`)
	b.WriteString(`<div>本文件由 nginx-report 生成，文件名带 <code>_ops</code>，与客户/上报版 HTML 区分。</div>`)
	b.WriteString(`</footer>`)
	b.WriteString(`<script type="application/json" id="ops-chart-data">`)
	b.Write(chartJSON)
	b.WriteString(`</script>`)
	b.WriteString(`<script>` + opsJS + `</script>`)
	b.WriteString(`</div></body></html>`)

	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func opsE(s string) string { return html.EscapeString(cleanText(s)) }

// sortedDays 取报告里的日期（升序）
func sortedDays(in Input) []analyzer.DaySummary {
	days := make([]analyzer.DaySummary, len(in.Days))
	copy(days, in.Days)
	if len(days) == 0 && in.Stats != nil {
		for day := range in.Stats.Days {
			days = append(days, analyzer.DaySummary{Day: day})
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })
	return days
}

// ---------------- 交互图表数据 ----------------

// opsSeries 一条趋势线：单日按 1 分钟（或 5 分钟）粒度，多日按天。
type opsSeries struct {
	Title  string    `json:"title"`
	Kind   string    `json:"kind"` // count | mb
	Values []float64 `json:"values"`
	Step   int       `json:"step"`             // 每个点代表多少分钟；多日为 0
	Labels []string  `json:"labels,omitempty"` // 多日：日期标签
	Unit   string    `json:"unit"`
}

type opsCharts struct {
	PV      opsSeries `json:"pv"`
	Traffic opsSeries `json:"traffic"`
}

// buildOpsCharts 组装前端图表数据。单日补零到整天（24h），
// 前端首次渲染会自动缩放到有数据的区间，用户可再滚轮缩放。
func buildOpsCharts(g *analyzer.FileStats, days []analyzer.DaySummary) opsCharts {
	ch := opsCharts{
		PV:      opsSeries{Title: "PV 趋势", Kind: "count", Unit: "次/分"},
		Traffic: opsSeries{Title: "流量趋势", Kind: "mb", Unit: "MB/分"},
	}
	if len(days) == 0 {
		return ch
	}
	if len(days) == 1 {
		vals, step := dayTrend(g, days[0].Day)
		if step <= 0 {
			step = 1
		}
		ch.PV.Step = step
		ch.PV.Values = padCounts(vals, 24*60/step)
		if step > 1 {
			ch.PV.Unit = fmt.Sprintf("次/%d分", step)
		}
		da := g.DayAgg(days[0].Day)
		if da != nil && sumOf(da.MinuteBytes) > 0 {
			tr := make([]float64, len(da.MinuteBytes))
			for i, v := range da.MinuteBytes {
				tr[i] = float64(v) / 1024 / 1024
			}
			ch.Traffic.Step = 1
			ch.Traffic.Values = padFloats(tr, 24*60)
		}
		return ch
	}

	labels := make([]string, 0, len(days))
	for _, d := range days {
		labels = append(labels, shortDay(d.Day))
		ch.PV.Values = append(ch.PV.Values, float64(d.Total))
		var bytes int64
		if da := g.DayAgg(d.Day); da != nil {
			bytes = sumOf(da.MinuteBytes)
		}
		if bytes == 0 {
			bytes = d.Size
		}
		ch.Traffic.Values = append(ch.Traffic.Values, float64(bytes)/1024/1024)
	}
	ch.PV.Labels, ch.Traffic.Labels = labels, labels
	ch.PV.Step, ch.Traffic.Step = 0, 0
	ch.PV.Unit, ch.Traffic.Unit = "次/天", "MB/天"
	return ch
}

func padCounts(v []int64, n int) []float64 {
	if n < len(v) {
		n = len(v)
	}
	out := make([]float64, n)
	for i := range out {
		if i < len(v) {
			out[i] = float64(v[i])
		}
	}
	return out
}

func padFloats(v []float64, n int) []float64 {
	if n < len(v) {
		n = len(v)
	}
	out := make([]float64, n)
	copy(out, v)
	return out
}

// opsChartCard 一个可缩放图表的容器 + 快捷键
func opsChartCard(id, title, unit string, singleDay bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="chart-card"><div class="chart-head"><h3>%s</h3>`, opsE(title))
	if singleDay {
		fmt.Fprintf(&b, `<div class="zoom" data-chart="%s">`, id)
		for _, btn := range []struct {
			label string
			span  string
		}{
			{"1小时", "60"}, {"6小时", "360"}, {"12小时", "720"}, {"全天", "0"},
		} {
			fmt.Fprintf(&b, `<button type="button" data-span="%s">%s</button>`, btn.span, btn.label)
		}
		b.WriteString(`</div>`)
	}
	fmt.Fprintf(&b, `<span class="unit">%s</span></div>`, opsE(unit))
	fmt.Fprintf(&b, `<div class="canvas-wrap"><canvas id="chart-%s"></canvas><div class="tip" id="tip-%s"></div></div>`, id, id)
	fmt.Fprintf(&b, `<div class="chart-note" id="note-%s"></div></div>`, id)
	return b.String()
}

// opsStatusStack 2xx/3xx/4xx/5xx 占比堆叠条
func opsStatusStack(groups []groupCount, total int64) string {
	if total <= 0 {
		return `<p class="muted">（无数据）</p>`
	}
	colors := map[string]string{"2xx": "#43a047", "3xx": "#039be5", "4xx": "#fb8c00", "5xx": "#e53935", "其他": "#90a4ae"}
	var b strings.Builder
	b.WriteString(`<div class="stack">`)
	for _, grp := range groups {
		c := colors[grp.Name]
		if c == "" {
			c = "#90a4ae"
		}
		fmt.Fprintf(&b, `<span style="width:%.3f%%;background:%s" title="%s %d（%.2f%%）"></span>`,
			pct(grp.Count, total), c, opsE(grp.Name), grp.Count, pct(grp.Count, total))
	}
	b.WriteString(`</div><div class="legend">`)
	for _, grp := range groups {
		c := colors[grp.Name]
		if c == "" {
			c = "#90a4ae"
		}
		fmt.Fprintf(&b, `<span><i style="background:%s"></i>%s %.2f%%</span>`, c, opsE(grp.Name), pct(grp.Count, total))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// opsBar 一行横向柱
type opsBar struct {
	Label string
	Value float64
	Text  string
}

// opsBars 横向柱状图：宽度按最大值归一化
func opsBars(rows []opsBar, color string) string {
	if len(rows) == 0 {
		return `<p class="muted">（无数据）</p>`
	}
	maxV := 0.0
	for _, r := range rows {
		if r.Value > maxV {
			maxV = r.Value
		}
	}
	var b strings.Builder
	b.WriteString(`<div class="bars">`)
	for _, r := range rows {
		w := 0.0
		if maxV > 0 {
			w = r.Value / maxV * 100
		}
		fmt.Fprintf(&b, `<div class="bar"><div class="bar-label" title="%s">%s</div><div class="bar-track"><div class="bar-fill" style="width:%.2f%%;background:%s"></div></div><div class="bar-val">%s</div></div>`,
			opsE(r.Label), opsE(r.Label), w, color, opsE(r.Text))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// opsBarsFromKV 把 map 计数转成横向柱（按次数降序）
func opsBarsFromKV(rows []kv, total int64, color string) string {
	if len(rows) == 0 {
		return `<p class="muted">（无数据）</p>`
	}
	out := make([]opsBar, 0, len(rows))
	for _, r := range rows {
		out = append(out, opsBar{
			Label: r.Key,
			Value: float64(r.Value),
			Text:  fmt.Sprintf("%d（%.1f%%）", r.Value, pct(r.Value, total)),
		})
	}
	return opsBars(out, color)
}

// opsURIBars URL 调用量横向柱
func opsURIBars(rows []uriStat, total int64) string {
	if len(rows) == 0 {
		return `<p class="muted">（无数据）</p>`
	}
	out := make([]opsBar, 0, len(rows))
	for _, r := range rows {
		out = append(out, opsBar{
			Label: displayURI(r.URI),
			Value: float64(r.Calls),
			Text:  fmt.Sprintf("%d（%.1f%%）", r.Calls, pct(r.Calls, total)),
		})
	}
	return opsBars(out, "#5e35b1")
}

// opsBytesURIBars 大流量 URL（MB）
func opsBytesURIBars(rows []uriStat) string {
	if len(rows) == 0 {
		return `<p class="muted">（无数据）</p>`
	}
	out := make([]opsBar, 0, len(rows))
	for _, r := range rows {
		out = append(out, opsBar{
			Label: displayURI(r.URI),
			Value: float64(r.Bytes) / 1024 / 1024,
			Text:  fmt.Sprintf("%.2f MB", float64(r.Bytes)/1024/1024),
		})
	}
	return opsBars(out, "#00838f")
}

// opsErrorCodeBars 错误状态码分布
func opsErrorCodeBars(g *analyzer.FileStats, errors int64) string {
	if len(g.ErrorStatus) == 0 {
		return `<p class="muted">（无错误）</p>`
	}
	out := make([]opsBar, 0, len(g.ErrorStatus))
	for _, code := range sortedStatusCodes(g.ErrorStatus) {
		n := g.ErrorStatus[code]
		if n == 0 {
			continue
		}
		out = append(out, opsBar{
			Label: code,
			Value: float64(n),
			Text:  fmt.Sprintf("%d（占错误 %.1f%%）", n, pct(n, errors)),
		})
	}
	return opsBars(out, "#e53935")
}

// opsTimeBins 响应时间分布
func opsTimeBins(g *analyzer.FileStats, total int64) string {
	bins := responseTimeBins(&g.Agg)
	if len(bins) == 0 {
		return `<p class="muted">（无耗时数据）</p>`
	}
	out := make([]opsBar, 0, len(bins))
	for _, bin := range bins {
		out = append(out, opsBar{
			Label: bin.Label,
			Value: float64(bin.Count),
			Text:  fmt.Sprintf("%d（%.1f%%）", bin.Count, pct(bin.Count, total)),
		})
	}
	return opsBars(out, "#fb8c00")
}

// sumClass 某类状态码（2xx/4xx/5xx）的总数
func sumClass(g *analyzer.FileStats, class string) int64 {
	var n int64
	for code, c := range g.StatusCnt {
		if statusClass(code) == class {
			n += c
		}
	}
	return n
}

// opsJS HTML5 Canvas 交互图表：滚轮缩放、拖拽平移、双击复位、触屏双指缩放。
// 全部内联，不依赖任何外部 CDN。
const opsJS = `
(function(){
  'use strict';
  var holder=document.getElementById('ops-chart-data');
  if(!holder)return;
  var DATA;
  try{DATA=JSON.parse(holder.textContent||'{}');}catch(e){return;}
  var DPR=Math.max(1,Math.min(3,window.devicePixelRatio||1));

  function p2(n){return (n<10?'0':'')+n;}
  function num(v){
    if(v>=1e8)return (v/1e8).toFixed(2)+' 亿';
    if(v>=1e4)return (v/1e4).toFixed(1)+' 万';
    if(v>=1000)return Math.round(v).toLocaleString('en-US');
    return (Math.round(v*100)/100).toString();
  }
  function fmt(v,kind){return kind==='mb'?v.toFixed(2):num(v);}
  function hhmm(idx,step){
    var m=idx*step;
    return p2(Math.floor(m/60)%24)+':'+p2(m%60);
  }
  function niceMax(v){
    if(!(v>0))return 1;
    var exp=Math.floor(Math.log(v)/Math.LN10);
    var base=Math.pow(10,exp);
    var f=v/base;
    var nf=f<=1?1:f<=2?2:f<=2.5?2.5:f<=5?5:10;
    return nf*base;
  }
  function clamp(v,a,b){return v<a?a:(v>b?b:v);}

  function Chart(series,id){
    this.id=id;
    this.s=series||{};
    this.values=this.s.values||[];
    this.kind=this.s.kind||'count';
    this.step=this.s.step||1;
    this.labels=this.s.labels||null;
    this.canvas=document.getElementById('chart-'+id);
    this.tip=document.getElementById('tip-'+id);
    this.note=document.getElementById('note-'+id);
    if(!this.canvas)return;
    this.ctx=this.canvas.getContext('2d');
    this.hover=-1;
    this.W=0;this.H=280;
    this.L=58;this.R=16;this.T=16;this.B=30;
    this.minSpan=3;
    this.start=0;this.end=Math.max(0,this.values.length-1);
    this.initView();
    this.summary();
    var self=this;
    this.bind();
    this.fit();
    if(window.ResizeObserver){new ResizeObserver(function(){self.fit();self.draw();}).observe(this.canvas.parentNode);}
    window.addEventListener('resize',function(){self.fit();self.draw();});
    var wrap=this.canvas.closest('.chart-card');
    if(wrap){
      wrap.querySelectorAll('.zoom button').forEach(function(btn){
        btn.addEventListener('click',function(){
          var minutes=parseFloat(btn.getAttribute('data-span'))||0;
          var n=self.values.length;
          var pts=minutes>0?minutes/self.step:(n-1);
          pts=clamp(pts,self.minSpan,Math.max(self.minSpan,n-1));
          var c=(self.start+self.end)/2;
          self.start=c-pts/2;self.end=c+pts/2;
          self.clampView();self.draw();
          wrap.querySelectorAll('.zoom button').forEach(function(x){x.classList.remove('on');});
          btn.classList.add('on');
        });
      });
    }
    this.draw();
  }

  Chart.prototype.initView=function(){
    var n=this.values.length;
    if(n<=1){this.start=0;this.end=Math.max(0,n-1);return;}
    this.minSpan=this.labels?Math.min(2,n-1):Math.min(3,n-1);
    if(this.labels){this.start=0;this.end=n-1;return;}
    var first=-1,last=-1;
    for(var i=0;i<n;i++){if(this.values[i]>0){if(first<0)first=i;last=i;}}
    if(first<0){first=0;last=n-1;}
    var pad=Math.max(3,Math.round((last-first)*0.03));
    this.start=Math.max(0,first-pad);
    this.end=Math.min(n-1,last+pad);
    if(this.end-this.start<Math.min(n-1,15)){
      this.start=Math.max(0,first-7);
      this.end=Math.min(n-1,last+7);
    }
  };

  Chart.prototype.clampView=function(){
    var n=this.values.length;
    if(n<=1){this.start=0;this.end=Math.max(0,n-1);return;}
    var span=clamp(this.end-this.start,this.minSpan,n-1);
    var c=(this.start+this.end)/2;
    var start=c-span/2;
    if(start<0)start=0;
    if(start+span>n-1)start=n-1-span;
    this.start=start;this.end=start+span;
  };

  Chart.prototype.fit=function(){
    var rect=this.canvas.parentNode.getBoundingClientRect();
    var w=Math.max(280,Math.round(rect.width));
    this.W=w;this.H=280;
    this.canvas.width=Math.round(w*DPR);
    this.canvas.height=Math.round(this.H*DPR);
    this.canvas.style.width=w+'px';
    this.canvas.style.height=this.H+'px';
    this.ctx.setTransform(DPR,0,0,DPR,0,0);
  };

  Chart.prototype.plot=function(){
    return {l:this.L,r:this.W-this.R,t:this.T,b:this.H-this.B,
      w:Math.max(10,this.W-this.L-this.R),h:Math.max(10,this.H-this.T-this.B)};
  };
  Chart.prototype.x=function(i){
    var p=this.plot(),span=this.end-this.start||1;
    return p.l+(i-this.start)/span*p.w;
  };
  Chart.prototype.y=function(v,max){
    var p=this.plot();
    return p.t+p.h-(v/max)*p.h;
  };
  Chart.prototype.idxAt=function(px){
    var p=this.plot();
    return this.start+((px-p.l)/p.w)*(this.end-this.start);
  };
  Chart.prototype.label=function(i){
    if(this.labels){var k=Math.round(i);return this.labels[clamp(k,0,this.labels.length-1)]||'';}
    return hhmm(i,this.step);
  };

  Chart.prototype.summary=function(){
    if(!this.note)return;
    var v=this.values,n=v.length,max=-1,at=0,sum=0;
    for(var i=0;i<n;i++){sum+=v[i];if(v[i]>max){max=v[i];at=i;}}
    if(max<=0){this.note.textContent='（无数据）';return;}
    var base=this.kind==='mb'?'MB':'次';
    var peakUnit=this.s.unit||base;
    this.note.textContent='峰值 '+fmt(max,this.kind)+' '+peakUnit+' @ '+this.label(at)+'　·　合计 '+fmt(sum,this.kind)+' '+base;
  };

  Chart.prototype.maxVisible=function(){
    var i0=Math.max(0,Math.floor(this.start)),i1=Math.min(this.values.length-1,Math.ceil(this.end)),m=0;
    for(var i=i0;i<=i1;i++){if(this.values[i]>m)m=this.values[i];}
    return m;
  };

  Chart.prototype.draw=function(){
    var ctx=this.ctx,p=this.plot(),n=this.values.length;
    ctx.clearRect(0,0,this.W,this.H);
    if(!n)return;
    var i0=Math.max(0,Math.floor(this.start)),i1=Math.min(n-1,Math.ceil(this.end));
    var max=0;
    for(var i=i0;i<=i1;i++){if(this.values[i]>max)max=this.values[i];}
    var yMax=niceMax(max);

    ctx.font='11px -apple-system,Segoe UI,Microsoft YaHei,sans-serif';
    ctx.textAlign='right';ctx.textBaseline='middle';
    for(var g=0;g<=4;g++){
      var gy=p.t+p.h*g/4;
      ctx.strokeStyle='#e3e7f0';ctx.lineWidth=1;
      ctx.beginPath();ctx.moveTo(p.l,gy);ctx.lineTo(p.r,gy);ctx.stroke();
      ctx.fillStyle='#90a4ae';
      ctx.fillText(fmt(yMax*(1-g/4),this.kind),p.l-8,gy);
    }

    ctx.textAlign='center';ctx.textBaseline='top';
    for(var k=0;k<=5;k++){
      var idx=this.start+(this.end-this.start)*k/5;
      ctx.fillStyle='#90a4ae';
      ctx.fillText(this.label(idx),this.x(idx),p.b+8);
    }

    ctx.save();
    ctx.beginPath();ctx.rect(p.l,p.t,p.w,p.h);ctx.clip();

    var color=this.kind==='mb'?'#00897b':'#3949ab';
    var grad=ctx.createLinearGradient(0,p.t,0,p.b);
    grad.addColorStop(0,this.kind==='mb'?'rgba(0,137,123,.30)':'rgba(57,73,171,.30)');
    grad.addColorStop(1,this.kind==='mb'?'rgba(0,137,123,0)':'rgba(57,73,171,0)');

    ctx.beginPath();
    ctx.moveTo(this.x(i0),p.b);
    for(i=i0;i<=i1;i++){ctx.lineTo(this.x(i),this.y(this.values[i],yMax));}
    ctx.lineTo(this.x(i1),p.b);ctx.closePath();
    ctx.fillStyle=grad;ctx.fill();

    ctx.beginPath();
    for(i=i0;i<=i1;i++){
      var xx=this.x(i),yy=this.y(this.values[i],yMax);
      if(i===i0)ctx.moveTo(xx,yy);else ctx.lineTo(xx,yy);
    }
    ctx.strokeStyle=color;ctx.lineWidth=2;ctx.lineJoin='round';ctx.lineCap='round';ctx.stroke();

    if(this.end-this.start<=160){
      for(i=i0;i<=i1;i++){
        ctx.beginPath();ctx.arc(this.x(i),this.y(this.values[i],yMax),2.2,0,Math.PI*2);
        ctx.fillStyle=color;ctx.fill();
      }
    }
    ctx.restore();

    if(this.hover>=i0&&this.hover<=i1){
      var hx=this.x(this.hover),hy=this.y(this.values[this.hover],yMax);
      ctx.strokeStyle='rgba(57,73,171,.45)';ctx.lineWidth=1;
      ctx.beginPath();ctx.moveTo(hx,p.t);ctx.lineTo(hx,p.b);ctx.stroke();
      ctx.beginPath();ctx.arc(hx,hy,4,0,Math.PI*2);
      ctx.fillStyle='#fff';ctx.fill();
      ctx.strokeStyle=color;ctx.lineWidth=2;ctx.stroke();
    }
  };

  Chart.prototype.showTip=function(clientX){
    if(!this.tip)return;
    var rect=this.canvas.getBoundingClientRect();
    var idx=Math.round(this.idxAt(clientX-rect.left));
    if(idx<0||idx>=this.values.length){this.hideTip();return;}
    this.hover=idx;
    this.draw();
    var x=this.x(idx),y=this.y(this.values[idx],niceMax(this.maxVisible()));
    this.tip.innerHTML='<b>'+this.label(idx)+'</b><span>'+fmt(this.values[idx],this.kind)+' '+(this.s.unit||'')+'</span>';
    this.tip.style.display='block';
    var tw=this.tip.offsetWidth||120;
    this.tip.style.left=clamp(x+12,4,Math.max(4,this.W-tw-4))+'px';
    this.tip.style.top=clamp(y-44,4,this.H-56)+'px';
  };
  Chart.prototype.hideTip=function(){this.hover=-1;if(this.tip)this.tip.style.display='none';this.draw();};

  Chart.prototype.bind=function(){
    var self=this,canvas=this.canvas;
    canvas.addEventListener('mousemove',function(e){self.showTip(e.clientX);});
    canvas.addEventListener('mouseleave',function(){self.hideTip();});
    canvas.addEventListener('dblclick',function(){self.initView();self.draw();});
    canvas.addEventListener('wheel',function(e){
      e.preventDefault();
      var rect=canvas.getBoundingClientRect();
      var anchor=self.idxAt(e.clientX-rect.left);
      var span=self.end-self.start;
      var factor=e.deltaY<0?0.8:1.25;
      var ns=clamp(span*factor,self.minSpan,Math.max(self.minSpan,self.values.length-1));
      var ratio=span>0?(anchor-self.start)/span:0.5;
      self.start=anchor-ns*ratio;self.end=self.start+ns;
      self.clampView();self.draw();
    },{passive:false});

    var pointers={},drag=null,pinch=null;
    function count(){return Object.keys(pointers).length;}
    function points(){var a=[];for(var k in pointers)a.push(pointers[k]);return a;}
    function dist(a,b){var dx=a.x-b.x,dy=a.y-b.y;return Math.sqrt(dx*dx+dy*dy)||1;}
    canvas.addEventListener('pointerdown',function(e){
      canvas.setPointerCapture(e.pointerId);
      pointers[e.pointerId]={x:e.clientX,y:e.clientY};
      if(count()===1){
        drag={x:e.clientX,start:self.start,end:self.end};
      }else if(count()===2){
        var ps=points();
        pinch={d:dist(ps[0],ps[1]),span:self.end-self.start,center:(self.start+self.end)/2};
        drag=null;
      }
    });
    canvas.addEventListener('pointermove',function(e){
      if(!pointers[e.pointerId])return;
      pointers[e.pointerId]={x:e.clientX,y:e.clientY};
      if(count()===2&&pinch){
        var ps=points(),d=dist(ps[0],ps[1]);
        var ns=clamp(pinch.span*pinch.d/d,self.minSpan,Math.max(self.minSpan,self.values.length-1));
        self.start=pinch.center-ns/2;self.end=self.start+ns;
        self.clampView();self.draw();
      }else if(drag&&count()===1){
        var p=self.plot();
        var shift=-(e.clientX-drag.x)/p.w*(drag.end-drag.start);
        self.start=drag.start+shift;self.end=drag.end+shift;
        self.clampView();self.draw();
      }else{
        self.showTip(e.clientX);
      }
    });
    function up(e){
      delete pointers[e.pointerId];
      if(count()<2)pinch=null;
      if(count()===0)drag=null;
    }
    canvas.addEventListener('pointerup',up);
    canvas.addEventListener('pointercancel',up);
    canvas.addEventListener('pointerleave',function(){if(count()===0)self.hideTip();});
  };

  if(DATA.pv&&DATA.pv.values&&DATA.pv.values.length){new Chart(DATA.pv,'pv');}
  if(DATA.traffic&&DATA.traffic.values&&DATA.traffic.values.length){new Chart(DATA.traffic,'traffic');}
})();
`

// opsCSS 运维报告独立样式（不依赖主报告 css）
const opsCSS = `
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:#eef1f7;color:#263238;font-size:14px;line-height:1.55}
.wrap{max-width:1280px;margin:0 auto;padding:24px}
.hero{background:linear-gradient(120deg,#1a237e,#283593 55%,#1565c0);color:#fff;border-radius:14px;padding:28px 30px;margin-bottom:20px;box-shadow:0 8px 24px rgba(26,35,126,.25)}
.hero .tag{font-size:12px;letter-spacing:2px;color:#c5cae9;margin-bottom:6px}
.hero h1{font-size:26px;font-weight:700;margin-bottom:6px}
.hero .sub{color:#e8eaf6;font-size:13px;margin-bottom:10px}
.hero .meta{color:#c5cae9;font-size:12.5px;margin-top:3px}
.badge{display:inline-block;margin:10px 8px 0 0;background:rgba(255,255,255,.14);border:1px solid rgba(255,255,255,.28);color:#fff;padding:3px 10px;border-radius:20px;font-size:12px}
.badge.ok{background:rgba(67,160,71,.35);border-color:rgba(129,199,132,.7)}
section{background:#fff;border-radius:12px;padding:22px 24px;margin-bottom:20px;box-shadow:0 2px 12px rgba(0,0,0,.06)}
h2{font-size:17px;color:#1a237e;padding-bottom:10px;margin-bottom:14px;border-bottom:2px solid #e8eaf6;display:flex;align-items:center;gap:8px}
h3{font-size:14px;color:#3949ab;margin:18px 0 8px}
h3:first-child{margin-top:0}
.aud{font-size:11px;font-weight:400;color:#7986cb;background:#e8eaf6;padding:2px 8px;border-radius:10px}
.kpis{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;background:transparent;box-shadow:none;padding:0}
.kpis.small{grid-template-columns:repeat(5,1fr);margin-bottom:8px}
.kpi{background:#fff;border-radius:10px;padding:14px 16px;box-shadow:0 2px 10px rgba(0,0,0,.05);border-left:4px solid #3949ab}
.k-label{font-size:12px;color:#78909c}
.k-value{font-size:22px;font-weight:700;color:#1a237e;margin:2px 0}
.k-extra{font-size:12px;color:#90a4ae}
.grid2{display:grid;grid-template-columns:1fr 1fr;gap:22px}
.grid3{display:grid;grid-template-columns:repeat(3,1fr);gap:18px}
.chart{width:100%;height:240px;display:block;background:#fbfcff;border-radius:8px}
.chart .grid{stroke:#e3e7f0;stroke-width:1}
.chart .axis{fill:#90a4ae;font-size:11px}
.chart .line{fill:none;stroke:#3949ab;stroke-width:2.2;stroke-linejoin:round;stroke-linecap:round}
.chart .area{fill:rgba(57,73,171,.14)}
.stack{display:flex;height:26px;border-radius:6px;overflow:hidden;background:#eceff1;margin-bottom:8px}
.stack span{display:block;height:100%}
.legend{display:flex;flex-wrap:wrap;gap:14px;font-size:12px;color:#546e7a;margin-bottom:10px}
.legend i{display:inline-block;width:10px;height:10px;border-radius:3px;margin-right:5px}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{padding:7px 8px;border-bottom:1px solid #eceff1;text-align:left}
th{background:#f5f7fb;color:#546e7a;font-weight:600;font-size:12px}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
td.mono,.mono{font-family:ui-monospace,Consolas,monospace;font-size:12px;word-break:break-all}
.bars{display:flex;flex-direction:column;gap:7px}
.bar{display:grid;grid-template-columns:minmax(110px,34%) 1fr auto;gap:10px;align-items:center}
.bar-label{font-size:12.5px;color:#37474f;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bar-track{background:#eceff1;border-radius:5px;height:12px;overflow:hidden}
.bar-fill{height:100%;border-radius:5px}
.bar-val{font-size:12px;color:#546e7a;font-variant-numeric:tabular-nums;white-space:nowrap}
.code{padding:2px 7px;border-radius:6px;font-size:12px;background:#eceff1}
.code.s2xx{background:#e8f5e9;color:#2e7d32}.code.s3xx{background:#e1f5fe;color:#0277bd}
.code.s4xx{background:#fff3e0;color:#ef6c00}.code.s5xx{background:#ffebee;color:#c62828}
.muted{color:#90a4ae;font-size:13px}
footer{color:#78909c;font-size:12px;text-align:center;padding:8px 4px 26px;line-height:1.8}
footer code{background:#e8eaf6;padding:1px 5px;border-radius:4px;color:#3949ab}
.chart-card{background:#fbfcff;border:1px solid #eceff1;border-radius:10px;padding:12px 14px 8px}
.chart-head{display:flex;align-items:center;gap:10px;margin-bottom:2px}
.chart-head h3{margin:0;flex:1}
.zoom{display:flex;gap:4px}
.zoom button{border:1px solid #c5cae9;background:#fff;color:#3949ab;border-radius:6px;font-size:11px;padding:2px 7px;cursor:pointer;font-family:inherit}
.zoom button:hover,.zoom button.on{background:#3949ab;color:#fff;border-color:#3949ab}
.unit{font-size:11px;color:#90a4ae;white-space:nowrap}
.canvas-wrap{position:relative;touch-action:none;margin-top:4px}
.canvas-wrap canvas{display:block;width:100%;height:280px;cursor:crosshair}
.tip{position:absolute;display:none;pointer-events:none;background:rgba(26,35,126,.94);color:#fff;font-size:12px;padding:6px 9px;border-radius:7px;z-index:3;box-shadow:0 4px 12px rgba(0,0,0,.18);white-space:nowrap;line-height:1.4}
.tip b{display:block;font-size:11px;color:#c5cae9;font-weight:500}
.chart-note{font-size:11.5px;color:#78909c;margin-top:4px;min-height:16px}
@media(max-width:1000px){.kpis,.kpis.small{grid-template-columns:repeat(2,1fr)}.grid2,.grid3{grid-template-columns:1fr}}
`
