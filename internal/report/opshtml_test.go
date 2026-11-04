package report

import (
	"strings"
	"testing"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
)

// 运维 HTML 要包含图表和全部深度分析板块，且不能把日志内容原样注入
func TestOpsHTMLSectionsAndCharts(t *testing.T) {
	in := opsSampleInput()
	in.Source = "/data/_log/28888_access_json.log"
	evil := `/x"><script>alert(1)</script>?a=1`
	in.Stats.Agg.UriStat[evil] = &analyzer.UriStat{Calls: 2, TimeS: 1, Bytes: 10}
	in.Stats.Agg.RefererCnt[`ref"><img src=x>`] = 2

	out := OpsHTML(in)
	for _, want := range []string{
		"<!DOCTYPE html>", "</html>",
		"一、访问趋势", "二、请求结构", "三、Top 排行", "四、错误分析",
		"五、响应性能", "六、终端与来源",
		"<canvas", "ops-chart-data", "data-span=\"60\"", "PV 趋势", "流量趋势",
		"状态码分布", "HTTP 方法", "URL 类型分布",
		"大流量 URL", "404", "5xx", "400",
		"响应时间分布", "慢请求 URL", "设备类型", "浏览器", "文件类型", "Referer 域名",
		"已排除静态资源",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("运维 HTML 缺少: %s", want)
		}
	}
	for _, bad := range []string{"<script>alert(1)</script>", "<img src=x>"} {
		if strings.Contains(out, bad) {
			t.Fatalf("运维 HTML 出现未转义内容: %s", bad)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("预期的转义结果未出现")
	}
}

// 空数据不能 panic，也不能出现 NaN/Inf
func TestOpsHTMLEmpty(t *testing.T) {
	out := OpsHTML(Input{
		Cfg: config.Default(), Stats: analyzer.NewStats(),
		StartDate: "2026-11-02", EndDate: "2026-11-02",
	})
	if !strings.Contains(out, "没有数据") {
		t.Error("空数据报告缺少提示")
	}
	for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("空数据报告出现 %s", bad)
		}
	}
}

// TopN 参数要同时作用于文字和 HTML 报告的榜单
func TestOpsHTMLRespectsTopN(t *testing.T) {
	in := opsSampleInput()
	in.Cfg.TopN = 2
	out := OpsHTML(in)
	if !strings.Contains(out, "Top 榜 2 条") || !strings.Contains(out, "前 2 条") {
		t.Error("TopN 未生效")
	}
}
