// Package history 负责统计数据的持久化：
//   - 日记录 daily/report_<day>.json：每天只统计头一天，供周报汇总
//   - 周汇总 report_<start>_to_<end>.json：用于环比
package history

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/fsutil"
)

var (
	reWeeklyName = regexp.MustCompile(`^report_\d{4}-\d{2}-\d{2}_to_(\d{4}-\d{2}-\d{2})\.json$`)
	reDailyName  = regexp.MustCompile(`^report_(\d{4}-\d{2}-\d{2})\.json$`)
)

// Summary 周汇总数据（用于环比）
type Summary struct {
	StartDate  string  `json:"start_date"`
	EndDate    string  `json:"end_date"`
	Total      int64   `json:"total"`
	PeakQPS    int64   `json:"peak_qps"`
	Size       int64   `json:"size"`
	AvgTime    float64 `json:"avg_time"`
	ErrorRate  float64 `json:"error_rate"`
	UniqueIPs  int64   `json:"unique_ips"`
	ErrorCount int64   `json:"error_count"`
	SlowCount  int64   `json:"slow_count"`
}

// Daily 单日统计记录。字段足够重建周报，因此周报无需再读日志。
type Daily struct {
	Day string `json:"day"`
	// Partial 表示该日数据尚未完整（例如统计的是"今天"正在写入的日志），
	// 周报聚合时会优先用日志重算，避免用不完整数据出周报。
	Partial    bool    `json:"partial,omitempty"`
	Total      int64   `json:"total"`
	SizeTotal  int64   `json:"size_total"`
	ErrorCount int64   `json:"error_count"`
	SlowCount  int64   `json:"slow_count"`
	AvgTime    float64 `json:"avg_time"`
	TimeSum    float64 `json:"time_sum"`
	// TimedCount 带耗时记录的请求数（旧记录没有该字段，读入为 0，
	// analyzer.Agg.Timed() 会自动回退到 Total）
	TimedCount  int64   `json:"timed_count,omitempty"`
	TimeBuckets []int64 `json:"time_buckets"`
	// TimeOverflow 10 秒以上的耗时分布（旧日记录没有，读入为 nil）
	TimeOverflow []int64 `json:"time_overflow,omitempty"`
	SlotCnt      []int64 `json:"slot_cnt,omitempty"` // 5 分钟粒度请求量
	// MinuteCnt 1 分钟粒度请求量（长度最长 1440，尾部 0 会裁掉）。
	// 运维报告要用真正的分钟级趋势，不能退化成 5 分钟。
	MinuteCnt []int64 `json:"minute_cnt,omitempty"`
	// MinuteBytes 1 分钟粒度响应字节数（流量趋势）
	MinuteBytes []int64 `json:"minute_bytes,omitempty"`
	// MaxTime 最大响应时间（秒）
	MaxTime float64 `json:"max_time,omitempty"`

	PeakQPS  int64  `json:"peak_qps"`
	PeakTime string `json:"peak_time"`

	HourCnt   map[int]int64    `json:"hour_cnt"`
	StatusCnt map[string]int64 `json:"status_cnt"`
	// 新格式：每接口/每上游一行（省 4 倍哈希与 map 项）
	UriStat      map[string]*analyzer.UriStat      `json:"uri_stat,omitempty"`
	UpstreamStat map[string]*analyzer.UpstreamStat `json:"upstream_stat,omitempty"`

	// 旧格式，仅为兼容历史日记录（读入后会自动折叠进 UriStat/UpstreamStat）
	UriCnt         map[string]int64   `json:"uri_cnt,omitempty"`
	UriTimeSum     map[string]float64 `json:"uri_time_sum,omitempty"`
	UriErrorCnt    map[string]int64   `json:"uri_error_cnt,omitempty"`
	UriSlowCnt     map[string]int64   `json:"uri_slow_cnt,omitempty"`
	ClientCnt      map[string]int64   `json:"client_cnt"`
	MethodCnt      map[string]int64   `json:"method_cnt"`
	UpstreamCnt    map[string]int64   `json:"upstream_cnt,omitempty"`
	UpstreamTimeS  map[string]float64 `json:"upstream_time_sum,omitempty"`
	UpstreamTimeC  map[string]int64   `json:"upstream_time_cnt,omitempty"`
	UpstreamErrCnt map[string]int64   `json:"upstream_err_cnt,omitempty"`
	ErrorStatus    map[string]int64   `json:"error_status"`
	// 深度分析字段（旧日记录没有，读入为空，报告会自动隐藏对应板块）
	ErrorURI    map[string]map[string]int64 `json:"error_uri,omitempty"`
	DeviceCnt   map[string]int64            `json:"device_cnt,omitempty"`
	BrowserCnt  map[string]int64            `json:"browser_cnt,omitempty"`
	RefererCnt  map[string]int64            `json:"referer_cnt,omitempty"`
	DirectCnt   int64                       `json:"direct_cnt,omitempty"`
	FileTypeCnt map[string]int64            `json:"file_type_cnt,omitempty"`

	// 多站点：根日记录的 Sites 保存各站点当天统计；站点 Daily 的 Site/SiteKind
	// 用于周报恢复站点维度。旧记录没有这些字段，读入为空。
	Sites         map[string]*Daily `json:"sites,omitempty"`
	Site          string            `json:"site,omitempty"`
	SiteKind      string            `json:"site_kind,omitempty"`
	SiteName      string            `json:"site_name,omitempty"`
	StaticSkipped int64             `json:"static_skipped,omitempty"`
	StaticBytes   int64             `json:"static_bytes,omitempty"`
	BotSkipped    int64             `json:"bot_skipped,omitempty"`
	SiteSourceCnt map[string]int64  `json:"site_source_cnt,omitempty"`
	// SiteConflicts / SiteInvalid 行内域名与文件映射冲突、以及非法站点值计数。
	SiteConflicts int64 `json:"site_conflicts,omitempty"`
	SiteInvalid   int64 `json:"site_invalid,omitempty"`
}

// DailyDir 返回日记录目录
func DailyDir(historyDir string) string {
	return filepath.Join(historyDir, "daily")
}

// NewDaily 由天级统计量构造日记录；partial=true 表示数据未完整（当天仍在写入）
func NewDaily(day string, agg *analyzer.Agg, partial bool) *Daily {
	peakTime, peakCnt := agg.Peak()
	d := &Daily{
		Day:          day,
		Partial:      partial,
		Total:        agg.Total,
		SizeTotal:    agg.SizeTotal,
		ErrorCount:   agg.ErrorCount,
		SlowCount:    agg.SlowCount,
		AvgTime:      agg.AvgTime(),
		TimeSum:      agg.TimeSum,
		TimedCount:   agg.TimedCount,
		TimeBuckets:  trimBuckets(agg.TimeBuckets),
		TimeOverflow: trimBuckets(agg.TimeOverflow),
		SlotCnt:      trimBuckets(agg.SlotCnt),
		MinuteCnt:    trimBuckets(agg.MinuteCnt),
		MinuteBytes:  trimBuckets(agg.MinuteBytes),
		MaxTime:      agg.MaxTime,
		PeakQPS:      peakCnt,
		HourCnt:      agg.HourCnt,
		StatusCnt:    agg.StatusCnt,
		UriStat:      copyUriStat(agg.UriStat),
		ClientCnt:    agg.ClientCnt,
		MethodCnt:    agg.MethodCnt,
		UpstreamStat: copyUpstreamStat(agg.UpstreamStat),
		ErrorStatus:  agg.ErrorStatus,
		ErrorURI:     copyNestedI64(agg.ErrorURI),
		DeviceCnt:    agg.DeviceCnt,
		BrowserCnt:   agg.BrowserCnt,
		RefererCnt:   agg.RefererCnt,
		DirectCnt:    agg.DirectCnt,
		FileTypeCnt:  agg.FileTypeCnt,
	}
	if peakCnt > 0 {
		// 带时区偏移保存：日志时区与服务器时区可能不同，
		// 只存"墙钟时间"会让周报里的峰值时刻错位。
		d.PeakTime = peakTime.Format(time.RFC3339)
	}
	return d
}

// ToAgg 还原为可累加的统计量
func (d *Daily) ToAgg() *analyzer.Agg {
	a := analyzer.NewAgg()
	a.Total = d.Total
	a.SizeTotal = d.SizeTotal
	a.ErrorCount = d.ErrorCount
	a.SlowCount = d.SlowCount
	a.TimeSum = d.TimeSum
	a.TimedCount = d.TimedCount
	a.MaxTime = d.MaxTime
	a.DirectCnt = d.DirectCnt
	for i, c := range d.TimeBuckets {
		if i >= len(a.TimeBuckets) {
			break
		}
		a.TimeBuckets[i] = c
	}
	for i, c := range d.TimeOverflow {
		if i >= len(a.TimeOverflow) {
			break
		}
		a.TimeOverflow[i] = c
	}
	for i, c := range d.SlotCnt {
		if i >= len(a.SlotCnt) {
			break
		}
		a.SlotCnt[i] = c
	}
	for i, c := range d.MinuteCnt {
		if i >= len(a.MinuteCnt) {
			break
		}
		a.MinuteCnt[i] = c
	}
	for i, c := range d.MinuteBytes {
		if i >= len(a.MinuteBytes) {
			break
		}
		a.MinuteBytes[i] = c
	}
	if d.PeakQPS > 0 {
		if t, err := parsePeakTime(d.PeakTime); err == nil {
			a.SetPeak(t.Unix(), int32(d.PeakQPS))
			a.SetLoc(t.Location())
		}
	}
	copyI64 := func(src map[string]int64) map[string]int64 {
		out := make(map[string]int64, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	a.HourCnt = make(map[int]int64, len(d.HourCnt))
	for k, v := range d.HourCnt {
		a.HourCnt[k] = v
	}
	a.StatusCnt = copyI64(d.StatusCnt)
	d.foldLegacy()
	a.UriStat = copyUriStat(d.UriStat)
	a.ClientCnt = copyI64(d.ClientCnt)
	a.MethodCnt = copyI64(d.MethodCnt)
	a.UpstreamStat = copyUpstreamStat(d.UpstreamStat)
	a.ErrorStatus = copyI64(d.ErrorStatus)
	a.ErrorURI = copyNestedI64(d.ErrorURI)
	a.DeviceCnt = copyI64(d.DeviceCnt)
	a.BrowserCnt = copyI64(d.BrowserCnt)
	a.RefererCnt = copyI64(d.RefererCnt)
	a.FileTypeCnt = copyI64(d.FileTypeCnt)
	return a
}

// copyNestedI64 深拷贝 status -> uri -> count 二层表
func copyNestedI64(src map[string]map[string]int64) map[string]map[string]int64 {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]map[string]int64, len(src))
	for status, m := range src {
		inner := make(map[string]int64, len(m))
		for k, v := range m {
			inner[k] = v
		}
		out[status] = inner
	}
	return out
}

// parsePeakTime 解析峰值时刻：新格式为 RFC3339（带偏移），
// 兼容旧记录的 "2006-01-02 15:04:05"（按本机时区解释）。
func parsePeakTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("空时间")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
}

// foldLegacy 把旧版日记录里的 4 张 URI 表 / 4 张上游表折进新结构（内存中，不写盘）
func (d *Daily) foldLegacy() {
	if len(d.UriCnt) > 0 {
		if d.UriStat == nil {
			d.UriStat = make(map[string]*analyzer.UriStat, len(d.UriCnt))
		}
		for k, calls := range d.UriCnt {
			u := d.UriStat[k]
			if u == nil {
				u = &analyzer.UriStat{}
				d.UriStat[k] = u
			}
			u.Calls += calls
		}
		for k, v := range d.UriTimeSum {
			if u := d.UriStat[k]; u != nil {
				u.TimeS += v
			}
		}
		for k, v := range d.UriErrorCnt {
			if u := d.UriStat[k]; u != nil {
				u.Errors += v
			}
		}
		for k, v := range d.UriSlowCnt {
			if u := d.UriStat[k]; u != nil {
				u.Slows += v
			}
		}
		d.UriCnt, d.UriTimeSum, d.UriErrorCnt, d.UriSlowCnt = nil, nil, nil, nil
	}
	if len(d.UpstreamCnt) > 0 {
		if d.UpstreamStat == nil {
			d.UpstreamStat = make(map[string]*analyzer.UpstreamStat, len(d.UpstreamCnt))
		}
		for k, calls := range d.UpstreamCnt {
			u := d.UpstreamStat[k]
			if u == nil {
				u = &analyzer.UpstreamStat{}
				d.UpstreamStat[k] = u
			}
			u.Calls += calls
		}
		for k, v := range d.UpstreamTimeS {
			if u := d.UpstreamStat[k]; u != nil {
				u.TimeS += v
			}
		}
		for k, v := range d.UpstreamTimeC {
			if u := d.UpstreamStat[k]; u != nil {
				u.TimeC += v
			}
		}
		for k, v := range d.UpstreamErrCnt {
			if u := d.UpstreamStat[k]; u != nil {
				u.Errors += v
			}
		}
		d.UpstreamCnt, d.UpstreamTimeS, d.UpstreamTimeC, d.UpstreamErrCnt = nil, nil, nil, nil
	}
}

// copyUriStat 深拷贝并做轻度量化：time_sum 保留到毫秒即可，
// 否则 JSON 里会出现 "335472.6989995139" 这种 17 位浮点，日记录白白多出几 MB。
func copyUriStat(src map[string]*analyzer.UriStat) map[string]*analyzer.UriStat {
	if src == nil {
		return nil
	}
	out := make(map[string]*analyzer.UriStat, len(src))
	for k, v := range src {
		cp := *v
		cp.TimeS = math.Round(cp.TimeS*1000) / 1000
		out[k] = &cp
	}
	return out
}

func copyUpstreamStat(src map[string]*analyzer.UpstreamStat) map[string]*analyzer.UpstreamStat {
	if src == nil {
		return nil
	}
	out := make(map[string]*analyzer.UpstreamStat, len(src))
	for k, v := range src {
		cp := *v
		cp.TimeS = math.Round(cp.TimeS*1000) / 1000
		out[k] = &cp
	}
	return out
}

// trimBuckets 去掉尾部连续的 0 桶，减小 JSON 体积
func trimBuckets(b []int64) []int64 {
	last := 0
	for i, c := range b {
		if c != 0 {
			last = i + 1
		}
	}
	if last == 0 {
		return []int64{}
	}
	out := make([]int64, last)
	copy(out, b[:last])
	return out
}

// SaveDaily 保存单日记录
func SaveDaily(dir, day string, d *Daily) error {
	dst := DailyDir(dir)
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dst, fmt.Sprintf("report_%s.json", day)), data, 0644)
}

// LoadDaily 读取单日记录
func LoadDaily(dir, day string) (*Daily, error) {
	fpath := filepath.Join(DailyDir(dir), fmt.Sprintf("report_%s.json", day))
	data, err := os.ReadFile(fpath)
	if err != nil {
		return nil, err
	}
	var d Daily
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// IsDailyFinal 判断某天的日记录是否存在且为最终数据（非 partial）。
// 常驻调度用它判断"昨天是否已经统计完成"，partial 记录会在当天结束后的
// 第一次 daily 任务里被重算覆盖。
func IsDailyFinal(dir, day string) bool {
	d, err := LoadDaily(dir, day)
	if err != nil {
		return false
	}
	return !d.Partial
}

// Save 保存周汇总到 JSON
func Save(dir, start, end string, summary *Summary) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	fname := fmt.Sprintf("report_%s_to_%s.json", start, end)
	fpath := filepath.Join(dir, fname)

	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(fpath, data, 0644)
}

// LoadPrevious 加载「紧邻的上一个等长周期」的历史数据用于环比。
//
// 默认周报是 7 天窗口 → 取上周一~上周日；用 -start/-end 指定自定义区间时
// 按窗口长度等长回溯，避免拿 3 天去比 7 天（环比失真）。
func LoadPrevious(dir, startDate, endDate string) (*Summary, error) {
	currStart, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil, err
	}
	length := 7
	if e, err2 := time.Parse("2006-01-02", endDate); err2 == nil && !e.Before(currStart) {
		length = int(e.Sub(currStart).Hours()/24) + 1
	}
	prevStart := currStart.AddDate(0, 0, -length)
	prevEnd := currStart.AddDate(0, 0, -1)

	fname := fmt.Sprintf("report_%s_to_%s.json",
		prevStart.Format("2006-01-02"), prevEnd.Format("2006-01-02"))
	fpath := filepath.Join(dir, fname)

	data, err := os.ReadFile(fpath)
	if err != nil {
		return nil, err
	}

	var summary Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

// LoadRecent 加载 end 日期早于 beforeStart 的最近 n 个周汇总（按时间升序），
// 用于报告里的「近 N 周趋势」。文件损坏或缺失的周会被跳过。
func LoadRecent(dir, beforeStart string, n int) []*Summary {
	if n <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*Summary
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := reWeeklyName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		// 只取 start 早于当前周的历史周
		if beforeStart != "" && m[1] >= beforeStart {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var sum Summary
		if err := json.Unmarshal(data, &sum); err != nil {
			continue
		}
		out = append(out, &sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartDate < out[j].StartDate })
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Cleanup 删除超过保留天数的历史数据（周汇总与日记录），避免磁盘无限增长。
// 以各文件自身的结束日期/日期判断，与运行时间无关。
func Cleanup(dir string, keepDays int) error {
	if keepDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)

	if err := cleanupDir(dir, reWeeklyName, cutoff); err != nil {
		return err
	}
	return cleanupDir(DailyDir(dir), reDailyName, cutoff)
}

func cleanupDir(dir string, re *regexp.Regexp, cutoff time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		end, err := time.Parse("2006-01-02", m[1])
		if err != nil || !end.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
