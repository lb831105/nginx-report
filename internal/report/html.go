// Package report 生成面向多受众（管理/甲方、运维、开发）的 HTML 周报
package report

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
	"nginx-report/internal/detect"
	"nginx-report/internal/history"
	"nginx-report/internal/staticasset"
)

// weekdays 按 time.Weekday（周日=0）索引
var weekdays = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// Input 报告输入
type Input struct {
	Cfg       *config.Config
	Days      []analyzer.DaySummary
	Stats     *analyzer.FileStats
	StartDate string
	EndDate   string
	Prev      *history.Summary   // 上周汇总（环比）
	Recent    []*history.Summary // 更早的周（趋势，按时间升序）
	Issues    []detect.Issue
	// Source 日志来源（单文件路径或目录/模式），仅用于运维版报告的抬头
	Source string

	// 多站点报告：SiteName 非空表示当前渲染的是单站点报告。
	SiteName   string
	SiteKey    string
	SiteKind   string
	SiteSource string
	SiteRows   []SiteRow
}

type kv struct {
	Key   string
	Value int64
}

// ---------------------------------------------------------------- 小工具

func weekdayName(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "-"
	}
	return weekdays[t.Weekday()]
}

func humanBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}

func humanNum(n int64) string {
	switch {
	case n >= 100000000: // 亿，避免出现 "10000.0 万"
		return fmt.Sprintf("%.2f 亿", float64(n)/100000000.0)
	case n >= 10000:
		return fmt.Sprintf("%.1f 万", float64(n)/10000.0)
	}
	return fmt.Sprintf("%d", n)
}

// pct 计算占比（分母为 0 时返回 0）
// slowThresholdText 返回慢请求阈值文案（不含 > 符号），如 1s / 500ms。
func slowThresholdText(cfg *config.Config) string {
	ms := 1000
	if cfg != nil && cfg.SlowMS > 0 {
		ms = cfg.SlowMS
	}
	if ms%1000 == 0 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	return fmt.Sprintf("%dms", ms)
}

// timeSourceText 返回响应时间口径说明。
func timeSourceText(field string) string {
	switch field {
	case "response":
		return "取 responsetime（端到端耗时）"
	case "upstream":
		return "只取 upstream_response_time"
	default:
		return "优先 upstream_response_time，缺失回退 responsetime"
	}
}

func pct(v, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(v) / float64(total) * 100
}

// calcChange 计算环比变化
func calcChange(curr, prev float64) (float64, string) {
	if prev == 0 {
		return 0, "→"
	}
	p := (curr - prev) / prev * 100
	if p > 0 {
		return p, "↑"
	}
	if p < 0 {
		return p, "↓"
	}
	return 0, "→"
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// change 生成环比 HTML
func change(curr, prev float64, lowerBetter bool) string {
	ch, arrow := calcChange(curr, prev)
	if arrow == "→" {
		return `<span class="chg-flat">→ 持平/无数据</span>`
	}
	color := "#43a047"
	if lowerBetter {
		if ch > 0 {
			color = "#e53935"
		}
	} else if ch < 0 {
		color = "#e53935"
	}
	return fmt.Sprintf(`<span style="color:%s;">%s %.1f%%</span>`, color, arrow, abs(ch))
}

// bar 生成一行「标签 + 进度条 + 数值」
func bar(label string, value, max int64, color string) string {
	w := 0
	if max > 0 {
		w = int(float64(value) / float64(max) * 100)
	}
	return fmt.Sprintf(`<div class="bar-row"><div class="bar-label" title="%s">%s</div><div class="bar-track"><div class="bar-fill" style="width:%d%%;background:%s;"></div></div><div class="bar-value">%s</div></div>`,
		html.EscapeString(label), html.EscapeString(label), w, color, humanNum(value))
}

// topN 取 map 中数值最大的前 n 个
func topN(m map[string]int64, n int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value == out[j].Value {
			return out[i].Key < out[j].Key
		}
		return out[i].Value > out[j].Value
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// statusClass 把状态码归到 2xx/3xx/4xx/5xx/其他
func statusClass(code string) string {
	if len(code) == 0 {
		return "其他"
	}
	switch code[0] {
	case '2':
		return "2xx"
	case '3':
		return "3xx"
	case '4':
		return "4xx"
	case '5':
		return "5xx"
	}
	return "其他"
}

// uriStat 单接口健康度
type uriStat struct {
	URI   string
	Calls int64
	Err   int64
	Slow  int64
	AvgMs float64
	Bytes int64
}

// uriStatOf 读单个接口的统计（不存在返回零值）
func uriStatOf(st *analyzer.FileStats, uri string) uriStat {
	u := uriStat{URI: uri}
	if s, ok := st.UriStat[uri]; ok {
		u.Calls = s.Calls
		u.Err = s.Errors
		u.Slow = s.Slows
		u.Bytes = s.Bytes
		if s.Calls > 0 {
			u.AvgMs = s.TimeS / float64(s.Calls) * 1000
		}
	}
	return u
}

// uriStats 按调用量取前 n 个接口的健康度
func uriStats(st *analyzer.FileStats, n int, byError bool) []uriStat {
	keys := topN(uriCallsMap(st, byError), n)
	out := make([]uriStat, 0, len(keys))
	for _, k := range keys {
		out = append(out, uriStatOf(st, k.Key))
	}
	return out
}

// uriCallsMap 把 UriStat 投影成 map[string]int64（用于通用排序）
func uriCallsMap(st *analyzer.FileStats, byError bool) map[string]int64 {
	m := make(map[string]int64, len(st.UriStat))
	for k, v := range st.UriStat {
		if byError {
			m[k] = v.Errors
		} else {
			m[k] = v.Calls
		}
	}
	return m
}

// isStaticAsset 判断接口是否为静态资源（规则见 internal/staticasset，
// 与解析阶段「是否排除静态记录」共用同一份实现）
func isStaticAsset(uri string) bool { return staticasset.Is(uri) }

// displayURI 展示用的接口名：空 URI 与溢出桶要有明确文案
func displayURI(uri string) string {
	u := strings.TrimSpace(uri)
	if u == "" {
		return "(无 URI)"
	}
	if u == analyzer.OverflowKey {
		return "(其他接口)"
	}
	return cleanText(u)
}

// staticAgg 汇总所有静态资源接口的量
func staticAgg(st *analyzer.FileStats) (calls, errs, slows int64, avgMs float64, files int) {
	var sum float64
	for uri, v := range st.UriStat {
		if !isStaticAsset(uri) {
			continue
		}
		files++
		calls += v.Calls
		errs += v.Errors
		slows += v.Slows
		sum += v.TimeS
	}
	if calls > 0 {
		avgMs = sum / float64(calls) * 1000
	}
	return
}

// topAPIs 取调用量最高的 n 个「非静态」接口
func topAPIs(st *analyzer.FileStats, n int) []uriStat {
	keys := topN(uriCallsMap(st, false), 2000)
	out := make([]uriStat, 0, n)
	for _, k := range keys {
		if isStaticAsset(k.Key) {
			continue
		}
		out = append(out, uriStatOf(st, k.Key))
		if len(out) >= n {
			break
		}
	}
	return out
}

// apiErrors 取错误次数最多的 n 个接口（跳过静态资源，空 URI 保留）
func apiErrors(st *analyzer.FileStats, n int, excludeStatic bool) []uriStat {
	keys := topN(uriCallsMap(st, true), 2000)
	out := make([]uriStat, 0, n)
	for _, k := range keys {
		if k.Value == 0 {
			break
		}
		if excludeStatic && isStaticAsset(k.Key) {
			continue
		}
		out = append(out, uriStatOf(st, k.Key))
		if len(out) >= n {
			break
		}
	}
	return out
}

// daySeries 一天的时间序列（24 个小时桶或 288 个 5 分钟桶）
type daySeries struct {
	Day      string
	Values   []int64
	Step     int   // 每个桶代表多少分钟
	DayTotal int64 // 当天总访问量（用于给出峰值占全天的比例，方便一眼自查）
}

// sumOf / maxOf 序列求和与最大值
func sumOf(vals []int64) int64 {
	var s int64
	for _, v := range vals {
		s += v
	}
	return s
}

func maxOf(vals []int64) int64 {
	var m int64
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// padSlots 把日记录的 SlotCnt 补齐到 288 个桶（尾部缺的补 0）
func padSlots(v []int64) []int64 {
	out := make([]int64, analyzer.SlotCount)
	copy(out, v)
	return out
}

// bucketLabel 把桶下标换成时间标签。
// step 是每个桶代表多少分钟：>=60 按小时给区间（09:00–10:00），
// 否则给时刻（09:05）。之前所有桶都用 5 分钟标签，导致 24 小时图显示成「00:45」。
func bucketLabel(idx, step int) string {
	if step <= 0 {
		step = analyzer.SlotMinutes
	}
	if step >= 60 {
		h := idx * step / 60
		return fmt.Sprintf("%02d:00–%02d:00", h, h+step/60)
	}
	m := idx * step
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// peakOf 返回序列的最大值与对应的时间标签（按桶宽度格式化）
func peakOf(vals []int64, step int) (int64, string) {
	var best int64
	idx := 0
	for i, v := range vals {
		if v > best {
			best, idx = v, i
		}
	}
	if best == 0 {
		return 0, "-"
	}
	return best, bucketLabel(idx, step)
}

// writeSparkRow 画一行「某天的时间序列」：标签 + 迷你柱状图 + 峰值说明。
// globalMax>0 时按全局最大值归一化（跨行可比），否则按本行最大值。
func writeSparkRow(b *strings.Builder, row daySeries, height int, globalMax int64) {
	maxV := globalMax
	if maxV <= 0 {
		maxV = 1
		for _, v := range row.Values {
			if v > maxV {
				maxV = v
			}
		}
	}
	peak, peakAt := peakOf(row.Values, row.Step)
	fmt.Fprintf(b, `<div class="dayrow"><span class="daytag">%s %s</span><span class="spark" style="height:%dpx;">`,
		html.EscapeString(shortDay(row.Day)), weekdayName(row.Day), height+8)
	for _, v := range row.Values {
		h := int(float64(v) / float64(maxV) * float64(height))
		if h < 1 && v > 0 {
			h = 1
		}
		cls := ""
		if v == peak && peak > 0 {
			cls = ` class="peak"`
		}
		fmt.Fprintf(b, `<i%s style="height:%dpx;"></i>`, cls, h)
	}
	fmt.Fprintf(b, `</span><span class="daypeak">峰值 %s · %s</span></div>`, peakAt, peakNote(peak, row.Step, row.DayTotal))
}

// peakNote 峰值说明：5 分钟桶额外换算成 req/s（和「峰值 QPS」卡片同口径），
// 并给出占全天比例 —— 数字大到离谱时一眼能看出来。
func peakNote(v int64, step int, dayTotal int64) string {
	if v <= 0 {
		return "0 次"
	}
	s := timesStr(v)
	if dayTotal > 0 && v > dayTotal {
		// 单个桶比全天还多，只可能是数据被重复累加（历史 bug 的典型症状），必须显式提示
		return s + "（占全天 >100%，数据异常，建议重算该日记录）"
	}
	if step > 0 && step < 60 {
		s += fmt.Sprintf("（≈%s req/s", rateStr(float64(v)/float64(step*60)))
		if dayTotal > 0 {
			s += fmt.Sprintf("，占全天 %.1f%%", float64(v)/float64(dayTotal)*100)
		}
		return s + "）"
	}
	if dayTotal > 0 {
		s += fmt.Sprintf("（占全天 %.1f%%）", float64(v)/float64(dayTotal)*100)
	}
	return s
}

// timesStr 「N 次」：万/亿后面不加空格，小数字后面加一格，避免"250次"/"6.8 万 次"
func timesStr(v int64) string {
	s := humanNum(v)
	if strings.HasSuffix(s, " 万") || strings.HasSuffix(s, " 亿") {
		return s + "次"
	}
	return s + " 次"
}

// rateStr 每秒速率：小于 10 保留一位小数，避免出现「≈0 req/s」
func rateStr(v float64) string {
	if v < 9.95 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// upStat 单台上游健康度
type upStat struct {
	Addr  string
	Calls int64
	Err   int64
	AvgMs float64
}

func upstreamStats(st *analyzer.FileStats, n int) []upStat {
	m := make(map[string]int64, len(st.UpstreamStat))
	for k, v := range st.UpstreamStat {
		m[k] = v.Calls
	}
	keys := topN(m, n)
	out := make([]upStat, 0, len(keys))
	for _, k := range keys {
		v := st.UpstreamStat[k.Key]
		u := upStat{Addr: k.Key, Calls: v.Calls, Err: v.Errors}
		if v.TimeC > 0 {
			u.AvgMs = v.TimeS / float64(v.TimeC) * 1000
		}
		out = append(out, u)
	}
	return out
}

// health 计算服务健康度（0-100）与等级，公式对读者公开
func health(errorRate, slowRatio float64, avgMs, p99Ms float64) (float64, string, string) {
	score := 100.0
	score -= min(40, errorRate*4)          // 错误率 10% 扣满 40
	score -= min(25, slowRatio*100*2)      // 慢请求占比 12.5% 扣满 25
	score -= min(20, max(0, p99Ms-300)/50) // P99 超 300ms，每 50ms 扣 1
	score -= min(15, max(0, avgMs-100)/20) // 平均响应超 100ms，每 20ms 扣 1
	if score < 0 {
		score = 0
	}
	switch {
	case score >= 95:
		return score, "优秀", "A+"
	case score >= 90:
		return score, "良好", "A"
	case score >= 80:
		return score, "正常", "B"
	case score >= 70:
		return score, "需关注", "C"
	}
	return score, "异常", "D"
}

// maxInt 取两个 int 中的较大值
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// max64 取两个 int64 中的较大值（除零保护用）
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// trunc 截断过长文本用于展示
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------- 样式

const css = `
*{margin:0;padding:0;box-sizing:border-box;}
body{font-family:"Microsoft YaHei",-apple-system,"PingFang SC",sans-serif;background:#f0f2f5;color:#333;padding:20px;line-height:1.6;}
.container{max-width:1200px;margin:0 auto;}
.cover{background:linear-gradient(135deg,#1a237e 0%,#283593 50%,#3949ab 100%);color:#fff;padding:44px 40px;border-radius:12px;margin-bottom:22px;position:relative;}
.cover .tag{display:inline-block;background:rgba(255,255,255,0.15);padding:4px 14px;border-radius:20px;font-size:12px;margin-bottom:14px;}
.cover h1{font-size:30px;font-weight:600;margin-bottom:10px;}
.cover .subtitle{font-size:14px;opacity:.85;}
.cover .meta{margin-top:20px;padding-top:16px;border-top:1px solid rgba(255,255,255,.2);font-size:13px;opacity:.8;}
.grade{position:absolute;right:36px;top:38px;text-align:center;background:rgba(255,255,255,.12);border:1px solid rgba(255,255,255,.25);border-radius:12px;padding:12px 20px;}
.grade .num{font-size:30px;font-weight:700;line-height:1.1;}
.grade .lbl{font-size:12px;opacity:.85;margin-top:2px;}
.cards{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-bottom:22px;}
.card{background:#fff;padding:16px 18px;border-radius:10px;box-shadow:0 2px 12px rgba(0,0,0,.06);}
.card .label{font-size:12px;color:#888;margin-bottom:6px;}
.card .value{font-size:21px;font-weight:600;color:#1a237e;}
.card .change{font-size:11px;margin-top:6px;color:#888;}
.chg-flat{color:#999;}
.section{background:#fff;padding:24px 26px;border-radius:10px;box-shadow:0 2px 12px rgba(0,0,0,.06);margin-bottom:22px;}
.section h2{font-size:17px;color:#1a237e;margin-bottom:16px;padding-bottom:10px;border-bottom:2px solid #e8eaf6;display:flex;align-items:center;gap:8px;}
.section h2 .aud{font-size:11px;font-weight:400;color:#7986cb;background:#e8eaf6;padding:2px 8px;border-radius:10px;}
.section h3{font-size:14px;color:#3949ab;margin:18px 0 10px;}
.section h3:first-of-type{margin-top:0;}
.grid-2{display:grid;grid-template-columns:1fr 1fr;gap:22px;}
table{width:100%;border-collapse:collapse;font-size:13px;}
th{background:#f8fafc;padding:9px 10px;text-align:left;font-weight:600;color:#555;font-size:12px;white-space:nowrap;}
td{padding:8px 10px;border-bottom:1px solid #f0f2f5;}
tr:hover td{background:#fafbff;}
.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap;}
.mono{font-family:Consolas,Monaco,monospace;font-size:12px;}
.bar-row{display:flex;align-items:center;margin-bottom:7px;font-size:12px;}
.bar-label{width:300px;color:#555;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.bar-track{flex:1;height:18px;background:#f0f4f8;border-radius:4px;overflow:hidden;}
.bar-fill{height:100%;background:linear-gradient(90deg,#5c6bc0,#283593);border-radius:4px;}
.bar-value{width:90px;text-align:right;color:#333;font-variant-numeric:tabular-nums;}
.stack{display:flex;height:26px;border-radius:5px;overflow:hidden;margin:6px 0 10px;font-size:11px;color:#fff;}
.stack div{display:flex;align-items:center;justify-content:center;min-width:0;overflow:hidden;white-space:nowrap;}
.legend{font-size:12px;color:#666;display:flex;gap:16px;flex-wrap:wrap;}
.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:5px;}
.issue-list>div{margin-bottom:10px;}
.badge-tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:11px;white-space:nowrap;}
.badge-green{background:#e8f5e9;color:#2e7d32;}
.badge-amber{background:#fff3e0;color:#ef6c00;}
.badge-red{background:#ffebee;color:#c62828;}
.badge-blue{background:#e3f2fd;color:#1565c0;}
.summary-list{margin:0;padding-left:20px;font-size:13.5px;color:#444;}
.summary-list li{margin-bottom:8px;}
.qps{display:flex;align-items:flex-end;gap:1px;height:110px;padding:8px 0;border-bottom:1px solid #eee;}
.dayrow{display:flex;align-items:center;gap:10px;padding:4px 0;border-bottom:1px dashed #f0f0f0;}
.dayrow:last-child{border-bottom:none;}
.daytag{width:92px;font-size:12px;color:#555;white-space:nowrap;}
.spark{flex:1;display:flex;align-items:flex-end;gap:1px;height:38px;background:#fafbfc;border-radius:3px;padding:0 2px;}
.spark i{flex:1;background:#7986cb;border-radius:1px 1px 0 0;min-height:1px;}
.spark i.peak{background:#e53935;}
.daypeak{width:190px;text-align:right;font-size:11px;color:#888;white-space:nowrap;}
.qps i{flex:1;background:#7986cb;border-radius:1px 1px 0 0;min-height:1px;}
.qps i.peak{background:#e53935;}
.hours{display:flex;justify-content:space-between;font-size:10px;color:#aaa;margin-top:4px;}
.footer{text-align:center;color:#aaa;font-size:12px;padding:18px;}
.note{font-size:12px;color:#999;margin-top:10px;}
`

// ---------------------------------------------------------------- 主入口

// HTML 生成完整报告
func HTML(in Input) string {
	cfg, grand := in.Cfg, in.Stats
	if cfg == nil {
		cfg = config.Default()
		in.Cfg = cfg
	}
	if grand == nil {
		grand = analyzer.NewStats()
		in.Stats = grand
	}
	titleText := cfg.ReportTitle
	if in.SiteName != "" {
		titleText = in.SiteName + " - " + titleText
	}
	escTitle := html.EscapeString(titleText)
	escStart := html.EscapeString(in.StartDate)
	escEnd := html.EscapeString(in.EndDate)

	avgTime := grand.AvgTime()
	p50 := grand.Percentile(0.50)
	p90 := grand.Percentile(0.90)
	p99 := grand.Percentile(0.99)
	avgMs, p99Ms := avgTime*1000, p99*1000

	total := grand.Total
	errCount := grand.ErrorCount
	slowCount := grand.SlowCount
	errorRate := pct(errCount, total)
	slowRatio := float64(0)
	if total > 0 {
		slowRatio = float64(slowCount) / float64(total)
	}
	uniqueIPs := int64(uniqueClients(grand.ClientCnt))
	// 成功率与服务端错误率分开算：把 4xx（客户端/扫描/参数错）算进"不可用"会冤枉服务
	var successCount, serverErrCount int64
	for code, n := range grand.StatusCnt {
		switch statusClass(code) {
		case "2xx", "3xx":
			successCount += n
		case "5xx":
			serverErrCount += n
		}
	}
	score, gradeName, gradeKey := health(errorRate, slowRatio, avgMs, p99Ms)

	// 统计窗口天数：日均要用它做分母（-start/-end 自定义区间时不一定是 7 天）
	windowDays := 7
	if sd, e := time.Parse("2006-01-02", in.StartDate); e == nil {
		if ed, e2 := time.Parse("2006-01-02", in.EndDate); e2 == nil {
			if d := int(ed.Sub(sd).Hours()/24) + 1; d > 0 {
				windowDays = d
			}
		}
	}

	// 接口 Top 列表只排一次序，摘要与表格共用（URI 基数可能上万）
	var topCalls []uriStat
	if in.Cfg.StaticExclude {
		topCalls = topAPIs(grand, 15)
	} else {
		topCalls = uriStats(grand, 15, false)
	}
	topErr := apiErrors(grand, 10, in.Cfg.StaticExclude)

	// 峰值
	peakDay := analyzer.DaySummary{}
	for _, d := range in.Days {
		if d.PeakCnt > peakDay.PeakCnt {
			peakDay = d
		}
	}

	// 状态码分组
	groups := map[string]int64{}
	for code, n := range grand.StatusCnt {
		groups[statusClass(code)] += n
	}
	var n4, n5 int64 = groups["4xx"], groups["5xx"]

	var prevTotal, prevPeak, prevSize, prevAvg, prevErrRate, prevErr, prevSlow, prevIPs float64
	if in.Prev != nil {
		prevTotal = float64(in.Prev.Total)
		prevPeak = float64(in.Prev.PeakQPS)
		prevSize = float64(in.Prev.Size)
		prevAvg = in.Prev.AvgTime * 1000
		prevErrRate = in.Prev.ErrorRate
		prevErr = float64(in.Prev.ErrorCount)
		prevSlow = float64(in.Prev.SlowCount)
		prevIPs = float64(in.Prev.UniqueIPs)
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"zh-CN\"><head><meta charset=\"UTF-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	fmt.Fprintf(&b, "<title>%s</title>", escTitle)
	b.WriteString("<style>" + css + "</style></head><body><div class=\"container\">\n")

	// ---------------- 封面 ----------------
	b.WriteString(`<div class="cover"><div class="tag">运维周报 / Weekly Operations Report</div>`)
	fmt.Fprintf(&b, "<h1>%s</h1>", escTitle)
	b.WriteString(`<div class="subtitle">流量 · 性能 · 稳定性 · 接口质量 —— 面向管理、运维与研发的全维度数据报告</div>`)
	period := escStart + " — " + escEnd
	if in.StartDate == in.EndDate || in.EndDate == "" {
		period = escStart
	}
	fmt.Fprintf(&b, `<div class="meta">报告周期：%s　|　日志总量：%s 条　|　独立来源 IP：%d</div>`,
		period, humanNum(total), uniqueIPs)
	fmt.Fprintf(&b, `<div class="grade"><div class="num">%s</div><div class="lbl">%s · %.0f 分</div></div></div>`,
		gradeKey, gradeName, score)

	// ---------------- 一、执行摘要（管理/甲方） ----------------
	b.WriteString(`<div class="section"><h2>一、执行摘要<span class="aud">管理 / 甲方</span></h2><ul class="summary-list">`)
	for _, line := range executiveSummary(in, total, errorRate, errCount, n4, n5, avgMs, p99Ms,
		slowCount, uniqueIPs, peakDay, score, gradeName, topErr, topCalls, windowDays) {
		fmt.Fprintf(&b, "<li>%s</li>", line)
	}
	b.WriteString(`</ul><div class="note">健康度评分 = 100 − 错误率扣分(≤40) − 慢请求占比扣分(≤25) − P99 超 300ms 扣分(≤20) − 平均响应超 100ms 扣分(≤15)；等级：A+≥95、A≥90、B≥80、C≥70、D&lt;70。</div></div>`)

	// ---------------- 二、核心指标 ----------------
	b.WriteString(`<div class="section"><h2>二、核心指标（含环比）<span class="aud">管理 / 甲方</span></h2><div class="cards">`)
	card := func(label, value, chg string) {
		fmt.Fprintf(&b, `<div class="card"><div class="label">%s</div><div class="value">%s</div><div class="change">%s</div></div>`, label, value, chg)
	}
	card("总访问量", humanNum(total), change(float64(total), prevTotal, false))
	card("峰值 QPS", fmt.Sprintf("%d", peakDay.PeakCnt), change(float64(peakDay.PeakCnt), prevPeak, false))
	card("总流量", humanBytes(float64(grand.SizeTotal)), change(float64(grand.SizeTotal), prevSize, false))
	card("平均响应", fmt.Sprintf("%.0f ms", avgMs), change(avgMs, prevAvg, true))
	card("错误率", fmt.Sprintf("%.2f%%", errorRate), change(errorRate, prevErrRate, true))
	card("错误次数", fmt.Sprintf("%d", errCount), change(float64(errCount), prevErr, true))
	card("慢请求 (&gt;"+slowThresholdText(cfg)+")", fmt.Sprintf("%d", slowCount), change(float64(slowCount), prevSlow, true))
	card("独立 IP 数", fmt.Sprintf("%d", uniqueIPs), change(float64(uniqueIPs), prevIPs, false))
	b.WriteString(`</div></div>`)

	// ---------------- 站点对比 ----------------
	writeSiteComparisonHTML(&b, in.SiteRows, total)

	// ---------------- 三、问题发现 ----------------
	b.WriteString(`<div class="section"><h2>三、本周问题发现<span class="aud">运维 / 开发</span></h2>`)
	if len(in.Issues) == 0 {
		b.WriteString(`<div style="text-align:center;padding:28px;color:#43a047;font-size:15px;">✅ 本周各项指标正常，未发现异常</div>`)
	} else {
		b.WriteString(`<div class="issue-list">`)
		for _, iss := range in.Issues {
			icon, bg, border := "⚠️", "#fff3e0", "#fb8c00"
			switch iss.Level {
			case "error":
				icon, bg, border = "❌", "#ffebee", "#e53935"
			case "info":
				icon, bg, border = "ℹ️", "#e3f2fd", "#1e88e5"
			}
			fmt.Fprintf(&b, `<div style="background:%s;border-left:4px solid %s;padding:12px 16px;border-radius:0 8px 8px 0;"><div style="font-weight:600;margin-bottom:3px;">%s %s</div><div style="font-size:13px;color:#555;">%s</div></div>`,
				bg, border, icon, html.EscapeString(iss.Title), html.EscapeString(iss.Desc))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)

	// ---------------- 四、业务趋势 ----------------
	b.WriteString(`<div class="section"><h2>四、业务趋势<span class="aud">管理 / 甲方</span></h2>`)

	// 4.1 近 N 周趋势
	if len(in.Recent) > 0 {
		b.WriteString(`<h3>近几周访问量趋势</h3><div style="display:flex;align-items:flex-end;gap:18px;height:150px;padding:10px 6px;background:#fafbfc;border-radius:8px;">`)
		maxWeek := int64(1)
		for _, w := range in.Recent {
			if w.Total > maxWeek {
				maxWeek = w.Total
			}
		}
		if total > maxWeek {
			maxWeek = int64(total)
		}
		weeks := append(append([]*history.Summary{}, in.Recent...), &history.Summary{
			StartDate: in.StartDate, EndDate: in.EndDate,
			Total: total, ErrorRate: errorRate,
		})
		for i, w := range weeks {
			h := int(float64(w.Total)/float64(maxWeek)*100) + 6
			color := "#5c6bc0"
			if i == len(weeks)-1 {
				color = "#283593"
			}
			label := w.StartDate
			if len(label) >= 10 {
				label = label[5:]
			}
			tag := ""
			if i == len(weeks)-1 {
				tag = `<div style="font-size:10px;color:#283593;">本周</div>`
			}
			fmt.Fprintf(&b, `<div style="flex:1;display:flex;flex-direction:column;align-items:center;justify-content:flex-end;">`+
				`<div style="font-size:11px;color:#666;margin-bottom:3px;">%s</div>`+
				`<div style="width:70%%;max-width:56px;height:%dpx;background:%s;border-radius:4px 4px 0 0;"></div>`+
				`<div style="font-size:11px;color:#888;margin-top:4px;">%s</div>`+
				`<div style="font-size:10px;color:%s;">错误率 %.2f%%</div>%s</div>`,
				humanNum(w.Total), h, color, html.EscapeString(label), errorRateColor(w.ErrorRate), w.ErrorRate, tag)
		}
		b.WriteString(`</div>`)
	}

	// 4.2 每日趋势
	// 天数多时压缩柱宽、抽稀标签，避免把页面撑成横向滚动
	barW, labelEvery := 72, 1
	if n := len(in.Days); n > 16 {
		if w := 1000 / n; w < barW {
			barW = w
		}
		labelEvery = n/24 + 1
	}
	if barW < 8 {
		barW = 8
	}
	// 只有一天时，"每日访问量"就是一根柱子，没有信息量（当天节奏看 QPS 曲线即可）
	if len(in.Days) > 1 {
		dailyTitle := "本周每日访问量"
		if len(in.Days) > 21 {
			dailyTitle = fmt.Sprintf("每日访问量（%d 天）", len(in.Days))
		}
		fmt.Fprintf(&b, `<h3>%s</h3><div style="display:flex;align-items:flex-end;justify-content:space-around;height:150px;padding:8px 6px;background:#fafbfc;border-radius:8px;">`, dailyTitle)
		maxDaily := int64(1)
		for _, d := range in.Days {
			if d.Total > maxDaily {
				maxDaily = d.Total
			}
		}
		for i, d := range in.Days {
			h := int(float64(d.Total)/float64(maxDaily)*110) + 8
			color := "#5c6bc0"
			if pct(d.ErrorCount, d.Total) > cfg.ErrorAlert {
				color = "#e53935"
			}
			dayLabel, weekLabel := "", ""
			if i%labelEvery == 0 {
				dayLabel, weekLabel = html.EscapeString(shortDay(d.Day)), weekdayName(d.Day)
			}
			fmt.Fprintf(&b, `<div style="display:flex;flex-direction:column;align-items:center;width:%dpx;overflow:hidden;"><div style="font-size:10px;color:#666;margin-bottom:3px;white-space:nowrap;">%s</div><div style="width:%dpx;height:%dpx;background:%s;border-radius:4px 4px 0 0;"></div><div style="font-size:10px;color:#888;margin-top:4px;white-space:nowrap;">%s</div><div style="font-size:9px;color:#aaa;white-space:nowrap;">%s</div></div>`,
				barW, humanNum(d.Total), maxInt(barW-8, 4), h, color, dayLabel, weekLabel)
		}
		b.WriteString(`</div>`)
		if wk, we, nwk, nwe := splitWeekend(in.Days); len(in.Days) > 1 && (nwk > 0 || nwe > 0) {
			note := `<div class="note">`
			if nwk > 0 {
				note += fmt.Sprintf("工作日日均 %s 次", humanNum(wk/nwk))
			}
			if nwe > 0 {
				if nwk > 0 {
					note += "，"
				}
				note += fmt.Sprintf("周末日均 %s 次", humanNum(we/nwe))
			}
			// 用浮点均值比较：wk/nwk 是整数除法，量小时会变成 0 导致 +Inf%
			if avgWk := float64(wk) / float64(max64(nwk, 1)); nwe > 0 && nwk > 0 && avgWk > 0 {
				avgWe := float64(we) / float64(nwe)
				note += fmt.Sprintf("；周末日均流量为工作日的 %.0f%%", avgWe/avgWk*100)
			}
			note += `</div>`
			b.WriteString(note)
		}

	}

	// 4.3 QPS 曲线：单天画一条，多天每天画一条（叠加成一条毫无意义）
	qpsRows := make([]daySeries, 0, len(in.Days))
	var qpsPeakDay string
	var qpsPeakVal int64
	qpsPeakAt := ""
	var qpsMax int64
	if len(in.Days) > 1 {
		for _, d := range in.Days {
			a := in.Stats.DayAgg(d.Day)
			if a == nil || len(a.SlotCnt) == 0 {
				continue
			}
			vals := padSlots(a.SlotCnt)
			// 全天 5 分钟桶加起来应等于当天访问量；对不上说明日记录缺时段数据，不画（否则是一条假平线）
			if sumOf(vals) == 0 {
				continue
			}
			qpsRows = append(qpsRows, daySeries{Day: d.Day, Values: vals, Step: analyzer.SlotMinutes, DayTotal: d.Total})
			if v, at := peakOf(vals, analyzer.SlotMinutes); v > qpsPeakVal {
				qpsPeakVal, qpsPeakDay, qpsPeakAt = v, d.Day, at
			}
			if v := maxOf(vals); v > qpsMax {
				qpsMax = v
			}
		}
	}

	switch {
	case len(qpsRows) > 0:
		title := "QPS 曲线（5 分钟粒度，每天一条）"
		if qpsPeakVal > 0 {
			// 5 分钟窗口内的总请求数除以 300 秒才是 req/s，否则和「峰值 QPS」卡片对不上
			title = fmt.Sprintf("QPS 曲线（5 分钟粒度，每天一条）｜全周最高 %s %s：5 分钟 %s ≈ %s req/s",
				html.EscapeString(shortDay(qpsPeakDay)), qpsPeakAt,
				timesStr(qpsPeakVal), rateStr(float64(qpsPeakVal)/float64(analyzer.SlotMinutes*60)))
		}
		fmt.Fprintf(&b, `<h3>%s</h3>`, title)
		b.WriteString(`<div style="padding:2px 0;">`)
		for _, row := range qpsRows {
			writeSparkRow(&b, row, 38, qpsMax)
		}
		b.WriteString(`</div>`)
		fmt.Fprintf(&b, `<div class="note">每行一天，横向覆盖 00:00–24:00（每格 5 分钟），红色为该天峰值时段；纵轴按全周最大值归一化，可直接跨天比高低。</div>`)

	case len(grand.SlotCnt) > 0:
		maxSlot := int64(1)
		peakSlot, peakSlotCnt := 0, int64(0)
		for i, c := range grand.SlotCnt {
			if c > maxSlot {
				maxSlot = c
			}
			if c > peakSlotCnt {
				peakSlot, peakSlotCnt = i, c
			}
		}
		if peakSlotCnt > 0 {
			// 标题里的日期必须是「图里真实覆盖的日期」，不能拿报告周期的起始日凑
			scope := html.EscapeString(in.StartDate)
			if len(in.Days) == 1 {
				scope = html.EscapeString(in.Days[0].Day)
			}
			if len(in.Days) > 1 {
				// 日记录里没有时段明细时只能把多天叠加画一条，必须标注清楚，否则会被当成单日流量
				scope = fmt.Sprintf("%s ~ %s 合计（%d 天叠加，日记录缺少时段明细）",
					html.EscapeString(in.StartDate), html.EscapeString(in.EndDate), len(in.Days))
			}
			// 多天叠加时峰值桶里是 7 天之和，换算 req/s 要除以天数
			nd := float64(len(in.Days))
			if nd < 1 {
				nd = 1
			}
			fmt.Fprintf(&b, `<h3>QPS 曲线（%s，5 分钟粒度）；峰值时段 %s 共 %s ≈ %s req/s</h3><div class="qps">`,
				scope, slotLabel(peakSlot), timesStr(peakSlotCnt),
				rateStr(float64(peakSlotCnt)/nd/float64(analyzer.SlotMinutes*60)))
			for _, c := range grand.SlotCnt {
				h := int(float64(c) / float64(maxSlot) * 100)
				if h < 1 && c > 0 {
					h = 1
				}
				cls := ""
				if c == maxSlot {
					cls = ` class="peak"`
				}
				fmt.Fprintf(&b, `<i%s style="height:%dpx;"></i>`, cls, h)
			}
			b.WriteString(`</div><div class="hours">`)
			for h := 0; h < 24; h += 3 {
				fmt.Fprintf(&b, "<span>%02d:00</span>", h)
			}
			b.WriteString(`</div>`)
			days := float64(len(in.Days))
			if days < 1 {
				days = 1
			}
			avgQPS := float64(total) / days / float64(len(grand.SlotCnt)) / float64(analyzer.SlotMinutes) / 60
			fmt.Fprintf(&b, `<div class="note">纵轴为每 5 分钟请求量峰值归一化；平均 QPS ≈ %.2f，峰值 5 分钟均值 ≈ %.2f req/s。</div>`,
				avgQPS, float64(peakSlotCnt)/days/float64(analyzer.SlotMinutes*60))
		}
	}
	b.WriteString(`</div>`)

	// ---------------- 五、每日明细 ----------------
	b.WriteString(`<div class="section"><h2>五、每日运行明细<span class="aud">运维</span></h2><table><thead><tr>`)
	b.WriteString(`<th>日期</th><th>星期</th><th class="num">访问量</th><th class="num">峰值QPS</th><th class="num">总流量</th><th class="num">平均响应</th><th class="num">错误数</th><th class="num">错误率</th><th class="num">慢请求</th><th>状态</th></tr></thead><tbody>`)
	for _, d := range in.Days {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td class="num">%s</td><td class="num">%d</td><td class="num">%s</td><td class="num">%.0fms</td><td class="num">%d</td><td class="num">%.2f%%</td><td class="num">%d</td><td>%s</td></tr>`,
			html.EscapeString(d.Day), weekdayName(d.Day), humanNum(d.Total), d.PeakCnt,
			humanBytes(float64(d.Size)), d.AvgTime*1000, d.ErrorCount, pct(d.ErrorCount, d.Total), d.SlowCount,
			dayBadge(d, cfg))
	}
	b.WriteString(`</tbody></table></div>`)

	// ---------------- 六、运维视角 ----------------
	b.WriteString(`<div class="section"><h2>六、运维视角<span class="aud">运维</span></h2>`)

	// 6.1 状态码分组
	b.WriteString(`<h3>响应状态分组</h3>`)
	colors := map[string]string{"2xx": "#43a047", "3xx": "#1e88e5", "4xx": "#fb8c00", "5xx": "#e53935", "其他": "#9e9e9e"}
	b.WriteString(`<div class="stack">`)
	for _, g := range []string{"2xx", "3xx", "4xx", "5xx", "其他"} {
		n := groups[g]
		if n == 0 {
			continue
		}
		share := pct(n, total)
		label := ""
		if share >= 0.1 {
			label = fmt.Sprintf("%s %.1f%%", g, share)
		}
		fmt.Fprintf(&b, `<div style="width:%.2f%%;background:%s;">%s</div>`, share, colors[g], label)
	}
	b.WriteString(`</div><div class="legend">`)
	for _, g := range []string{"2xx", "3xx", "4xx", "5xx"} {
		if groups[g] == 0 {
			continue
		}
		fmt.Fprintf(&b, `<span><i style="background:%s;"></i>%s：%s 次（%.2f%%）</span>`, colors[g], g, humanNum(groups[g]), pct(groups[g], total))
	}
	b.WriteString(`</div>`)

	// 6.2 错误明细
	if errCount > 0 {
		b.WriteString(`<h3>错误状态码明细（4xx / 5xx）</h3><table><thead><tr><th>状态码</th><th>类别</th><th class="num">次数</th><th class="num">占错误总数</th><th class="num">占总请求</th></tr></thead><tbody>`)
		for _, k := range topN(grand.ErrorStatus, 12) {
			fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td>%s</td><td class="num">%s</td><td class="num">%.1f%%</td><td class="num">%.2f%%</td></tr>`,
				html.EscapeString(k.Key), statusClass(k.Key), humanNum(k.Value), pct(k.Value, errCount), pct(k.Value, total))
		}
		b.WriteString(`</tbody></table>`)
	}

	// 6.3 上游服务健康度
	if ups := upstreamStats(grand, 10); len(ups) > 0 {
		b.WriteString(`<h3>上游（后端）服务健康度</h3><table><thead><tr><th>上游地址</th><th class="num">调用次数</th><th class="num">占比</th><th class="num">平均耗时</th><th class="num">上游错误</th><th class="num">错误率</th></tr></thead><tbody>`)
		for _, u := range ups {
			fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%.0fms</td><td class="num">%d</td><td class="num">%.2f%%</td></tr>`,
				html.EscapeString(u.Addr), humanNum(u.Calls), pct(u.Calls, total), u.AvgMs, u.Err, pct(u.Err, u.Calls))
		}
		var upCalls int64
		for _, v := range grand.UpstreamStat {
			upCalls += v.Calls
		}
		fmt.Fprintf(&b, `</tbody></table><div class="note">上游错误率 = 该上游返回 4xx/5xx 的占比（基于 upstream_status），可快速区分「网关问题」还是「后端问题」。`)
		if rest := total - upCalls; rest > 0 {
			fmt.Fprintf(&b, `另有 %s 次请求（%.2f%%）未经过上游，多为静态资源由 nginx 直接返回。`, humanNum(rest), pct(rest, total))
		}
		b.WriteString(`</div>`)
	}

	// 6.4 客户端来源 Top10
	if clients := topN(grand.ClientCnt, 10); len(clients) > 0 {
		b.WriteString(`<h3>客户端来源 Top 10</h3><table><thead><tr><th>来源 IP</th><th class="num">请求数</th><th class="num">占比</th></tr></thead><tbody>`)
		for _, c := range clients {
			key := c.Key
			if key == "" {
				key = "(未记录)"
			}
			fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="num">%s</td><td class="num">%.2f%%</td></tr>`,
				html.EscapeString(key), humanNum(c.Value), pct(c.Value, total))
		}
		b.WriteString(`</tbody></table>`)
	}

	// 6.5 24 小时分布：单天看绝对值，多天每天一行（可看出工作日/周末节奏差异）
	if len(in.Days) > 1 {
		rows := make([]daySeries, 0, len(in.Days))
		var hMax int64
		for _, d := range in.Days {
			a := in.Stats.DayAgg(d.Day)
			if a == nil {
				continue
			}
			vals := make([]int64, 24)
			for h := 0; h < 24; h++ {
				vals[h] = a.HourCnt[h]
			}
			if sumOf(vals) == 0 {
				continue
			}
			rows = append(rows, daySeries{Day: d.Day, Values: vals, Step: 60, DayTotal: d.Total})
			if v := maxOf(vals); v > hMax {
				hMax = v
			}
		}
		fmt.Fprintf(&b, `<h3>24 小时流量分布（%d 天，每天一行）</h3>`, len(rows))
		b.WriteString(`<div style="padding:2px 0;">`)
		for _, row := range rows {
			writeSparkRow(&b, row, 30, hMax)
		}
		b.WriteString(`</div><div class="hours">`)
		for h := 0; h < 24; h += 3 {
			fmt.Fprintf(&b, "<span>%02d:00</span>", h)
		}
		b.WriteString(`</div>`)
		fmt.Fprintf(&b, `<div class="note">横向 00:00–24:00，每格 1 小时；每行一天，纵轴按全周单小时最大值归一化，柱高可直接跨天比较。</div>`)
	} else {
		b.WriteString(`<h3>24 小时流量分布</h3><div style="display:flex;align-items:flex-end;justify-content:space-between;height:92px;padding:6px 0;">`)
		maxHour := int64(1)
		for _, v := range grand.HourCnt {
			if v > maxHour {
				maxHour = v
			}
		}
		for h := 0; h < 24; h++ {
			cnt := grand.HourCnt[h]
			height := int(float64(cnt)/float64(maxHour)*62) + 2
			fmt.Fprintf(&b, `<div style="display:flex;flex-direction:column;align-items:center;flex:1;"><div style="width:12px;height:%dpx;background:#7986cb;border-radius:2px 2px 0 0;"></div><div style="font-size:9px;color:#999;margin-top:2px;">%02d</div></div>`, height, h)
		}
		b.WriteString(`</div>`)
		busyHour, busyCnt := 0, int64(-1)
		idleHour, idleCnt := 0, int64(1<<62)
		for h := 0; h < 24; h++ {
			if grand.HourCnt[h] > busyCnt {
				busyHour, busyCnt = h, grand.HourCnt[h]
			}
			if grand.HourCnt[h] < idleCnt {
				idleHour, idleCnt = h, grand.HourCnt[h]
			}
		}
		if busyCnt > 0 {
			// 全域流量均匀时（峰谷差 <5%），写"最忙/最闲"会误导，直接说明分布均匀
			if float64(busyCnt-idleCnt) < float64(busyCnt)*0.05 {
				fmt.Fprintf(&b, `<div class="note">全天流量分布均匀：每小时约 %s，峰值只在 %02d:00–%02d:00 略高（占 %.1f%%）。</div>`,
					timesStr(busyCnt), busyHour, (busyHour+1)%24, pct(busyCnt, total))
			} else {
				fmt.Fprintf(&b, `<div class="note">最忙时段 %02d:00–%02d:00（%s，占 %.1f%%）；最闲时段 %02d:00–%02d:00（%s）。</div>`,
					busyHour, (busyHour+1)%24, timesStr(busyCnt), pct(busyCnt, total), idleHour, (idleHour+1)%24, timesStr(idleCnt))
			}
		}
	}
	b.WriteString(`</div>`)

	// ---------------- 七、开发视角 ----------------
	b.WriteString(`<div class="section"><h2>七、开发视角<span class="aud">开发</span></h2>`)

	// 7.1 接口健康度
	sCalls, sErr, sSlow, sAvg, sFiles := staticAgg(grand)
	if len(topCalls) > 0 || sCalls > 0 {
		title := "核心接口 Top 15（调用量 / 耗时 / 错误 / 慢请求）"
		if sCalls > 0 && cfg.StaticExclude {
			title += "　·　已排除静态资源"
		}
		fmt.Fprintf(&b, `<h3>%s</h3><table><thead><tr><th>接口</th><th class="num">调用次数</th><th class="num">占比</th><th class="num">平均耗时</th><th class="num">慢请求</th><th class="num">错误数</th><th class="num">错误率</th></tr></thead><tbody>`, title)
		for _, u := range topCalls {
			badge := ""
			if pct(u.Err, u.Calls) > cfg.ErrorAlert {
				badge = ` <span class="badge-tag badge-red">高错误</span>`
			}
			if u.AvgMs > 500 {
				badge += ` <span class="badge-tag badge-amber">慢接口</span>`
			}
			fmt.Fprintf(&b, `<tr><td class="mono">%s%s</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%.0fms</td><td class="num">%d</td><td class="num">%d</td><td class="num">%.2f%%</td></tr>`,
				html.EscapeString(trunc(displayURI(u.URI), 70)), badge, humanNum(u.Calls), pct(u.Calls, total), u.AvgMs, u.Slow, u.Err, pct(u.Err, u.Calls))
		}
		// 静态资源单独一行：它们量大但和业务排障无关，混在 Top 里会把真正的接口挤掉
		if sCalls > 0 && cfg.StaticExclude {
			fmt.Fprintf(&b, `<tr style="color:#888;"><td class="mono">(静态资源 %d 个：js/css/图片/字体)</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%.0fms</td><td class="num">%d</td><td class="num">%d</td><td class="num">%.2f%%</td></tr>`,
				sFiles, humanNum(sCalls), pct(sCalls, total), sAvg, sSlow, sErr, pct(sErr, sCalls))
		}
		b.WriteString(`</tbody></table>`)
	}

	// 7.2 错误接口 Top10
	if uris := topErr; len(uris) > 0 {
		b.WriteString(`<h3>错误次数最多的接口 Top 10</h3><table><thead><tr><th>接口</th><th class="num">错误数</th><th class="num">占全部错误</th><th class="num">该接口错误率</th><th class="num">调用次数</th></tr></thead><tbody>`)
		for _, u := range uris {
			fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="num">%d</td><td class="num">%.1f%%</td><td class="num">%.2f%%</td><td class="num">%s</td></tr>`,
				html.EscapeString(trunc(displayURI(u.URI), 70)), u.Err, pct(u.Err, errCount), pct(u.Err, u.Calls), humanNum(u.Calls))
		}
		b.WriteString(`</tbody></table>`)
	}

	// 7.3 响应时间分布
	b.WriteString(`<h3>响应时间分布</h3>`)
	dist := []struct {
		Label  string
		Lo, Hi int // 桶下标，Hi=-1 表示到末尾
	}{
		{"&lt; 10ms", 0, 1}, {"10–50ms", 1, 5}, {"50–100ms", 5, 10}, {"100–200ms", 10, 20},
		{"200–500ms", 20, 50}, {"0.5–1s", 50, 100}, {"≥ 1s", 100, -1},
	}
	maxBucket := int64(1)
	counts := make([]int64, len(dist))
	for i, d := range dist {
		hi := d.Hi
		if hi < 0 || hi > len(grand.TimeBuckets) {
			hi = len(grand.TimeBuckets)
		}
		for bi := d.Lo; bi < hi && bi < len(grand.TimeBuckets); bi++ {
			counts[i] += grand.TimeBuckets[bi]
		}
		if d.Hi < 0 { // ≥1s 档包含 10 秒以上的溢出桶
			counts[i] += sumOf(grand.TimeOverflow)
		}
		if counts[i] > maxBucket {
			maxBucket = counts[i]
		}
	}
	for i, d := range dist {
		barHTML := ""
		w := int(float64(counts[i]) / float64(maxBucket) * 100)
		color := "#5c6bc0"
		if i >= 5 {
			color = "#e53935"
		}
		barHTML = fmt.Sprintf(`<div class="bar-row"><div class="bar-label" style="width:120px;">%s</div><div class="bar-track"><div class="bar-fill" style="width:%d%%;background:%s;"></div></div><div class="bar-value">%s（%.2f%%）</div></div>`,
			d.Label, w, color, humanNum(counts[i]), pct(counts[i], total))
		b.WriteString(barHTML)
	}
	b.WriteString(`</div>`)

	// ---------------- 八、响应性能 ----------------
	b.WriteString(`<div class="section"><h2>八、响应性能指标<span class="aud">运维 / 开发</span></h2><div class="grid-2"><div><table>`)
	fmt.Fprintf(&b, `<tr><th>指标</th><th class="num">数值</th></tr>
<tr><td>平均响应</td><td class="num">%.0f ms</td></tr>
<tr><td>P50（中位数）</td><td class="num">%.0f ms</td></tr>
<tr><td>P90</td><td class="num">%.0f ms</td></tr>
<tr><td>P95</td><td class="num">%.0f ms</td></tr>
<tr><td>P99</td><td class="num">%.0f ms</td></tr>
<tr><td>慢请求 (&gt;%s)</td><td class="num">%d 次（%.3f%%）</td></tr>
<tr><td>成功率（2xx+3xx）</td><td class="num">%.3f%%</td></tr>
<tr><td>服务端错误率（5xx）</td><td class="num">%.3f%%</td></tr>`,
		avgMs, p50*1000, p90*1000, grand.Percentile(0.95)*1000, p99Ms, slowThresholdText(cfg), slowCount, pct(slowCount, total),
		pct(successCount, total), pct(serverErrCount, total))
	// 响应时间口径：没有耗时记录的请求（nginx 直接返回的静态资源等）不计入分位数
	if timed := grand.Timed(); timed != total && total > 0 {
		fmt.Fprintf(&b, `<tr><td colspan="2" style="font-size:11px;color:#888;">口径：平均响应与 P50~P99 仅统计有 upstream 计时的 %s 次请求（占 %.1f%%）；其余 %s 次为 nginx 直接返回，无耗时记录，不计入。</td></tr>`,
			humanNum(timed), pct(timed, total), humanNum(total-timed))
	}
	b.WriteString(`</table></div><div><table>`)
	// 请求方法分布
	b.WriteString(`<tr><th>请求方法</th><th class="num">次数</th><th class="num">占比</th></tr>`)
	for _, m := range topN(grand.MethodCnt, 8) {
		name := m.Key
		if strings.TrimSpace(name) == "" {
			name = "(未记录)"
		}
		fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="num">%s</td><td class="num">%.2f%%</td></tr>`,
			html.EscapeString(name), humanNum(m.Value), pct(m.Value, total))
	}
	b.WriteString(`</table></div></div></div>`)

	// ---------------- 九、附录：口径说明 ----------------
	b.WriteString(`<div class="section"><h2>九、指标口径说明<span class="aud">附录</span></h2><ul class="summary-list">`)
	b.WriteString(`<li><b>错误</b>：nginx 返回状态码 ≥ 400 的请求；错误率 = 错误数 / 总请求。</li>`)
	fmt.Fprintf(&b, `<li><b>慢请求</b>：响应耗时 &gt; %s（-time-field 决定取 upstream 还是 responsetime）。</li>`, slowThresholdText(cfg))
	b.WriteString(`<li><b>P50/P90/P95/P99</b>：由 10ms 粒度直方图插值，误差 ≤ 10ms；平均值为精确值。</li>`)
	fmt.Fprintf(&b, `<li><b>响应时间口径</b>：只统计有 upstream 计时的 %s 次请求（占 %.1f%%）；nginx 直接返回的请求（静态资源等）没有耗时字段，若把它们当作 0ms 会把平均响应和分位数严重拉低，因此不计入。</li>`,
		humanNum(grand.Timed()), pct(grand.Timed(), total))
	b.WriteString(`<li><b>成功率</b>：2xx+3xx 占比；<b>服务端错误率</b>：5xx 占比。4xx 多为客户端/参数/扫描问题，单独看错误率，不算服务不可用。</li>`)
	b.WriteString(`<li><b>峰值 QPS</b>：按秒统计的最大请求数；QPS 曲线为 5 分钟粒度。</li>`)
	b.WriteString(`<li><b>独立 IP</b>：按日志 client 字段去重；Top 来源仅用于异常/爬虫排查。</li>`)
	b.WriteString(`</ul></div>`)

	fmt.Fprintf(&b, `<div class="footer">本报告由 Nginx 周报系统自动生成 · %s · 数据来源：Nginx access log</div>`,
		time.Now().Format("2006-01-02 15:04"))
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// ---------------------------------------------------------------- 文案

func shortDay(day string) string {
	if len(day) >= 10 {
		return day[5:]
	}
	return day
}

func slotLabel(slot int) string {
	h := slot * analyzer.SlotMinutes / 60
	m := slot * analyzer.SlotMinutes % 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

func errorRateColor(rate float64) string {
	switch {
	case rate > 1.0:
		return "#e53935"
	case rate > 0.5:
		return "#fb8c00"
	}
	return "#43a047"
}

// splitWeekend 返回工作日总量/周末总量与各自天数
func splitWeekend(days []analyzer.DaySummary) (weekdaySum, weekendSum, weekdayN, weekendN int64) {
	for _, d := range days {
		t, err := time.Parse("2006-01-02", d.Day)
		if err != nil {
			continue
		}
		if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
			weekendSum += d.Total
			weekendN++
		} else {
			weekdaySum += d.Total
			weekdayN++
		}
	}
	return
}

// executiveSummary 生成给管理/甲方的要点
func executiveSummary(in Input, total int64, errorRate float64, errCount, n4, n5 int64,
	avgMs, p99Ms float64, slowCount, uniqueIPs int64, peakDay analyzer.DaySummary,
	score float64, gradeName string, topErr, topCalls []uriStat, windowDays int) []string {

	var lines []string

	// 1. 访问量
	chgText := "上周无可比数据"
	if in.Prev != nil && in.Prev.Total > 0 {
		ch, arrow := calcChange(float64(total), float64(in.Prev.Total))
		chgText = fmt.Sprintf("环比%s %.1f%%", strings.TrimSpace(arrow), abs(ch))
	}
	peakText := "无"
	if peakDay.PeakCnt > 0 {
		peakText = fmt.Sprintf("%d req/s（%s %s）", peakDay.PeakCnt, html.EscapeString(peakDay.Day), peakDay.PeakTime.Format("15:04"))
	}
	if windowDays <= 0 {
		windowDays = 7
	}
	lines = append(lines, fmt.Sprintf("<b>访问规模</b>：本周期总请求 <b>%s</b> 次（%s），日均 %s 次；峰值 %s。",
		humanNum(total), chgText, humanNum(total/int64(windowDays)), peakText))

	// 2. 稳定性
	if errCount == 0 {
		lines = append(lines, fmt.Sprintf("<b>稳定性</b>：本周无错误请求，服务运行稳定（5xx 0 次）。"))
	} else {
		lines = append(lines, fmt.Sprintf("<b>稳定性</b>：错误 <b>%d</b> 次，错误率 <b>%.2f%%</b>；其中 5xx %s 次（服务端）、4xx %s 次（客户端/接口约定）。",
			errCount, errorRate, humanNum(n5), humanNum(n4)))
	}

	// 3. 性能
	// 错误率异常的日子单独点名（管理/甲方最关心"哪天出问题"）
	var worstDay analyzer.DaySummary
	worstRate := 0.0
	for _, d := range in.Days {
		if r := pct(d.ErrorCount, d.Total); r > worstRate {
			worstDay, worstRate = d, r
		}
	}
	if worstRate > in.Cfg.ErrorAlert && worstDay.Total > 0 {
		lines = append(lines, fmt.Sprintf("<b>需要关注</b>：%s（%s）错误率高达 <b>%.2f%%</b>（%d 次错误），明显高于其他日期，建议优先排查当日变更或上游状态。",
			html.EscapeString(worstDay.Day), weekdayName(worstDay.Day), worstRate, worstDay.ErrorCount))
	}

	lines = append(lines, fmt.Sprintf("<b>性能</b>：平均响应 <b>%.0f ms</b>，P99 %.0f ms；慢请求（&gt;%s）<b>%d</b> 次，占 %.3f%%。",
		avgMs, p99Ms, slowThresholdText(in.Cfg), slowCount, pct(slowCount, total)))

	// 4. 覆盖
	lines = append(lines, fmt.Sprintf("<b>用户覆盖</b>：独立来源 IP <b>%d</b> 个，总流量 %s。", uniqueIPs, humanBytes(float64(in.Stats.SizeTotal))))

	// 5. 结论
	worstURI := ""
	for _, u := range topErr {
		if u.Err == 0 {
			continue
		}
		worstURI = fmt.Sprintf("，错误最集中的接口是 <code>%s</code>（%d 次）",
			html.EscapeString(trunc(displayURI(u.URI), 60)), u.Err)
		break
	}
	slowestURI := ""
	var slowest uriStat
	for _, u := range topCalls {
		if u.Calls >= 10 && u.AvgMs > slowest.AvgMs {
			slowest = u
		}
	}
	if slowest.AvgMs > 200 && strings.TrimSpace(slowest.URI) != "" {
		slowestURI = fmt.Sprintf("；耗时最高的热点接口是 <code>%s</code>（平均 %.0fms）",
			html.EscapeString(trunc(displayURI(slowest.URI), 60)), slowest.AvgMs)
	}
	conclusion := "整体运行平稳，无需要立即处理的异常"
	switch {
	case score < 70:
		conclusion = "存在明显稳定性问题，建议本周内优先处理"
	case score < 80:
		conclusion = "存在需要关注的指标，建议排期优化"
	case score < 90:
		conclusion = "整体运行平稳，个别指标可继续优化"
	}
	lines = append(lines, fmt.Sprintf("<b>结论</b>：健康度 <b>%s（%.0f 分）</b>，%s%s%s。",
		gradeName, score, conclusion, worstURI, slowestURI))

	return lines
}

// dayBadge 根据当日错误率/慢请求生成状态标签
func dayBadge(d analyzer.DaySummary, cfg *config.Config) string {
	if d.Total == 0 {
		return `<span class="badge-tag badge-green">正常</span>`
	}
	rate := pct(d.ErrorCount, d.Total)
	// 慢请求阈值是「周」口径，摊到单日再留 20% 余量；
	// 用浮点比较避免 int 截断把 100/7 变成 14 而漏判
	slowLimit := float64(cfg.SlowAlert) / 7 * 1.2
	switch {
	case rate > cfg.ErrorAlert:
		return `<span class="badge-tag badge-red">异常</span>`
	case rate > cfg.ErrorAlert*0.5 || float64(d.SlowCount) > slowLimit:
		return `<span class="badge-tag badge-amber">关注</span>`
	}
	return `<span class="badge-tag badge-green">正常</span>`
}
