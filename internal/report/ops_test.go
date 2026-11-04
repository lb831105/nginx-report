package report

import (
	"strings"
	"testing"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
)

// opsSampleInput 构造一份「文件级统计 + 当天明细」的输入。
// 直接用 analyzer.FileStats.Days 挂当天数据，避免 MergeDay 会把文件级 Agg 再加一遍。
func opsSampleInput() Input {
	g := analyzer.NewStats()
	g.Agg.Total = 1000
	g.Agg.SizeTotal = 5 << 20
	g.Agg.ErrorCount = 60
	g.Agg.SlowCount = 12
	g.Agg.TimeSum = 100
	g.Agg.TimedCount = 1000
	g.Agg.StatusCnt = map[string]int64{"200": 900, "404": 40, "502": 20}
	g.Agg.ErrorStatus = map[string]int64{"404": 40, "502": 20}
	g.Agg.ErrorURI = map[string]map[string]int64{
		"400": {"": 20},
		"404": {"/api/b": 40},
		"502": {"/api/b": 20},
	}
	g.Agg.DeviceCnt = map[string]int64{"移动端": 900, "桌面端": 100}
	g.Agg.BrowserCnt = map[string]int64{"Chrome": 800, "Safari": 200}
	g.Agg.RefererCnt = map[string]int64{"a.example:80": 500}
	g.Agg.DirectCnt = 100
	g.Agg.MaxTime = 182.319
	g.Agg.FileTypeCnt = map[string]int64{"xlsx": 3, "pdf": 2}
	g.Agg.ClientCnt = map[string]int64{"[IP]": 700, "10.0.0.9": 300}
	g.Agg.MethodCnt = map[string]int64{"GET": 950, "POST": 50}
	g.Agg.UriStat = map[string]*analyzer.UriStat{
		"/api/a":            {Calls: 700, TimeS: 70, Errors: 5, Slows: 3, SlowTimeS: 30},
		"/api/b":            {Calls: 300, TimeS: 300, Errors: 20, Slows: 9, SlowTimeS: 90},
		"/pro/assets/x.svg": {Calls: 4000, TimeS: 4, Bytes: 4 << 20},
	}
	g.Agg.TimeBuckets[1] = 200
	g.Agg.TimeBuckets[30] = 700
	g.Agg.TimeBuckets[120] = 100

	day := analyzer.NewAgg()
	day.Total = 1000
	day.MinuteCnt[10*60+5] = 5
	day.MinuteCnt[10*60+6] = 995
	day.MinuteBytes[10*60+5] = 1 << 20
	day.MinuteBytes[10*60+6] = 4 << 20
	g.Days["2026-10-26"] = day

	return Input{
		Cfg:   config.Default(),
		Stats: g,
		Days: []analyzer.DaySummary{{
			Day: "2026-10-26", Total: 1000, PeakCnt: 30, Size: 5 << 20, AvgTime: 0.1,
		}},
		StartDate: "2026-10-26",
		EndDate:   "2026-10-26",
	}
}

// 运维报告必须保留原 analyze_log_fast.sh 的全部栏目，
// 运维同学可以照着这些栏目逐项和日志对账。
func TestOpsTextKeepsScriptLayout(t *testing.T) {
	in := opsSampleInput()
	in.Source = "/data/_log/backup/28888_access_json.log"
	out := OpsText(in)

	for _, want := range []string{
		"【总PV】: 1000",
		"【按 PV 趋势】",
		"10:05\t5",
		"10:06\t995",
		"【状态码分布】",
		"200: 900 (90.00%)",
		"404: 40 (4.00%)",
		"【Top 20 URL】",
		"【Top 20 客户端IP】",
		"【Top 20 大流量URL】",
		"【错误请求】: 60 (4xx/5xx)，占 6.00%",
		"404: 40 (66.67%)",
		"【Top 20 404 URL】",
		"40	/api/b",
		"【Top 20 5xx URL】",
		"【Top 20 400 URL】",
		"20	(无 URI)",
		"【响应时间统计】",
		"最大响应时间: 182.319 s",
		"【Top 20 慢请求URL】",
		"10.000	9	/api/b",
		"xlsx: 3",
		"【文件类型统计】",
		"【设备类型】",
		"移动端: 900 (90.0%)",
		"【浏览器统计】",
		"Chrome: 800 (80.0%)",
		"【Top 20 Referer域名】",
		"500	a.example:80",
		"【HTTP方法统计】",
		"【URL类型分布】",
		"【直接访问】: 100 (10.0%)",
		"【按 流量趋势】",
		"【总流量】: 5.00 MB",
		"【平均响应大小】: 5.12 KB",
		"慢请求(>1s): 12",
		"/data/_log/backup/28888_access_json.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("运维报告缺少: %q", want)
		}
	}
}

// 原脚本的 Top 榜不排除静态资源，运维报告保持一致（HTML 才按受众过滤）
func TestOpsTextTopURIsIncludeStatic(t *testing.T) {
	out := OpsText(opsSampleInput())
	i := strings.Index(out, "【Top 20 URL】")
	if i < 0 {
		t.Fatal("缺少 URL 榜")
	}
	block := out[i:]
	if j := strings.Index(block, "【Top 20 客户端IP】"); j > 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "/pro/assets/x.svg") {
		t.Error("运维报告的 URL 榜应包含静态资源（与脚本口径一致）")
	}
	// 4000 次的静态资源必须排在 700 次的接口前面
	if strings.Index(block, "x.svg") > strings.Index(block, "/api/a") {
		t.Error("URL 榜未按调用量倒序")
	}
}

// 单日报告逐分钟输出；跨多天时每天一行，避免打印十几万行
func TestOpsTextMinuteTrend(t *testing.T) {
	cfg := config.Default()
	g := analyzer.NewStats()
	g.Agg.Total = 12
	g.Agg.StatusCnt = map[string]int64{"200": 12}
	g.Agg.TimeBuckets[0] = 12
	g.Agg.ClientCnt = map[string]int64{"[IP]": 12}

	day := analyzer.NewAgg()
	day.Total = 12
	day.MinuteCnt[10*60+5] = 5
	day.MinuteCnt[10*60+6] = 7
	g.MergeDay("2026-11-02", day)
	// MergeDay 会再次累加文件级 Agg；修正成当天口径
	g.Agg.Total = 12
	g.Agg.StatusCnt = map[string]int64{"200": 12}

	out := OpsText(Input{
		Cfg: cfg, Stats: g,
		Days:      []analyzer.DaySummary{{Day: "2026-11-02", Total: 12}},
		StartDate: "2026-11-02", EndDate: "2026-11-02",
	})
	if !strings.Contains(out, "10:05\t5") || !strings.Contains(out, "10:06\t7") {
		t.Errorf("单日报告缺少逐分钟趋势:\n%s", out)
	}
	if strings.Contains(out, "每天一行") {
		t.Error("单日报告不应使用「每天一行」的多天格式")
	}

	// 多天：同一天的数据 + 另一天，应改为每天一行
	out2 := OpsText(Input{
		Cfg: cfg, Stats: g,
		Days: []analyzer.DaySummary{
			{Day: "2026-11-02", Total: 12},
			{Day: "2026-11-03", Total: 0},
		},
		StartDate: "2026-11-02", EndDate: "2026-11-03",
	})
	if !strings.Contains(out2, "每天一行") || !strings.Contains(out2, "2026-11-02\t周一\t12") {
		t.Errorf("多天报告格式不正确:\n%s", out2)
	}
}

// 没有数据时不能 panic，也不能出现 NaN/Inf
func TestOpsTextEmpty(t *testing.T) {
	out := OpsText(Input{
		Cfg: config.Default(), Stats: analyzer.NewStats(),
		StartDate: "2026-11-02", EndDate: "2026-11-02",
	})
	if !strings.Contains(out, "没有有效日志记录") {
		t.Error("空数据报告缺少提示")
	}
	for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("空数据报告出现 %s", bad)
		}
	}
}

// 排除静态资源后，报告要明确写清楚「原始多少行、排除了多少」，方便对账
func TestOpsTextReportsExcludedStatic(t *testing.T) {
	in := opsSampleInput()
	in.Stats.StaticSkipped = 1234
	out := OpsText(in)
	for _, want := range []string{
		"已排除静态资源: 1234 条",
		"原始行数 2234",
		"【总PV】: 1000（仅业务请求，已排除静态资源）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("运维报告缺少: %q", want)
		}
	}
}

// 10 秒以上的请求必须计入「>5s」档，不能被截断到 10s
func TestOpsTimeBinsIncludeOverflow(t *testing.T) {
	in := opsSampleInput()
	in.Stats.TimeOverflow[0] = 3 // 10~11s 三条
	bins := responseTimeBins(&in.Stats.Agg)
	for _, b := range bins {
		if b.Label == ">5s" {
			if b.Count != 3 {
				t.Errorf(">5s=%d, want 3", b.Count)
			}
			return
		}
	}
	t.Fatal("没有找到 >5s 档")
}

func TestOpsTextCleansControlChars(t *testing.T) {
	in := opsSampleInput()
	esc := string([]byte{0x1b})
	in.Stats.UriStat = map[string]*analyzer.UriStat{"/api/" + esc + "[31m": {Calls: 10, TimeS: 1}}
	in.Stats.ClientCnt = map[string]int64{"1.2.3.4" + esc + "[0m": 10}
	in.Stats.RefererCnt = map[string]int64{"evil" + esc + "[31m": 10}
	out := OpsText(in)
	if strings.ContainsRune(out, 0x1b) {
		t.Fatal("运维文本报告仍含 ESC 控制字符")
	}
}
