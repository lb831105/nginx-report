package report

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
	"nginx-report/internal/detect"
	"nginx-report/internal/history"
)

// 日志里的 request_uri 完全由客户端控制，必须转义后才能写进 HTML，
// 否则可以注入 <script>（报告通常在浏览器里打开看）。
func TestHTMLEscapesLogDerivedStrings(t *testing.T) {
	cfg := config.Default()

	evilURI := `/x"><script>alert(1)</script>`
	evilStatus := `2<script>`

	grand := analyzer.NewStats()
	grand.Agg.Total = 10
	grand.Agg.UriStat = map[string]*analyzer.UriStat{evilURI: {Calls: 10, Errors: 3, Slows: 4, TimeS: 1.5}}
	grand.Agg.StatusCnt = map[string]int64{evilStatus: 10}
	grand.Agg.ClientCnt = map[string]int64{"1.1.1.1": 10}
	grand.Agg.TimeBuckets[0] = 10
	// 新增的表格同样渲染日志内容，必须全部转义
	evilUp := `10.9.9.9:80"<img src=x onerror=alert(1)>`
	grand.Agg.UpstreamStat = map[string]*analyzer.UpstreamStat{evilUp: {Calls: 5, Errors: 2, TimeC: 5, TimeS: 0.05}}
	grand.Agg.MethodCnt = map[string]int64{"GE<script>T": 5}
	grand.Agg.HourCnt[3] = 5
	grand.Agg.SlotCnt[7] = 5

	days := []analyzer.DaySummary{{
		Day:      `2026-09-01<script>`,
		Total:    10,
		PeakCnt:  10,
		PeakTime: time.Now(),
	}}

	out := HTML(Input{
		Cfg: cfg, Days: days, Stats: grand,
		StartDate: "2026-09-01", EndDate: "2026-09-07",
		Issues: []detect.Issue{{Level: "error", Title: "t<script>", Desc: "d<script>"}},
	})

	for _, bad := range []string{"<script>", "<img"} {
		if strings.Contains(out, bad) {
			t.Fatalf("报告中出现了未转义的 %s，存在 HTML 注入风险", bad)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatal("预期的转义结果未出现")
	}
	// 正常的转义字符不应被二次编码成 &amp;lt;
	if strings.Contains(out, "&amp;lt;") {
		t.Fatal("发生了重复转义")
	}
}

// Total 为 0 时不应出现 NaN / Inf
func TestHTMLNoNaNWhenEmpty(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	out := HTML(Input{
		Cfg: cfg, Stats: grand,
		StartDate: "2026-09-01", EndDate: "2026-09-07",
	})
	for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("空统计时报告出现 %s", bad)
		}
	}
}

// 报告需覆盖多受众板块；缺数据的板块要自动隐藏，不能出现空的"上游服务"表
func TestHTMLSectionsByAudience(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	grand.Agg.Total = 100
	grand.Agg.SizeTotal = 1 << 20
	grand.Agg.TimeSum = 5.0
	grand.Agg.StatusCnt = map[string]int64{"200": 90, "404": 7, "502": 3}
	grand.Agg.ErrorCount = 10
	grand.Agg.ErrorStatus = map[string]int64{"404": 7, "502": 3}
	grand.Agg.SlowCount = 4
	grand.Agg.UriStat = uriStatMap(map[string]int64{"/a": 60, "/b": 40})

	uriStatSet(&grand.Agg, "errors", map[string]int64{"/b": 10})
	uriStatSet(&grand.Agg, "slows", map[string]int64{"/b": 4})
	grand.Agg.ClientCnt = map[string]int64{"10.9.9.1": 70, "10.9.9.2": 30}
	grand.Agg.UpstreamStat = upstreamStatMap(map[string]int64{"10.9.8.1:8080": 60, "10.9.8.2:8080": 40})
	grand.Agg.MethodCnt = map[string]int64{"GET": 80, "POST": 20}
	grand.Agg.HourCnt[9] = 80
	grand.Agg.HourCnt[3] = 20
	grand.Agg.SlotCnt[108] = 90
	grand.Agg.TimeBuckets[0] = 20
	grand.Agg.TimeBuckets[60] = 80

	days := []analyzer.DaySummary{
		{Day: "2026-10-30", Total: 20, Size: 100, AvgTime: 0.01, ErrorCount: 0, PeakCnt: 1, PeakTime: time.Now()},
		{Day: "2026-11-01", Total: 80, Size: 900, AvgTime: 0.02, ErrorCount: 10, PeakCnt: 2, PeakTime: time.Now()},
	}
	// 周报由日记录聚合而来时，每天都有自己的 SlotCnt/HourCnt（供"每天一行"曲线用）
	for _, d := range days {
		da := analyzer.NewAgg()
		da.Total = d.Total
		da.SlotCnt[100] = d.Total / 2
		da.SlotCnt[101] = d.Total - d.Total/2
		da.HourCnt[10] = d.Total
		grand.MergeDay(d.Day, da)
	}

	out := HTML(Input{
		Cfg: cfg, Days: days, Stats: grand,
		StartDate: "2026-10-26", EndDate: "2026-11-01",
		Prev: &history.Summary{Total: 80, PeakQPS: 2, ErrorRate: 5.0, AvgTime: 0.03, UniqueIPs: 2},
		Recent: []*history.Summary{
			{StartDate: "2026-10-12", EndDate: "2026-10-18", Total: 50, ErrorRate: 0.5},
			{StartDate: "2026-10-19", EndDate: "2026-10-25", Total: 70, ErrorRate: 1.5},
		},
	})

	for _, want := range []string{
		"执行摘要", "核心指标", "本周问题发现", "业务趋势", "每日运行明细",
		"运维视角", "开发视角", "响应性能指标", "指标口径说明",
		"健康度", "近几周访问量趋势", "QPS 曲线", "工作日日均", "上游（后端）服务健康度",
		"客户端来源 Top 10", "核心接口 Top 15", "错误次数最多的接口", "成功率",
		"每天一条", "每天一行", "环比",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("报告缺少板块/内容: %s", want)
		}
	}
	// 上周有数据时应出现环比箭头
	if !strings.Contains(out, "↑") && !strings.Contains(out, "↓") {
		t.Error("有上周数据时未出现环比箭头")
	}
}

// 没有上游信息时不应渲染上游表
func TestHTMLHidesUpstreamSectionWhenNoUpstream(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	grand.Agg.Total = 1
	grand.Agg.StatusCnt = map[string]int64{"200": 1}
	grand.Agg.TimeBuckets[0] = 1
	out := HTML(Input{Cfg: cfg, Stats: grand, StartDate: "2026-10-26", EndDate: "2026-11-01"})
	if strings.Contains(out, "上游（后端）服务健康度") {
		t.Error("无上游数据时不应出现上游健康度表")
	}
}

// 报告是手写字符串拼接，标签必须闭合（新增板块时容易漏 </tbody></table>）
func TestHTMLTagsBalanced(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	grand.Agg.Total = 10
	grand.Agg.StatusCnt = map[string]int64{"200": 9, "500": 1}
	grand.Agg.ErrorCount = 1
	grand.Agg.ErrorStatus = map[string]int64{"500": 1}
	grand.Agg.UriStat = uriStatMap(map[string]int64{"/a": 10})
	uriStatSet(&grand.Agg, "errors", map[string]int64{"/a": 1})
	uriStatSet(&grand.Agg, "slows", map[string]int64{"/a": 1})
	grand.Agg.ClientCnt = map[string]int64{"10.9.9.1": 10}
	grand.Agg.MethodCnt = map[string]int64{"GET": 10}
	grand.Agg.UpstreamStat = upstreamStatMap(map[string]int64{"10.9.8.1:8080": 10})
	grand.Agg.TimeBuckets[0] = 10
	grand.Agg.SlotCnt[3] = 10
	grand.Agg.HourCnt[10] = 10

	out := HTML(Input{
		Cfg: cfg, Stats: grand,
		Days:      []analyzer.DaySummary{{Day: "2026-10-30", Total: 10, ErrorCount: 1}},
		StartDate: "2026-10-26", EndDate: "2026-11-01",
		Prev:   &history.Summary{Total: 8, PeakQPS: 1},
		Recent: []*history.Summary{{StartDate: "2026-10-19", EndDate: "2026-10-25", Total: 8}},
		Issues: []detect.Issue{{Level: "error", Title: "t", Desc: "d"}},
	})

	for _, tag := range []string{"div", "table", "tbody", "thead", "tr", "td", "th", "ul", "li", "h2", "h3"} {
		open := strings.Count(out, "<"+tag+">") + strings.Count(out, "<"+tag+" ")
		close := strings.Count(out, "</"+tag+">")
		if open != close {
			t.Errorf("<%s> 未闭合: 开 %d / 闭 %d", tag, open, close)
		}
	}
}

// 数据量极小（工作日总量 < 天数）时不能出现 +Inf / NaN
func TestHTMLTinyWeekendRatioNoInf(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	grand.Agg.Total = 3
	grand.Agg.StatusCnt = map[string]int64{"200": 3}
	grand.Agg.TimeBuckets[0] = 3
	out := HTML(Input{
		Cfg: cfg, Stats: grand,
		Days: []analyzer.DaySummary{
			{Day: "2026-10-26", Total: 1}, // 周一 1 条
			{Day: "2026-10-31", Total: 2}, // 周六 2 条
		},
		StartDate: "2026-10-26", EndDate: "2026-11-01",
	})
	for _, bad := range []string{"+Inf", "NaN", "%!f"} {
		if strings.Contains(out, bad) {
			t.Errorf("出现异常数值 %s", bad)
		}
	}
}

// 单天报告：24 小时分布用绝对值（含最忙/最闲），QPS 一条曲线，不显示"每日"柱图
func TestHTMLSingleDaySections(t *testing.T) {
	cfg := config.Default()
	grand := analyzer.NewStats()
	grand.Agg.Total = 100
	grand.Agg.StatusCnt = map[string]int64{"200": 100}
	grand.Agg.TimeBuckets[0] = 100
	grand.Agg.HourCnt[10] = 90
	grand.Agg.HourCnt[3] = 10
	grand.Agg.SlotCnt[120] = 50
	grand.Agg.UriStat = uriStatMap(map[string]int64{"/api/a": 60, "/static/x.js": 40})

	grand.Agg.ClientCnt = map[string]int64{"10.9.9.1": 100}
	grand.Agg.MethodCnt = map[string]int64{"GET": 100}
	grand.Agg.ErrorStatus = map[string]int64{}
	grand.MergeDay("2026-11-02", &grand.Agg)

	out := HTML(Input{
		Cfg: cfg, Stats: grand,
		Days:      []analyzer.DaySummary{{Day: "2026-11-02", Total: 100}},
		StartDate: "2026-11-02", EndDate: "2026-11-02",
	})
	for _, want := range []string{"最忙时段", "QPS 曲线", "静态资源", "报告周期：2026-11-02"} {
		if !strings.Contains(out, want) {
			t.Errorf("单天报告缺少: %s", want)
		}
	}
	// 单天不应出现"每天一行"和区间横线
	if strings.Contains(out, "每天一行") || strings.Contains(out, "2026-11-02 — 2026-11-02") {
		t.Error("单天报告不应出现多天/区间写法")
	}
	// 单天不应显示每日柱图（只有一根柱子）
	if strings.Contains(out, "本周每日访问量") {
		t.Error("单天报告不应显示每日柱图")
	}
	// 静态资源不应出现在接口表主行，而是单独汇总
	if strings.Contains(out, "/static/x.js") {
		t.Error("静态资源不应混进接口 Top 表")
	}
}

// ---------------------------------------------------------------- 测试辅助

// uriStatMap 由 map[uri]calls 构造接口统计表
func uriStatMap(calls map[string]int64) map[string]*analyzer.UriStat {
	out := make(map[string]*analyzer.UriStat, len(calls))
	for k, v := range calls {
		out[k] = &analyzer.UriStat{Calls: v}
	}
	return out
}

// uriStatSet 往接口统计表里补耗时/错误/慢请求字段
func uriStatSet(a *analyzer.Agg, field string, vals map[string]int64) {
	for k, v := range vals {
		u := a.Uri(k)
		switch field {
		case "errors":
			u.Errors = v
		case "slows":
			u.Slows = v
		case "time":
			u.TimeS = float64(v)
		}
	}
}

// upstreamStatMap 由 map[addr]calls 构造上游统计表
func upstreamStatMap(calls map[string]int64) map[string]*analyzer.UpstreamStat {
	out := make(map[string]*analyzer.UpstreamStat, len(calls))
	for k, v := range calls {
		out[k] = &analyzer.UpstreamStat{Calls: v, TimeC: v, TimeS: float64(v) * 0.05}
	}
	return out
}

// 静态资源识别：后缀、路径段、大小写、query 都要覆盖
func TestIsStaticAsset(t *testing.T) {
	cases := map[string]bool{
		"/pro/assets/user-b8d95580.svg":   true,
		"/pro/assets/reload-0c860b1f.svg": true,
		"/static/app.a1b2c3.js":           true,
		"/static/app.9f8e7d":              true, // 无扩展名但在 static 段下
		"/assets/img/logo.PNG":            true, // 大小写
		"/css/main.css?v=2":               true, // 带 query
		"/fonts/song.woff2":               true,
		"/bbt-api/org/online/heartbeat":   false,
		"/bbt-api/sys/dict/item/select":   false,
		"/":                               false,
		"/manage/index.html":              false, // html 不算静态（可能是页面/接口）
		"/bbt-api/assets/list":            false, // 业务接口里带 assets 关键字但不在静态目录段
	}
	for uri, want := range cases {
		if got := isStaticAsset(uri); got != want {
			t.Errorf("isStaticAsset(%q)=%v, want %v", uri, got, want)
		}
	}
}

// 接口 Top 表默认排除静态资源并汇总成一行；关掉开关后恢复原样
func TestHTMLStaticExclude(t *testing.T) {
	mk := func() *analyzer.FileStats {
		g := analyzer.NewStats()
		g.Agg.Total = 1000
		g.Agg.StatusCnt = map[string]int64{"200": 1000}
		g.Agg.TimeBuckets[0] = 1000
		g.Agg.UriStat = map[string]*analyzer.UriStat{
			"/bbt-api/a":        {Calls: 100, TimeS: 1.0},
			"/pro/assets/x.svg": {Calls: 400, TimeS: 0.1},
			"/static/app.js":    {Calls: 500, TimeS: 0.2},
		}
		g.Agg.ClientCnt = map[string]int64{"10.9.9.1": 1000}
		g.MergeDay("2026-11-02", &g.Agg)
		return g
	}
	in := Input{
		Cfg: config.Default(), Stats: mk(),
		Days:      []analyzer.DaySummary{{Day: "2026-11-02", Total: 1000}},
		StartDate: "2026-11-02", EndDate: "2026-11-02",
	}
	out := HTML(in)

	if !strings.Contains(out, "/bbt-api/a") {
		t.Error("业务接口应出现在 Top 表")
	}
	for _, asset := range []string{"/pro/assets/x.svg", "/static/app.js"} {
		if strings.Contains(out, asset) {
			t.Errorf("静态资源 %s 不应出现在接口 Top 表", asset)
		}
	}
	if !strings.Contains(out, "(静态资源 2 个") {
		t.Error("应有静态资源汇总行")
	}
	if !strings.Contains(out, "已排除静态资源") {
		t.Error("标题应说明已排除静态资源")
	}

	// 关闭排除：静态资源重新出现，且没有汇总行
	in.Cfg.StaticExclude = false
	out = HTML(in)
	if !strings.Contains(out, "/pro/assets/x.svg") {
		t.Error("关闭排除后静态资源应回到表里")
	}
	if strings.Contains(out, "(静态资源 2 个") {
		t.Error("关闭排除后不应再有汇总行")
	}
}

// 时间标签必须按桶宽度算：24 小时图是整点区间，5 分钟图是时刻。
// 之前的 bug 是全都用 5 分钟标签，导致小时级峰值显示成「00:45」这种形式。
func TestBucketLabelByStep(t *testing.T) {
	cases := []struct {
		idx, step int
		want      string
	}{
		{0, 60, "00:00–01:00"},
		{9, 60, "09:00–10:00"},
		{23, 60, "23:00–24:00"},
		{0, 5, "00:00"},
		{108, 5, "09:00"},
		{181, 5, "15:05"},
		{287, 5, "23:55"},
	}
	for _, c := range cases {
		if got := bucketLabel(c.idx, c.step); got != c.want {
			t.Errorf("bucketLabel(%d, %d) = %q, want %q", c.idx, c.step, got, c.want)
		}
	}
}

// 多天报告：QPS 每行的峰值时刻/数值必须是「当天某个 5 分钟桶」，不是全天合计
func TestHTMLQPSRowsPerDay(t *testing.T) {
	// 造 3 天：每天 600 次，全部集中在 hour 指定的一小时内 → 每个 5 分钟桶 50 次
	hours := []int{9, 15, 20}
	days := []string{"2026-10-26", "2026-10-27", "2026-10-28"}
	st := analyzer.NewStats()
	var summaries []analyzer.DaySummary
	for i, day := range days {
		da := analyzer.NewAgg()
		da.Total = 600
		da.HourCnt[hours[i]] = 600
		da.SlotCnt[hours[i]*12] = 50 // 该小时第一个 5 分钟桶
		da.SlotCnt[hours[i]*12+1] = 50
		st.MergeDay(day, da)
		summaries = append(summaries, analyzer.DaySummary{Day: day, Total: 600})
	}
	out := HTML(Input{
		Cfg: config.Default(), Stats: st, Days: summaries,
		StartDate: days[0], EndDate: days[2],
	})

	for i, day := range days {
		want := fmt.Sprintf("峰值 %02d:00", hours[i])
		if !strings.Contains(out, want) {
			t.Errorf("%s 的 5 分钟峰值应显示 %q", day, want)
		}
		// 全天合计 600 不能出现在行内峰值位置（那是 bug 现象）
		if strings.Contains(out, "峰值 00:45") || strings.Contains(out, fmt.Sprintf("峰值 %02d:45", hours[i])) {
			t.Errorf("%s 的峰值时刻被按错误桶宽渲染", day)
		}
	}
	// 小时级：应显示整点区间
	if !strings.Contains(out, "峰值 09:00–10:00") {
		t.Error("24 小时图应显示整点区间（09:00–10:00）")
	}
	// 5 分钟峰值占比：50/600 = 8.3%
	if !strings.Contains(out, "占全天 8.3%") {
		t.Error("5 分钟峰值应带占全天比例，便于自查")
	}
	if !strings.Contains(out, "req/s") {
		t.Error("5 分钟峰值应换算成 req/s，与峰值 QPS 卡片同口径")
	}
}

// 单个时段桶大于当天总量时必须显式报异常（数据被重复累加的症状）
func TestPeakNoteFlagsImpossibleValue(t *testing.T) {
	got := peakNote(1200, 5, 600)
	if !strings.Contains(got, "数据异常") {
		t.Errorf("峰值大于全天总量时应提示数据异常，实际: %q", got)
	}
	// 正常情况：50/600 = 8.3%
	ok := peakNote(50, 5, 600)
	if !strings.Contains(ok, "占全天 8.3%") || strings.Contains(ok, "异常") {
		t.Errorf("正常占比输出不符: %q", ok)
	}
	// 小时级：给区间占比
	h := peakNote(600, 60, 600)
	if !strings.Contains(h, "占全天 100.0%") {
		t.Errorf("小时级占比输出不符: %q", h)
	}
	// 小速率不要显示成 0
	if r := rateStr(0.17); r != "0.2" {
		t.Errorf("rateStr(0.17)=%q, want 0.2", r)
	}
}
