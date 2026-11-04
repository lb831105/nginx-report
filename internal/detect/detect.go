// Package detect 自动检测本周运行问题
package detect

import (
	"fmt"
	"strings"
)

// Issue 检测到的问题
type Issue struct {
	Level string
	Title string
	Desc  string
}

// 阈值（比例类优先于绝对量：大流量站点上"慢请求 100 次"毫无意义）
const (
	slowRatioError   = 0.05 // 慢请求占比 > 5% → error
	slowRatioWarning = 0.01 // > 1% → warning
	slowRatioInfo    = 0.001

	avgErrorMs   = 1000.0 // 平均响应 > 1s → error
	avgWarningMs = 500.0  // > 500ms → warning
	p99WarnMs    = 5000.0 // P99 > 5s → 长尾告警
	p99InfoMs    = 3000.0

	serverRatioError   = 0.005 // 5xx 占比 > 0.5% → error
	serverRatioWarning = 0.001 // > 0.1% → warning
	serverMinCount     = 10    // 低于这个绝对量视为偶发，不做 warning（防止小样本误报）
)

// Issues 根据统计数据检测问题
//   - total: 总请求数
//   - errorRate: 错误率（百分比）
//   - slowCount: 慢请求数（阈值由 -slow-ms 控制）
//   - errorStatus: 状态码分布
//   - errorAlert: 错误率告警阈值（%）
//   - slowAlert: 慢请求数量参考阈值（占比极低时才使用）
//   - avgMs / p99Ms: 平均响应与 P99（毫秒），用于判断"服务变慢"
func Issues(total int64, errorRate float64, slowCount int64,
	errorStatus map[string]int64, errorAlert float64, slowAlert int,
	avgMs, p99Ms float64) []Issue {

	var issues []Issue

	if errorRate > errorAlert {
		issues = append(issues, Issue{
			Level: "error",
			Title: "错误率偏高",
			Desc:  fmt.Sprintf("本周期错误率 %.2f%%，超过 %.1f%% 警戒线", errorRate, errorAlert),
		})
	} else if errorRate > errorAlert*0.5 {
		issues = append(issues, Issue{
			Level: "warning",
			Title: "错误率需关注",
			Desc:  fmt.Sprintf("本周期错误率 %.2f%%，接近警戒线", errorRate),
		})
	}

	// 慢请求：先看占比（有区分度），占比很低时才用绝对量兜底
	if total > 0 {
		ratio := float64(slowCount) / float64(total)
		switch {
		case ratio > slowRatioError:
			issues = append(issues, Issue{
				Level: "error",
				Title: "慢请求占比过高",
				Desc: fmt.Sprintf("慢请求 %d 次，占 %.2f%%，超过 %.0f%% 警戒线",
					slowCount, ratio*100, slowRatioError*100),
			})
		case ratio > slowRatioWarning:
			issues = append(issues, Issue{
				Level: "warning",
				Title: "慢请求占比偏高",
				Desc: fmt.Sprintf("慢请求 %d 次，占 %.2f%%",
					slowCount, ratio*100),
			})
		case ratio > slowRatioInfo && slowCount > int64(slowAlert):
			issues = append(issues, Issue{
				Level: "info",
				Title: "慢请求数量较多",
				Desc: fmt.Sprintf("慢请求 %d 次（占比 %.3f%%），绝对量偏多",
					slowCount, ratio*100),
			})
		}
	}

	// 响应时间：平均与长尾分开看（平均不高但 P99 很长 = 长尾问题）
	switch {
	case avgMs > avgErrorMs:
		issues = append(issues, Issue{
			Level: "error",
			Title: "平均响应时间过长",
			Desc:  fmt.Sprintf("平均响应 %.0f ms，超过 %.0f ms 警戒线", avgMs, avgErrorMs),
		})
	case avgMs > avgWarningMs:
		issues = append(issues, Issue{
			Level: "warning",
			Title: "平均响应偏慢",
			Desc:  fmt.Sprintf("平均响应 %.0f ms（>%.0f ms）", avgMs, avgWarningMs),
		})
	}
	switch {
	case p99Ms > p99WarnMs:
		issues = append(issues, Issue{
			Level: "warning",
			Title: "长尾延迟严重",
			Desc:  fmt.Sprintf("P99 响应 %.1f s，超过 %.0f s；建议查看「接口 Top」里的慢接口", p99Ms/1000, p99WarnMs/1000),
		})
	case p99Ms > p99InfoMs:
		issues = append(issues, Issue{
			Level: "info",
			Title: "长尾延迟偏高",
			Desc:  fmt.Sprintf("P99 响应 %.1f s", p99Ms/1000),
		})
	}

	// 5xx 也按占比判定：小站点 11 次 5xx 可能是 20% 请求失败（严重），
	// 千万级站点 11 次 5xx 是噪声（0.0001%）。绝对量只做兜底。
	var serverErrors int64
	for k, v := range errorStatus {
		if strings.HasPrefix(k, "5") {
			serverErrors += v
		}
	}
	if total > 0 && serverErrors > 0 {
		ratio := float64(serverErrors) / float64(total)
		switch {
		case ratio > serverRatioError:
			issues = append(issues, Issue{
				Level: "error",
				Title: "服务器错误（5xx）偏多",
				Desc: fmt.Sprintf("本周期 %d 次 5xx，占 %.2f%%，超过 %.1f%% 警戒线",
					serverErrors, ratio*100, serverRatioError*100),
			})
		case ratio > serverRatioWarning && serverErrors >= serverMinCount:
			issues = append(issues, Issue{
				Level: "warning",
				Title: "存在服务器错误（5xx）",
				Desc:  fmt.Sprintf("本周期 %d 次 5xx，占 %.3f%%", serverErrors, ratio*100),
			})
		case serverErrors >= serverMinCount*10:
			issues = append(issues, Issue{
				Level: "info",
				Title: "服务器错误（5xx）绝对值偏多",
				Desc:  fmt.Sprintf("本周期 %d 次 5xx（占比仅 %.4f%%），建议排查是否有偶发故障", serverErrors, ratio*100),
			})
		}
	}

	if total > 0 {
		notFound := errorStatus["404"]
		if float64(notFound)/float64(total) > 0.05 {
			issues = append(issues, Issue{
				Level: "info",
				Title: "404 请求较多",
				Desc:  fmt.Sprintf("本周期共 %d 次 404，占比 %.2f%%，可能有死链或爬虫", notFound, float64(notFound)/float64(total)*100),
			})
		}
	}

	return issues
}
