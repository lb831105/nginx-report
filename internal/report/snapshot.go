package report

import (
	"sort"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
	"nginx-report/internal/detect"
	"nginx-report/internal/history"
)

// snapshot 是 HTML / PDF / Excel 三种输出共用的中间数据。
// 三个渲染器都从同一份数字出发，避免"同一周的报告，HTML 和 Excel 数字不一样"。
type snapshot struct {
	Stats *analyzer.FileStats // 原始统计量（部分明细需要直接查表）
	Title string              // 报告标题（cfg.ReportTitle）

	StartDate string
	EndDate   string
	Days      []analyzer.DaySummary
	DayCount  int

	Total      int64
	SizeTotal  int64
	AvgMs      float64
	TimedCount int64   // 带耗时记录的请求数
	TimedShare float64 // 有耗时请求占比（%）
	P50, P90   float64
	P95, P99   float64
	PeakQPS    int64
	PeakTime   time.Time
	ErrorCount int64
	SlowCount  int64
	ErrorRate  float64
	SlowRate   float64
	SlowLabel  string // 慢请求阈值文案，如 "1s" / "500ms"
	TimeField  string // 响应时间口径：auto / upstream / response
	AvgQPS     float64
	ClientNum  int
	Score      float64
	Grade      string
	GradeText  string

	StatusGroups []groupCount // 2xx/3xx/4xx/5xx/其他
	StatusCodes  []kv         // 各状态码明细，按次数降序
	Methods      []kv

	TopAPIs []uriStat
	TopErr  []uriStat
	Static  staticSum
	Ups     []upStat
	Clients []kv
	Hours   map[int]int64
	Slots   []int64
	Series  []daySeriesData // 每天的时间序列（QPS 曲线 / 24h 分布按天画）

	// 区间内 5 分钟峰值（按天取最大，不是多天相加）
	SlotPeakVal int64
	SlotPeakDay string
	SlotPeakAt  string
	TimeDist    []kv // 响应时间直方图（有值的档）
	SlowTop     []uriStat

	WeekdayAvg int64
	WeekendAvg int64
	WeekdayN   int64
	WeekendN   int64

	Prev     *historySummary
	Recent   []*historySummary
	Issues   []detect.Issue
	SiteRows []SiteRow
}

type groupCount struct {
	Name  string
	Count int64
}

type staticSum struct {
	Calls int64
	Errs  int64
	Slows int64
	AvgMs float64
	Files int
}

type historySummary struct {
	Start, End string
	Total      int64
	ErrorCount int64
	Size       int64
}

// buildSnapshot 汇总三种输出共用的指标
func buildSnapshot(in Input) *snapshot {
	g := in.Stats
	if g == nil {
		g = analyzer.NewStats()
	}
	title := titleOf(in.Cfg)
	if in.SiteName != "" {
		title = in.SiteName + " - " + title
	}
	s := &snapshot{
		Stats:     g,
		Title:     title,
		StartDate: in.StartDate,
		EndDate:   in.EndDate,
		Days:      in.Days,
		DayCount:  windowDayCount(in.StartDate, in.EndDate, len(in.Days)),
		Prev:      toHistorySummary(in.Prev),
		Issues:    in.Issues,
		SiteRows:  in.SiteRows,
	}
	for _, r := range in.Recent {
		s.Recent = append(s.Recent, toHistorySummary(r))
	}
	s.Total = g.Total
	s.SizeTotal = g.SizeTotal
	s.AvgMs = g.AvgTime() * 1000
	s.TimedCount = g.Timed()
	s.TimedShare = pct(s.TimedCount, s.Total)
	s.P50 = g.Percentile(0.5) * 1000
	s.P90 = g.Percentile(0.90) * 1000
	s.P95 = g.Percentile(0.95) * 1000
	s.P99 = g.Percentile(0.99) * 1000
	pkTime, pkCnt := g.Peak()
	s.PeakTime, s.PeakQPS = pkTime, pkCnt
	s.ErrorCount = g.ErrorCount
	s.SlowCount = g.SlowCount
	s.ErrorRate = pct(s.ErrorCount, s.Total)
	s.SlowRate = pct(s.SlowCount, s.Total)
	s.SlowLabel = slowThresholdText(in.Cfg)
	if in.Cfg != nil {
		s.TimeField = in.Cfg.TimeField
	}
	// 平均 QPS 用「窗口天数」而不是「有数据的天数」：
	// 一周里缺 2 天数据时，除以 7 才能反映真实压力水平（与 HTML 报告口径一致）
	s.AvgQPS = float64(s.Total) / float64(s.DayCount) / 86400
	s.ClientNum = uniqueClients(g.ClientCnt)

	// 状态分组
	byGroup := map[string]int64{}
	for code, n := range g.StatusCnt {
		byGroup[statusClass(code)] += n
	}
	for _, name := range []string{"2xx", "3xx", "4xx", "5xx", "其他"} {
		if n := byGroup[name]; n > 0 {
			s.StatusGroups = append(s.StatusGroups, groupCount{Name: name, Count: n})
		}
	}
	for _, kvp := range topN(g.StatusCnt, 20) {
		s.StatusCodes = append(s.StatusCodes, kvp)
	}
	s.Methods = topN(g.MethodCnt, 10)

	// 接口：默认排除静态资源
	excludeStatic := in.Cfg == nil || in.Cfg.StaticExclude
	if excludeStatic {
		s.TopAPIs = topAPIs(g, 20)
	} else {
		s.TopAPIs = uriStats(g, 20, false)
	}
	s.TopErr = apiErrors(g, 10, excludeStatic)
	if calls, errs, slows, avg, files := staticAgg(g); calls > 0 {
		s.Static = staticSum{Calls: calls, Errs: errs, Slows: slows, AvgMs: avg, Files: files}
	}
	s.Ups = upstreamStats(g, 15)
	s.Clients = topN(g.ClientCnt, 15)
	s.Hours = g.HourCnt
	s.Slots = padSlots(g.SlotCnt)

	// 响应时间直方图：合并成 7 档，和 HTML 报告口径一致
	for i, bucket := range timeBuckets() {
		var cnt int64
		for j := bucket.from; j < bucket.to && j < len(g.TimeBuckets); j++ {
			cnt += g.TimeBuckets[j]
		}
		// ≥1s 这一档还要加上 10 秒以上的溢出桶
		if bucket.label == "≥ 1s" {
			cnt += sumOf(g.TimeOverflow)
		}
		if cnt > 0 {
			s.TimeDist = append(s.TimeDist, kv{Key: bucket.label, Value: cnt})
		}
		_ = i
	}

	// 慢请求最多的接口
	slowMap := map[string]int64{}
	for uri, st := range g.UriStat {
		if st.Slows > 0 {
			slowMap[uri] = st.Slows
		}
	}
	for _, kvp := range topN(slowMap, 10) {
		if u := uriStatOf(g, kvp.Key); u.URI != "" {
			s.SlowTop = append(s.SlowTop, u)
		}
	}

	s.Score, s.GradeText, s.Grade = health(s.ErrorRate, s.SlowRate, s.AvgMs, s.P99)

	// 每天的时间序列：多天报告要按天画，避免 7 天累加到同一组桶里
	for _, d := range in.Days {
		da := g.DayAgg(d.Day)
		if da == nil {
			continue
		}
		row := daySeriesData{Day: d.Day, Total: d.Total}
		for h := 0; h < 24; h++ {
			row.Hours[h] = da.HourCnt[h]
		}
		row.Slots = padSlots(da.SlotCnt)
		if sumOf(row.Slots) == 0 {
			row.Slots = nil
		}
		s.Series = append(s.Series, row)
		if v, at := peakOf(row.Slots, analyzer.SlotMinutes); v > s.SlotPeakVal && len(row.Slots) > 0 {
			s.SlotPeakVal, s.SlotPeakDay, s.SlotPeakAt = v, d.Day, at
		}
	}

	wk, we, nwk, nwe := splitWeekend(in.Days)
	s.WeekdayN, s.WeekendN = nwk, nwe
	if nwk > 0 {
		s.WeekdayAvg = wk / nwk
	}
	if nwe > 0 {
		s.WeekendAvg = we / nwe
	}
	return s
}

func toHistorySummary(h *history.Summary) *historySummary {
	if h == nil {
		return nil
	}
	return &historySummary{Start: h.StartDate, End: h.EndDate, Total: h.Total, ErrorCount: h.ErrorCount, Size: h.Size}
}

// daySeriesData 一天的时间序列
type daySeriesData struct {
	Day   string
	Total int64
	Hours [24]int64
	Slots []int64
}

// timeBucket 一档响应时间桶
type timeBucketDef struct {
	label    string
	from, to int
}

// timeBuckets 把 1001 个 10ms 直方图桶合并成 7 档（和 HTML 报告一致）
func timeBuckets() []timeBucketDef {
	return []timeBucketDef{
		{"< 10ms", 0, 1},
		{"10–50ms", 1, 5},
		{"50–100ms", 5, 10},
		{"100–200ms", 10, 20},
		{"200–500ms", 20, 50},
		{"0.5–1s", 50, 100},
		{"≥ 1s", 100, analyzer.TimeBucketCount},
	}
}

// titleOf 取报告标题
func titleOf(cfg *config.Config) string {
	if cfg != nil && cfg.ReportTitle != "" {
		return cfg.ReportTitle
	}
	return "Nginx 访问日志统计报告"
}

// windowDayCount 统计窗口天数（缺日也算）
func windowDayCount(start, end string, fallback int) int {
	sd, err1 := time.Parse("2006-01-02", start)
	ed, err2 := time.Parse("2006-01-02", end)
	if err1 == nil && err2 == nil && !ed.Before(sd) {
		if d := int(ed.Sub(sd).Hours()/24) + 1; d > 0 {
			return d
		}
	}
	if fallback < 1 {
		fallback = 1
	}
	return fallback
}

// uniqueClients 独立客户端数：基数溢出时 map 里会多出一个 (其他) 汇总项，
// 那不是真实客户端，要减掉。
func uniqueClients(m map[string]int64) int {
	n := len(m)
	if _, ok := m[analyzer.OverflowKey]; ok {
		n--
	}
	return n
}

// errorTopStatus 出现过的错误状态码（4xx/5xx），按次数降序
func errorStatusList(g *analyzer.FileStats) []kv {
	var out []kv
	for _, kvp := range topN(g.StatusCnt, 100) {
		if len(kvp.Key) > 0 && (kvp.Key[0] == '4' || kvp.Key[0] == '5') {
			out = append(out, kvp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}
