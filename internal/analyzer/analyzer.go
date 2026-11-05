// Package analyzer 负责解析 Nginx 访问日志（JSON / combined / 自定义 / CSV）并做统计
//
// 性能与低负载设计：
//   - 不使用正则逐字段匹配，改为 strings.Index 直查字段，避免每行十余次正则扫描
//   - 时间戳手写解析（固定布局），不调用 time.Parse，也不产生额外分配
//   - 内置 CPU 占空比节流：按 ChunkLines 分块，按目标占用率插入 sleep，
//     保证长时间扫描时也只占用设定比例的 CPU
//   - 日志按时间递增时，连续出现「超出结束日期」的行会提前结束扫描
//
// 统计量统一收敛到 Agg：文件级、天级复用同一套累加逻辑，
// 这样「日记录」可以单独落盘，周报再由 7 份日记录合并出来。
package analyzer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nginx-report/internal/accesslog"
	"nginx-report/internal/site"
	"nginx-report/internal/staticasset"
)

// 响应时间直方图：10ms 一桶，覆盖 0 ~ 10s，>=10s 归入最后一桶。
// 用固定桶代替保存全部耗时，内存恒定，且日记录 JSON 体积可控。
const (
	TimeBucketMS    = 10
	TimeBucketCount = 1001

	// SlotMinutes 业务曲线粒度（分钟）：一天 288 个槽，用于 QPS 曲线与峰值定位
	SlotMinutes = 5
	// SlotCount 每天的槽数 = 24*60/SlotMinutes
	SlotCount = 24 * 60 / SlotMinutes

	// TimeOverflowCount 10 秒以上的耗时桶：
	//   0..49   : [10s, 60s)，1 秒一桶
	//   50..103 : [60s, 600s)，10 秒一桶
	//   104     : [600s, +Inf)
	// 原来的 TimeBuckets 只覆盖 0~10s，超过 10s 的请求会挤在最后一桶，
	// 导致 P95/P99 永远显示成 10s。加一层溢出桶后百分位才准。
	TimeOverflowCount = 105

	// MinuteCount 一天有多少分钟（1440）。
	// 分钟级明细只服务于「直接读日志」时生成的运维报告，
	// 体积比 5 分钟槽大 5 倍，因此不写入日记录，周报聚合时自动退化为 SlotCnt。
	MinuteCount = 24 * 60

	// maxSecondKeys 逐秒表条目上限。
	// 一天最多 86400 个不同秒，20 万条足以覆盖 2 天以上的逐秒数据；
	// 再大（例如把几个月的日志当单文件跑）就只保留流式峰值，避免内存被撑爆。
	maxSecondKeys = 200000
)

// 默认参数
const (
	// DefaultChunkLines 每处理多少行做一次节流检查
	DefaultChunkLines = 4000
	// earlyExitLines 连续多少行「比结束日期还新」后提前结束（日志通常按时间递增）
	earlyExitLines = 2000
)

// Options 日志处理选项
type Options struct {
	// Start / End 只统计该日期区间内的记录，格式 YYYY-MM-DD，空表示不限
	Start string
	End   string
	// CPUPercent 目标 CPU 占用百分比（1-100），<=0 或 >=100 表示不限制
	CPUPercent int
	// ChunkLines 节流检查的行间隔，<=0 取 DefaultChunkLines
	ChunkLines int
	// EarlyExitLines 跨日提前退出阈值：0 取默认值，负数表示关闭
	EarlyExitLines int
	// URINormalize 统计接口时去掉 ?query 部分（默认 false，由调用方决定）
	URINormalize bool
	// SkipStatic 解析时直接跳过静态资源请求（css/js/图片/字体等）。
	// 跳过的行不进 PV / 分钟趋势 / 状态码 / IP / 流量 / 错误 / 慢请求，
	// 单独计入 FileStats.StaticSkipped，便于对账。
	SkipStatic bool
	// FromMinute 只统计每天 HH:MM（含）之后的记录，值为自 00:00 起的分钟数；
	// <0 表示不限制。对应原 analyze_log_fast.sh 的第二个参数（如 11:30）。
	FromMinute int
	// RealIP 优先使用 real_client（代理/CDN 场景下的真实客户端 IP），
	// 为空或 "-" 时回退到 client。
	RealIP bool
	// TimeField 响应时间取字段：auto（默认，上游优先）/ upstream / response
	TimeField string
	// SlowMS 慢请求阈值（毫秒），<=0 取 1000
	SlowMS int
	// ExcludeBots 按 User-Agent 排除爬虫/探针/监控请求
	ExcludeBots bool
	// LogFormat 日志格式：auto（默认）/ json / combined / csv / nginx log_format 模板
	LogFormat string
	// CSVFields 当 -log-format csv 且文件没有表头时，按该列顺序解析
	CSVFields string
	// CSVSkipHeader 强制跳过 CSV 第一行（列名无法识别但确实是表头时用）
	CSVSkipHeader bool
	// JSONFields 自定义 JSON 字段映射，格式：源字段=目标,源字段=目标
	// 目标可用 time/client/real_client/method/uri/request/status/size/
	// upstream_addr/upstream_status/upstream_time/response_time/agent/referer
	JSONFields string

	// SiteEnabled 开启站点维度统计。关闭时保持旧路径，零额外开销。
	SiteEnabled bool
	// SiteDefault 文件级默认站点 key（d:example.com / i:1.2.3.4 / u:unknown）。
	SiteDefault string
	// SiteDefaultSource 文件级站点来源：map / filename / name / unknown。
	SiteDefaultSource string
	// SiteField 行内站点字段名；空表示按 site 别名自动尝试。
	SiteField string
	// SiteFrom auto / field / map / filename / dir。
	SiteFrom string
	// SiteIPMode separate / inherit / ignore。
	SiteIPMode string
	// SiteForce 为 true 时忽略行内字段，全部使用 SiteDefault。
	SiteForce bool
	// SiteStripWWW 将 www.example.com 归并到 example.com。
	SiteStripWWW bool
	// SiteAliases 域名别名 -> 主域名（已归一化）。
	SiteAliases map[string]string
	// SiteMax 站点数量上限，超出归入 (其他站点)。
	SiteMax int
}

// Agg 一组可累加的统计量（文件级 / 天级共用）
type Agg struct {
	Total      int64
	SizeTotal  int64
	ErrorCount int64
	SlowCount  int64
	TimeSum    float64
	// TimedCount 真正带耗时记录的请求数（TimeSum/TimeBuckets 的分母）。
	// Total 包含没有耗时的请求（静态资源等），两者不等时报告里要说明口径。
	TimedCount  int64
	TimeBuckets []int64
	// TimeOverflow 10 秒以上的耗时分布（见 TimeOverflowCount）
	TimeOverflow []int64
	SlotCnt      []int64 // 5 分钟粒度请求量，长度 SlotCount
	// MinuteCnt 分钟粒度请求量，长度 MinuteCount。
	// 只在按天聚合时用于运维报告的「按分钟 PV 趋势」；
	// 文件级 Agg 上它是各天同一分钟之和，没有业务含义，不要直接展示。
	MinuteCnt []int64
	// MinuteBytes 分钟粒度响应字节数，与 MinuteCnt 对齐（流量趋势）
	MinuteBytes []int64
	// MaxTime 最大响应时间（秒，仅统计有耗时记录的行）
	MaxTime float64
	// SecondCnt 逐秒请求数（用于精确峰值与前 N 秒）。
	// 有条目上限：日志跨很多天时不会再无限增长，超过上限后靠下面的流式峰值兜底。
	SecondCnt map[int64]int32
	peakSec   int64
	peakCnt   int32
	lastSec   int64
	curCnt    int32
	loc       *time.Location // 日志时区，用于把 Unix 秒还原成可展示的峰值时刻
	HourCnt   map[int]int64
	// UriStat 每个接口一行统计。
	// 合成一张表是性能关键：逐行统计时同一 URI 只哈希一次
	// （拆成 4 张表意味着同一字符串被哈希 4 遍，profile 里 1/4 CPU 都在这）。
	UriStat map[string]*UriStat
	// lastURI/lastStat 一级缓存：访问日志里同一接口常常连续出现（心跳、轮询），
	// 命中时连哈希都不用做，直接复用上一行的统计项指针。
	lastURI   string
	lastStat  *UriStat
	ClientCnt map[string]int64
	StatusCnt map[string]int64
	MethodCnt map[string]int64
	// UpstreamStat 每台上游一行统计（同样合并成一张表）
	UpstreamStat map[string]*UpstreamStat
	ErrorStatus  map[string]int64
	// ErrorURI 错误状态码 -> URI -> 次数（用于 404/400/5xx Top 榜）
	ErrorURI map[string]map[string]int64
	// DeviceCnt / BrowserCnt 设备、浏览器分布（来自 agent 字段）
	DeviceCnt  map[string]int64
	BrowserCnt map[string]int64
	// RefererCnt 来源域名分布（直接访问单独计数）
	RefererCnt map[string]int64
	DirectCnt  int64
	// FileTypeCnt 文档/媒体文件类型分布（从原始 request_uri 的 query 里识别，如 name=xx.xlsx）
	FileTypeCnt map[string]int64
}

// NewAgg 创建空的统计量
func NewAgg() *Agg {
	return &Agg{
		TimeBuckets:  make([]int64, TimeBucketCount),
		TimeOverflow: make([]int64, TimeOverflowCount),
		SlotCnt:      make([]int64, SlotCount),
		MinuteCnt:    make([]int64, MinuteCount),
		MinuteBytes:  make([]int64, MinuteCount),
		SecondCnt:    make(map[int64]int32),
		HourCnt:      make(map[int]int64),
		UriStat:      make(map[string]*UriStat),
		ClientCnt:    make(map[string]int64),
		StatusCnt:    make(map[string]int64),
		MethodCnt:    make(map[string]int64),
		UpstreamStat: make(map[string]*UpstreamStat),
		ErrorStatus:  make(map[string]int64),
		ErrorURI:     make(map[string]map[string]int64),
		DeviceCnt:    make(map[string]int64),
		BrowserCnt:   make(map[string]int64),
		RefererCnt:   make(map[string]int64),
		FileTypeCnt:  make(map[string]int64),
	}
}

// UriStat 单个接口的累计统计
type UriStat struct {
	Calls  int64   `json:"calls"`
	TimeS  float64 `json:"time_sum"`
	Errors int64   `json:"errors"`
	Slows  int64   `json:"slows"`
	// SlowTimeS 慢请求的耗时总和（用来算「慢请求 URL」的慢请求平均耗时）
	SlowTimeS float64 `json:"slow_time_sum,omitempty"`
	// Bytes 该接口的响应字节总数（Top 大流量 URL 用）；旧日记录没有这个字段
	Bytes int64 `json:"bytes,omitempty"`
}

// UpstreamStat 单台上游的累计统计
type UpstreamStat struct {
	Calls  int64   `json:"calls"`
	TimeS  float64 `json:"time_sum"`
	TimeC  int64   `json:"time_count"`
	Errors int64   `json:"errors"`
}

// interner 复用重复出现的小字段字符串（status/method/upstream_addr），
// 避免每行都 Clone 一次；上限保护防止异常日志把表撑爆。
type interner struct {
	m map[string]string
}

const internLimit = 4096

func (in *interner) get(b []byte) string {
	if in.m == nil {
		in.m = make(map[string]string, 64)
	}
	// 注意：m[string(b)] 这种写法不会为查找分配内存
	if s, ok := in.m[string(b)]; ok {
		return s
	}
	// string(b) 本身就会复制一份独立底层数组，直接存即可；
	// 再调 strings.Clone 会多一次分配/拷贝。
	s := string(b)
	if len(in.m) < internLimit {
		in.m[s] = s
	}
	return s
}

// record 一行日志解析出的关键字段
type record struct {
	ts           time.Time
	day          string
	hour         int
	uri          string
	status       string
	client       string
	method       string
	upstreamAddr string
	// device / browser 由 agent 归类（移动端/桌面端、Chrome/Edge/...）
	device  string
	browser string
	// referHost 来源域名（含端口）；isDirect 表示 referer 为空/-
	referHost    string
	isDirect     bool
	fileType     string
	size         int64
	upstreamTime float64
	// hasTime 该行是否真的记录了耗时（nginx 对静态资源/本地直接返回会写 "-"）。
	// 没有耗时的行不能进直方图，否则会把 P50/P99 和平均响应整体拉低。
	hasTime bool
	isError bool
	isSlow  bool
	// 上游（后端）自身返回 4xx/5xx 时为 true，与 nginx 侧状态分开统计
	isUpstreamErr bool
}

// timeBucket 耗时（秒）落入的直方图桶。
// 先做浮点比较再转 int：日志里出现离谱的耗时时，int() 转换会溢出成负数，
// 直接拿去索引数组会 panic。
func timeBucket(sec float64) int {
	// 用 !(sec > 0) 同时挡住负数、0 和 NaN，避免 int(NaN) 变成负索引
	if !(sec > 0) {
		return 0
	}
	if sec >= float64(TimeBucketCount-1)*TimeBucketMS/1000.0 {
		return TimeBucketCount - 1
	}
	return int(sec * 1000 / TimeBucketMS)
}

// add 累加一条记录
func (a *Agg) add(r *record) {
	// 防御畸形/外部构造的记录：NaN、Inf、负数会让 TimeSum/百分比/报表全部污染。
	if r.upstreamTime < 0 || math.IsNaN(r.upstreamTime) || math.IsInf(r.upstreamTime, 0) {
		r.upstreamTime = 0
		r.hasTime = false
		r.isSlow = false
	} else if r.upstreamTime > maxTimingSeconds {
		r.upstreamTime = maxTimingSeconds
	}
	if r.size < 0 || r.size > maxSizeBytes {
		r.size = 0
	}
	a.Total++
	addSaturating(&a.SizeTotal, r.size)
	if r.hasTime {
		a.TimeSum += r.upstreamTime
		a.TimedCount++
		if r.upstreamTime < 10.0 {
			a.TimeBuckets[timeBucket(r.upstreamTime)]++
		} else if i := overflowBucket(r.upstreamTime); i >= 0 {
			a.TimeOverflow[i]++
		}
	}
	minute := r.hour*60 + r.ts.Minute()
	slot := minute / SlotMinutes
	if slot >= 0 && slot < len(a.SlotCnt) {
		a.SlotCnt[slot]++
	}
	if minute >= 0 && minute < len(a.MinuteCnt) {
		a.MinuteCnt[minute]++
	}
	if minute >= 0 && minute < len(a.MinuteBytes) {
		addSaturating(&a.MinuteBytes[minute], r.size)
	}
	sec := r.ts.Unix()
	// 流式峰值：日志按时间递增时，这一对变量就足以精确得到峰值，且只占 O(1) 内存
	if sec == a.lastSec {
		a.curCnt++
	} else {
		a.lastSec, a.curCnt = sec, 1
	}
	if a.curCnt > a.peakCnt {
		a.peakCnt, a.peakSec = a.curCnt, sec
	}
	if len(a.SecondCnt) < maxSecondKeys {
		a.SecondCnt[sec]++
	} else if _, ok := a.SecondCnt[sec]; ok {
		a.SecondCnt[sec]++
	}
	if a.loc == nil {
		a.loc = r.ts.Location()
	}
	a.HourCnt[r.hour]++
	a.StatusCnt[cappedKey(a.StatusCnt, r.status)]++
	a.MethodCnt[cappedKey(a.MethodCnt, r.method)]++

	if r.hasTime {
		if r.upstreamTime > a.MaxTime {
			a.MaxTime = r.upstreamTime
		}
	}

	// 一次查找拿到接口统计，后续字段就地累加（同一字符串只哈希一次）
	u := a.Uri(r.uri)
	u.Calls++
	u.TimeS += r.upstreamTime
	addSaturating(&u.Bytes, r.size)
	if r.isError {
		u.Errors++
	}
	if r.isSlow {
		u.Slows++
		u.SlowTimeS += r.upstreamTime
	}

	a.ClientCnt[cappedKey(a.ClientCnt, r.client)]++
	a.DeviceCnt[r.device]++
	a.BrowserCnt[r.browser]++
	if r.isDirect {
		a.DirectCnt++
	} else if r.referHost != "" {
		a.RefererCnt[cappedKey(a.RefererCnt, r.referHost)]++
	}
	if r.fileType != "" {
		a.FileTypeCnt[cappedKey(a.FileTypeCnt, r.fileType)]++
	}

	if r.upstreamAddr != "" && r.upstreamAddr != "-" {
		up := a.Upstream(r.upstreamAddr)
		up.Calls++
		up.TimeS += r.upstreamTime
		up.TimeC++
		if r.isUpstreamErr {
			up.Errors++
		}
	}
	if r.isError {
		a.ErrorCount++
		a.ErrorStatus[cappedKey(a.ErrorStatus, r.status)]++
		statusKey := r.status
		if len(a.ErrorURI) >= maxErrorStatusKeys {
			if _, ok := a.ErrorURI[statusKey]; !ok {
				statusKey = OverflowKey
			}
		}
		m := a.ErrorURI[statusKey]
		if m == nil {
			m = make(map[string]int64)
			a.ErrorURI[statusKey] = m
		}
		key := r.uri
		if _, ok := m[key]; !ok && len(m) >= maxMapKeys {
			key = OverflowKey
		}
		m[key]++
	}
	if r.isSlow {
		a.SlowCount++
	}
}

// flushSecond 把「当前正在累计的这一秒」写入逐秒明细表。
// 每翻到新的一秒调用一次，扫描结束时要再调一次，否则最后一秒会漏记。
func (a *Agg) flushSecond() {
	if a.lastSec == 0 || a.curCnt <= 0 {
		return
	}
	if len(a.SecondCnt) < maxSecondKeys {
		a.SecondCnt[a.lastSec] = a.curCnt
	} else if _, ok := a.SecondCnt[a.lastSec]; ok {
		a.SecondCnt[a.lastSec] = a.curCnt
	}
}

// Uri 取（或创建）接口统计项，带基数上限保护
func (a *Agg) Uri(uri string) *UriStat {
	key := cappedKeyU(a.UriStat, uri)
	if v, ok := a.UriStat[key]; ok {
		return v
	}
	v := &UriStat{}
	a.UriStat[key] = v
	return v
}

// Upstream 取（或创建）上游统计项
func (a *Agg) Upstream(addr string) *UpstreamStat {
	key := cappedKeyU(a.UpstreamStat, addr)
	if v, ok := a.UpstreamStat[key]; ok {
		return v
	}
	v := &UpstreamStat{}
	a.UpstreamStat[key] = v
	return v
}

// 基数上限：URI 带随机参数或遭遇扫描时，map 可能无限膨胀，
// 既吃内存也让日记录文件暴涨。超过上限后统一归入 OverflowKey。
const (
	// maxMapKeys 单日保留的 URI / 客户端 / 上游条目上限。
	// 2 万条足够支撑「Top 15 / Top 10」分析；开启 URI 归一化后真实站点通常只有几百条，
	// 这个上限主要防"带随机参数的 URI"把日记录撑到几十 MB。
	maxMapKeys  = 20000
	OverflowKey = "(其他)"

	// 日志字段长度上限：防止单行超长 URI/UA/状态码作为 map key 长期驻留，
	// 被恶意日志放大成几十 MB 甚至 GB 的常驻内存。只对超长字段截断，
	// 正常值不会触发额外分配。
	maxURIKey      = 2048
	maxClientKey   = 128
	maxUpstreamKey = 256
	maxRefererKey  = 256
	maxShortKey    = 32
	// maxErrorStatusKeys 错误状态码到 URI 的外层表更严格；正常站点只有个位数状态码。
	maxErrorStatusKeys = 256

	// maxTimingSeconds / maxSizeBytes 防止恶意/损坏日志把累计量推到 Inf 或 int64 溢出。
	// 真实 nginx 访问不会出现超过 31 年或 1TB 的单条记录。
	maxTimingSeconds = 1e9
	maxSizeBytes     = int64(1) << 40
)

// capBytes 截断超长字段并加省略标记；正常长度直接返回原切片，不分配。
func capBytes(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	out := make([]byte, max+3)
	copy(out, b[:max])
	copy(out[max:], "...")
	return out
}

// addSaturating 累加非负字节数，溢出时钉在 MaxInt64，避免大流量日志把统计写成负数。
func addDayInt(m *map[string]int64, day string, v int64) {
	if v <= 0 {
		return
	}
	if day == "" {
		day = "unknown"
	}
	if *m == nil {
		*m = make(map[string]int64)
	}
	(*m)[day] += v
}

func addDaySaturating(m *map[string]int64, day string, v int64) {
	if v <= 0 {
		return
	}
	if day == "" {
		day = "unknown"
	}
	if *m == nil {
		*m = make(map[string]int64)
	}
	cur := (*m)[day]
	addSaturating(&cur, v)
	(*m)[day] = cur
}

func mergeDayI64(dst *map[string]int64, src map[string]int64) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]int64, mapCapHint(len(src), maxMapKeys))
	}
	for day, v := range src {
		cur := (*dst)[day]
		addSaturating(&cur, v)
		(*dst)[day] = cur
	}
}

func addSaturating(dst *int64, v int64) {
	if v <= 0 {
		return
	}
	if *dst > math.MaxInt64-v {
		*dst = math.MaxInt64
		return
	}
	*dst += v
}

// mapCapHint 限制从外部日记录/合并输入继承的 make 容量，避免损坏 JSON
// 里的超大 map 在合并时先申请巨量内存。
func mapCapHint(n, max int) int {
	if n > max {
		return max
	}
	return n
}

func cappedKey(m map[string]int64, key string) string {
	// 绝大多数时间 map 还没到上限，直接返回，省掉一次哈希查找。
	if len(m) < maxMapKeys {
		return key
	}
	if _, ok := m[key]; ok {
		return key
	}
	return OverflowKey
}

// cappedKeyU 同 cappedKey，用于 map[string]*T 形式的统计表
func cappedKeyU[T any](m map[string]*T, key string) string {
	if len(m) < maxMapKeys {
		return key
	}
	if _, ok := m[key]; ok {
		return key
	}
	return OverflowKey
}

// MergeFrom 将 o 累加到 a。o 为 nil 或与 a 相同（自合并会翻倍）时直接返回。
func (a *Agg) MergeFrom(o *Agg) {
	if o == nil || a == o {
		return // 自我合并会凭空翻倍，直接忽略
	}
	// 接收方本身也可能是旧日记录：先把它的样本数补成 Total，再合并。
	if a.TimedCount == 0 && a.Total > 0 &&
		(a.TimeSum > 0 || a.MaxTime > 0 || (len(a.TimeBuckets) > 0 && a.TimeBuckets[0] > 0)) {
		a.TimedCount = a.Total
	}
	a.Total += o.Total
	if o.TimedCount > 0 {
		a.TimedCount += o.TimedCount
	} else if o.Total > 0 && (o.TimeSum > 0 || o.MaxTime > 0 || (len(o.TimeBuckets) > 0 && o.TimeBuckets[0] > 0)) {
		// 旧版日记录没有 TimedCount；TimeSum/MaxTime/bucket0 有值说明这些
		// 记录带耗时。合并时必须补上它们的样本数，否则和新记录混算时
		// 平均响应/P95/P99 会用旧 TimeSum 除以新 TimedCount，数值偏大。
		a.TimedCount += o.Total
	}
	addSaturating(&a.SizeTotal, o.SizeTotal)
	a.ErrorCount += o.ErrorCount
	a.SlowCount += o.SlowCount
	a.TimeSum += o.TimeSum
	if o.MaxTime > a.MaxTime {
		a.MaxTime = o.MaxTime
	}
	a.DirectCnt += o.DirectCnt

	if len(o.TimeBuckets) > len(a.TimeBuckets) {
		grown := make([]int64, len(o.TimeBuckets))
		copy(grown, a.TimeBuckets)
		a.TimeBuckets = grown
	}
	for i, c := range o.TimeBuckets {
		a.TimeBuckets[i] += c
	}

	if len(o.TimeOverflow) > len(a.TimeOverflow) {
		grown := make([]int64, len(o.TimeOverflow))
		copy(grown, a.TimeOverflow)
		a.TimeOverflow = grown
	}
	for i, c := range o.TimeOverflow {
		a.TimeOverflow[i] += c
	}

	if len(o.SlotCnt) > len(a.SlotCnt) {
		grown := make([]int64, len(o.SlotCnt))
		copy(grown, a.SlotCnt)
		a.SlotCnt = grown
	}
	for i, c := range o.SlotCnt {
		a.SlotCnt[i] += c
	}

	if len(o.MinuteCnt) > len(a.MinuteCnt) {
		grown := make([]int64, len(o.MinuteCnt))
		copy(grown, a.MinuteCnt)
		a.MinuteCnt = grown
	}
	for i, c := range o.MinuteCnt {
		a.MinuteCnt[i] += c
	}

	if len(o.MinuteBytes) > len(a.MinuteBytes) {
		grown := make([]int64, len(o.MinuteBytes))
		copy(grown, a.MinuteBytes)
		a.MinuteBytes = grown
	}
	for i, c := range o.MinuteBytes {
		addSaturating(&a.MinuteBytes[i], c)
	}

	mergeI64 := func(dst *map[string]int64, src map[string]int64) {
		if len(src) == 0 {
			return
		}
		if *dst == nil {
			*dst = make(map[string]int64, mapCapHint(len(src), maxMapKeys))
		}
		for k, v := range src {
			if _, ok := (*dst)[k]; !ok && len(*dst) >= maxMapKeys {
				(*dst)[OverflowKey] += v
				continue
			}
			(*dst)[k] += v
		}
	}

	if o.peakCnt > a.peakCnt {
		a.peakCnt, a.peakSec = o.peakCnt, o.peakSec
	}
	// 时区必须先继承：从日记录还原的 Agg 只有峰值秒（没有逐秒表），
	// 若把 loc 绑在 SecondCnt 上会丢时区，导致峰值时刻按服务器本地时区显示。
	if a.loc == nil && o.loc != nil {
		a.loc = o.loc
	}
	if len(o.SecondCnt) > 0 {
		if a.SecondCnt == nil {
			a.SecondCnt = make(map[int64]int32, mapCapHint(len(o.SecondCnt), maxSecondKeys))
		}
		for k, v := range o.SecondCnt {
			if len(a.SecondCnt) < maxSecondKeys {
				a.SecondCnt[k] += v
			} else if _, ok := a.SecondCnt[k]; ok {
				a.SecondCnt[k] += v
			}
		}
	}
	if len(o.HourCnt) > 0 {
		if a.HourCnt == nil {
			a.HourCnt = make(map[int]int64, len(o.HourCnt))
		}
		for k, v := range o.HourCnt {
			a.HourCnt[k] += v
		}
	}
	mergeUri(&a.UriStat, o.UriStat)
	mergeI64(&a.ClientCnt, o.ClientCnt)
	mergeI64(&a.StatusCnt, o.StatusCnt)
	mergeI64(&a.MethodCnt, o.MethodCnt)
	mergeUpstream(&a.UpstreamStat, o.UpstreamStat)
	mergeI64(&a.ErrorStatus, o.ErrorStatus)
	mergeI64(&a.DeviceCnt, o.DeviceCnt)
	mergeI64(&a.BrowserCnt, o.BrowserCnt)
	mergeI64(&a.RefererCnt, o.RefererCnt)
	mergeI64(&a.FileTypeCnt, o.FileTypeCnt)
	// 错误 URL 是二层表，逐状态合并；外层/内层都做基数上限保护。
	if len(o.ErrorURI) > 0 {
		if a.ErrorURI == nil {
			a.ErrorURI = make(map[string]map[string]int64, mapCapHint(len(o.ErrorURI), maxErrorStatusKeys))
		}
		for status, src := range o.ErrorURI {
			if _, ok := a.ErrorURI[status]; !ok && len(a.ErrorURI) >= maxErrorStatusKeys {
				status = OverflowKey
			}
			dst := a.ErrorURI[status]
			if dst == nil {
				dst = make(map[string]int64, mapCapHint(len(src), maxMapKeys))
				a.ErrorURI[status] = dst
			}
			for k, v := range src {
				if _, ok := dst[k]; !ok && len(dst) >= maxMapKeys {
					dst[OverflowKey] += v
					continue
				}
				dst[k] += v
			}
		}
	}
}

// SetPeak 直接设置峰值（从日记录还原时没有逐秒明细，只能拿到峰值本身）
func (a *Agg) SetPeak(sec int64, cnt int32) {
	if cnt > a.peakCnt {
		a.peakCnt, a.peakSec = cnt, sec
	}
}

// SetLoc 设置峰值时刻展示用的时区（从日记录恢复统计量时使用）。
// 日志时区与服务器时区可能不同，峰值时间要按日志时区展示才对得上。
func (a *Agg) SetLoc(loc *time.Location) {
	if loc != nil {
		a.loc = loc
	}
}

// mergeUri 合并接口统计表
func mergeUri(dst *map[string]*UriStat, src map[string]*UriStat) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]*UriStat, mapCapHint(len(src), maxMapKeys))
	}
	add := func(cur, v *UriStat) {
		cur.Calls += v.Calls
		cur.TimeS += v.TimeS
		cur.Errors += v.Errors
		cur.Slows += v.Slows
		cur.SlowTimeS += v.SlowTimeS
		addSaturating(&cur.Bytes, v.Bytes)
	}
	for k, v := range src {
		if cur, ok := (*dst)[k]; ok {
			add(cur, v)
			continue
		}
		if len(*dst) >= maxMapKeys {
			if cur, ok := (*dst)[OverflowKey]; ok {
				add(cur, v)
			} else {
				cp := *v
				(*dst)[OverflowKey] = &cp
			}
			continue
		}
		cp := *v
		(*dst)[k] = &cp
	}
}

// mergeUpstream 合并上游统计表
func mergeUpstream(dst *map[string]*UpstreamStat, src map[string]*UpstreamStat) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]*UpstreamStat, mapCapHint(len(src), maxMapKeys))
	}
	add := func(cur, v *UpstreamStat) {
		cur.Calls += v.Calls
		cur.TimeS += v.TimeS
		cur.TimeC += v.TimeC
		cur.Errors += v.Errors
	}
	for k, v := range src {
		if cur, ok := (*dst)[k]; ok {
			add(cur, v)
			continue
		}
		if len(*dst) >= maxMapKeys {
			if cur, ok := (*dst)[OverflowKey]; ok {
				add(cur, v)
			} else {
				cp := *v
				(*dst)[OverflowKey] = &cp
			}
			continue
		}
		cp := *v
		(*dst)[k] = &cp
	}
}

// Peak 取最高每秒请求数（同一秒跨文件时也能正确累加）
func (a *Agg) Peak() (time.Time, int64) {
	// 先取流式峰值，再用逐秒表里的精确值覆盖
	bestSec, best := a.peakSec, a.peakCnt
	// 计数相同时取更早的那一秒：map 遍历顺序随机，
	// 不做 tie-break 会让同一份数据每次跑出不同的"峰值时刻"
	for sec, c := range a.SecondCnt {
		if c > best || (c == best && best > 0 && sec < bestSec) {
			best, bestSec = c, sec
		}
	}
	if best <= 0 {
		return time.Time{}, 0
	}
	loc := a.loc
	if loc == nil {
		loc = time.Local
	}
	return time.Unix(bestSec, 0).In(loc), int64(best)
}

// Timed 返回带耗时的请求数；旧日记录没有这个字段时回退到 Total，
// 保证历史数据不会因为升级而变成 0。
func (a *Agg) Timed() int64 {
	if a.TimedCount > 0 {
		return a.TimedCount
	}
	// 旧日记录没有 TimedCount，但有耗时总和/最大值，回退到 Total。
	// 新版日志如果整份都没有耗时字段（例如 combined 默认格式），
	// TimeSum/MaxTime 都是 0，此时必须返回 0，否则 Percentile 会把
	// 空直方图误判成 10s，报告会出现“P99 10s”的假长尾。
	if a.TimeSum > 0 || a.MaxTime > 0 || (len(a.TimeBuckets) > 0 && a.TimeBuckets[0] > 0) {
		return a.Total
	}
	return 0
}

// AvgTime 平均响应时间（秒）。分母是「带耗时的请求数」而不是总请求数：
// 静态资源等请求 nginx 不记录耗时，把它们算作 0ms 会把平均响应压低几倍。
func (a *Agg) AvgTime() float64 {
	n := a.Timed()
	if n == 0 {
		return 0
	}
	return a.TimeSum / float64(n)
}

// Percentile 响应时间分位数（秒），p 取 0~1，如 0.99 表示 P99。
// 0~10s 用 10ms 桶插值；10s 以上用溢出桶插值，避免 P99 被截断到 10s。
func (a *Agg) Percentile(p float64) float64 {
	n := a.Timed()
	if n <= 0 {
		return 0
	}
	if p <= 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	rank := int64(p * float64(n))
	if rank >= n {
		rank = n - 1
	}

	var cum int64
	for i, c := range a.TimeBuckets {
		if c == 0 {
			continue
		}
		if cum+c > rank {
			frac := float64(rank-cum) / float64(c)
			return (float64(i)*TimeBucketMS + frac*TimeBucketMS) / 1000
		}
		cum += c
	}
	for i, c := range a.TimeOverflow {
		if c == 0 {
			continue
		}
		if cum+c > rank {
			from, to := overflowBounds(i)
			frac := float64(rank-cum) / float64(c)
			if math.IsInf(to, 1) {
				if c == 1 {
					return a.MaxTime
				}
				if a.MaxTime > from {
					return from + frac*(a.MaxTime-from)
				}
				return from
			}
			return from + frac*(to-from)
		}
		cum += c
	}
	if a.MaxTime > 0 {
		return a.MaxTime
	}
	return float64(TimeBucketCount-1) * TimeBucketMS / 1000
}

// SiteStats 单个站点的统计结果。
type SiteStats struct {
	Site string
	Name string
	Kind site.Kind
	Agg  Agg
	Days map[string]*Agg

	Skipped          int64
	OutOfRange       int64
	StaticSkipped    int64
	StaticBytes      int64
	BotSkipped       int64
	StaticSkippedDay map[string]int64
	StaticBytesDay   map[string]int64
	BotSkippedDay    map[string]int64
	SourceCnt        map[string]int64
	Conflicts        int64
	Invalid          int64
	MultiDomainLines int64
}

// ensureSite 取/创建站点统计；超过上限归入 (其他站点)。
func (s *FileStats) ensureSite(info site.Info, max int) *SiteStats {
	if s.Sites == nil {
		s.Sites = make(map[string]*SiteStats)
	}
	if info.Key == "" {
		info = site.Unknown()
	}
	if st := s.Sites[info.Key]; st != nil {
		return st
	}
	if max <= 0 {
		max = 1000
	}
	// 溢出桶本身不占 -site-max 的名额：否则合并时先遇到 o:other
	// 会挤掉真实站点，导致保留哪些站点取决于 map 遍历顺序。
	named := len(s.Sites)
	if _, ok := s.Sites[site.OtherKey]; ok {
		named--
	}
	if named >= max && info.Key != site.OtherKey {
		info = site.Other()
		if st := s.Sites[info.Key]; st != nil {
			return st
		}
	}
	st := &SiteStats{
		Site: info.Key, Name: info.Name, Kind: info.Kind,
		Days: make(map[string]*Agg), SourceCnt: make(map[string]int64),
		StaticSkippedDay: make(map[string]int64),
		StaticBytesDay:   make(map[string]int64),
		BotSkippedDay:    make(map[string]int64),
	}
	st.Agg = *NewAgg()
	s.Sites[info.Key] = st
	return st
}

// MergeSiteDay 合并外部日记录中的站点统计。
func (s *FileStats) MergeSiteDay(key, day string, agg *Agg, kind, name string,
	staticSkipped, staticBytes, botSkipped, outOfRange, skipped, conflicts, invalid, multi int64,
	sources map[string]int64) {
	if agg == nil {
		return
	}
	info := site.ParseKey(key)
	if kind != "" {
		info.Kind = site.Kind(kind)
	}
	if name != "" {
		info.Name = name
	}
	max := 1000
	if s.SiteLimit > 0 {
		max = s.SiteLimit
	}
	st := s.ensureSite(info, max)
	if day != "" {
		if st.Days == nil {
			st.Days = make(map[string]*Agg)
		}
		d := st.Days[day]
		if d == nil {
			d = NewAgg()
			st.Days[day] = d
		}
		d.MergeFrom(agg)
	}
	st.Agg.MergeFrom(agg)
	st.StaticSkipped += staticSkipped
	addDayInt(&st.StaticSkippedDay, day, staticSkipped)
	addSaturating(&st.StaticBytes, staticBytes)
	addDaySaturating(&st.StaticBytesDay, day, staticBytes)
	st.BotSkipped += botSkipped
	addDayInt(&st.BotSkippedDay, day, botSkipped)
	st.OutOfRange += outOfRange
	st.Skipped += skipped
	st.Conflicts += conflicts
	st.Invalid += invalid
	st.MultiDomainLines += multi
	for k, v := range sources {
		st.SourceCnt[k] += v
	}
}

// AsFileStats 把站点统计包装成报告输入；不复制 map，报告只读。
func (s *SiteStats) AsFileStats() *FileStats {
	fs := &FileStats{Agg: s.Agg, Days: s.Days}
	fs.StaticSkipped = s.StaticSkipped
	fs.StaticBytes = s.StaticBytes
	fs.BotSkipped = s.BotSkipped
	fs.StaticSkippedDay = s.StaticSkippedDay
	fs.StaticBytesDay = s.StaticBytesDay
	fs.BotSkippedDay = s.BotSkippedDay
	fs.OutOfRange = s.OutOfRange
	fs.Skipped = s.Skipped
	return fs
}

// FileStats 单个日志文件的统计结果
type FileStats struct {
	Agg
	Skipped    int64
	OutOfRange int64
	// StaticSkipped 按 -skip-static 跳过的静态资源请求数（css/js/图片/字体等）
	StaticSkipped int64
	// StaticBytes 被跳过的静态资源响应字节数（只用于报告里展示“排除了多少流量”）
	StaticBytes int64
	// BotSkipped 按 -exclude-bots 跳过的爬虫/探针请求数
	BotSkipped int64
	// 按天明细，供回退补写日记录时保持每天口径一致；旧记录没有这些 map。
	StaticSkippedDay map[string]int64
	StaticBytesDay   map[string]int64
	BotSkippedDay    map[string]int64
	// Format 实际识别的日志格式：json / combined / custom / csv / mixed
	Format string
	// FormatWarning 识别阶段的可读提示（不参与统计），例如 combined 末尾字段歧义
	FormatWarning string
	// FirstDay / LastDay 日志里实际出现的最早/最晚日期（YYYY-MM-DD），
	// 仅用于排查"为什么没有数据"，与统计区间无关。
	FirstDay string
	LastDay  string
	// Days 按日志时间归日，key 为 YYYY-MM-DD
	Days map[string]*Agg
	// Sites 站点维度统计；仅在 Options.SiteEnabled 时填充。
	Sites map[string]*SiteStats
	// SiteLimit 合并/回填站点统计时的数量上限；0 表示使用默认 1000。
	// 解析阶段也会按 Options.SiteMax 建站点，这里保证多文件合并与日记录
	// 回填不会绕过 -site-max。
	SiteLimit int
}

// DaySummary 每日汇总
type DaySummary struct {
	Day        string
	Total      int64
	PeakCnt    int64
	PeakTime   time.Time
	Size       int64
	AvgTime    float64
	ErrorCount int64
	SlowCount  int64
}

// DateInFilename 从文件名中提取 YYYY-MM-DD 日期（扫描形如片段）
func DateInFilename(name string) string {
	for i := 0; i+10 <= len(name); i++ {
		if isDigit(name[i]) && isDigit(name[i+1]) && isDigit(name[i+2]) && isDigit(name[i+3]) &&
			name[i+4] == '-' &&
			isDigit(name[i+5]) && isDigit(name[i+6]) && name[i+7] == '-' &&
			isDigit(name[i+8]) && isDigit(name[i+9]) {
			return name[i : i+10]
		}
	}
	return ""
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// gzReadCloser 同时关闭 gzip.Reader 与底层文件，避免文件句柄泄漏
type gzReadCloser struct {
	gz *gzip.Reader
	f  *os.File
}

func (g *gzReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzReadCloser) Close() error {
	err := g.gz.Close()
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// openLogFile 打开日志文件，自动识别 gz 压缩（后缀大小写不敏感）
func openLogFile(path string) (io.ReadCloser, error) {
	if strings.EqualFold(filepath.Ext(path), ".gz") {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &gzReadCloser{gz: gz, f: f}, nil
	}
	return os.Open(path)
}

// jsonNeedsAlias 判断 JSON 是否使用了默认字段名之外的命名。
// 默认 JSON（client/request_uri/status/...）保持原快速路径；
// 只有出现 nginx 变量名、url/request、自定义键时才启用别名分支。
// 调用方会在前若干行内持续检测，避免首行是 banner/健康检查时漏掉别名。
func jsonNeedsAlias(line []byte, custom map[string]int, customTimeKey string) bool {
	if len(custom) > 0 || customTimeKey != "" {
		return true
	}
	if !hasJSONKey(line, "@timestamp") && anyJSONKey(line, "time_iso8601", "time_local", "timestamp", "datetime", "date", "time") {
		return true
	}
	if !hasJSONKey(line, "client") && anyJSONKey(line, "remote_addr", "remote_ip", "ip", "addr") {
		return true
	}
	if !hasJSONKey(line, "request_uri") && anyJSONKey(line, "uri", "path", "request", "url") {
		return true
	}
	if !hasJSONKey(line, "request_method") && anyJSONKey(line, "method", "request", "url") {
		return true
	}
	if !hasJSONKey(line, "status") && anyJSONKey(line, "code", "status_code") {
		return true
	}
	if !hasJSONKey(line, "size") && anyJSONKey(line, "body_bytes_sent", "bytes_sent", "bytes") {
		return true
	}
	if !hasJSONKey(line, "responsetime") && anyJSONKey(line, "request_time", "response_time", "rt") {
		return true
	}
	if !hasJSONKey(line, "upstream_response_time") && hasJSONKey(line, "upstream_time") {
		return true
	}
	if !hasJSONKey(line, "agent") && !hasJSONKey(line, "user_agent") && hasJSONKey(line, "http_user_agent") {
		return true
	}
	if !hasJSONKey(line, "refer") && anyJSONKey(line, "referer", "http_referer") {
		return true
	}
	if anyJSONKey(line, "x_forwarded_for", "http_x_forwarded_for", "http_x_real_ip", "realip_remote_addr", "proxy_add_x_forwarded_for") {
		return true
	}
	return false
}

func hasJSONKey(line []byte, key string) bool {
	quoted := make([]byte, 0, len(key)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, key...)
	quoted = append(quoted, '"')
	off := 0
	for {
		i := bytes.Index(line[off:], quoted)
		if i < 0 {
			return false
		}
		i += off
		j := i + len(quoted)
		for j < len(line) && (line[j] == ' ' || line[j] == 0x09) {
			j++
		}
		if j < len(line) && line[j] == ':' {
			return true
		}
		off = i + 1
	}
}

func anyJSONKey(line []byte, keys ...string) bool {
	for _, k := range keys {
		if hasJSONKey(line, k) {
			return true
		}
	}
	return false
}

// parseJSONFieldMap 解析 -json-fields，例如 "ts=time,remote=client,u=uri"。
// 标准写法是「源字段=目标」；兼容 target=source，但两边都像目标名时以左边为源。
func parseJSONFieldMap(spec string) (map[string]int, string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, "", nil
	}
	m := make(map[string]int)
	timeKey := ""
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, "", fmt.Errorf("-json-fields 项 %q 格式应为 源字段=目标", pair)
		}
		a, b := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		_, _, aTarget := jsonMapTarget(a)
		_, _, bTarget := jsonMapTarget(b)
		src, dst := a, b
		// 标准写法是「源字段=目标」；只有右边不是目标、左边是目标时，
		// 才按兼容写法「目标=源字段」反转。两边都像目标名时仍以左边为源，
		// 避免 ip=client、code=status 这类自然映射被误判。
		if !bTarget && aTarget {
			src, dst = b, a
		}
		kind, isTime, ok := jsonMapTarget(dst)
		if !ok {
			return nil, "", fmt.Errorf("-json-fields 项 %q 的目标 %q 不支持", pair, dst)
		}
		if src == "" {
			return nil, "", fmt.Errorf("-json-fields 项 %q 缺少源字段名", pair)
		}
		if isTime {
			timeKey = src
			continue
		}
		m[src] = kind
	}
	if len(m) == 0 && timeKey == "" {
		return nil, "", nil
	}
	return m, timeKey, nil
}

func jsonMapTarget(s string) (int, bool, bool) {
	switch s {
	case "time", "timestamp":
		return 0, true, true
	case "client", "ip":
		return fClient, false, true
	case "real_client", "realip":
		return fRealClient, false, true
	case "method":
		return fMethod, false, true
	case "uri", "path":
		return fURI, false, true
	case "request", "url":
		return fRequest, false, true
	case "status", "code":
		return fStatus, false, true
	case "size", "bytes":
		return fSize, false, true
	case "upstream_addr":
		return fUpAddr, false, true
	case "upstream_status":
		return fUpStatus, false, true
	case "upstream_time":
		return fUpTime, false, true
	case "response_time", "responsetime":
		return fRespTime, false, true
	case "agent", "user_agent":
		return fAgent, false, true
	case "referer", "refer":
		return fRefer, false, true
	case "site", "domain":
		return fSite, false, true
	default:
		return 0, false, false
	}
}

// resolveLineSite 解析行内站点；字段无效时回退文件默认站点，保证不丢日志。
func resolveLineSite(fl *lineFields, opt *Options, def site.Info, defSource string) (site.Info, string, bool, bool, bool) {
	from := opt.SiteFrom
	if from == "" {
		from = "auto"
	}
	if opt.SiteForce || from == "map" || from == "filename" || from == "dir" {
		return def, defSource, false, false, false
	}
	v, ok := fl.get(fSite)
	if !ok || len(v) == 0 {
		return def, defSource, false, false, false
	}
	info, ok, n := site.NormalizeWithCount(string(v), opt.SiteStripWWW)
	if !ok {
		return def, defSource, false, true, false
	}
	if info.Kind == site.KindDomain {
		info = site.ApplyAlias(info, opt.SiteAliases)
	}
	if info.Kind == site.KindIP {
		switch opt.SiteIPMode {
		case "inherit":
			if def.Kind == site.KindDomain {
				return def, defSource, false, false, n > 1
			}
		case "ignore":
			return def, defSource, false, false, n > 1
		}
	}
	conflict := def.Kind == site.KindDomain && info.Kind == site.KindDomain && def.Name != info.Name
	return info, "field", conflict, false, n > 1
}

// NewStats 初始化空的统计结构
func NewStats() *FileStats {
	return &FileStats{
		Agg:  *NewAgg(),
		Days: make(map[string]*Agg),
	}
}

// ProcessFile 处理单个日志文件（不按日期过滤、不限速）
func ProcessFile(fpath string) (*FileStats, error) {
	return ProcessFileWithOptions(fpath, Options{})
}

// ProcessFileRange 处理单个日志文件，仅统计 [start, end] 内的记录
func ProcessFileRange(fpath, start, end string) (*FileStats, error) {
	return ProcessFileWithOptions(fpath, Options{Start: start, End: end})
}

// ProcessFileWithOptions 按选项处理单个日志文件
func ProcessFileWithOptions(fpath string, opt Options) (*FileStats, error) {
	rc, err := openLogFile(fpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return ProcessReader(rc, opt)
}

// ProcessReader 从任意 reader 解析日志。
// 把「打开文件」与「解析」分开，便于测试与基准在内存里跑，
// 也让 IO 策略（gz/普通文件）和统计逻辑各自独立演进。
func ProcessReader(rc io.Reader, opt Options) (*FileStats, error) {
	stats := NewStats()
	if opt.SiteEnabled {
		stats.SiteLimit = opt.SiteMax
	}

	// 日期边界预先转成整数，避免逐行字符串比较
	var startInt, endInt int64
	if opt.Start != "" {
		if v, ok := dateInt(opt.Start); ok {
			startInt = v
		}
	}
	if opt.End != "" {
		if v, ok := dateInt(opt.End); ok {
			endInt = v
		}
	}

	chunk := opt.ChunkLines
	if chunk <= 0 {
		chunk = DefaultChunkLines
	}
	th := newThrottle(opt.CPUPercent)
	th.mark()

	earlyLimit := opt.EarlyExitLines
	switch {
	case earlyLimit == 0:
		earlyLimit = earlyExitLines
	case earlyLimit < 0:
		earlyLimit = 0
	}

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)

	var in interner
	var (
		lines       int
		futureRun   int
		lastDateInt int64
		lastDayKey  string
		firstDate   int64
		lastDate    int64
	)

	customFields, customTimeKey, ferr := parseJSONFieldMap(opt.JSONFields)
	if ferr != nil {
		return nil, ferr
	}
	tsKeys := timestampKeys
	if customTimeKey != "" {
		tsKeys = append([][]byte{[]byte(customTimeKey)}, timestampKeys...)
	}

	var parser *accesslog.Format
	var formatDetected bool
	var isJSON bool
	var aliasJSON bool
	var aliasChecked int
	var detectTries int
	var csvBuf []byte
	var csvInQuotes bool
	var bomChecked bool
	var textFields accesslog.Fields
	var fl lineFields
	fl.custom = customFields

	siteDefault := site.ParseKey(opt.SiteDefault)
	if opt.SiteDefault == "" {
		siteDefault = site.Unknown()
	}
	siteSourceDefault := opt.SiteDefaultSource
	if siteSourceDefault == "" {
		siteSourceDefault = siteDefault.Source
	}
	siteKeys := siteFieldKeys
	if opt.SiteField != "" {
		siteKeys = [][]byte{[]byte(opt.SiteField)}
	}
	parseSiteLine := opt.SiteEnabled && !opt.SiteForce && opt.SiteFrom != "map" && opt.SiteFrom != "filename" && opt.SiteFrom != "dir"
	lineSite := siteDefault
	siteSource := siteSourceDefault

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bomChecked {
			line = bytes.TrimPrefix(line, []byte{0xEF, 0xBB, 0xBF})
			bomChecked = true
		}

		// 启动/首行：先识别格式，再处理；同一文件后续行复用 parser。
		if !formatDetected {
			if len(bytes.TrimSpace(line)) == 0 {
				stats.Skipped++
				continue
			}
			pf, perr := accesslog.DetectWithOptions(line, accesslog.DetectOptions{
				Configured:    opt.LogFormat,
				CSVFields:     opt.CSVFields,
				CSVSkipHeader: opt.CSVSkipHeader,
				SiteField:     opt.SiteField,
			})
			if perr != nil {
				detectTries++
				// 允许文件头部有少量 banner/空行；连续 20 行都认不出再报错
				if detectTries >= 20 {
					return stats, fmt.Errorf("识别日志格式失败: %w", perr)
				}
				stats.Skipped++
				continue
			}
			parser = pf
			stats.Format = string(pf.Kind)
			stats.FormatWarning = pf.Warning
			isJSON = pf.Kind == accesslog.KindJSON
			formatDetected = true
			if pf.SkipHeader {
				stats.Skipped++
				continue
			}
		}

		// CSV 引号字段可能跨行：累积到引号闭合后再解析。
		if formatDetected && parser.Kind == accesslog.KindCSV {
			if len(csvBuf) > 0 {
				csvBuf = append(csvBuf, 0x0a)
				csvBuf = append(csvBuf, line...)
				if len(csvBuf) > 8*1024*1024 {
					stats.Skipped++
					csvBuf = csvBuf[:0]
					csvInQuotes = false
					continue
				}
				complete, next := parser.RecordCompleteFrom(line, csvInQuotes)
				if !complete {
					csvInQuotes = next
					continue
				}
				line = csvBuf
				csvBuf = csvBuf[:0]
				csvInQuotes = false
			} else {
				complete, next := parser.RecordCompleteFrom(line, false)
				if !complete {
					csvBuf = append(csvBuf[:0], line...)
					csvInQuotes = next
					continue
				}
			}
		}

		// 首行可能是健康检查/banner；在前 20 个有效 JSON 行内持续检测别名，
		// 找到后后续行走别名分支，避免整份日志的扩展字段被静默忽略。
		if isJSON && !aliasJSON && aliasChecked < 20 {
			if jsonNeedsAlias(line, customFields, customTimeKey) {
				aliasJSON = true
			}
			aliasChecked++
		}

		var ts time.Time
		var diAll int64

		if isJSON {
			tsStr, ok := fastTimestamp(line)
			if !ok || len(tsStr) == 0 {
				tsStr, ok = fieldBytesAny(line, tsKeys)
			}
			if !ok {
				stats.Skipped++
				continue
			}
			ts, ok = parseTimestamp(tsStr)
			if !ok {
				ts, ok = parseTimestampAny(tsStr)
			}
			if !ok {
				stats.Skipped++
				continue
			}

			diAll = int64(ts.Year())*10000 + int64(ts.Month())*100 + int64(ts.Day())
			if firstDate == 0 || diAll < firstDate {
				firstDate = diAll
			}
			if diAll > lastDate {
				lastDate = diAll
			}

			// 按日期范围过滤（日期整数比较，无需再格式化字符串）
			if startInt != 0 || endInt != 0 {
				di := diAll
				if (startInt != 0 && di < startInt) || (endInt != 0 && di > endInt) {
					stats.OutOfRange++
					// 日志一般按时间递增：连续越过结束日期即可提前收工
					if endInt != 0 && di > endInt {
						futureRun++
						if earlyLimit > 0 && futureRun > earlyLimit {
							break
						}
					} else {
						futureRun = 0
					}
					if lines++; lines%chunk == 0 {
						th.tick()
					}
					continue
				}
				futureRun = 0
			}

			// 按天过滤「HH:MM 之后」（可跨天各自生效，语义与原脚本一致）
			if opt.FromMinute >= 0 {
				if ts.Hour()*60+ts.Minute() < opt.FromMinute {
					stats.OutOfRange++
					if lines++; lines%chunk == 0 {
						th.tick()
					}
					continue
				}
			}

			scanFields(line, &fl)
			if aliasJSON {
				scanFieldsAlias(line, &fl)
			}
		} else {
			// 文本格式（combined / 自定义 log_format / CSV）：先解析出字段，
			// 再走与 JSON 完全相同的日期过滤、静态资源、业务统计逻辑。
			if !parser.Parse(line, &textFields) {
				stats.Skipped++
				continue
			}
			t, ok := parseTimestampAny(textFields.Timestamp)
			if !ok {
				stats.Skipped++
				continue
			}
			ts = t

			diAll = int64(ts.Year())*10000 + int64(ts.Month())*100 + int64(ts.Day())
			if firstDate == 0 || diAll < firstDate {
				firstDate = diAll
			}
			if diAll > lastDate {
				lastDate = diAll
			}

			// 按日期范围过滤（与 JSON 分支保持同一套语义）
			if startInt != 0 || endInt != 0 {
				di := diAll
				if (startInt != 0 && di < startInt) || (endInt != 0 && di > endInt) {
					stats.OutOfRange++
					if endInt != 0 && di > endInt {
						futureRun++
						if earlyLimit > 0 && futureRun > earlyLimit {
							break
						}
					} else {
						futureRun = 0
					}
					if lines++; lines%chunk == 0 {
						th.tick()
					}
					continue
				}
				futureRun = 0
			}

			if opt.FromMinute >= 0 {
				if ts.Hour()*60+ts.Minute() < opt.FromMinute {
					stats.OutOfRange++
					if lines++; lines%chunk == 0 {
						th.tick()
					}
					continue
				}
			}

			populateLineFields(&fl, &textFields)
		}

		// JSON 的 "url"/"request"、文本格式的完整请求行，统一在这里拆 method/URI。
		// 只有显式 URI 或 method 缺失时才需要拆，避免常规 JSON 每行白扫一次请求行。
		if aliasJSON && fl.has(fRequest) && (!fl.has(fURI) || !fl.has(fMethod)) {
			resolveRequestFields(&fl)
		}

		// 行内站点字段只在启用站点功能时读取，避免默认 JSON 路径多一次扫描。
		if parseSiteLine && isJSON {
			if v, ok := fieldBytesAny(line, siteKeys); ok {
				fl.put(fSite, v)
			}
		}

		// 复用上一条的日期字符串，日志按天聚集时几乎不产生分配
		if diAll != lastDateInt {
			lastDateInt = diAll
			lastDayKey = formatDateInt(diAll)
		}

		// 站点解析：字段优先，失败回退文件默认站点；不因站点异常丢行。
		lineSite, siteSource = siteDefault, siteSourceDefault
		var siteConflict, siteInvalid, siteMulti bool
		if opt.SiteEnabled {
			lineSite, siteSource, siteConflict, siteInvalid, siteMulti = resolveLineSite(&fl, &opt, siteDefault, siteSourceDefault)
		}

		// 爬虫/探针整行丢弃（默认关闭，显式 -exclude-bots 才生效）
		var ua []byte
		if v, ok := fl.get(fAgent); ok {
			ua = v
		}
		var lineSiteStats *SiteStats
		if opt.SiteEnabled {
			lineSiteStats = stats.ensureSite(lineSite, opt.SiteMax)
			lineSiteStats.SourceCnt[siteSource]++
			if siteConflict {
				lineSiteStats.Conflicts++
			}
			if siteInvalid {
				lineSiteStats.Invalid++
			}
			if siteMulti {
				lineSiteStats.MultiDomainLines++
			}
		}
		if opt.ExcludeBots && looksLikeBot(ua) {
			stats.BotSkipped++
			addDayInt(&stats.BotSkippedDay, lastDayKey, 1)
			if lineSiteStats != nil {
				lineSiteStats.BotSkipped++
				addDayInt(&lineSiteStats.BotSkippedDay, lastDayKey, 1)
			}
			if lines++; lines%chunk == 0 {
				th.tick()
			}
			continue
		}

		// 静态资源整行丢弃：不进入任何业务统计（PV/分钟/状态码/IP/流量/错误），
		// 只累计条数，供运维报告说明“原始 N 条、排除了多少”。
		if opt.SkipStatic {
			if v, ok := fl.get(fURI); ok {
				if len(v) > maxURIKey {
					v = capBytes(v, maxURIKey)
				}
				if staticasset.IsBytes(v) {
					stats.StaticSkipped++
					addDayInt(&stats.StaticSkippedDay, lastDayKey, 1)
					if lineSiteStats != nil {
						lineSiteStats.StaticSkipped++
						addDayInt(&lineSiteStats.StaticSkippedDay, lastDayKey, 1)
					}
					if sv, ok := fl.get(fSize); ok {
						if n, ok2 := parseIntBytes(sv); ok2 {
							addSaturating(&stats.StaticBytes, n)
							addDaySaturating(&stats.StaticBytesDay, lastDayKey, n)
							if lineSiteStats != nil {
								addSaturating(&lineSiteStats.StaticBytes, n)
								addDaySaturating(&lineSiteStats.StaticBytesDay, lastDayKey, n)
							}
						}
					}
					if lines++; lines%chunk == 0 {
						th.tick()
					}
					continue
				}
			}
		}

		var r record
		r.ts = ts
		r.day = lastDayKey
		r.hour = ts.Hour()
		if v, ok := fl.get(fURI); ok {
			if len(v) > maxURIKey {
				v = capBytes(v, maxURIKey)
			}
			if ext := detectFileType(v); ext != "" {
				r.fileType = ext
			}
			if opt.URINormalize {
				// 只保留路径：/a/b?x=1&y=2 → /a/b
				if i := bytes.IndexByte(v, '?'); i >= 0 {
					v = v[:i]
				}
			}
			r.uri = string(capBytes(v, maxURIKey))
		}
		if v, ok := fl.get(fStatus); ok {
			if len(v) > maxShortKey {
				v = capBytes(v, maxShortKey)
			}
			r.status = in.get(v)
		}
		if v, ok := fl.get(fSize); ok {
			r.size, _ = parseIntBytes(v)
		}
		var hasTime bool
		switch opt.TimeField {
		case "response":
			r.upstreamTime, hasTime = firstFloatOf(&fl, fRespTime)
		case "upstream":
			r.upstreamTime, hasTime = firstFloatOf(&fl, fUpTime)
		default:
			r.upstreamTime, hasTime = firstFloatOf(&fl, fUpTime, fRespTime)
		}
		// 真实客户端 IP：real_client 优先（CDN/代理场景），为空才用 client
		clientSet := false
		if opt.RealIP {
			if v, ok := fl.get(fRealClient); ok {
				if el := firstListElem(v); len(el) > 0 {
					r.client = string(capBytes(el, maxClientKey))
					clientSet = true
				}
			}
		}
		if !clientSet {
			if v, ok := fl.get(fClient); ok {
				r.client = string(capBytes(v, maxClientKey))
			}
		}
		if v, ok := fl.get(fMethod); ok {
			r.method = in.get(capBytes(v, maxShortKey))
		}
		if len(ua) > 0 {
			r.device = classifyDevice(ua)
			r.browser = classifyBrowser(ua)
		} else {
			r.device, r.browser = "未知", "未知"
		}
		if v, ok := fl.get(fRefer); ok {
			r.referHost, r.isDirect = parseReferer(v)
			if len(r.referHost) > maxRefererKey {
				r.referHost = r.referHost[:maxRefererKey] + "..."
			}
		} else {
			r.isDirect = true
		}
		if v, ok := fl.get(fUpAddr); ok {
			// nginx 重试时是列表："10.0.0.1:80, 10.0.0.2:80"，
			// 只取第一跳地址，否则每行都会生成一个"组合 key"，上游表直接报废
			if el := firstListElem(v); len(el) > 0 {
				r.upstreamAddr = in.get(capBytes(el, maxUpstreamKey))
			}
		}
		if r.status == "" {
			r.status = "(未知)"
		}
		if r.method == "" {
			r.method = "(未知)"
		}
		r.isError = len(r.status) > 0 && r.status[0] >= '4'
		// 没有耗时字段（nginx 对静态资源/本地响应写 "-"）的行不进直方图
		r.hasTime = hasTime
		slowSec := 1.0
		if opt.SlowMS > 0 {
			slowSec = float64(opt.SlowMS) / 1000
		}
		r.isSlow = hasTime && r.upstreamTime > slowSec
		if r.upstreamAddr != "" && r.upstreamAddr != "-" {
			if v, ok := fl.get(fUpStatus); ok {
				// 状态列表同样取最后一个（最终结果），"502, 200" 视为成功重试
				r.isUpstreamErr = isErrStatus(lastListElem(v))
			}
		}

		// 按天聚合：一个日志文件可能跨天，不能只按文件名归属。
		// 只往「当天」这一张表里累加，扫描结束后再汇总到文件级 Agg——
		// 高基数日志（URI/IP 几十万条）时，避免同一行做两份 map 操作。
		if opt.SiteEnabled {
			da := lineSiteStats.Days[r.day]
			if da == nil {
				da = NewAgg()
				lineSiteStats.Days[r.day] = da
			}
			da.add(&r)
		} else {
			da := stats.Days[r.day]
			if da == nil {
				da = NewAgg()
				stats.Days[r.day] = da
			}
			da.add(&r)
		}

		if lines++; lines%chunk == 0 {
			th.tick()
		}
	}
	if len(csvBuf) > 0 {
		// 文件末尾仍未闭合的 CSV 记录按一条坏行计，避免静默吞掉
		stats.Skipped++
	}
	th.tick()

	// 站点模式：先把站点天级数据汇总到站点 Agg 和文件级 Days，
	// 再走原有文件级 Agg 合并，保证站点/全站只读同一份数据。
	if opt.SiteEnabled {
		for _, ss := range stats.Sites {
			for day, da := range ss.Days {
				ss.Agg.MergeFrom(da)
				gd := stats.Days[day]
				if gd == nil {
					gd = NewAgg()
					stats.Days[day] = gd
				}
				gd.MergeFrom(da)
			}
		}
	}

	// 汇总到文件级统计量（按天合并，而不是逐行双写）。
	// 注意：这里不做「单天直接复用」的别名优化 —— 别名会让 Agg 与 Days[day]
	// 共享底层 map，之后任何 MergeDay(day, &stats.Agg) 都会自我合并导致数据翻倍。
	// 合并一次 2 万条 map 只要几毫秒，不值得为此埋雷。
	for _, da := range stats.Days {
		stats.Agg.MergeFrom(da)
	}

	if firstDate != 0 {
		stats.FirstDay = formatDateInt(firstDate)
		stats.LastDay = formatDateInt(lastDate)
	}

	// 即使读取中途出错（如超长行）也保留已解析的数据
	return stats, scanner.Err()
}

// formatDateInt 把 yyyymmdd 转回 YYYY-MM-DD
func formatDateInt(v int64) string {
	return fmt.Sprintf("%04d-%02d-%02d", v/10000, v/100%100, v%100)
}

// sortedSiteStatsKeys 按 PV 降序返回站点 key，保证 -site-max 截断时
// 保留哪些站点不依赖 Go map 的随机遍历顺序。
func sortedSiteStatsKeys(m map[string]*SiteStats) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := m[keys[i]], m[keys[j]]
		at, bt := int64(0), int64(0)
		if a != nil {
			at = a.Agg.Total
		}
		if b != nil {
			bt = b.Agg.Total
		}
		if at != bt {
			return at > bt
		}
		return keys[i] < keys[j]
	})
	return keys
}

// Merge 将 o 合并到 s
func (s *FileStats) Merge(o *FileStats) {
	if o == nil {
		return
	}
	if s.SiteLimit == 0 && o.SiteLimit > 0 {
		s.SiteLimit = o.SiteLimit
	}
	s.Agg.MergeFrom(&o.Agg)
	s.Skipped += o.Skipped
	s.OutOfRange += o.OutOfRange
	s.StaticSkipped += o.StaticSkipped
	addSaturating(&s.StaticBytes, o.StaticBytes)
	s.BotSkipped += o.BotSkipped
	mergeDayI64(&s.StaticSkippedDay, o.StaticSkippedDay)
	mergeDayI64(&s.StaticBytesDay, o.StaticBytesDay)
	mergeDayI64(&s.BotSkippedDay, o.BotSkippedDay)
	if o.Format != "" {
		if s.Format == "" {
			s.Format = o.Format
		} else if s.Format != o.Format {
			s.Format = "mixed"
		}
	}
	if s.FormatWarning == "" && o.FormatWarning != "" {
		s.FormatWarning = o.FormatWarning
	}
	if o.FirstDay != "" && (s.FirstDay == "" || o.FirstDay < s.FirstDay) {
		s.FirstDay = o.FirstDay
	}
	if o.LastDay != "" && o.LastDay > s.LastDay {
		s.LastDay = o.LastDay
	}

	if s.Days == nil {
		s.Days = make(map[string]*Agg)
	}
	for day, agg := range o.Days {
		d := s.Days[day]
		if d == nil {
			d = NewAgg()
			s.Days[day] = d
		}
		d.MergeFrom(agg)
	}
	if len(o.Sites) > 0 {
		max := 1000
		if s.SiteLimit > 0 {
			max = s.SiteLimit
		}
		for _, key := range sortedSiteStatsKeys(o.Sites) {
			src := o.Sites[key]
			if src == nil {
				continue
			}
			info := site.Info{Key: src.Site, Name: src.Name, Kind: src.Kind}
			if info.Key == "" {
				info = site.ParseKey(key)
			}
			dst := s.ensureSite(info, max)
			dst.Agg.MergeFrom(&src.Agg)
			for day, agg := range src.Days {
				d := dst.Days[day]
				if d == nil {
					d = NewAgg()
					dst.Days[day] = d
				}
				d.MergeFrom(agg)
			}
			dst.StaticSkipped += src.StaticSkipped
			addSaturating(&dst.StaticBytes, src.StaticBytes)
			dst.BotSkipped += src.BotSkipped
			mergeDayI64(&dst.StaticSkippedDay, src.StaticSkippedDay)
			mergeDayI64(&dst.StaticBytesDay, src.StaticBytesDay)
			mergeDayI64(&dst.BotSkippedDay, src.BotSkippedDay)
			dst.OutOfRange += src.OutOfRange
			dst.Skipped += src.Skipped
			dst.Conflicts += src.Conflicts
			dst.Invalid += src.Invalid
			dst.MultiDomainLines += src.MultiDomainLines
			for k, v := range src.SourceCnt {
				dst.SourceCnt[k] += v
			}
		}
	}
}

// MergeDay 将某天的统计量并入汇总（用于由日记录重建周报）
func (s *FileStats) MergeDay(day string, agg *Agg) {
	if agg == nil {
		return
	}
	// 防呆：传入的正是本对象自己的「文件级」Agg 时，Days 里已经包含它了，
	// 再合并一次会让该天数据翻倍（单天文件尤其容易踩到），直接返回。
	if agg == &s.Agg {
		if s.Days == nil {
			s.Days = make(map[string]*Agg)
		}
		if s.Days[day] == nil {
			d := NewAgg()
			d.MergeFrom(agg)
			s.Days[day] = d
		}
		return
	}
	s.Agg.MergeFrom(agg)
	if s.Days == nil {
		s.Days = make(map[string]*Agg)
	}
	d := s.Days[day]
	if d == nil {
		d = NewAgg()
		s.Days[day] = d
	}
	if d != agg {
		d.MergeFrom(agg)
	}
}

// DayAgg 返回某天的统计量（不存在返回 nil）
func (s *FileStats) DayAgg(day string) *Agg {
	return s.Days[day]
}

// DaySummaries 按日期升序返回每日汇总
func (s *FileStats) DaySummaries() []DaySummary {
	days := make([]DaySummary, 0, len(s.Days))
	for day, da := range s.Days {
		if da.Total == 0 {
			continue
		}
		peakTime, peakCnt := da.Peak()
		days = append(days, DaySummary{
			Day:        day,
			Total:      da.Total,
			PeakCnt:    peakCnt,
			PeakTime:   peakTime,
			Size:       da.SizeTotal,
			AvgTime:    da.AvgTime(),
			ErrorCount: da.ErrorCount,
			SlowCount:  da.SlowCount,
		})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })
	return days
}

// Peak 取整个统计集的最高每秒请求数
func (s *FileStats) Peak() (time.Time, int64) { return s.Agg.Peak() }

// ---------------------------------------------------------------- 字段解析
//
// 全部基于 []byte 工作：逐行不复制整行（scanner.Bytes），只把需要作为 map key
// 保留的小字段复制成 string，避免「map key 引用整行」导致的内存滞留。

// keyTimestamp 单独保留：先用它做时间判断，再决定是否值得完整解析这一行
// （落在统计区间外的行只付一次字段查找的代价）。其余字段由 scanFields 一次取全。
var keyTimestamp = []byte("@timestamp")

// siteFieldKeys 行内站点字段别名（按优先级）。
var siteFieldKeys = [][]byte{
	[]byte("domain"),
	[]byte("host"),
	[]byte("http_host"),
	[]byte("server_name"),
	[]byte("vhost"),
	[]byte("site"),
	[]byte("website"),
	[]byte("site_name"),
	[]byte("server"),
	[]byte("request_host"),
}

// timestampKeys JSON 日志常见的时间字段名（按优先级）。
// 默认 @timestamp 走 fastTimestamp 快速路径；其它名字在首行格式识别后兜底。
var timestampKeys = [][]byte{
	[]byte("@timestamp"),
	[]byte("time_iso8601"),
	[]byte("time_local"),
	[]byte("timestamp"),
	[]byte("datetime"),
	[]byte("date"),
	[]byte("time"),
}

// 行内字段下标（一次性扫描后按位判断是否取到）
const (
	fURI = iota
	fStatus
	fSize
	fClient
	fMethod
	fUpAddr
	fUpStatus
	fUpTime
	fRespTime
	fAgent
	fRefer
	fRealClient
	fRequest
	fSite
	fieldKindCount
)

// ValidateJSONFields 在启动阶段校验 -json-fields，避免等到第一个文件才报错。
func ValidateJSONFields(spec string) error {
	_, _, err := parseJSONFieldMap(spec)
	return err
}

// lineFields 一行日志中关心的字段值（都指向原行，不复制）
type lineFields struct {
	vals   [fieldKindCount][]byte
	mask   uint32
	custom map[string]int // -json-fields 自定义字段映射（仅 JSON 路径使用）
}

func (f *lineFields) reset() { f.mask = 0 }

func (f *lineFields) set(kind int) {
	f.mask |= 1 << uint(kind)
}

func (f *lineFields) has(kind int) bool { return f.mask&(1<<uint(kind)) != 0 }

func (f *lineFields) get(kind int) ([]byte, bool) {
	if !f.has(kind) {
		return nil, false
	}
	return f.vals[kind], true
}

func (f *lineFields) put(kind int, val []byte) {
	f.vals[kind] = val
	f.set(kind)
}

// populateLineFields 把通用文本解析结果填进 JSON 快速路径使用的 lineFields，
// 让后面的静态资源、爬虫、业务统计逻辑只维护一份。
func populateLineFields(fl *lineFields, f *accesslog.Fields) {
	fl.reset()
	put := func(kind int, v []byte) {
		if len(v) > 0 {
			fl.put(kind, v)
		}
	}
	put(fURI, f.URI)
	put(fStatus, f.Status)
	put(fSize, f.Size)
	put(fClient, f.Client)
	put(fMethod, f.Method)
	put(fUpAddr, f.UpstreamAddr)
	put(fUpStatus, f.UpstreamStatus)
	put(fUpTime, f.UpstreamTime)
	put(fRespTime, f.ResponseTime)
	put(fAgent, f.Agent)
	put(fRefer, f.Referer)
	put(fRealClient, f.RealClient)
	put(fRequest, f.Request)
	put(fSite, f.Site)
}

// resolveRequestFields 把完整请求行拆成 method / URI；显式字段优先。
func resolveRequestFields(fl *lineFields) {
	_, hasURI := fl.get(fURI)
	_, hasMethod := fl.get(fMethod)
	if hasURI && hasMethod {
		return
	}
	v, ok := fl.get(fRequest)
	if !ok || len(v) == 0 {
		return
	}
	method, uri := splitRequestBytes(v)
	if _, ok := fl.get(fMethod); !ok && len(method) > 0 {
		fl.put(fMethod, method)
	}
	if _, ok := fl.get(fURI); !ok && len(uri) > 0 {
		fl.put(fURI, uri)
	}
}

func splitRequestBytes(req []byte) (method, uri []byte) {
	req = bytes.TrimSpace(req)
	if len(req) == 0 {
		return nil, nil
	}
	i := bytes.IndexByte(req, ' ')
	if i < 0 {
		return nil, req
	}
	method = req[:i]
	rest := bytes.TrimLeft(req[i:], " ")
	j := bytes.IndexByte(rest, ' ')
	if j < 0 {
		return method, rest
	}
	return method, rest[:j]
}

// setByKey 默认 JSON 快速路径：只认项目默认字段名。
// 自定义字段名/nginx 变量名由 scanFieldsAlias 二次扫描处理。
func (f *lineFields) setByKey(key, val []byte) {
	switch len(key) {
	case 4:
		if key[0] == 's' && string(key) == "size" {
			f.put(fSize, val)
		}
	case 5:
		if key[0] == 'a' && string(key) == "agent" {
			f.put(fAgent, val)
		} else if key[0] == 'r' && string(key) == "refer" {
			f.put(fRefer, val)
		}
	case 6:
		if key[0] == 's' && string(key) == "status" {
			f.put(fStatus, val)
		} else if key[0] == 'c' && string(key) == "client" {
			f.put(fClient, val)
		}
	case 10:
		if string(key) == "user_agent" {
			f.put(fAgent, val)
		}
	case 11:
		if string(key) == "request_uri" {
			f.put(fURI, val)
		} else if string(key) == "real_client" {
			f.put(fRealClient, val)
		}
	case 12:
		if string(key) == "responsetime" {
			f.put(fRespTime, val)
		}
	case 13:
		if string(key) == "upstream_addr" {
			f.put(fUpAddr, val)
		}
	case 14:
		if string(key) == "request_method" {
			f.put(fMethod, val)
		}
	case 15:
		if string(key) == "upstream_status" {
			f.put(fUpStatus, val)
		}
	case 22:
		if string(key) == "upstream_response_time" {
			f.put(fUpTime, val)
		}
	}
}

func (f *lineFields) setByKeyAlias(key, val []byte) {
	if f.custom != nil {
		if kind, ok := f.custom[string(key)]; ok {
			f.put(kind, val)
			return
		}
	}
	switch len(key) {
	case 2:
		switch key[0] {
		case 'i':
			if string(key) == "ip" {
				f.put(fClient, val)
			}
		case 'r':
			if string(key) == "rt" {
				f.put(fRespTime, val)
			}
		}
	case 3:
		if key[0] == 'u' {
			if string(key) == "uri" {
				f.put(fURI, val)
			} else if string(key) == "url" {
				f.put(fRequest, val)
			}
		}
	case 4:
		switch key[0] {
		case 's':
			if string(key) == "size" {
				f.put(fSize, val)
			}
		case 'c':
			if string(key) == "code" {
				f.put(fStatus, val)
			}
		case 'p':
			if string(key) == "path" {
				f.put(fURI, val)
			}
		case 'a':
			if string(key) == "addr" {
				f.put(fClient, val)
			}
		}
	case 5:
		switch key[0] {
		case 'a':
			if string(key) == "agent" {
				f.put(fAgent, val)
			}
		case 'r':
			if string(key) == "refer" {
				f.put(fRefer, val)
			}
		case 'b':
			if string(key) == "bytes" {
				f.put(fSize, val)
			}
		}
	case 6:
		switch key[0] {
		case 's':
			if string(key) == "status" {
				f.put(fStatus, val)
			}
		case 'c':
			if string(key) == "client" {
				f.put(fClient, val)
			}
		case 'm':
			if string(key) == "method" {
				f.put(fMethod, val)
			}
		}
	case 7:
		if key[0] == 'r' {
			if string(key) == "request" {
				f.put(fRequest, val)
			} else if string(key) == "referer" {
				f.put(fRefer, val)
			}
		}
	case 9:
		switch key[0] {
		case 'r':
			if string(key) == "remote_ip" {
				f.put(fClient, val)
			}
		case 't':
			// timestamp 时间字段由 fieldBytesAny 单独读取
		}
	case 10:
		switch key[0] {
		case 'u':
			if string(key) == "user_agent" {
				f.put(fAgent, val)
			}
		case 'b':
			if string(key) == "bytes_sent" {
				f.put(fSize, val)
			}
		case 't':
			// time_local 时间字段由 fieldBytesAny 单独读取
		}
	case 11:
		switch key[0] {
		case 'r':
			if string(key) == "request_uri" {
				f.put(fURI, val)
			} else if string(key) == "real_client" {
				f.put(fRealClient, val)
			} else if string(key) == "remote_addr" {
				f.put(fClient, val)
			}
		case 's':
			if string(key) == "status_code" {
				f.put(fStatus, val)
			}
		}
	case 12:
		switch key[0] {
		case 'r':
			if string(key) == "responsetime" {
				f.put(fRespTime, val)
			} else if string(key) == "request_time" {
				f.put(fRespTime, val)
			}
		case 'h':
			if string(key) == "http_referer" {
				f.put(fRefer, val)
			}
		case 't':
			// time_iso8601 时间字段由 fieldBytesAny 单独读取
		}
	case 13:
		switch key[0] {
		case 'u':
			if string(key) == "upstream_addr" {
				f.put(fUpAddr, val)
			} else if string(key) == "upstream_time" {
				f.put(fUpTime, val)
			}
		case 'r':
			if string(key) == "response_time" {
				f.put(fRespTime, val)
			}
		}
	case 14:
		switch key[0] {
		case 'r':
			if string(key) == "request_method" {
				f.put(fMethod, val)
			}
		case 'h':
			if string(key) == "http_x_real_ip" {
				f.put(fRealClient, val)
			}
		}
	case 15:
		switch key[0] {
		case 'u':
			if string(key) == "upstream_status" {
				f.put(fUpStatus, val)
			}
		case 'h':
			if string(key) == "http_user_agent" {
				f.put(fAgent, val)
			}
		case 'b':
			if string(key) == "body_bytes_sent" {
				f.put(fSize, val)
			}
		case 'x':
			if string(key) == "x_forwarded_for" {
				f.put(fRealClient, val)
			}
		}
	case 18:
		if key[0] == 'r' && string(key) == "realip_remote_addr" {
			f.put(fRealClient, val)
		}
	case 20:
		if key[0] == 'h' && string(key) == "http_x_forwarded_for" {
			f.put(fRealClient, val)
		}
	case 25:
		if key[0] == 'p' && string(key) == "proxy_add_x_forwarded_for" {
			f.put(fRealClient, val)
		}
	case 22:
		if key[0] == 'u' && string(key) == "upstream_response_time" {
			f.put(fUpTime, val)
		}
	}
}

// scanFields 单趟扫描一行 JSON，取出所有需要的字段。
// 相比「每个字段各扫一遍」少走 7 遍内存；同时正确处理 \" 转义
// （旧实现遇到含引号的 URI 会被截断）。
func scanFields(line []byte, f *lineFields) {
	f.reset()
	i := 0
	for i < len(line) {
		if line[i] != '"' {
			i++
			continue
		}
		// key
		ks := i + 1
		j := ks
		// 字段名多数很短，但 agent 之类长键也存在，IndexByte 更快且能正确处理转义
		for j < len(line) {
			n := bytes.IndexByte(line[j:], '"')
			if n < 0 {
				return
			}
			j += n
			bs := 0
			for p := j - 1; p >= ks && line[p] == '\\'; p-- {
				bs++
			}
			if bs%2 == 0 {
				break
			}
			j++
		}
		if j >= len(line) {
			return
		}
		key := line[ks:j]
		k := j + 1
		for k < len(line) && line[k] <= ' ' {
			k++
		}
		if k >= len(line) || line[k] != ':' {
			i = j + 1
			continue
		}
		k++
		for k < len(line) && line[k] <= ' ' {
			k++
		}
		if k >= len(line) {
			return
		}
		if line[k] == '"' {
			// 字符串值：用 IndexByte（AVX2 加速）定位结束引号，遇到转义再往后找。
			// 原来逐字节循环，在长字段（agent、uri）上明显更慢。
			vs := k + 1
			e := vs
			for e <= len(line) {
				n := bytes.IndexByte(line[e:], '"')
				if n < 0 {
					e = len(line)
					break
				}
				e += n
				bs := 0
				for j := e - 1; j >= vs && line[j] == '\\'; j-- {
					bs++
				}
				if bs%2 == 0 { // 前导反斜杠为偶数 → 该引号未被转义
					break
				}
				e++
			}
			if e > len(line) {
				e = len(line)
			}
			f.setByKey(key, line[vs:e])
			i = e + 1
		} else {
			vs := k
			e := vs
			for e < len(line) && !isValueEnd(line[e]) {
				e++
			}
			f.setByKey(key, line[vs:e])
			i = e
		}
	}
}

// scanFieldsAlias 与 scanFields 的扫描逻辑保持一致，只是把字段分发换成
// setByKeyAlias（支持 nginx 变量名/自定义 JSON 键）。只在 aliasJSON=true 时调用，
// 不重置 mask，让默认字段和别名字段可以同时存在。
func scanFieldsAlias(line []byte, f *lineFields) {
	i := 0
	for i < len(line) {
		if line[i] != '"' {
			i++
			continue
		}
		// key
		ks := i + 1
		j := ks
		// 字段名多数很短，但 agent 之类长键也存在，IndexByte 更快且能正确处理转义
		for j < len(line) {
			n := bytes.IndexByte(line[j:], '"')
			if n < 0 {
				return
			}
			j += n
			bs := 0
			for p := j - 1; p >= ks && line[p] == '\\'; p-- {
				bs++
			}
			if bs%2 == 0 {
				break
			}
			j++
		}
		if j >= len(line) {
			return
		}
		key := line[ks:j]
		k := j + 1
		for k < len(line) && line[k] <= ' ' {
			k++
		}
		if k >= len(line) || line[k] != ':' {
			i = j + 1
			continue
		}
		k++
		for k < len(line) && line[k] <= ' ' {
			k++
		}
		if k >= len(line) {
			return
		}
		if line[k] == '"' {
			// 字符串值：用 IndexByte（AVX2 加速）定位结束引号，遇到转义再往后找。
			// 原来逐字节循环，在长字段（agent、uri）上明显更慢。
			vs := k + 1
			e := vs
			for e <= len(line) {
				n := bytes.IndexByte(line[e:], '"')
				if n < 0 {
					e = len(line)
					break
				}
				e += n
				bs := 0
				for j := e - 1; j >= vs && line[j] == '\\'; j-- {
					bs++
				}
				if bs%2 == 0 { // 前导反斜杠为偶数 → 该引号未被转义
					break
				}
				e++
			}
			if e > len(line) {
				e = len(line)
			}
			f.setByKeyAlias(key, line[vs:e])
			i = e + 1
		} else {
			vs := k
			e := vs
			for e < len(line) && !isValueEnd(line[e]) {
				e++
			}
			f.setByKeyAlias(key, line[vs:e])
			i = e
		}
	}
}

// timestampPrefix nginx JSON 日志的固定开头（@timestamp 是第一个字段）
var timestampPrefix = []byte(`{"@timestamp": "`)

// fastTimestamp 行首就是 @timestamp 时直接取值（生产日志绝大多数命中），
// 省掉一次全行 Index 搜索；形态不匹配时返回 false，由 fieldBytes 兜底。
func fastTimestamp(line []byte) ([]byte, bool) {
	if len(line) < len(timestampPrefix)+2 || !bytes.HasPrefix(line, timestampPrefix) {
		return nil, false
	}
	rest := line[len(timestampPrefix):]
	i := bytes.IndexByte(rest, '"')
	if i < 0 {
		return nil, false
	}
	return rest[:i], true
}

// fieldBytes 取出 "<key>": "<value>"（或裸值）的原始字节，不分配内存。
// key 必须完整匹配（前后为引号），因此 status 不会误命中 upstream_status。
func fieldBytes(line, key []byte) ([]byte, bool) {
	if len(key) == 0 || len(line) == 0 {
		return nil, false
	}
	idx := 0
	for {
		i := bytes.Index(line[idx:], key)
		if i < 0 {
			return nil, false
		}
		i += idx
		idx = i + len(key)
		if i == 0 || line[i-1] != '"' {
			continue
		}
		j := i + len(key)
		if j >= len(line) || line[j] != '"' {
			continue
		}
		k := j + 1
		for k < len(line) && (line[k] <= ' ') {
			k++
		}
		if k >= len(line) || line[k] != ':' {
			continue
		}
		k++
		for k < len(line) && (line[k] <= ' ') {
			k++
		}
		if k >= len(line) {
			return nil, false
		}
		if line[k] == '"' {
			end := bytes.IndexByte(line[k+1:], '"')
			if end < 0 {
				return nil, false
			}
			return line[k+1 : k+1+end], true
		}
		// 裸值：读到 , } 或空白为止
		e := k
		for e < len(line) && !isValueEnd(line[e]) {
			e++
		}
		if e > k {
			return line[k:e], true
		}
		return nil, false
	}
}

// fieldBytesAny 按顺序尝试多个 JSON 键，返回第一个命中的值。
func fieldBytesAny(line []byte, keys [][]byte) ([]byte, bool) {
	for _, k := range keys {
		if v, ok := fieldBytes(line, k); ok {
			return v, true
		}
	}
	return nil, false
}

// firstListElem 取逗号列表的第一个非空元素（"a, b" -> "a"）
func firstListElem(v []byte) []byte {
	for len(v) > 0 {
		i := bytes.IndexByte(v, ',')
		if i < 0 {
			break
		}
		if cand := trimSpaceBytes(v[:i]); len(cand) > 0 && !isDash(cand) {
			return cand
		}
		v = v[i+1:]
	}
	if e := trimSpaceBytes(v); !isDash(e) {
		return e
	}
	return nil
}

// lastListElem 取逗号列表的最后一个非空元素（"502, 200" -> "200"）
func lastListElem(v []byte) []byte {
	for len(v) > 0 {
		i := bytes.LastIndexByte(v, ',')
		if i < 0 {
			break
		}
		if cand := trimSpaceBytes(v[i+1:]); len(cand) > 0 && !isDash(cand) {
			return cand
		}
		v = v[:i]
	}
	if e := trimSpaceBytes(v); !isDash(e) {
		return e
	}
	return nil
}

func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && b[0] <= ' ' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] <= ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func isDash(b []byte) bool { return len(b) == 1 && b[0] == '-' }

// isErrStatus 状态码（可带前导空白）是否为 4xx/5xx
func isErrStatus(v []byte) bool {
	v = trimSpaceBytes(v)
	return len(v) > 0 && v[0] >= '4' && v[0] <= '5'
}

// isValueEnd 判断裸值结束（逗号、右括号或空白）
func isValueEnd(c byte) bool {
	return c == ',' || c == '}' || c <= ' '
}

// atoiBytes 解析定长十进制数字
func atoiBytes(s []byte) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// parseIntBytes 解析非负整数（size 等）。
// 超过 18 位或超过 maxSizeBytes 直接判为非法，避免 int64 溢出把流量统计变成负数。
func parseIntBytes(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if n > maxSizeBytes {
		return 0, false
	}
	return n, true
}

// parseFloatBytes 解析简单小数（响应时间），异常格式回退 strconv。
// 只接受有限、非负值：NaN/Inf/负数会污染平均响应、百分比和 PDF/XLSX。
func parseFloatBytes(b []byte) (float64, bool) {
	if len(b) == 0 || len(b) > 64 {
		return 0, false
	}
	if len(b) > 18 {
		return parseFloatFallback(b)
	}
	var ip, fp int64
	div := 1.0
	seenDot := false
	digits := false
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			digits = true
			if !seenDot {
				ip = ip*10 + int64(c-'0')
			} else {
				fp = fp*10 + int64(c-'0')
				div *= 10
			}
		case c == '.':
			if seenDot {
				return parseFloatFallback(b)
			}
			seenDot = true
		case c == ',':
			// 多个上游（"0.005, 0.006"）只取第一个，与原正则行为一致
			if !digits {
				return 0, false
			}
			v := float64(ip) + float64(fp)/div
			if !validTimeValue(v) {
				return 0, false
			}
			if v > maxTimingSeconds {
				v = maxTimingSeconds
			}
			return v, true
		default:
			return parseFloatFallback(b)
		}
	}
	if !digits {
		return 0, false
	}
	v := float64(ip) + float64(fp)/div
	if !validTimeValue(v) {
		return 0, false
	}
	if v > maxTimingSeconds {
		v = maxTimingSeconds
	}
	return v, true
}

func parseFloatFallback(b []byte) (float64, bool) {
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil || !validTimeValue(v) {
		return 0, false
	}
	if v > maxTimingSeconds {
		v = maxTimingSeconds
	}
	return v, true
}

func validTimeValue(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// firstFloatOf 在已扫描的字段里依次尝试，取第一个可解析为数字的值
// （upstream_response_time 为 "-" 时回退 responsetime）。
// 第二个返回值表示是否真的取到了耗时——取不到的行不能当成 0ms 参与统计。
func firstFloatOf(f *lineFields, kinds ...int) (float64, bool) {
	for _, k := range kinds {
		v, ok := f.get(k)
		if !ok {
			continue
		}
		if val, ok2 := parseFloatList(v); ok2 {
			return val, true
		}
	}
	return 0, false
}

// parseFloatList 解析可能是逗号列表的耗时值（nginx 重试时会写多个），
// 取第一个能解析的元素；"-" 之类的无效元素跳过。
func parseFloatList(b []byte) (float64, bool) {
	for {
		i := bytes.IndexByte(b, ',')
		elem := b
		if i >= 0 {
			elem = b[:i]
		}
		if f, ok := parseFloatBytes(trimSpaceBytes(elem)); ok {
			return f, true
		}
		if i < 0 {
			return 0, false
		}
		b = b[i+1:]
	}
}

// dateInt 将 YYYY-MM-DD 转为 yyyymmdd 整数
func dateInt(s string) (int64, bool) {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return 0, false
	}
	y, ok1 := atoiBytes([]byte(s[0:4]))
	m, ok2 := atoiBytes([]byte(s[5:7]))
	d, ok3 := atoiBytes([]byte(s[8:10]))
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return int64(y)*10000 + int64(m)*100 + int64(d), true
}

// 固定时区缓存（同一 offset 只创建一次 Location）
var zoneCache sync.Map // int(offsetSeconds) -> *time.Location

func fixedZone(offsetSeconds int) *time.Location {
	if v, ok := zoneCache.Load(offsetSeconds); ok {
		return v.(*time.Location)
	}
	sign := byte('+')
	abs := offsetSeconds
	if abs < 0 {
		sign = '-'
		abs = -abs
	}
	name := fmt.Sprintf("UTC%c%02d:%02d", sign, abs/3600, abs%3600/60)
	loc := time.FixedZone(name, offsetSeconds)
	zoneCache.Store(offsetSeconds, loc)
	return loc
}

// parseTimestamp 快速解析 nginx 常用的 ISO8601 时间戳：
// 2006-01-02T15:04:05+08:00 / 2006-01-02 15:04:05 / 带小数秒 / 带 Z
// parseTimestampAny 支持 JSON ISO 时间和 nginx 的 $time_local / $time_iso8601。
func parseTimestampAny(s []byte) (time.Time, bool) {
	if len(s) == 0 || len(s) > 64 {
		return time.Time{}, false
	}
	if t, ok := parseTimestamp(s); ok {
		return t, true
	}
	if t, ok := parseNginxTime(s); ok {
		return t, true
	}
	// 兜底：常见但非严格布局（CSV 导出等）
	if t, err := time.Parse("2006-01-02 15:04:05", string(s)); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, string(s)); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// parseNginxTime 解析 nginx combined 的 $time_local：
// 02/Jan/2006:15:04:05 -0700（如 30/Oct/2026:10:00:00 +0800）。
func parseNginxTime(s []byte) (time.Time, bool) {
	if len(s) < 26 || len(s) > 64 || s[2] != '/' || s[6] != '/' || s[11] != ':' ||
		s[14] != ':' || s[17] != ':' || s[20] != ' ' || (s[21] != '+' && s[21] != '-') {
		return time.Time{}, false
	}
	d, ok1 := atoiBytes(s[0:2])
	mo, ok2 := nginxMonth(s[3:6])
	y, ok3 := atoiBytes(s[7:11])
	h, ok4 := atoiBytes(s[12:14])
	mi, ok5 := atoiBytes(s[15:17])
	sec, ok6 := atoiBytes(s[18:20])
	oh, ok7 := atoiBytes(s[22:24])
	om, ok8 := atoiBytes(s[24:26])
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 {
		return time.Time{}, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 60 {
		return time.Time{}, false
	}
	if !nginxDayValid(y, mo, d) || oh > 14 || om > 59 || (oh == 14 && om > 0) {
		return time.Time{}, false
	}
	off := oh*3600 + om*60
	if s[21] == '-' {
		off = -off
	}
	return time.Date(y, time.Month(mo), d, h, mi, sec, 0, fixedZone(off)), true
}

func nginxDayValid(year, month, day int) bool {
	switch month {
	case 4, 6, 9, 11:
		return day <= 30
	case 2:
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if leap {
			return day <= 29
		}
		return day <= 28
	default:
		return day <= 31
	}
}

func nginxMonth(s []byte) (int, bool) {
	if len(s) != 3 {
		return 0, false
	}
	switch s[0] {
	case 'J':
		if s[1] == 'a' && s[2] == 'n' {
			return 1, true
		}
		if s[1] == 'u' && s[2] == 'n' {
			return 6, true
		}
		if s[1] == 'u' && s[2] == 'l' {
			return 7, true
		}
	case 'F':
		if s[1] == 'e' && s[2] == 'b' {
			return 2, true
		}
	case 'M':
		if s[1] == 'a' && s[2] == 'r' {
			return 3, true
		}
		if s[1] == 'a' && s[2] == 'y' {
			return 5, true
		}
	case 'A':
		if s[1] == 'p' && s[2] == 'r' {
			return 4, true
		}
		if s[1] == 'u' && s[2] == 'g' {
			return 8, true
		}
	case 'S':
		if s[1] == 'e' && s[2] == 'p' {
			return 9, true
		}
	case 'O':
		if s[1] == 'c' && s[2] == 't' {
			return 10, true
		}
	case 'N':
		if s[1] == 'o' && s[2] == 'v' {
			return 11, true
		}
	case 'D':
		if s[1] == 'e' && s[2] == 'c' {
			return 12, true
		}
	}
	return 0, false
}
func parseTimestamp(s []byte) (time.Time, bool) {
	if len(s) < 19 || len(s) > 64 || s[4] != '-' || s[7] != '-' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	if s[10] != 'T' && s[10] != 't' && s[10] != ' ' {
		return time.Time{}, false
	}
	y, ok1 := atoiBytes(s[0:4])
	mo, ok2 := atoiBytes(s[5:7])
	d, ok3 := atoiBytes(s[8:10])
	h, ok4 := atoiBytes(s[11:13])
	mi, ok5 := atoiBytes(s[14:16])
	sec, ok6 := atoiBytes(s[17:19])
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return time.Time{}, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 60 {
		return time.Time{}, false
	}
	if !nginxDayValid(y, mo, d) {
		return time.Time{}, false
	}

	loc := time.Local
	i := 19
	if i < len(s) && (s[i] == '.' || s[i] == ',') {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) {
		switch s[i] {
		case 'Z', 'z':
			if i+1 != len(s) {
				return time.Time{}, false
			}
			loc = time.UTC
		case '+', '-':
			if i+3 > len(s) {
				return time.Time{}, false
			}
			oh, okh := atoiBytes(s[i+1 : i+3])
			if !okh {
				return time.Time{}, false
			}
			j := i + 3
			if j < len(s) && s[j] == ':' {
				j++
			}
			if j+2 > len(s) {
				return time.Time{}, false
			}
			om, okm := atoiBytes(s[j : j+2])
			if !okm || j+2 != len(s) {
				return time.Time{}, false
			}
			if oh > 14 || om > 59 || (oh == 14 && om > 0) {
				return time.Time{}, false
			}
			off := oh*3600 + om*60
			if s[i] == '-' {
				off = -off
			}
			loc = fixedZone(off)
		default:
			return time.Time{}, false
		}
	}
	return time.Date(y, time.Month(mo), d, h, mi, sec, 0, loc), true
}

// ---------------------------------------------------------------- 节流

// throttle 按目标 CPU 占用率插入 sleep，实现「长时间扫描也不吃满一个核」
type throttle struct {
	target float64 // 目标占比 0~1，>=1 表示不限速
	work   time.Duration
	slept  time.Duration
	stamp  time.Time
	active bool
}

func newThrottle(cpuPercent int) *throttle {
	if cpuPercent <= 0 || cpuPercent >= 100 {
		return &throttle{target: 1}
	}
	return &throttle{target: float64(cpuPercent) / 100.0, active: true}
}

func (t *throttle) mark() {
	if t.active {
		t.stamp = time.Now()
	}
}

// tick 在每个分块结束时调用：工作耗时 W，则应累计睡眠 W*(1-r)/r
func (t *throttle) tick() {
	if !t.active {
		return
	}
	t.work += time.Since(t.stamp)
	want := time.Duration(float64(t.work) * (1 - t.target) / t.target)
	if want > t.slept {
		time.Sleep(want - t.slept)
		t.slept = want
	}
	t.stamp = time.Now()
}

// ---------------------------------------------------------------- UA / Referer 归类

// containsFold 在字节串里做大小写不敏感的子串查找。
// UA 每行都要判断十几次，不能为每个关键字都 ToLower 一次。
func containsFold(b []byte, s string) bool {
	n := len(s)
	if n == 0 {
		return true
	}
	if n > len(b) {
		return false
	}
	for i := 0; i+n <= len(b); i++ {
		match := true
		for j := 0; j < n; j++ {
			c, d := b[i+j], s[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if d >= 'A' && d <= 'Z' {
				d += 'a' - 'A'
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// classifyDevice 设备类型（UA 缺失记“未知”）
const maxUAClassify = 2048

func classifyDevice(ua []byte) string {
	if len(ua) == 0 {
		return "未知"
	}
	if len(ua) > maxUAClassify {
		ua = ua[:maxUAClassify]
	}
	switch {
	case containsFold(ua, "mobile"), containsFold(ua, "android"),
		containsFold(ua, "iphone"), containsFold(ua, "ipad"),
		containsFold(ua, "ipod"), containsFold(ua, "windows phone"),
		containsFold(ua, "harmonyos"):
		return "移动端"
	}
	return "桌面端"
}

// classifyBrowser 浏览器归类（先判断会“包含 Chrome”的派生浏览器）
func classifyBrowser(ua []byte) string {
	if len(ua) == 0 {
		return "未知"
	}
	if len(ua) > maxUAClassify {
		ua = ua[:maxUAClassify]
	}
	switch {
	case containsFold(ua, "micromessenger"):
		return "微信"
	case containsFold(ua, "edg/"), containsFold(ua, "edge/"):
		return "Edge"
	case containsFold(ua, "opr/"), containsFold(ua, "opera"):
		return "Opera"
	case containsFold(ua, "chrome/"), containsFold(ua, "crios/"):
		return "Chrome"
	case containsFold(ua, "firefox/"), containsFold(ua, "fxios/"):
		return "Firefox"
	case containsFold(ua, "msie"), containsFold(ua, "trident/"):
		return "IE"
	case containsFold(ua, "safari/"):
		return "Safari"
	case containsFold(ua, "curl"), containsFold(ua, "wget"),
		containsFold(ua, "python"), containsFold(ua, "java/"),
		containsFold(ua, "okhttp"), containsFold(ua, "go-http-client"):
		return "其他客户端"
	}
	return "其他"
}

// parseReferer 从 referer 里取域名（保留端口），空/"-"/null 视为直接访问。
func parseReferer(b []byte) (host string, direct bool) {
	if len(b) > 2048 {
		b = b[:2048]
	}
	if len(b) == 0 {
		return "", true
	}
	if len(b) == 1 && b[0] == '-' {
		return "", true
	}
	if bytes.EqualFold(b, []byte("null")) {
		return "", true
	}
	if i := bytes.Index(b, []byte("://")); i >= 0 {
		b = b[i+3:]
	}
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '/', '?', '#':
			b = b[:i]
			i = len(b)
		}
	}
	if len(b) == 0 {
		return "", true
	}
	return strings.ToLower(string(b)), false
}

// docFileExts 文档 / 媒体 / 压缩包类型。
// 文件名经常只出现在 query 里（/bbt-api/sys/file/load?name=xx.xlsx），
// 而且可能是 URL 编码的，所以直接在原始 request_uri 上找扩展名。
var docFileExts = map[string]bool{
	"xlsx": true, "xls": true, "docx": true, "doc": true,
	"pptx": true, "ppt": true, "pdf": true, "csv": true,
	"txt": true, "json": true, "xml": true, "html": true, "htm": true,
	"zip": true, "rar": true, "7z": true, "tar": true, "gz": true,
	"mp4": true, "avi": true, "mov": true, "wmv": true, "mkv": true, "webm": true,
	"mp3": true, "wav": true, "flac": true,
}

// detectFileType 从原始 URI（含 query）里找文档/媒体扩展名，找不到返回空。
// 从后往前找：URL 里可能先出现 .html 页面，真正的文件名在 query 最后。
func detectFileType(uri []byte) string {
	const maxDocExtLen = 8 // docFileExts 里最长 4 个字符，留余量
	for i := len(uri) - 1; i >= 1; i-- {
		if uri[i] != '.' {
			continue
		}
		j := i + 1
		for j < len(uri) && isAlphaNum(uri[j]) {
			j++
		}
		if j == i+1 || j-i-1 > maxDocExtLen {
			continue
		}
		// 扩展名后面必须是边界，避免把 .xlsxbackup 当成 xlsx
		if j < len(uri) && (isAlphaNum(uri[j]) || uri[j] == '.' || uri[j] == '_' || uri[j] == '-') {
			continue
		}
		var buf [maxDocExtLen]byte
		ext := buf[:j-i-1]
		for k := 0; k < len(ext); k++ {
			c := uri[i+1+k]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			ext[k] = c
		}
		if docFileExts[string(ext)] {
			return string(ext)
		}
	}
	return ""
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// overflowBucket 10 秒以上的耗时落桶；<10s 返回 -1（由 TimeBuckets 负责）。
// NaN 已由 Agg.add 过滤，这里不再做浮点异常判断（分支会让热路径变慢）。
func overflowBucket(sec float64) int {
	switch {
	case sec < 10:
		return -1
	case sec < 60:
		return int(sec) - 10
	case sec < 600:
		return 50 + int((sec-60)/10)
	default:
		return TimeOverflowCount - 1
	}
}

// overflowBounds 返回溢出桶的 [from,to) 秒区间；最后一桶 to=+Inf。
func overflowBounds(i int) (float64, float64) {
	switch {
	case i < 0:
		return 0, 0
	case i < 50:
		return 10 + float64(i), 11 + float64(i)
	case i < TimeOverflowCount-1:
		from := 60 + float64(i-50)*10
		return from, from + 10
	default:
		return 600, math.Inf(1)
	}
}

// looksLikeBot 粗略识别爬虫 / 探针 / 监控 UA（默认不启用）。
func looksLikeBot(ua []byte) bool {
	if len(ua) == 0 {
		return false
	}
	if len(ua) > maxUAClassify {
		ua = ua[:maxUAClassify]
	}
	for _, kw := range []string{
		"bot", "spider", "crawler", "slurp", "bingpreview",
		"facebookexternalhit", "semrush", "ahrefs", "mj12",
		"python-requests", "python-urllib", "curl/", "wget/",
		"go-http-client", "java/", "okhttp", "headlesschrome",
		"phantomjs", "uptimerobot", "pingdom", "zabbix",
		"prometheus", "nagios", "site24x7", "kube-probe", "healthcheck",
	} {
		if containsFold(ua, kw) {
			return true
		}
	}
	return false
}
