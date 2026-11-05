// Package config 提供配置定义与默认值
package config

// Config 周报系统配置
type Config struct {
	// 日志目录
	LogDir string
	// 单文件模式：-logdir 指向某个具体文件时记录其绝对路径。
	// 非空时只处理这一个文件（不匹配同目录其他文件、不套周窗口）。
	SingleFile string
	// 日志文件名匹配模式（支持 gz 与未压缩）
	LogPattern string
	// 报告输出目录
	ReportDir string
	// 历史数据目录（存 JSON，用于环比）
	HistoryDir string
	// 历史报告保留天数
	KeepDays int
	// 运行日志文件基准路径（按天轮转成 app-YYYY-MM-DD.log）；
	// 空字符串表示自动使用 <ReportDir>/logs/nginx-report.log
	LogFile string
	// 运行日志保留天数（0=默认 30 天，负数=不清理）
	LogKeepDays int
	// 每个文件处理完休眠秒数（降低 CPU 压力）
	SleepSec float64
	// 进程优先级 0-19，19 最低
	NiceLevel int
	// 报告标题
	ReportTitle string
	// 错误率告警阈值（百分比）
	ErrorAlert float64
	// 慢请求参考阈值（次数）：占比很低时才用它兜底，同时作为每日状态标签的基准
	SlowAlert int
	// 常驻模式：每天处理头一天的时间（HH:MM）
	DailyAt string
	// 常驻模式：周汇总在周几（1=周一 ... 7=周日）
	WeeklyDay int
	// 常驻模式：周汇总时间（HH:MM）
	WeeklyAt string
	// 常驻模式：任务失败后的重试间隔（分钟）
	RetryMin int
	// 常驻模式：单个任务最多尝试次数（含首次），超过后本周期放弃
	MaxRetry int
	// 常驻模式：每隔多少分钟刷新一次「今天」的部分统计，0=关闭
	TodayEvery int
	// 处理日志时的目标 CPU 占用百分比（1-100，>=100 表示不限速）
	CPUPercent int
	// 跨日提前退出阈值（连续多少行超出结束日期即停扫），负数表示关闭
	EarlyExitLines int
	// FromClock 只统计每天 HH:MM 之后的记录（空=不限）；
	// 对应原 analyze_log_fast.sh 的 START_TIME，例如 11:30
	FromClock string
	// 接口 Top 表是否排除静态资源（css/js/图片/字体，默认开启）
	StaticExclude bool
	// RealIP 优先使用 real_client（CDN/代理场景下的真实客户端 IP），默认开启
	RealIP bool
	// TimeField 响应时间取字段：auto / upstream / response
	TimeField string
	// SlowMS 慢请求阈值（毫秒）
	SlowMS int
	// ExcludeBots 按 UA 排除爬虫/探针/监控请求（默认关闭）
	ExcludeBots bool
	// LogFormat 日志格式：auto（默认）/ json / combined / csv / nginx log_format 模板
	LogFormat string
	// CSVFields 当日志是 CSV 且没有表头时，按该列顺序解析
	CSVFields string
	// CSVSkipHeader 强制跳过 CSV 第一行（表头列名无法识别时用）
	CSVSkipHeader bool
	// JSONFields 自定义 JSON 字段映射，如 ts=time,cip=client,u=uri
	JSONFields string
	// SkipStatic 解析时直接跳过静态资源记录（css/js/图片/字体等），默认开启：
	// 这些请求不进 PV、分钟趋势、状态码、IP、流量、错误/慢请求统计。
	SkipStatic bool
	// Format 输出格式：html（默认）/ pdf / xlsx，可用逗号组合，或 all
	Format string
	// OpsReport 是否额外输出运维/开发版报告（*_ops.html / *_ops.txt）
	OpsReport bool
	// Preview 只出报告，不读写日记录/周汇总（复盘某个 gz / 补算一次用）
	Preview bool
	// TopN 运维报告里 Top 列表显示多少条（默认 20）
	TopN int
	// MaxMemMB 进程堆内存软上限（MB），0=不限制。超过后 Go 会主动 GC，
	// 保证统计任务不会把服务器内存吃满。
	MaxMemMB int
	// 统计接口维度时去掉 ?query（默认开启）。
	// 带随机参数的 URI 会让基数爆炸：日记录体积暴涨、报告里的"接口 Top"全是噪音。
	URINormalize bool

	// 多站点报告
	SiteEnabled   bool
	SiteMap       string
	SiteAliasMap  string
	SiteReport    string // aggregate / both / per
	SiteFrom      string // auto / field / map / filename / dir
	SiteField     string
	SiteName      string
	SiteFilter    string
	SiteIPMode    string // separate / inherit / ignore
	SiteIPReport  string // none / group / per-ip
	SiteStripWWW  bool
	SiteMax       int
	SiteMinPV     int64
	SiteMaxReport int
}

// Default 返回默认配置
func Default() *Config {
	return &Config{
		LogDir:         "/data/_log/backup",
		LogPattern:     "28888_access_json.log*",
		ReportDir:      "/data/reports",
		HistoryDir:     "/data/reports/history",
		KeepDays:       90,
		LogFile:        "",
		LogKeepDays:    30,
		SleepSec:       0.5,
		NiceLevel:      19,
		ReportTitle:    "系统运行周报",
		ErrorAlert:     1.0,
		SlowAlert:      100,
		DailyAt:        "00:10",
		WeeklyDay:      1,
		WeeklyAt:       "08:00",
		RetryMin:       10,
		MaxRetry:       12,
		TodayEvery:     0,
		CPUPercent:     50,
		EarlyExitLines: 2000,
		StaticExclude:  true,
		SkipStatic:     true,
		RealIP:         true,
		TimeField:      "auto",
		SlowMS:         1000,
		LogFormat:      "auto",
		Format:         "html",
		OpsReport:      true,
		TopN:           20,
		MaxMemMB:       256,
		URINormalize:   true,
		SiteReport:     "both",
		SiteFrom:       "auto",
		SiteIPMode:     "separate",
		SiteIPReport:   "none",
		SiteMax:        1000,
		SiteMaxReport:  100,
	}
}
