package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
)

// OpsText 生成面向「运维 / 开发」的纯文本深度分析报告。
//
// 版式对齐原 analyze_log_fast.sh（深度分析版）：
//
//	总PV / 独立IP / 直接访问
//	按分钟 PV 趋势 + 按分钟流量趋势
//	状态码、HTTP 方法、URL 类型
//	Top N URL / 客户端 IP / 大流量 URL
//	错误请求、Top N 404 / 5xx / 400 URL
//	响应时间统计、Top N 慢请求 URL
//	文件类型、设备类型、浏览器、Referer 域名
//	总流量、平均响应大小
//
// 和 HTML/PDF/Excel 共用同一份统计量（Input.Stats），数字必然一致。
func OpsText(in Input) string {
	s := buildSnapshot(in)
	g := in.Stats
	top := opsTopN(in.Cfg)
	var b strings.Builder

	rangeText := in.StartDate
	if in.EndDate != "" && in.EndDate != in.StartDate {
		rangeText = in.StartDate + " ~ " + in.EndDate
	}

	separator := strings.Repeat("=", 41)
	fmt.Fprintf(&b, "%s\n", separator)
	fmt.Fprintf(&b, "%s（运维 / 开发）\n", cleanText(s.Title))
	fmt.Fprintf(&b, "时间范围: %s\n", cleanText(rangeText))
	if in.Cfg != nil && in.Cfg.FromClock != "" {
		fmt.Fprintf(&b, "时间过滤: 每天 %s 之后（对应原脚本 START_TIME）\n", cleanText(in.Cfg.FromClock))
	}
	fmt.Fprintf(&b, "统计粒度: 分钟（1 分钟）\n")
	fmt.Fprintf(&b, "Top 列表显示: %d 条\n", top)
	if in.Source != "" {
		fmt.Fprintf(&b, "日志文件: %s\n", cleanText(in.Source))
	}
	if len(in.Days) > 0 {
		fmt.Fprintf(&b, "有数据的天: %d 天\n", len(in.Days))
	}
	if g != nil && g.StaticSkipped > 0 {
		fmt.Fprintf(&b, "已排除静态资源: %d 条 / %s（css/js/图片/字体等；原始行数 %d）\n",
			g.StaticSkipped, humanBytes(float64(g.StaticBytes)), s.Total+g.StaticSkipped+g.BotSkipped)
	}
	if g != nil && g.BotSkipped > 0 {
		fmt.Fprintf(&b, "已排除爬虫/探针: %d 条（-exclude-bots）\n", g.BotSkipped)
	}
	fmt.Fprintf(&b, "生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "%s\n\n", separator)

	if g == nil || s.Total == 0 {
		b.WriteString("（统计区间内没有有效日志记录）\n")
		return b.String()
	}

	// ---------------- 总量 ----------------
	totalNote := ""
	if in.Cfg != nil && in.Cfg.SkipStatic {
		totalNote = "（仅业务请求，已排除静态资源）"
	}
	fmt.Fprintf(&b, "【总PV】: %d%s\n", s.Total, totalNote)
	fmt.Fprintf(&b, "【独立访客(IP)】: %d\n", s.ClientNum)
	if g.DirectCnt > 0 || len(g.RefererCnt) > 0 {
		fmt.Fprintf(&b, "【直接访问】: %d (%.1f%%)\n", g.DirectCnt, pct(g.DirectCnt, s.Total))
	}
	b.WriteString("\n")
	if txt := OpsSiteComparison(in.SiteRows, s.Total); txt != "" {
		b.WriteString(txt)
	}

	// ---------------- 趋势 ----------------
	b.WriteString("【按 PV 趋势】\n")
	writeMinuteTrend(&b, in)
	b.WriteString("\n")

	b.WriteString("【按 流量趋势】\n")
	writeTrafficTrend(&b, in)
	b.WriteString("\n")

	// ---------------- 状态码 / 方法 / URL 类型 ----------------
	b.WriteString("【状态码分布】\n")
	for _, code := range sortedStatusCodes(g.StatusCnt) {
		n := g.StatusCnt[code]
		fmt.Fprintf(&b, "  %s: %d (%.2f%%)\n", cleanText(code), n, pct(n, s.Total))
	}
	b.WriteString("\n")

	b.WriteString("【HTTP方法统计】\n")
	for _, kvp := range topN(g.MethodCnt, top) {
		fmt.Fprintf(&b, "  %s: %d (%.1f%%)\n", cleanText(kvp.Key), kvp.Value, pct(kvp.Value, s.Total))
	}
	b.WriteString("\n")

	page, other := urlTypeCounts(g)
	fmt.Fprintf(&b, "【URL类型分布】\n")
	fmt.Fprintf(&b, "  业务页面: %d (%.1f%%)\n", page, pct(page, s.Total))
	fmt.Fprintf(&b, "  其他: %d (%.1f%%)\n\n", other, pct(other, s.Total))

	// ---------------- 各类 Top 榜 ----------------
	fmt.Fprintf(&b, "【Top %d URL】\n", top)
	for _, u := range topURIs(g, top) {
		fmt.Fprintf(&b, "  %d\t%s\n", u.Calls, displayURI(u.URI))
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "【Top %d 客户端IP】\n", top)
	for _, c := range topN(g.ClientCnt, top) {
		fmt.Fprintf(&b, "  %d\t%s\n", c.Value, cleanText(c.Key))
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "【Top %d 大流量URL】\n", top)
	for _, u := range topURIByBytes(g, top) {
		fmt.Fprintf(&b, "  %.2f\t%s\n", float64(u.Bytes)/1024/1024, displayURI(u.URI))
	}
	b.WriteString("\n")

	// ---------------- 错误分析 ----------------
	fmt.Fprintf(&b, "【错误请求】: %d (4xx/5xx)，占 %.2f%%\n", s.ErrorCount, s.ErrorRate)
	if len(g.ErrorStatus) > 0 {
		b.WriteString("  错误状态码分布:\n")
		for _, code := range sortedStatusCodes(g.ErrorStatus) {
			n := g.ErrorStatus[code]
			if n == 0 {
				continue
			}
			fmt.Fprintf(&b, "    %s: %d (%.2f%%)\n", cleanText(code), n, pct(n, s.ErrorCount))
		}
	}
	b.WriteString("\n")

	for _, code := range []string{"404", "5xx", "400"} {
		fmt.Fprintf(&b, "【Top %d %s URL】\n", top, code)
		rows := errURIList(g, code, top)
		if len(rows) == 0 {
			b.WriteString("  （无）\n")
		}
		for _, kvp := range rows {
			fmt.Fprintf(&b, "  %d\t%s\n", kvp.Value, displayURI(kvp.Key))
		}
		b.WriteString("\n")
	}

	// ---------------- 响应时间 ----------------
	b.WriteString("【响应时间统计】\n")
	fmt.Fprintf(&b, "  平均响应时间: %.3f s\n", s.AvgMs/1000)
	if g.MaxTime > 0 {
		fmt.Fprintf(&b, "  最大响应时间: %.3f s\n", g.MaxTime)
	}
	fmt.Fprintf(&b, "  慢请求(>%s): %d\n", slowThresholdText(in.Cfg), s.SlowCount)
	bins := responseTimeBins(&g.Agg)
	if len(bins) > 0 {
		b.WriteString("  响应时间分布:\n")
		for _, bin := range bins {
			fmt.Fprintf(&b, "    %s: %d (%.1f%%)\n", cleanText(bin.Label), bin.Count, pct(bin.Count, s.Total))
		}
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "【Top %d 慢请求URL】\n", top)
	slows := slowURIList(g, top)
	if len(slows) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, r := range slows {
		fmt.Fprintf(&b, "  %.3f\t%d\t%s\n", r.Avg, r.Calls, displayURI(r.URI))
	}
	b.WriteString("\n")

	// ---------------- 文件 / 设备 / 浏览器 / Referer ----------------
	fmt.Fprintf(&b, "【文件类型统计】\n")
	types := fileTypeCounts(g)
	if len(types) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, kvp := range types {
		fmt.Fprintf(&b, "  %s: %d (%.1f%%)\n", cleanText(kvp.Key), kvp.Value, pct(kvp.Value, s.Total))
	}
	b.WriteString("\n")

	if len(g.DeviceCnt) > 0 {
		b.WriteString("【设备类型】\n")
		for _, kvp := range topN(g.DeviceCnt, top) {
			fmt.Fprintf(&b, "  %s: %d (%.1f%%)\n", cleanText(kvp.Key), kvp.Value, pct(kvp.Value, s.Total))
		}
		b.WriteString("\n")
	}

	if len(g.BrowserCnt) > 0 {
		b.WriteString("【浏览器统计】\n")
		for _, kvp := range topN(g.BrowserCnt, top) {
			fmt.Fprintf(&b, "  %s: %d (%.1f%%)\n", cleanText(kvp.Key), kvp.Value, pct(kvp.Value, s.Total))
		}
		b.WriteString("\n")
	}

	if len(g.RefererCnt) > 0 {
		fmt.Fprintf(&b, "【Top %d Referer域名】\n", top)
		for _, kvp := range topN(g.RefererCnt, top) {
			fmt.Fprintf(&b, "  %d\t%s\n", kvp.Value, cleanText(kvp.Key))
		}
		b.WriteString("\n")
	}

	// ---------------- 流量与性能 ----------------
	fmt.Fprintf(&b, "【总流量】: %.2f MB\n", float64(s.SizeTotal)/1024/1024)
	fmt.Fprintf(&b, "【平均响应大小】: %.2f KB\n", float64(s.SizeTotal)/float64(s.Total)/1024)
	fmt.Fprintf(&b, "【平均QPS】: %.1f（总请求 / %d 天 / 86400 秒）\n\n", s.AvgQPS, s.DayCount)

	fmt.Fprintf(&b, "【响应性能】平均 %.0f ms | P50 %.0f | P90 %.0f | P95 %.0f | P99 %.0f\n",
		s.AvgMs, s.P50, s.P90, s.P95, s.P99)
	fmt.Fprintf(&b, "  样本: %d 条带耗时记录（占全部请求 %.2f%%）\n", s.TimedCount, s.TimedShare)
	var success, serverErr int64
	for code, n := range g.StatusCnt {
		switch statusClass(code) {
		case "2xx", "3xx":
			success += n
		case "5xx":
			serverErr += n
		}
	}
	fmt.Fprintf(&b, "【请求结果】成功率(2xx+3xx) %.2f%% | 服务端错误率(5xx) %.2f%%\n",
		pct(success, s.Total), pct(serverErr, s.Total))
	if s.PeakQPS > 0 {
		fmt.Fprintf(&b, "【峰值QPS】%d req/s（%s）\n", s.PeakQPS, s.PeakTime.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(&b, "\n%s\n", separator)

	return b.String()
}

// opsTopN 报告里 Top 列表条数（默认 20）
func opsTopN(cfg *config.Config) int {
	if cfg != nil && cfg.TopN > 0 {
		if cfg.TopN > 200 {
			return 200
		}
		return cfg.TopN
	}
	return 20
}

// writeMinuteTrend 输出分钟级 PV：
//   - 单天：逐分钟列出（只列有请求的分钟，等价于原脚本）
//   - 多天：每天一行，给出当天 PV 与峰值分钟，并附整体 24 小时分布
//
// 统计数据来自旧日记录（没有分钟明细）时自动退化为 5 分钟槽 / 小时。
func writeMinuteTrend(b *strings.Builder, in Input) {
	g := in.Stats
	days := make([]analyzer.DaySummary, len(in.Days))
	copy(days, in.Days)
	if len(days) == 0 {
		for day := range g.Days {
			days = append(days, analyzer.DaySummary{Day: day})
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })

	if len(days) == 1 {
		day := days[0].Day
		minutes, step := dayTrend(g, day)
		if len(minutes) == 0 {
			writeHourTrend(b, g.HourCnt, "  （分钟级明细不可用，按小时展示）\n")
			return
		}
		if step > 1 {
			fmt.Fprintf(b, "  （日志已聚合为旧日记录，按 %d 分钟粒度展示）\n", step)
		}
		for i, v := range minutes {
			if v == 0 {
				continue
			}
			fmt.Fprintf(b, "  %s\t%d\n", clockLabel(i, step), v)
		}
		return
	}

	b.WriteString("  （多天：每天一行，峰值按分钟定位）\n")
	for _, d := range days {
		minutes, step := dayTrend(g, d.Day)
		total := d.Total
		if total == 0 {
			total = sumOf(minutes)
		}
		if total == 0 {
			continue
		}
		peak, at := int64(0), -1
		for i, v := range minutes {
			if v > peak {
				peak, at = v, i
			}
		}
		if at < 0 {
			fmt.Fprintf(b, "  %s\t%s\t%d\n", d.Day, weekdayName(d.Day), total)
			continue
		}
		fmt.Fprintf(b, "  %s\t%s\t%d\t峰值 %s（%d 次/分）\n",
			d.Day, weekdayName(d.Day), total, clockLabel(at, step), peak)
	}

	// 24 小时分布作为补充（分钟级曲线跨多天时不适合整表打印）
	b.WriteString("\n【按小时 PV 分布】（全部天数合计）\n")
	writeHourTrend(b, g.HourCnt, "")
}

// writeTrafficTrend 输出分钟级流量趋势：
//   - 单天：逐分钟 MB
//   - 多天：每天一行 MB
func writeTrafficTrend(b *strings.Builder, in Input) {
	g := in.Stats
	days := make([]analyzer.DaySummary, len(in.Days))
	copy(days, in.Days)
	if len(days) == 0 {
		for day := range g.Days {
			days = append(days, analyzer.DaySummary{Day: day})
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })

	if len(days) == 1 {
		da := g.DayAgg(days[0].Day)
		if da == nil || sumOf(da.MinuteBytes) == 0 {
			b.WriteString("  （无分钟级流量数据，旧日记录请重跑 daily）\n")
			return
		}
		for i, v := range da.MinuteBytes {
			if v == 0 {
				continue
			}
			fmt.Fprintf(b, "  %s\t%.2f MB\n", clockLabel(i, 1), float64(v)/1024/1024)
		}
		return
	}

	b.WriteString("  （多天：每天一行）\n")
	for _, d := range days {
		var bytes int64
		if da := g.DayAgg(d.Day); da != nil {
			bytes = sumOf(da.MinuteBytes)
		}
		if bytes == 0 {
			bytes = d.Size
		}
		if bytes == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s\t%.2f MB\n", d.Day, float64(bytes)/1024/1024)
	}
}

// dayTrend 取某天的分钟 / 5 分钟序列。
// 返回 step=1 表示分钟粒度，step=analyzer.SlotMinutes 表示退化为 5 分钟槽。
func dayTrend(g *analyzer.FileStats, day string) ([]int64, int) {
	da := g.DayAgg(day)
	if da == nil {
		return nil, 1
	}
	if len(da.MinuteCnt) > 0 && sumOf(da.MinuteCnt) > 0 {
		return da.MinuteCnt, 1
	}
	if len(da.SlotCnt) > 0 && sumOf(da.SlotCnt) > 0 {
		return da.SlotCnt, analyzer.SlotMinutes
	}
	return nil, 1
}

// writeHourTrend 输出 24 小时分布
func writeHourTrend(b *strings.Builder, hours map[int]int64, emptyHint string) {
	var total int64
	for h := 0; h < 24; h++ {
		total += hours[h]
	}
	if total == 0 {
		if emptyHint != "" {
			b.WriteString(emptyHint)
		} else {
			b.WriteString("  （无小时级数据）\n")
		}
		return
	}
	for h := 0; h < 24; h++ {
		n := hours[h]
		if n == 0 {
			continue
		}
		fmt.Fprintf(b, "  %02d:00-%02d:59\t%d\n", h, h, n)
	}
}

// clockLabel 把桶下标转成 HH:MM
func clockLabel(idx, step int) string {
	if step <= 0 {
		step = 1
	}
	m := idx * step
	return fmt.Sprintf("%02d:%02d", m/60%24, m%60)
}

// sortedStatusCodes 状态码按数值升序
func sortedStatusCodes(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for code := range m {
		out = append(out, code)
	}
	sort.Slice(out, func(i, j int) bool {
		ni, ei := strconv.Atoi(out[i])
		nj, ej := strconv.Atoi(out[j])
		switch {
		case ei == nil && ej == nil:
			return ni < nj
		case ei == nil:
			return true
		case ej == nil:
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// topURIs 按调用量取前 n 个接口（静态资源若未被 -skip-static 排除，这里也不再二次过滤）
func topURIs(g *analyzer.FileStats, n int) []uriStat {
	rows := make([]uriStat, 0, len(g.UriStat))
	for uri, st := range g.UriStat {
		if st == nil || st.Calls == 0 {
			continue
		}
		rows = append(rows, uriStatOf(g, uri))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls == rows[j].Calls {
			return rows[i].URI < rows[j].URI
		}
		return rows[i].Calls > rows[j].Calls
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// topURIByBytes 按响应字节数取前 n 个接口（大流量 URL）
func topURIByBytes(g *analyzer.FileStats, n int) []uriStat {
	rows := make([]uriStat, 0, len(g.UriStat))
	for uri, st := range g.UriStat {
		if st == nil || st.Bytes == 0 {
			continue
		}
		rows = append(rows, uriStatOf(g, uri))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Bytes == rows[j].Bytes {
			return rows[i].URI < rows[j].URI
		}
		return rows[i].Bytes > rows[j].Bytes
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// errURIList 取某个状态码（"404"/"400"）或分组（"5xx"）下错误最多的 URI
func errURIList(g *analyzer.FileStats, status string, n int) []kv {
	counts := map[string]int64{}
	for code, uris := range g.ErrorURI {
		if code != status && !(status == "5xx" && strings.HasPrefix(code, "5")) {
			continue
		}
		for uri, c := range uris {
			counts[uri] += c
		}
	}
	return topN(counts, n)
}

// slowRow 慢请求 URL 行：平均耗时（秒）与调用次数
type slowRow struct {
	URI   string
	Avg   float64
	Calls int64
}

// slowURIList 按「慢请求平均耗时」倒序取前 n 个接口。
// 分母只算慢请求（>1s），否则一个调用量巨大、偶发慢的接口会被平均值掩盖。
func slowURIList(g *analyzer.FileStats, n int) []slowRow {
	rows := make([]slowRow, 0, len(g.UriStat))
	for uri, st := range g.UriStat {
		if st == nil || st.Slows == 0 || st.SlowTimeS <= 0 {
			continue
		}
		rows = append(rows, slowRow{URI: uri, Avg: st.SlowTimeS / float64(st.Slows), Calls: st.Slows})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Avg == rows[j].Avg {
			return rows[i].Calls > rows[j].Calls
		}
		return rows[i].Avg > rows[j].Avg
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// urlTypeCounts 统计「业务页面」与「其他」请求量。
// 判定：路径里带 api 段（/bbt-api/…）或带非 html 扩展名的算「其他」，
// 其余无扩展名 / .html 的算业务页面。
func urlTypeCounts(g *analyzer.FileStats) (page, other int64) {
	for uri, st := range g.UriStat {
		if st == nil {
			continue
		}
		if isBusinessPage(uri) {
			page += st.Calls
		} else {
			other += st.Calls
		}
	}
	return
}

func isBusinessPage(uri string) bool {
	path := uri
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		return false
	}
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if strings.Contains(strings.ToLower(seg), "api") {
			return false
		}
	}
	ext := uriExt(path)
	return ext == "" || ext == "html" || ext == "htm"
}

// fileTypeCounts 文件类型分布。
// 优先用解析时从原始 request_uri（含 query）里识别的结果；
// 旧日记录没有这个字段时，退化为按归一化后的 URL 路径后缀统计。
func fileTypeCounts(g *analyzer.FileStats) []kv {
	if len(g.FileTypeCnt) > 0 {
		return topN(g.FileTypeCnt, 50)
	}
	counts := map[string]int64{}
	for uri, st := range g.UriStat {
		if st == nil || st.Calls == 0 {
			continue
		}
		if ext := uriExt(uri); ext != "" {
			counts[ext] += st.Calls
		}
	}
	return topN(counts, 50)
}

// uriExt 取 URL 路径里的扩展名（小写、不含点）；query 与非法字符返回空
func uriExt(uri string) string {
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	base := uri
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	i := strings.LastIndexByte(base, '.')
	if i <= 0 || i == len(base)-1 {
		return ""
	}
	ext := strings.ToLower(base[i+1:])
	if len(ext) > 10 {
		return ""
	}
	for _, c := range ext {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return ext
}

// timeBin 响应时间分档
type timeBin struct {
	Label string
	Count int64
}

// responseTimeBins 对齐原脚本的 6 档响应时间分布（分母是总 PV）
func responseTimeBins(g *analyzer.Agg) []timeBin {
	if g == nil || len(g.TimeBuckets) == 0 {
		return nil
	}
	span := func(fromMS, toMS int) int64 {
		lo := fromMS / analyzer.TimeBucketMS
		hi := toMS / analyzer.TimeBucketMS
		if lo < 0 {
			lo = 0
		}
		if hi > len(g.TimeBuckets) {
			hi = len(g.TimeBuckets)
		}
		var n int64
		for i := lo; i < hi; i++ {
			n += g.TimeBuckets[i]
		}
		return n
	}
	defs := []struct {
		label    string
		from, to int
	}{
		{"<0.1s", 0, 100},
		{"0.1-0.5s", 100, 500},
		{"0.5-1s", 500, 1000},
		{"1-3s", 1000, 3000},
		{"3-5s", 3000, 5000},
		{">5s", 5000, analyzer.TimeBucketCount * analyzer.TimeBucketMS},
	}
	var out []timeBin
	for _, d := range defs {
		n := span(d.from, d.to)
		if d.label == ">5s" {
			n += sumOf(g.TimeOverflow)
		}
		if n > 0 {
			out = append(out, timeBin{Label: d.label, Count: n})
		}
	}
	return out
}
