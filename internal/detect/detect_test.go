package detect

import (
	"strings"
	"testing"
)

func titles(issues []Issue) map[string]string {
	m := make(map[string]string, len(issues))
	for _, i := range issues {
		m[i.Title] = i.Level
	}
	return m
}

func TestIssuesHealthy(t *testing.T) {
	if got := Issues(100000, 0.1, 10, map[string]int64{"200": 99900}, 1.0, 100, 50, 200); len(got) != 0 {
		t.Fatalf("健康数据不应报警: %v", got)
	}
}

// 大流量下"慢请求 100 次"不应报警，占比才是关键
func TestIssuesSlowByRatioNotAbsolute(t *testing.T) {
	// 1 亿请求、慢请求 10 万次 = 0.1% → 不报
	got := Issues(100_000_000, 0.01, 100_000, map[string]int64{"200": 99_990_000}, 1.0, 100, 80, 400)
	if _, ok := titles(got)["慢请求占比过高"]; ok {
		t.Error("0.1% 慢请求不应报 error")
	}
	// 同样的绝对量，占比 10% → error
	got = Issues(1_000_000, 0.01, 100_000, map[string]int64{"200": 999_000}, 1.0, 100, 80, 900)
	if lvl := titles(got)["慢请求占比过高"]; lvl != "error" {
		t.Errorf("10%% 慢请求应为 error，实际 %q (%v)", lvl, got)
	}
	// 3% → warning
	got = Issues(1_000_000, 0.01, 30_000, map[string]int64{"200": 999_000}, 1.0, 100, 80, 900)
	if lvl := titles(got)["慢请求占比偏高"]; lvl != "warning" {
		t.Errorf("3%% 慢请求应为 warning，实际 %q", lvl)
	}
}

// 响应时间：平均值与长尾分别判定
func TestIssuesLatency(t *testing.T) {
	got := Issues(1000, 0.1, 1, map[string]int64{"200": 999}, 1.0, 100, 1500, 20000)
	m := titles(got)
	if m["平均响应时间过长"] != "error" {
		t.Errorf("平均 1500ms 应报 error: %v", got)
	}
	if m["长尾延迟严重"] != "warning" {
		t.Errorf("P99 20s 应报长尾告警: %v", got)
	}

	// 平均不高但长尾严重
	if lvl := titles(Issues(1000, 0.1, 1, map[string]int64{"200": 999}, 1.0, 100, 120, 8000))["长尾延迟严重"]; lvl != "warning" {
		t.Errorf("P99 8s 应报长尾告警，实际 %q", lvl)
	}
}

func TestIssuesErrorsAnd5xx(t *testing.T) {
	got := Issues(1000, 2.0, 0, map[string]int64{"200": 950, "500": 30, "404": 20}, 1.0, 100, 50, 200)
	m := titles(got)
	if m["错误率偏高"] != "error" {
		t.Errorf("错误率 2%% 应报 error: %v", got)
	}
	if m["服务器错误（5xx）偏多"] != "error" {
		t.Errorf("3%% 的 5xx 应报 error: %v", got)
	}
	if _, ok := m["404 请求较多"]; ok {
		t.Errorf("404 占比 2%% 不应报（阈值 5%%）: %v", got)
	}
}

// 5xx 按占比判定：绝对值大小本身不再是唯一依据
func TestIssues5xxByRatio(t *testing.T) {
	// 千万级站点：11 次 5xx（0.0001%）不该报 error
	big := Issues(10000000, 0.01, 0, map[string]int64{"500": 11}, 1.0, 100, 100, 200)
	for _, is := range big {
		if is.Level == "error" && strings.Contains(is.Title, "5xx") {
			t.Errorf("大流量下 11 次 5xx 不应报 error: %+v", is)
		}
	}
	// 小站点：11 次里 3 次 5xx = 27% 必须报 error
	small := Issues(11, 27.0, 0, map[string]int64{"500": 3}, 1.0, 100, 100, 200)
	found := false
	for _, is := range small {
		if is.Level == "error" && strings.Contains(is.Title, "5xx") {
			found = true
		}
	}
	if !found {
		t.Errorf("小样本高占比必须报 error: %+v", small)
	}
	// 中间态：0.2% 且 20 次 → warning
	mid := Issues(10000, 0.2, 0, map[string]int64{"502": 20}, 1.0, 100, 100, 200)
	lvl := ""
	for _, is := range mid {
		if strings.Contains(is.Title, "5xx") {
			lvl = is.Level
		}
	}
	if lvl != "warning" {
		t.Errorf("0.2%% 的 5xx 应为 warning，实际 %q", lvl)
	}
}
