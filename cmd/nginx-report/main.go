// 主入口：Nginx 日志周报系统
//
// 三种运行模式（日志每天 00:00 切割压缩）：
//
//	daily  : 只处理头一天（昨天）的日志，落一份日记录 JSON，压力小（一次性）
//	weekly : 汇总上一个完整周（周一~周日）的 7 份日记录，生成 HTML 周报（一次性）
//	serve  : 常驻运行，内置调度器定时跑 daily / weekly，失败自动重试
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nginx-report/internal/accesslog"
	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
	"nginx-report/internal/detect"
	"nginx-report/internal/fsutil"
	"nginx-report/internal/history"
	"nginx-report/internal/logger"
	"nginx-report/internal/report"
	"nginx-report/internal/scheduler"
	"nginx-report/internal/site"
	"nginx-report/internal/sysctl"
	"nginx-report/internal/sysinfo"
)

func main() {
	startTime := time.Now()
	cfg := config.Default()

	modeFlag := flag.String("mode", "weekly", "运行模式: daily(处理单日) / weekly(汇总周报) / serve(常驻调度)")
	dateFlag := flag.String("date", "", "daily 模式的统计日期：YYYY-MM-DD / today / yesterday（默认 yesterday）")
	startFlag := flag.String("start", "", "weekly 模式开始日期 YYYY-MM-DD（默认上一个完整周的周一）")
	endFlag := flag.String("end", "", "weekly 模式结束日期 YYYY-MM-DD（默认上一个完整周的周日）")
	dailyAtFlag := flag.String("daily-at", "", "serve 模式：每天处理头一天的时间 HH:MM（默认 00:10）")
	weeklyAtFlag := flag.String("weekly-at", "", "serve 模式：周汇总时间 HH:MM（默认 08:00）")
	weeklyDayFlag := flag.Int("weekly-day", 0, "serve 模式：周汇总在周几 1-7（默认 1=周一）")
	retryMinFlag := flag.Int("retry-min", 0, "serve 模式：任务失败重试间隔分钟（默认 10）")
	cpuPercentFlag := flag.Int("cpu-percent", cfg.CPUPercent, "解析日志时的目标 CPU 占用百分比 1-100（100=不限速）")
	earlyExitFlag := flag.Int("early-exit", cfg.EarlyExitLines, "跨日提前退出阈值行数（0=关闭；非时间有序日志请关闭）")
	fromFlag := flag.String("from", cfg.FromClock, "只统计每天 HH:MM 之后的记录（如 11:30，对应原 analyze_log_fast.sh 的 START_TIME；空=不限）")
	niceFlag := flag.Int("nice", cfg.NiceLevel, "进程 nice 值 0-19（0=不调整；仅 Linux/macOS）")
	maxRetryFlag := flag.Int("max-retry", 0, "serve 模式：单周期最大尝试次数（默认 12，<=0 不限制）")
	todayEveryFlag := flag.Int("today-every", 0, "serve 模式：每隔 N 分钟刷新一次当天部分统计（0=关闭）")
	uriNormFlag := flag.Bool("uri-normalize", cfg.URINormalize, "统计接口时去掉 ?query（避免同一接口被参数拆成上万条）")
	staticFlag := flag.Bool("static-exclude", cfg.StaticExclude, "接口 Top 表排除静态资源（css/js/图片/字体），单独汇总一行")
	skipStaticFlag := flag.Bool("skip-static", cfg.SkipStatic, "解析时直接排除静态资源记录（css/js/图片/字体等），默认开；-skip-static=false 关闭")
	realIPFlag := flag.Bool("real-ip", cfg.RealIP, "优先使用 real_client 作为客户端 IP（CDN/代理场景），为空回退 client")
	timeFieldFlag := flag.String("time-field", cfg.TimeField, "响应时间取字段：auto（上游优先）/ upstream / response")
	slowMSFlag := flag.Int("slow-ms", cfg.SlowMS, "慢请求阈值（毫秒，默认 1000）")
	excludeBotsFlag := flag.Bool("exclude-bots", cfg.ExcludeBots, "按 UA 排除爬虫/探针/监控请求（默认关闭）")
	logFormatFlag := flag.String("log-format", cfg.LogFormat, "输入日志格式：auto/json/combined/csv，或直接传 nginx log_format 模板")
	csvFieldsFlag := flag.String("csv-fields", cfg.CSVFields, "CSV 无表头时的列顺序，如 time,client,method,uri,status,size")
	csvSkipHeaderFlag := flag.Bool("csv-skip-header", cfg.CSVSkipHeader, "CSV 第一行是表头但列名无法识别时，配合 -log-format csv -csv-fields 强制跳过")
	jsonFieldsFlag := flag.String("json-fields", cfg.JSONFields, "自定义 JSON 字段映射，如 ts=time,cip=client,u=uri,st=status")
	maxMemFlag := flag.Int("max-mem-mb", cfg.MaxMemMB, "堆内存软上限（MB），超过后主动 GC；0=不限制")
	formatFlag := flag.String("format", cfg.Format, "报告输出格式: "+report.FormatNames())
	opsFlag := flag.Bool("ops", cfg.OpsReport, "同时输出运维/开发版纯文本明细报告（*_ops.txt）")
	topFlag := flag.Int("top", cfg.TopN, "运维报告 Top 列表显示条数（默认 20）")
	previewFlag := flag.Bool("preview", cfg.Preview, "只出报告，不读写日记录/周汇总（复盘某个日志文件时用）")
	siteMapFlag := flag.String("site-map", cfg.SiteMap, "站点映射文件，格式：域名 = 文件匹配模式")
	siteAliasFlag := flag.String("site-alias-map", cfg.SiteAliasMap, "站点别名映射文件，格式：别名 = 主域名")
	siteReportFlag := flag.String("site-report", cfg.SiteReport, "站点报告范围: aggregate(仅全站) / both(全站+每站点) / per(仅每站点)")
	siteFromFlag := flag.String("site-from", cfg.SiteFrom, "站点来源: auto / field / map / filename / dir")
	siteFieldFlag := flag.String("site-field", cfg.SiteField, "行内站点字段名，如 domain、server_name、host")
	siteNameFlag := flag.String("site-name", cfg.SiteName, "强制所有日志归属该站点（适合单文件）")
	siteFilterFlag := flag.String("site", cfg.SiteFilter, "只输出指定站点，逗号分隔")
	siteIPModeFlag := flag.String("site-ip-mode", cfg.SiteIPMode, "IP 直连处理: separate / inherit / ignore")
	siteIPReportFlag := flag.String("site-ip-report", cfg.SiteIPReport, "IP 直连报告: none / group / per-ip")
	siteStripWWWFlag := flag.Bool("site-strip-www", cfg.SiteStripWWW, "将 www.example.com 归并到 example.com")
	siteMaxFlag := flag.Int("site-max", cfg.SiteMax, "最大站点数，超出归入 (其他站点)")
	siteMinPVFlag := flag.Int64("site-min-pv", cfg.SiteMinPV, "小于该 PV 的站点不生成独立报告")
	siteMaxReportFlag := flag.Int("site-max-report", cfg.SiteMaxReport, "最多生成多少个独立站点报告")
	titleFlag := flag.String("title", cfg.ReportTitle, "报告标题（显示在报告封面/表头）")
	logDirFlag := flag.String("logdir", "", "日志目录（按周/按天整体处理）；也可直接传某个日志文件，此时只处理该文件")
	patternFlag := flag.String("pattern", "", "日志文件名匹配模式（支持 *，.gz 压缩包自动解压）")
	reportDirFlag := flag.String("reportdir", "", "报告输出目录")
	logFileFlag := flag.String("logfile", "", "运行日志文件基准路径（按天轮转成 app-YYYY-MM-DD.log）；默认 <reportdir>/logs/nginx-report.log，off=关闭")
	logKeepFlag := flag.Int("log-keep-days", 0, "运行日志保留天数（默认 30，负数=不清理）")
	debugFlag := flag.Bool("debug", false, "开启 debug 日志")
	flag.Usage = usage
	flag.Parse()

	// 只有显式使用 -site-*（或 -site）才开启站点维度，保证旧命令零开销。
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "site" || strings.HasPrefix(f.Name, "site-") {
			cfg.SiteEnabled = true
		}
	})

	// 命令行参数覆盖配置
	if *patternFlag != "" {
		cfg.LogPattern = *patternFlag
	}
	if *logDirFlag != "" {
		if err := applyLogTarget(cfg, *logDirFlag, *patternFlag != ""); err != nil {
			logger.Error("%v", err)
			logger.CloseLogFile()
			os.Exit(1)
		}
	}
	if *reportDirFlag != "" {
		cfg.ReportDir = *reportDirFlag
		cfg.HistoryDir = filepath.Join(*reportDirFlag, "history")
	}
	if *dailyAtFlag != "" {
		cfg.DailyAt = *dailyAtFlag
	}
	if *weeklyAtFlag != "" {
		cfg.WeeklyAt = *weeklyAtFlag
	}
	if *weeklyDayFlag != 0 {
		cfg.WeeklyDay = *weeklyDayFlag
	}
	if *retryMinFlag != 0 {
		cfg.RetryMin = *retryMinFlag
	}
	if *maxRetryFlag != 0 {
		cfg.MaxRetry = *maxRetryFlag
	}
	if *todayEveryFlag > 0 {
		cfg.TodayEvery = *todayEveryFlag
	}
	cfg.CPUPercent = *cpuPercentFlag
	cfg.URINormalize = *uriNormFlag
	cfg.FromClock = strings.TrimSpace(*fromFlag)
	cfg.StaticExclude = *staticFlag
	cfg.SkipStatic = *skipStaticFlag
	cfg.RealIP = *realIPFlag
	cfg.TimeField = strings.ToLower(strings.TrimSpace(*timeFieldFlag))
	cfg.SlowMS = *slowMSFlag
	cfg.ExcludeBots = *excludeBotsFlag
	cfg.LogFormat = strings.TrimSpace(*logFormatFlag)
	if cfg.LogFormat == "" {
		cfg.LogFormat = "auto"
	}
	cfg.CSVFields = strings.TrimSpace(*csvFieldsFlag)
	cfg.CSVSkipHeader = *csvSkipHeaderFlag
	cfg.JSONFields = strings.TrimSpace(*jsonFieldsFlag)
	cfg.SiteMap = strings.TrimSpace(*siteMapFlag)
	cfg.SiteAliasMap = strings.TrimSpace(*siteAliasFlag)
	cfg.SiteReport = strings.ToLower(strings.TrimSpace(*siteReportFlag))
	cfg.SiteFrom = strings.ToLower(strings.TrimSpace(*siteFromFlag))
	cfg.SiteField = strings.TrimSpace(*siteFieldFlag)
	cfg.SiteName = strings.TrimSpace(*siteNameFlag)
	cfg.SiteFilter = strings.TrimSpace(*siteFilterFlag)
	cfg.SiteIPMode = strings.ToLower(strings.TrimSpace(*siteIPModeFlag))
	cfg.SiteIPReport = strings.ToLower(strings.TrimSpace(*siteIPReportFlag))
	cfg.SiteStripWWW = *siteStripWWWFlag
	cfg.SiteMax = *siteMaxFlag
	cfg.SiteMinPV = *siteMinPVFlag
	cfg.SiteMaxReport = *siteMaxReportFlag
	cfg.MaxMemMB = *maxMemFlag
	cfg.Format = *formatFlag
	cfg.OpsReport = *opsFlag
	cfg.Preview = *previewFlag
	if *topFlag > 0 {
		cfg.TopN = *topFlag
	}
	cfg.ReportTitle = *titleFlag
	if *earlyExitFlag <= 0 {
		cfg.EarlyExitLines = -1 // 0 表示关闭提前退出
	} else {
		cfg.EarlyExitLines = *earlyExitFlag
	}
	if *niceFlag < 0 {
		cfg.NiceLevel = 0
	} else {
		cfg.NiceLevel = *niceFlag
	}

	logger.SetDebug(*debugFlag)

	switch cfg.TimeField {
	case "auto", "upstream", "response":
	default:
		logger.Error("-time-field 无效: %q（可选 auto / upstream / response）", cfg.TimeField)
		logger.CloseLogFile()
		os.Exit(1)
	}
	if err := accesslog.ValidateConfigured(cfg.LogFormat); err != nil {
		logger.Error("-log-format 无效: %v", err)
		logger.CloseLogFile()
		os.Exit(1)
	}
	if err := accesslog.ValidateCSVFields(cfg.CSVFields); err != nil {
		logger.Error("-csv-fields 无效: %v", err)
		logger.CloseLogFile()
		os.Exit(1)
	}
	if err := analyzer.ValidateJSONFields(cfg.JSONFields); err != nil {
		logger.Error("-json-fields 无效: %v", err)
		logger.CloseLogFile()
		os.Exit(1)
	}
	switch cfg.SiteReport {
	case "aggregate", "both", "per":
	default:
		logger.Error("-site-report 无效: %q（可选 aggregate / both / per）", cfg.SiteReport)
		logger.CloseLogFile()
		os.Exit(1)
	}
	switch cfg.SiteFrom {
	case "auto", "field", "map", "filename", "dir":
	default:
		logger.Error("-site-from 无效: %q（可选 auto / field / map / filename / dir）", cfg.SiteFrom)
		logger.CloseLogFile()
		os.Exit(1)
	}
	switch cfg.SiteIPMode {
	case "separate", "inherit", "ignore":
	default:
		logger.Error("-site-ip-mode 无效: %q（可选 separate / inherit / ignore）", cfg.SiteIPMode)
		logger.CloseLogFile()
		os.Exit(1)
	}
	switch cfg.SiteIPReport {
	case "none", "group", "per-ip":
	default:
		logger.Error("-site-ip-report 无效: %q（可选 none / group / per-ip）", cfg.SiteIPReport)
		logger.CloseLogFile()
		os.Exit(1)
	}
	// 站点过滤器只允许 weekly / 单文件，避免 daily/serve 用过滤后数据污染历史。
	if cfg.SiteFilter != "" && *modeFlag != "weekly" {
		logger.Error("-site 只支持 weekly / 单文件模式（daily/serve 会写历史）")
		logger.CloseLogFile()
		os.Exit(1)
	}
	if cfg.SiteEnabled {
		if cfg.SiteName != "" {
			if _, ok := site.Normalize(cfg.SiteName, cfg.SiteStripWWW); !ok {
				logger.Error("-site-name 无效: %q", cfg.SiteName)
				logger.CloseLogFile()
				os.Exit(1)
			}
		}
		if cfg.SiteFilter != "" {
			if _, err := site.ParseFilter(cfg.SiteFilter, cfg.SiteStripWWW, nil); err != nil {
				logger.Error("-site 无效: %v", err)
				logger.CloseLogFile()
				os.Exit(1)
			}
		}
		if cfg.SiteMax <= 0 {
			cfg.SiteMax = 1000
		}
		if cfg.SiteMaxReport <= 0 {
			cfg.SiteMaxReport = 100
		}
		if cfg.SiteMap != "" {
			r := site.NewResolver(cfg.LogDir, cfg.SiteStripWWW)
			if err := r.LoadMap(cfg.SiteMap); err != nil {
				logger.Error("%v", err)
				logger.CloseLogFile()
				os.Exit(1)
			}
		}
		if cfg.SiteAliasMap != "" {
			r := site.NewResolver(cfg.LogDir, cfg.SiteStripWWW)
			if err := r.LoadAliases(cfg.SiteAliasMap); err != nil {
				logger.Error("%v", err)
				logger.CloseLogFile()
				os.Exit(1)
			}
		}
	}
	if cfg.SlowMS <= 0 {
		cfg.SlowMS = 1000
	}

	// -from 必须是 HH:MM，早点报错比跑到一半才发现好
	if cfg.FromClock != "" {
		if _, err := parseClockMinute(cfg.FromClock); err != nil {
			logger.Error("-from 无效: %v", err)
			logger.CloseLogFile()
			os.Exit(1)
		}
	}

	// 运行日志文件：默认 <reportdir>/logs/nginx-report.log，按天轮转保留 30 天
	if *logKeepFlag != 0 {
		cfg.LogKeepDays = *logKeepFlag
	}
	logFile := *logFileFlag
	if logFile == "" {
		logFile = cfg.LogFile
	}
	switch strings.ToLower(logFile) {
	case "off", "none", "false":
		logFile = ""
	case "":
		logFile = filepath.Join(cfg.ReportDir, "logs", "nginx-report.log")
	}
	if logFile != "" {
		if err := logger.SetLogFile(logFile, cfg.LogKeepDays); err != nil {
			logger.Warn("日志文件初始化失败，仅输出到终端: %v", err)
		} else if p := logger.LogFilePath(); p != "" {
			logger.Debug("运行日志: %s（保留 %d 天）", p, cfg.LogKeepDays)
		}
	}

	// 降低进程优先级（Linux/macOS 生效），避免抢占业务 CPU
	if cfg.NiceLevel > 0 {
		if err := sysctl.Nice(cfg.NiceLevel); err != nil {
			logger.Debug("设置进程优先级失败: %v", err)
		} else {
			logger.Debug("进程优先级已设置为 nice %d", cfg.NiceLevel)
		}
	}

	var err error
	// -from 只用于“看一段数据”：daily 会写日记录、serve 会定时写，
	// 用过滤后的数据落盘会污染后续周报，因此限制在 weekly / 单文件模式。
	if (cfg.FromClock != "" || cfg.Preview) && *modeFlag != "weekly" {
		err = fmt.Errorf("-from / -preview 只支持 weekly / 单文件模式（daily/serve 会把过滤后的数据写进历史）；当前模式 %s", *modeFlag)
		logger.Error("%v", err)
		logger.CloseLogFile()
		os.Exit(1)
	}
	switch *modeFlag {
	case "daily":
		logger.Info("开始生成 Nginx 周报（模式: daily）")
		day, derr := resolveDay(*dateFlag)
		if derr != nil {
			err = derr
			break
		}
		err = runDaily(cfg, day)
	case "weekly":
		logger.Info("开始生成 Nginx 周报（模式: weekly）")
		err = runWeekly(cfg, *startFlag, *endFlag)
	case "serve":
		err = runServe(cfg)
	default:
		err = fmt.Errorf("未知模式 %q，可选 daily / weekly / serve", *modeFlag)
		logger.Info("未知模式: %s", *modeFlag)
	}
	if err != nil {
		logger.Error("%v", err)
		logger.CloseLogFile()
		os.Exit(1)
	}
	if *modeFlag != "serve" {
		// 如实报告本次消耗：运维最关心"这个统计任务吃了多少 CPU/内存"
		u := sysinfo.Read()
		logger.Info("完成！本次运行：%s，耗时 %.1fs", u.Human(), time.Since(startTime).Seconds())
	}
	logger.CloseLogFile()
}

// usage 自定义 -h 输出：除了参数说明，再给一组可直接复制的完整示例。
// 很多同学拿到二进制第一件事就是 -h，只列参数不知道从哪下手。
func usage() {
	prog := filepath.Base(os.Args[0])
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, `nginx-report — Nginx 访问日志统计 / 周报 / 运维分析

用法:
  %s -mode <daily|weekly|serve> [参数]

输出文件:
  单个文件模式  single_report_<起>_to_<止>.html（客户/上报）
                single_report_<起>_to_<止>_ops.html / _ops.txt（运维/开发，可缩放图表）
  目录/周报模式 weekly_report_<起>_to_<止>.html + weekly_report_latest.html + *_ops.html / *_ops.txt

完整示例:
  # 1) 单个日志文件（明文或 .gz 都行；gz 自动解压，不需要先 gunzip）
  #    文件正好是一天：统计整个文件，只出报告，不写 history/
  %s -logdir /data/_log/backup/28888_access_json.log-2026-10-30.gz \
      -reportdir /data/reports -top 20

  # 2) 同一个 .gz 里有多天，只统计 2026-10-30
  #    -preview = 只出报告，不读写日记录/周汇总，不会污染正式周报
  %s -mode weekly \
      -logdir /data/_log/backup/28888_access_json.log-2026-10-30.gz \
      -start 2026-10-30 -end 2026-10-30 -preview \
      -reportdir /data/reports -top 20

  # 3) 只要 2026-10-30 的 08:00 之后（含边界）
  %s -mode weekly -logdir ./access.log.gz \
      -start 2026-10-30 -end 2026-10-30 -from 08:00 -preview \
      -reportdir ./reports

  # 4) 看今天正在写入的日志（不写历史）
  %s -mode weekly -logdir /data/_log/28888_access_json.log \
      -start $(date +%%F) -end $(date +%%F) -preview -reportdir ./reports

  # 5) 正式日统计：处理昨天，写 history/daily/report_<日期>.json（供周报聚合）
  %s -mode daily -date yesterday \
      -logdir /data/_log/backup -pattern '*.log*.gz' -reportdir /data/reports

  # 6) 正式周报：汇总上一个完整周，同时出 HTML + PDF + Excel
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*.gz' \
      -reportdir /data/reports -format all

  # 7) 常驻服务：每天 00:10 统计昨天，每周一 08:00 汇总上周
  %s -mode serve \
      -logdir /data/_log/backup -pattern '*.log*.gz' \
      -reportdir /data/reports -daily-at 00:10 -weekly-day 1 -weekly-at 08:00

  # 8) 数据口径更准：CDN 后取 real_client、按 500ms 判慢、排除爬虫/探针、按端到端耗时
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*.gz' \
      -real-ip=true -slow-ms 500 -exclude-bots -time-field response \
      -reportdir /data/reports -top 20

  # 9) nginx 默认 combined 格式：首行自动识别，不需要 -log-format
  %s -mode weekly \
      -logdir /data/_log/backup -pattern 'access.log*' \
      -reportdir /data/reports

  # 10) 自定义 log_format：把 nginx.conf 里的模板原样传入（用单引号防止 shell 展开 $）
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*.gz' \
      -log-format '$time_iso8601 $remote_addr "$request" $status $body_bytes_sent $request_time $upstream_response_time "$http_user_agent"' \
      -reportdir /data/reports

  # 11) 自定义 JSON 字段名（任意键名都可以映射）
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*.gz' \
      -json-fields 'ts=time,cip=client,u=uri,st=status,sz=size,rt=response_time' \
      -reportdir /data/reports

  # 12) 多站点目录：一个文件一个站点，用映射文件归属并输出每站点报告
  #     映射格式：example.com = example.com_*.log（相对 -logdir，支持通配符）
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*' \
      -site-map ./sites.conf -site-report both -site-ip-report per-ip \
      -reportdir /data/reports

  # 13) JSON 行内自带 domain / host：自动识别，IP 直连单独归类
  #     默认 combined 没有 host 字段，不会用 URI/Referer 猜测域名
  %s -mode weekly \
      -logdir /data/_log/backup -pattern '*.log*' \
      -site-from auto -site-report both -site-ip-report group \
      -reportdir /data/reports

  # 14) 一个域名一个目录：/data/_log/<domain>/access.log
  #     站点模式下 pattern 不含 / 会递归扫描子目录，目录名作为站点
  %s -mode weekly \
      -logdir /data/_log -pattern '*.log*' \
      -site-from dir -site-report per -site-ip-report group \
      -reportdir /data/reports

关键参数速查:
  -logdir      日志目录，或直接给一个日志文件（.gz 自动解压）
  -pattern     目录模式下匹配文件名（默认 28888_access_json.log*）
  -mode        daily / weekly / serve（默认 weekly）
  -date        daily 的日期：YYYY-MM-DD / today / yesterday
  -start/-end  weekly 的日期区间；单文件里只看某天时也用它
  -from        只统计每天 HH:MM 之后（如 08:00，对应原脚本 START_TIME）
  -preview     只出报告，不读写历史（复盘 gz / 临时看一天时用）
  -skip-static 解析时排除 css/js/图片/字体（默认 true）
  -log-format  输入日志格式：auto（默认）/ json / combined / combined_host / csv / nginx log_format 模板
  -csv-fields  CSV 无表头时的列顺序，如 time,client,method,uri,status,size
  -csv-skip-header CSV 首行是表头但列名无法识别时，配合 -log-format csv -csv-fields 强制跳过
  -json-fields 自定义 JSON 字段映射，如 ts=time,cip=client,u=uri,st=status
  -real-ip     优先用 real_client 作为客户端 IP（默认 true）
  -time-field  响应时间口径：auto（上游优先）/ upstream / response
  -slow-ms     慢请求阈值，单位毫秒（默认 1000）
  -exclude-bots 按 UA 排除爬虫/探针/监控（默认 false）
  -top         运维报告 Top 榜条数（默认 20）
  -format      报告输出格式：html（默认）/ pdf / xlsx / all
  -ops         是否输出运维报告（默认 true）

  -site-map    站点映射文件，如 example.com = example.com_*.log
  -site-alias-map 站点别名映射，如 www.example.com = example.com
  -site-report 站点报告范围：aggregate / both（默认）/ per
  -site-from   站点来源：auto（默认）/ field / map / filename / dir
  -site-field  行内站点字段名，如 domain / server_name / host
  -site-name   强制所有日志归属一个站点（适合单文件/单站点目录）
  -site        只输出指定站点，逗号分隔（仅 weekly / 单文件）
  -site-ip-mode IP 直连处理：separate（默认）/ inherit / ignore
  -site-ip-report IP 直连报告：none / group / per-ip
  -site-strip-www 将 www.example.com 归并到 example.com
  -site-max    最大站点数，超出归入 (其他站点)（默认 1000）
  -site-min-pv 小于该 PV 的站点不生成独立报告
  -site-max-report 最多生成多少个独立站点报告（默认 100）

参数说明:
`, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog)
	flag.PrintDefaults()
}

// serveDailyKey / serveWeeklyKey 用于把任务键拆成参数
const keySep = "|"

// runServe 常驻模式：内置调度器每天处理昨天、每周汇总上周，失败自动重试。
// 重启后会根据「日记录 / 周报是否已存在」自动补跑漏掉的任务。
func runServe(cfg *config.Config) error {
	dailyClock, err := scheduler.ParseClock(cfg.DailyAt)
	if err != nil {
		return fmt.Errorf("-daily-at 无效: %v", err)
	}
	weeklyClock, err := scheduler.ParseClock(cfg.WeeklyAt)
	if err != nil {
		return fmt.Errorf("-weekly-at 无效: %v", err)
	}
	if cfg.WeeklyDay < 1 || cfg.WeeklyDay > 7 {
		return fmt.Errorf("-weekly-day 应为 1-7（1=周一），当前 %d", cfg.WeeklyDay)
	}
	retryMin := cfg.RetryMin
	if retryMin <= 0 {
		retryMin = int(scheduler.DefaultRetry / time.Minute)
	}
	retry := time.Duration(retryMin) * time.Minute

	// 判断"这一周的周报是否已经出过"：要按实际输出格式找文件，
	// 否则 -format pdf 时永远找不到 html，重启后会白跑一遍。
	weeklyReportDone := func(start, end string) bool {
		base := fmt.Sprintf("weekly_report_%s_to_%s", start, end)
		if cfg.SiteEnabled && cfg.SiteReport == "per" {
			// per 模式没有全站报告，用任意一个站点目录里的本期报告判断。
			found := false
			root := filepath.Join(cfg.ReportDir, "sites")
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d == nil || d.IsDir() {
					return nil
				}
				for _, f := range report.Formats(cfg.Format) {
					if d.Name() == base+"."+f {
						found = true
						return fs.SkipAll
					}
				}
				return nil
			})
			return found
		}
		for _, f := range report.Formats(cfg.Format) {
			if _, err := os.Stat(filepath.Join(cfg.ReportDir, base+"."+f)); err == nil {
				return true
			}
		}
		return false
	}

	jobs := []scheduler.Job{
		{
			Name: "daily",
			Due: func(now time.Time) (string, time.Time) {
				// 每天 dailyAt 处理「昨天」；启动晚于该时刻也会补跑
				target := now.AddDate(0, 0, -1).Format("2006-01-02")
				return target, scheduler.AtClock(now, dailyClock)
			},
			Done: func(day string) bool {
				// partial（当天）记录不算完成，当天结束后会被重算覆盖
				return history.IsDailyFinal(cfg.HistoryDir, day)
			},
			Run: func(day string) error {
				logger.Info("—— 定时任务 daily: %s ——", day)
				return runDaily(cfg, day)
			},
			Retry:       retry,
			MaxAttempts: cfg.MaxRetry,
		},
		{
			Name: "weekly",
			Due: func(now time.Time) (string, time.Time) {
				start, end := lastWeekWindow(now)
				endDay, perr := time.ParseInLocation("2006-01-02", end, now.Location())
				if perr != nil {
					return "", time.Time{}
				}
				// 计划时刻：该周结束后的周一 + (weekday-1) 天 + 周汇总时间
				base := endDay.AddDate(0, 0, 1).AddDate(0, 0, cfg.WeeklyDay-1)
				return start + keySep + end, scheduler.AtClock(base, weeklyClock)
			},
			Done: func(key string) bool {
				parts := strings.SplitN(key, keySep, 2)
				if len(parts) != 2 {
					return false
				}
				return weeklyReportDone(parts[0], parts[1])
			},
			Run: func(key string) error {
				parts := strings.SplitN(key, keySep, 2)
				if len(parts) != 2 {
					return fmt.Errorf("任务键格式错误: %s", key)
				}
				logger.Info("—— 定时任务 weekly: %s ~ %s ——", parts[0], parts[1])
				return runWeekly(cfg, parts[0], parts[1])
			},
			// 周任务失败（如日志已清理且无日记录）间隔长一些，降低打扰
			Retry:       retry * 6,
			MaxAttempts: cfg.MaxRetry,
		},
	}

	// 可选：定时刷新「今天」的部分统计（日志仍在写入），方便随时看当天情况
	if cfg.TodayEvery > 0 {
		every := time.Duration(cfg.TodayEvery) * time.Minute
		jobs = append(jobs, scheduler.Job{
			Name: "today",
			Due: func(now time.Time) (string, time.Time) {
				// 以时间桶为任务键：每 every 分钟执行一次
				bucket := now.Unix() / int64(every/time.Second)
				at := time.Unix(bucket*int64(every/time.Second), 0)
				return now.Format("2006-01-02") + keySep + strconv.FormatInt(bucket, 10), at
			},
			Run: func(key string) error {
				day := strings.SplitN(key, keySep, 2)[0]
				if err := runDaily(cfg, day); err != nil {
					// 当天还没日志/还没数据属正常，下个周期再试
					logger.Warn("当天统计跳过（%s）: %v", day, err)
				}
				return nil
			},
		})
	}

	if cfg.MaxMemMB > 0 {
		// 软上限：Go 会在此之上更积极地 GC，避免统计任务把服务器内存吃满。
		// 我们的存活数据只有几十 MB，正常情况下根本碰不到这个限制。
		debug.SetMemoryLimit(int64(cfg.MaxMemMB) << 20)
		logger.Debug("堆内存软上限: %d MB", cfg.MaxMemMB)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched := &scheduler.Scheduler{
		Jobs: jobs,
		OnResult: func(name, key string, err error) {
			if err != nil {
				logger.Error("定时任务 %s 执行失败（%s）: %v；将于 %d 分钟后重试",
					name, key, err, retryMin)
				return
			}
			// 常驻模式下每个任务也报告资源消耗：运维据此判断要不要调低 -cpu-percent
			u := sysinfo.Read()
			logger.Info("定时任务 %s 完成（%s），累计消耗 %s", name, key, u.Human())
		},
		OnGiveUp: func(name, key string, attempts int) {
			logger.Error("定时任务 %s 已尝试 %d 次仍失败（%s），本周期放弃，等下一个周期再试",
				name, attempts, key)
		},
	}

	if cfg.SingleFile != "" {
		logger.Warn("-logdir 指向单个文件 %s：常驻模式只会反复统计这一个文件，"+
			"生产环境请传日志目录（如 -logdir /data/_log/backup）", cfg.SingleFile)
	}
	logger.Info("常驻模式已启动：每天 %s 处理昨天；每周%s %s 汇总上周；失败重试间隔 %d 分钟",
		scheduler.FormatClock(dailyClock), weekdayCN(cfg.WeeklyDay),
		scheduler.FormatClock(weeklyClock), retryMin)
	if cfg.TodayEvery > 0 {
		logger.Info("另有任务：每 %d 分钟刷新一次「今天」的部分统计（用于实时查看）", cfg.TodayEvery)
	}
	logger.Info("日志目录 %s（模式 %s），报告目录 %s（Ctrl+C 退出）", cfg.LogDir, cfg.LogPattern, cfg.ReportDir)
	if p := logger.LogFilePath(); p != "" {
		logger.Info("运行日志文件: %s（保留 %d 天，按天轮转）", p, cfg.LogKeepDays)
	}

	if err := sched.Run(ctx); err != nil {
		return err
	}
	logger.Info("常驻模式已退出")
	return nil
}

// weekdayCN 1=周一 ... 7=周日
func weekdayCN(day int) string {
	names := []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	if day < 1 || day > 7 {
		return fmt.Sprintf("周?%d", day)
	}
	return names[day-1]
}

// resolveDay 解析 -date：空或 yesterday = 昨天，today = 今天，或 YYYY-MM-DD
func resolveDay(s string) (string, error) {
	now := time.Now()
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "yesterday":
		return now.AddDate(0, 0, -1).Format("2006-01-02"), nil
	case "today":
		return now.Format("2006-01-02"), nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return "", fmt.Errorf("日期格式错误 %q，应为 YYYY-MM-DD / today / yesterday", s)
	}
	return s, nil
}

// applyLogTarget 处理 -logdir：既可以是日志目录（按周/按天整体处理），
// 也可以直接传某个日志文件（只处理这一个文件，统计它包含的全部数据）。
func applyLogTarget(cfg *config.Config, target string, patternSet bool) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("路径无效 %q: %v", target, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		// 不直接失败：周报只聚合日记录时并不需要日志。
		// 先按目录处理，具体任务在真正需要读日志时再报错。
		cfg.LogDir = abs
		logger.Warn("日志路径不存在: %s；若是笔误请检查（周报聚合日记录时不需要日志）", abs)
		return nil
	}
	if fi.IsDir() {
		cfg.LogDir = abs
		if !patternSet {
			logger.Debug("日志目录 %s，匹配模式 %s", cfg.LogDir, cfg.LogPattern)
		}
		return nil
	}
	// 具体文件：只处理这个文件，不去匹配同目录的其他文件
	cfg.LogDir = filepath.Dir(abs)
	cfg.SingleFile = abs
	logger.Info("单个日志文件: %s（只处理该文件，不匹配同目录其他文件）", abs)
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".gz":
		logger.Debug("检测到 gz 压缩包，将自动解压")
	}
	return nil
}

// lastWeekWindow 返回上一个完整周（周一~周日）的日期范围。
// 做法：先回溯到“严格早于今天的最近一个周日”作为结束日，再倒推 6 天作为周一起始。
func lastWeekWindow(now time.Time) (string, string) {
	t := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	back := int(t.Weekday()) // 距上一个周日的天数，周日（0）需回溯整周
	if back == 0 {
		back = 7
	}
	end := t.AddDate(0, 0, -back)
	start := end.AddDate(0, 0, -6)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// dateRange 返回 [start, end] 内的所有日期
func dateRange(start, end string) []string {
	s, err1 := time.Parse("2006-01-02", start)
	e, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil {
		return nil
	}
	var out []string
	for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

// logFilesTree 在 root 下递归查找文件名匹配 pattern 的普通文件。
// 只在站点模式且 pattern 不含路径分隔符时使用，保持旧目录模式只扫顶层。
func logFilesTree(root, pattern string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// 单个站点目录无权限时继续扫其他目录，但保留可排查的日志。
			logger.Debug("跳过无法访问的日志路径 %s: %v", path, err)
			return nil
		}
		if d == nil || d.IsDir() {
			return nil
		}
		ok, merr := filepath.Match(pattern, d.Name())
		if merr != nil {
			return merr
		}
		if ok {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// findLogFiles 按文件名日期挑出候选日志文件。
//
// 文件名日期窗口在 [start, end] 基础上放宽 1 天：不同轮转方案的命名既可能用
// 「数据当天」也可能用「轮转当天」，真正的过滤交给行内时间戳（ProcessFileRange）。
//
// 关于无日期文件（如未切割的 app.log、轮转序号 app.log.1.gz）：
//   - 默认都会读，因为数据可能还在里面（比如 00:00 还没切割就来统计昨天）；
//   - 只有当「存在一个文件名日期正好等于 start 的文件」时才跳过无日期文件，
//     避免 copytruncate 式轮转把同一天的日志在两个文件里重复计入。
func findLogFiles(cfg *config.Config, start, end string) (files []string, fallback bool, err error) {
	// 指定了具体文件：只处理它，不做任何同目录匹配
	if cfg.SingleFile != "" {
		fi, serr := os.Stat(cfg.SingleFile)
		if serr != nil {
			return nil, false, fmt.Errorf("日志文件不可用 %s: %v", cfg.SingleFile, serr)
		}
		if !fi.Mode().IsRegular() {
			return nil, false, fmt.Errorf("不是普通文件: %s", cfg.SingleFile)
		}
		return []string{cfg.SingleFile}, false, nil
	}

	pattern := filepath.Join(cfg.LogDir, cfg.LogPattern)
	var all []string
	var globErr error
	if cfg.SiteEnabled && !strings.ContainsAny(cfg.LogPattern, `/\`) {
		// 多站点常见布局：/data/_log/<domain>/access.log。
		// 站点模式下没有路径分隔符的模式按文件名递归匹配，避免用户为每个域名
		// 单独跑一次，也避免把几十个站点目录漏掉。
		all, globErr = logFilesTree(cfg.LogDir, cfg.LogPattern)
	} else {
		all, globErr = filepath.Glob(pattern)
	}
	if globErr != nil {
		return nil, false, globErr
	}
	if len(all) == 0 {
		return nil, false, fmt.Errorf("没有找到日志文件: %s", pattern)
	}

	limitEnd := end
	if limitEnd != "" {
		if e, perr := time.Parse("2006-01-02", limitEnd); perr == nil {
			limitEnd = e.AddDate(0, 0, 1).Format("2006-01-02")
		}
	}

	var dated, undated []string
	regular := make([]string, 0, len(all))
	hasDayFile := false
	for _, f := range all {
		// 只接受普通文件：目录/管道/设备被模式意外命中时不要当作日志读
		if fi, serr := os.Stat(f); serr != nil || !fi.Mode().IsRegular() {
			logger.Debug("跳过非普通文件: %s", f)
			continue
		}
		regular = append(regular, f)
		day := analyzer.DateInFilename(filepath.Base(f))
		if day == "" {
			undated = append(undated, f)
			continue
		}
		if start != "" && day == start {
			hasDayFile = true
		}
		if start != "" && day < start {
			continue
		}
		if limitEnd != "" && day > limitEnd {
			continue
		}
		dated = append(dated, f)
	}

	if len(dated) > 0 {
		// 站点模式保留无日期文件：多站点目录里经常同时存在
		// example.com_2026-10-30.log 和 api_access.log；后者需要靠 -site-map
		// 或行内 domain 字段归属，不能因为存在带日期文件就被跳过。
		if !hasDayFile || cfg.SiteEnabled {
			dated = append(dated, undated...)
		}
		return dated, false, nil
	}
	if len(regular) == 0 {
		return nil, false, fmt.Errorf("没有找到可读的普通日志文件: %s", pattern)
	}
	// 找不到带日期的文件（轮转未执行 / 命名不同），回退扫描全部普通文件，靠内容时间过滤
	return regular, true, nil
}

// runDaily 只处理头一天（或 -date 指定的一天），落日记录
func runDaily(cfg *config.Config, day string) error {
	if day == "" {
		day = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return fmt.Errorf("日期格式错误 %q，应为 YYYY-MM-DD", day)
	}
	// 统计"今天"时日志仍在写入，标记为 partial，避免影响周报
	partial := day >= time.Now().Format("2006-01-02")
	if partial {
		logger.Info("日统计: %s（当天，正在写入 → 部分统计）", day)
	} else {
		logger.Info("日统计: %s", day)
	}

	files, fallback, err := findLogFiles(cfg, day, day)
	if err != nil {
		return err
	}
	if fallback {
		if len(files) > 1 {
			logger.Warn("未找到带日期的日志文件，回退扫描全部 %d 个匹配文件（按内容时间过滤）", len(files))
		} else {
			logger.Debug("按内容时间过滤单个日志文件")
		}
	}
	logger.Info("待处理 %d 个日志文件", len(files))

	var siteResolver *site.Resolver
	if cfg.SiteEnabled {
		siteResolver, err = newSiteResolver(cfg)
		if err != nil {
			return err
		}
	}

	grand := analyzer.NewStats()
	for i, fpath := range files {
		fname := filepath.Base(fpath)
		logger.Info("[%d/%d] 处理 %s ...", i+1, len(files), fname)

		t0 := time.Now()
		stats, err := analyzer.ProcessFileWithOptions(fpath, processOptionsForFile(cfg, siteResolver, fpath, day, day))
		elapsed := time.Since(t0)
		if stats == nil {
			logger.Error("处理失败 %s: %v", fname, err)
			continue
		}
		if elapsed > time.Second {
			// 带上 MB/s：运维能直接判断"是磁盘慢还是 CPU 慢"
			if size := fileSize(fpath); size > 0 {
				logger.Info("%s 解析耗时 %.1fs（%.0f MB/s，%d 行）",
					fname, elapsed.Seconds(), float64(size)/elapsed.Seconds()/1024/1024, stats.Total)
			} else {
				logger.Info("%s 解析耗时 %.1fs（%d 行）", fname, elapsed.Seconds(), stats.Total)
			}
		}
		if err != nil {
			logger.Warn("%s 读取中断（已保留 %d 条）: %v", fname, stats.Total, err)
		}
		logger.Debug("%s: format=%s total=%d skipped=%d out_of_range=%d",
			fname, stats.Format, stats.Total, stats.Skipped, stats.OutOfRange)

		grand.Merge(stats)

		if i < len(files)-1 && cfg.SleepSec > 0 {
			time.Sleep(time.Duration(cfg.SleepSec * float64(time.Second)))
		}
	}
	if grand.Format == "mixed" {
		logger.Warn("检测到多种日志格式混合；请确认是否同一批日志")
	} else if grand.Format != "" {
		logger.Info("识别日志格式: %s", grand.Format)
	}
	logFormatWarning(grand)

	agg := grand.DayAgg(day)
	if agg == nil {
		agg = &grand.Agg
	}
	if agg.Total == 0 {
		return noDataError(day, day, grand)
	}

	daily := newDailyWithSites(day, agg, partial, grand.Sites, grand, true, nil)
	if err := history.SaveDaily(cfg.HistoryDir, day, daily); err != nil {
		return fmt.Errorf("保存日记录失败: %v", err)
	}

	logDailySummary(day, agg)
	reportIssues(agg, cfg)

	if err := history.Cleanup(cfg.HistoryDir, cfg.KeepDays); err != nil {
		logger.Debug("清理历史失败: %v", err)
	}
	tag := ""
	if partial {
		tag = "（部分数据，当天结束后会自动重算覆盖）"
	}
	logger.Info("日记录已保存: %s%s", filepath.Join(history.DailyDir(cfg.HistoryDir),
		fmt.Sprintf("report_%s.json", day)), tag)
	return nil
}

// runWeekly 汇总一个周期（默认上一个完整周）的日记录；缺失的日回退读日志并补写日记录
func runWeekly(cfg *config.Config, start, end string) error {
	// 指定了具体日志文件且没显式给区间：整份文件当"一个周期"处理
	if cfg.SingleFile != "" && start == "" && end == "" {
		return runWeeklySingleFile(cfg)
	}
	if (start == "") != (end == "") {
		return fmt.Errorf("-start 与 -end 必须同时指定（当前 start=%q end=%q）", start, end)
	}
	if start == "" {
		start, end = lastWeekWindow(time.Now())
	}
	windowDays := dateRange(start, end)
	if len(windowDays) == 0 {
		return fmt.Errorf("日期范围无效: %s ~ %s", start, end)
	}
	logger.Info("统计周期: %s ~ %s（%d 天）", start, end, len(windowDays))

	// 指定了 -from（每天 HH:MM 之后）时不能复用整天口径的日记录，
	// 也不把过滤后的结果写回历史，否则第二天的周报会跟着“缺一段”。
	clockFiltered := cfg.FromClock != ""
	forceLogs := clockFiltered || cfg.Preview
	if clockFiltered {
		logger.Info("时间过滤: 每天 %s 之后，将直接读日志重算", cfg.FromClock)
	}
	if cfg.Preview {
		logger.Info("预览模式: 直接读日志重算，不读写日记录 / 周汇总")
	}

	var siteResolver *site.Resolver
	if cfg.SiteEnabled {
		var serr error
		siteResolver, serr = newSiteResolver(cfg)
		if serr != nil {
			return serr
		}
	}

	grand := analyzer.NewStats()
	var missing []string
	partialRecs := make(map[string]*history.Daily)
	for _, day := range windowDays {
		if forceLogs {
			missing = append(missing, day)
			continue
		}
		d, err := history.LoadDaily(cfg.HistoryDir, day)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				logger.Warn("日记录 %s 读取失败（将用日志重算）: %v", day, err)
			}
			missing = append(missing, day)
			continue
		}
		if d.Partial {
			// 当天统计的部分数据：先尝试用日志重算，重算不出来时才退回使用
			partialRecs[day] = d
			missing = append(missing, day)
			logger.Debug("日记录 %s 为部分数据，将尝试重算", day)
			continue
		}
		if cfg.SiteEnabled && len(d.Sites) == 0 && d.Total > 0 {
			// 旧版全站日记录没有站点拆分，回退日志重算站点维度。
			logger.Debug("日记录 %s 无站点明细，将用日志重算站点", day)
			missing = append(missing, day)
			continue
		}
		grand.MergeDay(day, d.ToAgg())
		grand.StaticSkipped += d.StaticSkipped
		grand.StaticBytes += d.StaticBytes
		grand.BotSkipped += d.BotSkipped
		if cfg.SiteEnabled {
			for key, sd := range d.Sites {
				grand.MergeSiteDay(key, day, sd.ToAgg(), sd.SiteKind, sd.SiteName,
					sd.StaticSkipped, sd.StaticBytes, sd.BotSkipped, 0, 0,
					sd.SiteConflicts, sd.SiteInvalid, sd.SiteMultiDomain, sd.SiteSourceCnt)
			}
		}
		logger.Debug("载入日记录 %s: total=%d", day, d.Total)
	}
	if len(missing) > 0 && !forceLogs {
		logger.Warn("缺少 %d 天的日记录: %v", len(missing), missing)
	}

	// 缺失的天回退读日志并补写日记录（避免下次重算）。
	// 一次扫描覆盖「最早缺失日 ~ 最晚缺失日」，而不是逐天重复读同一批日志。
	if len(missing) > 0 {
		fallbackStart, fallbackEnd := missing[0], missing[len(missing)-1]
		files, fallback, ferr := findLogFiles(cfg, fallbackStart, fallbackEnd)
		if ferr != nil {
			logger.Warn("回退读取日志失败，缺失日将被跳过: %v", ferr)
		} else {
			if fallback && len(files) > 1 {
				logger.Warn("未找到带日期的日志文件，回退扫描全部 %d 个匹配文件（%s ~ %s）",
					len(files), fallbackStart, fallbackEnd)
			}
			logger.Info("回退读取日志 %s ~ %s，共 %d 个文件", fallbackStart, fallbackEnd, len(files))

			dayStats := analyzer.NewStats()
			formats := make(map[string]bool)
			for i, fpath := range files {
				stats, perr := analyzer.ProcessFileWithOptions(fpath,
					processOptionsForFile(cfg, siteResolver, fpath, fallbackStart, fallbackEnd))
				if stats == nil {
					logger.Error("处理失败 %s: %v", filepath.Base(fpath), perr)
					continue
				}
				if perr != nil {
					logger.Warn("%s 读取中断（已保留 %d 条）: %v", filepath.Base(fpath), stats.Total, perr)
				}
				if stats.Format != "" {
					formats[stats.Format] = true
					logger.Debug("%s: 日志格式 %s", filepath.Base(fpath), stats.Format)
				}
				dayStats.Merge(stats)
				if i < len(files)-1 && cfg.SleepSec > 0 {
					time.Sleep(time.Duration(cfg.SleepSec * float64(time.Second)))
				}
			}
			if len(formats) == 1 {
				for f := range formats {
					logger.Info("识别日志格式: %s", f)
				}
			} else if len(formats) > 1 {
				names := make([]string, 0, len(formats))
				for f := range formats {
					names = append(names, f)
				}
				sort.Strings(names)
				logger.Warn("检测到多种日志格式混合: %s", strings.Join(names, ", "))
			}

			rootMetaSaved := false
			dailySiteMeta := make(map[string]bool)
			grandSiteMeta := make(map[string]bool)
			for _, day := range missing {
				// 静态/爬虫排除量属于“原始行数”，即使当天没有业务请求也要计入；
				// 否则「某天只有静态资源」时周报的排除总数会少算。
				grand.StaticSkipped += dayStat(dayStats.StaticSkippedDay, day, 0)
				grand.StaticBytes += dayStat(dayStats.StaticBytesDay, day, 0)
				grand.BotSkipped += dayStat(dayStats.BotSkippedDay, day, 0)
				agg := dayStats.DayAgg(day)
				if agg == nil || agg.Total == 0 {
					logger.Warn("%s 没有日志数据", day)
					continue
				}
				grand.MergeDay(day, agg)
				if cfg.SiteEnabled {
					for key, ss := range dayStats.Sites {
						sa := ss.Days[day]
						if sa == nil || sa.Total == 0 {
							continue
						}
						staticSkipped := dayStat(ss.StaticSkippedDay, day, ss.StaticSkipped)
						staticBytes := dayStat(ss.StaticBytesDay, day, ss.StaticBytes)
						botSkipped := dayStat(ss.BotSkippedDay, day, ss.BotSkipped)
						var outOfRange, skipped, conflicts, invalid, multi int64
						var sources map[string]int64
						if !grandSiteMeta[key] {
							// 这些计数是整段回退扫描的总量，不能每补一天都加一遍。
							outOfRange, skipped = ss.OutOfRange, ss.Skipped
							conflicts, invalid, multi = ss.Conflicts, ss.Invalid, ss.MultiDomainLines
							sources = ss.SourceCnt
							grandSiteMeta[key] = true
						}
						grand.MergeSiteDay(key, day, sa, string(ss.Kind), ss.Name,
							staticSkipped, staticBytes, botSkipped, outOfRange, skipped,
							conflicts, invalid, multi, sources)
					}
				}
				if forceLogs {
					continue
				}
				siteMeta := make(map[string]bool)
				for key, ss := range dayStats.Sites {
					if ss.Days[day] != nil && ss.Days[day].Total > 0 && !dailySiteMeta[key] {
						siteMeta[key] = true
					}
				}
				if err := history.SaveDaily(cfg.HistoryDir, day,
					newDailyWithSites(day, agg, false, dayStats.Sites, dayStats, !rootMetaSaved, siteMeta)); err != nil {
					logger.Warn("补写日记录失败 %s: %v", day, err)
				} else {
					rootMetaSaved = true
					for key := range siteMeta {
						dailySiteMeta[key] = true
					}
					logger.Info("已补写日记录: %s", day)
				}
			}
		}
	}

	// 重算后仍无数据，但存在当时的 partial 记录时，退回使用并明确告警
	for _, day := range missing {
		if a := grand.DayAgg(day); a != nil && a.Total > 0 {
			continue
		}
		d, ok := partialRecs[day]
		if !ok {
			continue
		}
		grand.MergeDay(day, d.ToAgg())
		grand.StaticSkipped += d.StaticSkipped
		grand.StaticBytes += d.StaticBytes
		grand.BotSkipped += d.BotSkipped
		if cfg.SiteEnabled {
			for key, sd := range d.Sites {
				grand.MergeSiteDay(key, day, sd.ToAgg(), sd.SiteKind, sd.SiteName,
					sd.StaticSkipped, sd.StaticBytes, sd.BotSkipped, 0, 0,
					sd.SiteConflicts, sd.SiteInvalid, sd.SiteMultiDomain, sd.SiteSourceCnt)
			}
		}
		logger.Warn("%s 仅有当天的不完整统计（%d 条，partial），周报中该日数据可能偏少", day, d.Total)
	}

	return finishWeekly(cfg, grand, start, end, windowDays, !forceLogs)
}

// runWeeklySingleFile 把一份具体日志文件当作一个统计周期：
// 不套「上一个完整周」窗口，统计文件里的全部数据，周期取文件内实际首末日期。
// 只出报告，不写日记录/周汇总（避免用"随手给的某个日志文件"覆盖正式历史数据）。
func runWeeklySingleFile(cfg *config.Config) error {
	fpath := cfg.SingleFile
	logger.Info("单文件模式: %s", fpath)

	var siteResolver *site.Resolver
	if cfg.SiteEnabled {
		var err error
		siteResolver, err = newSiteResolver(cfg)
		if err != nil {
			return err
		}
	}
	t0 := time.Now()
	stats, err := analyzer.ProcessFileWithOptions(fpath, processOptionsForFile(cfg, siteResolver, fpath, "", ""))
	elapsed := time.Since(t0)
	if stats == nil {
		return fmt.Errorf("处理失败 %s: %v", filepath.Base(fpath), err)
	}
	if err != nil {
		logger.Warn("%s 读取中断（已保留 %d 条）: %v", filepath.Base(fpath), stats.Total, err)
	}
	if stats.Total == 0 {
		return noDataError("", "", stats)
	}

	days := make([]string, 0, len(stats.Days))
	for day, agg := range stats.Days {
		if agg.Total > 0 {
			days = append(days, day)
		}
	}
	if len(days) == 0 {
		return noDataError("", "", stats)
	}
	sort.Strings(days)
	start, end := days[0], days[len(days)-1]

	peakAt := func() string {
		if t, c := stats.Peak(); c > 0 {
			return fmt.Sprintf("%d req/s (%s)", c, t.Format("01-02 15:04:05"))
		}
		return "-"
	}()
	logger.Info("统计周期: %s ~ %s（%d 天，单文件）", start, end, len(days))
	logger.Info("%s: 总请求 %d，峰值 %s，流量 %s，平均响应 %.0fms，错误 %d，慢请求 %d",
		filepath.Base(fpath), stats.Total, peakAt, humanBytes(float64(stats.SizeTotal)),
		stats.AvgTime()*1000, stats.ErrorCount, stats.SlowCount)
	if stats.Format != "" {
		logger.Info("识别日志格式: %s", stats.Format)
	}
	if size := fileSize(fpath); size > 0 {
		logger.Info("解析耗时 %.1fs（%.0f MB/s，%d 行，CPU 目标占用 %d%%）",
			elapsed.Seconds(), float64(size)/elapsed.Seconds()/1024/1024, stats.Total, cfg.CPUPercent)
	} else {
		logger.Info("解析耗时 %.1fs（%d 行，CPU 目标占用 %d%%）", elapsed.Seconds(), stats.Total, cfg.CPUPercent)
	}

	return finishWeekly(cfg, stats, start, end, days, false)
}

// siteRowList 汇总全站报告里的站点行。默认按 PV 降序。
func siteRowList(cfg *config.Config, grand *analyzer.FileStats) []report.SiteRow {
	if grand == nil || len(grand.Sites) == 0 {
		return nil
	}
	filter := map[string]bool(nil)
	if cfg.SiteFilter != "" {
		aliases := map[string]string(nil)
		if cfg.SiteAliasMap != "" {
			if r := site.NewResolver(cfg.LogDir, cfg.SiteStripWWW); r.LoadAliases(cfg.SiteAliasMap) == nil {
				aliases = r.Aliases
			}
		}
		if f, err := site.ParseFilter(cfg.SiteFilter, cfg.SiteStripWWW, aliases); err == nil {
			filter = f
		}
	}
	rows := make([]report.SiteRow, 0, len(grand.Sites))
	for key, ss := range grand.Sites {
		if ss == nil || ss.Agg.Total == 0 {
			continue
		}
		if filter != nil && !filter[key] {
			continue
		}
		rows = append(rows, siteRowFrom(key, ss.Name, string(ss.Kind), &ss.Agg, ss))
	}
	return report.SiteRows(rows)
}

// siteRowFrom 把站点聚合包装成报告行。
func siteRowFrom(key, name, kind string, agg *analyzer.Agg, ss *analyzer.SiteStats) report.SiteRow {
	row := report.SiteRow{
		Key: key, Name: name, Kind: kind,
		Total: agg.Total, Size: agg.SizeTotal,
		ErrorCount: agg.ErrorCount, SlowCount: agg.SlowCount,
		AvgMs: agg.AvgTime() * 1000, P95: agg.Percentile(0.95) * 1000,
		P99: agg.Percentile(0.99) * 1000,
	}
	if ss != nil {
		row.StaticSkip = ss.StaticSkipped
		row.BotSkip = ss.BotSkipped
		row.Sources = ss.SourceCnt
	}
	if agg.Total > 0 {
		row.ErrorRate = float64(agg.ErrorCount) / float64(agg.Total) * 100
		row.SlowRate = float64(agg.SlowCount) / float64(agg.Total) * 100
	}
	return row
}

// mergeSitesByKind 合并某一类站点，用于 -site-ip-report group。
func mergeSitesByKind(grand *analyzer.FileStats, kind site.Kind, key, name string) *analyzer.SiteStats {
	if grand == nil || len(grand.Sites) == 0 {
		return nil
	}
	var out *analyzer.SiteStats
	for _, ss := range grand.Sites {
		if ss == nil || ss.Kind != kind || ss.Agg.Total == 0 {
			continue
		}
		if out == nil {
			out = &analyzer.SiteStats{
				Site: key, Name: name, Kind: kind,
				Days: make(map[string]*analyzer.Agg), SourceCnt: make(map[string]int64),
			}
			out.Agg = *analyzer.NewAgg()
		}
		out.Agg.MergeFrom(&ss.Agg)
		for day, a := range ss.Days {
			if a == nil {
				continue
			}
			d := out.Days[day]
			if d == nil {
				d = analyzer.NewAgg()
				out.Days[day] = d
			}
			d.MergeFrom(a)
		}
		out.StaticSkipped += ss.StaticSkipped
		out.StaticBytes += ss.StaticBytes
		out.BotSkipped += ss.BotSkipped
		for src, n := range ss.SourceCnt {
			out.SourceCnt[src] += n
		}
	}
	return out
}

// writeReportBundle 渲染并写出一个报告集合（客户版 + Ops 版）。
func writeReportBundle(cfg *config.Config, in report.Input, dir, base, latestBase string) ([]string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建报告目录失败: %v", err)
	}
	var written []string
	for _, f := range report.Formats(cfg.Format) {
		tRender := time.Now()
		var (
			data []byte
			err  error
			ext  = "." + f
		)
		switch f {
		case report.FormatHTML:
			data = []byte(report.HTML(in))
		case report.FormatPDF:
			data, err = report.PDF(in)
		case report.FormatXLSX:
			data, err = report.XLSX(in)
		}
		if err != nil {
			return written, fmt.Errorf("生成 %s 报告失败: %v", f, err)
		}
		logger.Debug("%s 渲染耗时 %.0f ms（%.1f KB）", f,
			float64(time.Since(tRender).Microseconds())/1000, float64(len(data))/1024)
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, base+ext), data, 0644); err != nil {
			return written, fmt.Errorf("写入报告失败: %v", err)
		}
		if latestBase != "" {
			if err := fsutil.WriteFileAtomic(filepath.Join(dir, latestBase+ext), data, 0644); err != nil {
				return written, fmt.Errorf("写入报告失败: %v", err)
			}
		}
		written = append(written, f)
		logger.Info("报告已生成: %s", filepath.Join(dir, base+ext))
	}
	if cfg.OpsReport {
		opsTxt := []byte(report.OpsText(in))
		opsHTML := []byte(report.OpsHTML(in))
		for _, f := range []struct {
			name string
			data []byte
		}{
			{base + "_ops.txt", opsTxt},
			{latestBase + "_ops.txt", opsTxt},
			{base + "_ops.html", opsHTML},
			{latestBase + "_ops.html", opsHTML},
		} {
			if latestBase == "" && strings.Contains(f.name, "latest") {
				continue
			}
			if err := fsutil.WriteFileAtomic(filepath.Join(dir, f.name), f.data, 0644); err != nil {
				return written, fmt.Errorf("写入运维报告失败: %v", err)
			}
		}
		logger.Info("运维明细报告已生成: %s", filepath.Join(dir, base+"_ops.txt"))
		logger.Info("运维图表报告已生成: %s", filepath.Join(dir, base+"_ops.html"))
	}
	return written, nil
}

// siteReportInput 把站点统计包装成单站点报告输入。
func siteReportInput(cfg *config.Config, row report.SiteRow, ss *analyzer.SiteStats, start, end string) report.Input {
	fs := ss.AsFileStats()
	days := fs.DaySummaries()
	errRate := 0.0
	if fs.Total > 0 {
		errRate = float64(fs.ErrorCount) / float64(fs.Total) * 100
	}
	issues := detect.Issues(fs.Total, errRate, fs.SlowCount, fs.ErrorStatus,
		cfg.ErrorAlert, cfg.SlowAlert, fs.AvgTime()*1000, fs.Percentile(0.99)*1000)
	return report.Input{
		Cfg: cfg, Days: days, Stats: fs, StartDate: start, EndDate: end,
		Issues: issues, Source: logSource(cfg),
		SiteName: row.Name, SiteKey: row.Key, SiteKind: row.Kind,
	}
}

// finishWeekly 统一下半段：问题检测 → 周汇总 → 环比/趋势 → 渲染 HTML。
// persist=false 时只出报告，不写历史（单文件预览模式）。
func finishWeekly(cfg *config.Config, grand *analyzer.FileStats, start, end string,
	windowDays []string, persist bool) error {
	if grand.Total == 0 {
		return fmt.Errorf("周期 %s ~ %s 内没有任何数据", start, end)
	}
	if grand.Format == "mixed" {
		logger.Warn("检测到多种日志格式混合；请确认是否同一批日志，必要时用 -log-format 显式指定")
	}
	logFormatWarning(grand)
	for _, day := range windowDays {
		if a := grand.DayAgg(day); a == nil || a.Total == 0 {
			logger.Warn("%s 无数据，报告中不显示该日", day)
		}
	}

	daySummaries := grand.DaySummaries()

	var errorRate float64
	errorRate = float64(grand.ErrorCount) / float64(grand.Total) * 100
	issues := detect.Issues(grand.Total, errorRate, grand.SlowCount,
		grand.ErrorStatus, cfg.ErrorAlert, cfg.SlowAlert,
		grand.AvgTime()*1000, grand.Percentile(0.99)*1000)
	logger.Info("检测到 %d 个问题", len(issues))
	for _, iss := range issues {
		logger.Warn("问题[%s] %s: %s", iss.Level, iss.Title, iss.Desc)
	}

	// 保存周汇总（用于环比）
	peakDay := analyzer.DaySummary{}
	for _, d := range daySummaries {
		if d.PeakCnt > peakDay.PeakCnt {
			peakDay = d
		}
	}
	summary := &history.Summary{
		StartDate: start, EndDate: end,
		Total: grand.Total, PeakQPS: peakDay.PeakCnt,
		Size: grand.SizeTotal, AvgTime: grand.AvgTime(),
		ErrorRate: errorRate, UniqueIPs: int64(uniqueClients(grand.ClientCnt)),
		ErrorCount: grand.ErrorCount, SlowCount: grand.SlowCount,
	}
	if persist {
		if err := history.Save(cfg.HistoryDir, start, end, summary); err != nil {
			logger.Error("保存周汇总失败: %v", err)
		} else {
			logger.Debug("周汇总已保存: %s_to_%s.json", start, end)
		}
	}

	// 环比：加载上周数据（-from / -preview 口径不同，不做环比，避免误导）
	var prev *history.Summary
	if cfg.FromClock == "" && !cfg.Preview {
		var err error
		prev, err = history.LoadPrevious(cfg.HistoryDir, start, end)
		if err != nil {
			logger.Debug("未找到上周历史数据，环比显示为 -")
		} else {
			logger.Debug("加载上周历史: total=%d peak=%d", prev.Total, prev.PeakQPS)
		}
	}

	// 清理超期历史（放在环比加载之后，避免误删上周数据）
	if persist {
		if err := history.Cleanup(cfg.HistoryDir, cfg.KeepDays); err != nil {
			logger.Debug("清理历史失败: %v", err)
		}
	}

	// 近 8 周趋势（不含本周）；-from / -preview 的数据与历史口径不同，不展示
	var recent []*history.Summary
	if cfg.FromClock == "" && !cfg.Preview {
		recent = history.LoadRecent(cfg.HistoryDir, start, 8)
	}

	if err := os.MkdirAll(cfg.ReportDir, 0755); err != nil {
		return fmt.Errorf("创建报告目录失败: %v", err)
	}

	rows := siteRowList(cfg, grand)
	if cfg.SiteEnabled {
		if len(rows) == 0 && len(grand.Sites) == 0 {
			logger.Warn("未识别到任何站点；默认 combined 不包含 host/domain 字段，请使用 -site-map / -site-name 或修改 log_format")
		}
		var unknown, conflicts, invalid, multi int64
		for _, row := range rows {
			if row.Kind == string(site.KindUnknown) {
				unknown += row.Total
			}
		}
		for _, ss := range grand.Sites {
			if ss == nil {
				continue
			}
			conflicts += ss.Conflicts
			invalid += ss.Invalid
			multi += ss.MultiDomainLines
		}
		if unknown > 0 {
			logger.Warn("有 %d 条记录无法识别站点，已归入 (未识别站点)；可用 -site-map / -site-name / -site-field 补充", unknown)
		}
		if conflicts > 0 {
			logger.Warn("检测到 %d 条行内域名与文件级站点不一致，已按行内字段统计", conflicts)
		}
		if invalid > 0 {
			logger.Warn("检测到 %d 条行内站点值无效，已回退文件级站点，未丢行", invalid)
		}
		if multi > 0 {
			logger.Warn("检测到 %d 条记录包含多个域名，已取第一个合法值", multi)
		}
	}
	in := report.Input{
		Cfg:       cfg,
		Days:      daySummaries,
		Stats:     grand,
		StartDate: start,
		EndDate:   end,
		Prev:      prev,
		Recent:    recent,
		Issues:    issues,
		Source:    logSource(cfg),
		SiteRows:  rows,
	}
	base := fmt.Sprintf("weekly_report_%s_to_%s", start, end)
	latestBase := "weekly_report_latest"
	if cfg.SingleFile != "" {
		base = fmt.Sprintf("single_report_%s_to_%s", start, end)
		latestBase = "single_report_latest"
	}
	if cfg.SiteReport != "per" {
		if _, err := writeReportBundle(cfg, in, cfg.ReportDir, base, latestBase); err != nil {
			return err
		}
	}

	// 每站点报告：默认只生成域名站点；IP 直连默认只在全站报告展示。
	var index []report.SiteIndexEntry
	if cfg.SiteEnabled && cfg.SiteReport != "aggregate" && len(rows) > 0 {
		limit := cfg.SiteMaxReport
		if limit <= 0 {
			limit = 100
		}
		generated := 0
		for _, row := range rows {
			if generated >= limit {
				break
			}
			if row.Total < cfg.SiteMinPV {
				continue
			}
			if row.Kind == string(site.KindUnknown) || row.Kind == string(site.KindOther) {
				continue
			}
			if row.Kind == string(site.KindIP) && cfg.SiteIPReport != "per-ip" && cfg.SiteName == "" {
				continue
			}
			ss := grand.Sites[row.Key]
			if ss == nil {
				continue
			}
			dir := filepath.Join(cfg.ReportDir, "sites", site.SafeDir(row.Key))
			sin := siteReportInput(cfg, row, ss, start, end)
			formats, err := writeReportBundle(cfg, sin, dir, base, latestBase)
			if err != nil {
				return err
			}
			index = append(index, report.SiteIndexEntry{
				SiteRow: row, Dir: filepath.ToSlash(filepath.Join("sites", site.SafeDir(row.Key))),
				Base: base, Formats: formats,
			})
			generated++
		}
	}
	// -site-ip-report group：把 IP 直连站点合并成一份报告，避免每 IP 一个目录。
	if cfg.SiteEnabled && cfg.SiteReport != "aggregate" && cfg.SiteIPReport == "group" && cfg.SiteName == "" {
		hasIP := false
		for _, row := range rows {
			if row.Kind == string(site.KindIP) {
				hasIP = true
				break
			}
		}
		if hasIP && (cfg.SiteMaxReport <= 0 || len(index) < cfg.SiteMaxReport) {
			group := mergeSitesByKind(grand, site.KindIP, "i:group", "(IP直连合计)")
			if group != nil && group.Agg.Total >= cfg.SiteMinPV {
				row := siteRowFrom("i:group", "(IP直连合计)", string(site.KindIP), &group.Agg, group)
				dir := filepath.Join(cfg.ReportDir, "sites", "ip", "_group")
				sin := siteReportInput(cfg, row, group, start, end)
				formats, err := writeReportBundle(cfg, sin, dir, base, latestBase)
				if err != nil {
					return err
				}
				index = append(index, report.SiteIndexEntry{
					SiteRow: row, Dir: filepath.ToSlash(filepath.Join("sites", "ip", "_group")),
					Base: base, Formats: formats,
				})
			}
		}
	}

	if len(index) > 0 {
		idxData := report.SitesIndexHTML(index, start, end, cfg.ReportTitle)
		if err := fsutil.WriteFileAtomic(filepath.Join(cfg.ReportDir, "sites_index.html"), idxData, 0644); err != nil {
			return fmt.Errorf("写入站点索引失败: %v", err)
		}
		if data, err := json.MarshalIndent(index, "", "  "); err == nil {
			if err := fsutil.WriteFileAtomic(filepath.Join(cfg.ReportDir, "sites_index.json"), data, 0644); err != nil {
				logger.Warn("写入站点索引 JSON 失败: %v", err)
			}
		}
		logger.Info("站点报告已生成: %d 个（索引：%s）", len(index), filepath.Join(cfg.ReportDir, "sites_index.html"))
	}
	return nil
}

// logSource 运维报告的「日志文件」抬头：单文件给路径，目录模式给「目录/模式」
func logSource(cfg *config.Config) string {
	if cfg.SingleFile != "" {
		return cfg.SingleFile
	}
	if cfg.LogDir == "" {
		return ""
	}
	return filepath.Join(cfg.LogDir, cfg.LogPattern)
}

// processOptions 组装日志处理选项（含 CPU 节流配置）
func logFormatWarning(st *analyzer.FileStats) {
	if st != nil && st.FormatWarning != "" {
		logger.Warn("%s", st.FormatWarning)
	}
}

func processOptions(cfg *config.Config, start, end string) analyzer.Options {
	from := -1
	if cfg.FromClock != "" {
		if m, err := parseClockMinute(cfg.FromClock); err == nil {
			from = m
		}
	}
	return analyzer.Options{
		Start:          start,
		End:            end,
		CPUPercent:     cfg.CPUPercent,
		EarlyExitLines: cfg.EarlyExitLines,
		URINormalize:   cfg.URINormalize,
		SkipStatic:     cfg.SkipStatic,
		RealIP:         cfg.RealIP,
		TimeField:      cfg.TimeField,
		SlowMS:         cfg.SlowMS,
		ExcludeBots:    cfg.ExcludeBots,
		LogFormat:      cfg.LogFormat,
		CSVFields:      cfg.CSVFields,
		CSVSkipHeader:  cfg.CSVSkipHeader,
		JSONFields:     cfg.JSONFields,
		FromMinute:     from,
		SiteEnabled:    cfg.SiteEnabled,
		SiteField:      cfg.SiteField,
		SiteFrom:       cfg.SiteFrom,
		SiteIPMode:     cfg.SiteIPMode,
		SiteStripWWW:   cfg.SiteStripWWW,
		SiteMax:        cfg.SiteMax,
	}
}

// newSiteResolver 构建站点解析器。只有启用站点功能时调用。
func newSiteResolver(cfg *config.Config) (*site.Resolver, error) {
	r := site.NewResolver(cfg.LogDir, cfg.SiteStripWWW)
	r.Mode = cfg.SiteFrom
	if cfg.SiteMap != "" {
		if err := r.LoadMap(cfg.SiteMap); err != nil {
			return nil, err
		}
	}
	if cfg.SiteAliasMap != "" {
		if err := r.LoadAliases(cfg.SiteAliasMap); err != nil {
			return nil, err
		}
	}
	if cfg.SiteName != "" {
		if err := r.SetForced(cfg.SiteName); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// processOptionsForFile 在基础选项上附加文件级默认站点。
func processOptionsForFile(cfg *config.Config, resolver *site.Resolver, path, start, end string) analyzer.Options {
	opt := processOptions(cfg, start, end)
	if cfg.SiteEnabled && resolver != nil {
		info := resolver.ResolveFile(path)
		opt.SiteDefault = info.Key
		opt.SiteDefaultSource = info.Source
		opt.SiteAliases = resolver.Aliases
		opt.SiteForce = resolver.Forced != nil
	}
	return opt
}

// parseClockMinute 把 "HH:MM" 解析为自 00:00 起的分钟数
func parseClockMinute(s string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("应为 HH:MM，实际 %q", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("应为 HH:MM，实际 %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("小时应为 0-23、分钟应为 0-59，实际 %q", s)
	}
	return h*60 + m, nil
}

// noDataError 生成"统计区间内没有数据"的错误，并带上文件里实际的时间范围，
// 方便一眼看出是日期选错、日志未切割，还是文件本身不含该天的数据。
func noDataError(start, end string, st *analyzer.FileStats) error {
	scope := start
	if end != "" && end != start {
		scope = start + " ~ " + end
	}
	hint := ""
	if st.FirstDay != "" && st.LastDay != "" {
		hint = fmt.Sprintf("；日志文件内实际记录范围 %s ~ %s", st.FirstDay, st.LastDay)
	}
	return fmt.Errorf("统计区间 %s 没有任何有效记录（区间内 %d 行，区间外 %d 行，无法解析 %d 行）%s；排查建议：1) 确认该日期日志已切割；2) 用 -date 指定文件里真实存在的日期；3) 加 -debug 看每个文件的时间范围",
		scope, st.Total, st.OutOfRange, st.Skipped, hint)
}

// dayStat 取按天计数；map 存在但当天没有记录时返回 0，
// map 不存在（手工构造/旧代码）时回退到总量。
func dayStat(m map[string]int64, day string, total int64) int64 {
	if m == nil {
		return total
	}
	return m[day]
}

// newDailyWithSites 构造根日记录，并把各站点当天统计放进 Sites。
// 根记录仍然保存全站汇总，兼容旧版周报/历史；站点数据用于多站点周报。
func newDailyWithSites(day string, agg *analyzer.Agg, partial bool,
	sites map[string]*analyzer.SiteStats, root *analyzer.FileStats,
	includeRootMeta bool, metaSites map[string]bool) *history.Daily {
	d := history.NewDaily(day, agg, partial)
	if root != nil {
		d.StaticSkipped = dayStat(root.StaticSkippedDay, day, root.StaticSkipped)
		d.StaticBytes = dayStat(root.StaticBytesDay, day, root.StaticBytes)
		d.BotSkipped = dayStat(root.BotSkippedDay, day, root.BotSkipped)
	}
	if len(sites) == 0 {
		return d
	}
	d.Sites = make(map[string]*history.Daily, len(sites))
	for key, ss := range sites {
		a := ss.Days[day]
		if a == nil || a.Total == 0 {
			continue
		}
		sd := history.NewDaily(day, a, partial)
		sd.Site = key
		sd.SiteKind = string(ss.Kind)
		sd.SiteName = ss.Name
		sd.StaticSkipped = dayStat(ss.StaticSkippedDay, day, ss.StaticSkipped)
		sd.StaticBytes = dayStat(ss.StaticBytesDay, day, ss.StaticBytes)
		sd.BotSkipped = dayStat(ss.BotSkippedDay, day, ss.BotSkipped)
		includeSiteMeta := includeRootMeta || (metaSites != nil && metaSites[key])
		if includeSiteMeta {
			sd.SiteConflicts = ss.Conflicts
			sd.SiteInvalid = ss.Invalid
			sd.SiteMultiDomain = ss.MultiDomainLines
		}
		if includeSiteMeta && len(ss.SourceCnt) > 0 {
			sd.SiteSourceCnt = make(map[string]int64, len(ss.SourceCnt))
			for k, v := range ss.SourceCnt {
				sd.SiteSourceCnt[k] = v
			}
		}
		d.Sites[key] = sd
	}
	return d
}

// logDailySummary 输出单日统计摘要
func logDailySummary(day string, agg *analyzer.Agg) {
	peakTime, peakCnt := agg.Peak()
	errorRate := 0.0
	if agg.Total > 0 {
		errorRate = float64(agg.ErrorCount) / float64(agg.Total) * 100
	}
	peakAt := "-"
	if peakCnt > 0 {
		peakAt = peakTime.Format("15:04:05")
	}
	logger.Info("%s: 总请求 %d，峰值 %d req/s (%s)，流量 %s，平均响应 %.0fms，错误 %d (%.2f%%)，慢请求 %d",
		day, agg.Total, peakCnt, peakAt, humanBytes(float64(agg.SizeTotal)),
		agg.AvgTime()*1000, agg.ErrorCount, errorRate, agg.SlowCount)
}

// reportIssues 检测并输出单日问题
func reportIssues(agg *analyzer.Agg, cfg *config.Config) {
	if agg == nil || agg.Total == 0 {
		return
	}
	errorRate := float64(agg.ErrorCount) / float64(agg.Total) * 100
	issues := detect.Issues(agg.Total, errorRate, agg.SlowCount, agg.ErrorStatus,
		cfg.ErrorAlert, cfg.SlowAlert, agg.AvgTime()*1000, agg.Percentile(0.99)*1000)
	for _, iss := range issues {
		logger.Warn("问题[%s] %s: %s", iss.Level, iss.Title, iss.Desc)
	}
}

// uniqueClients 独立客户端数（基数溢出时 map 里有一个 (其他) 汇总项，不算真实 IP）
func uniqueClients(m map[string]int64) int {
	n := len(m)
	if _, ok := m[analyzer.OverflowKey]; ok {
		n--
	}
	return n
}

// fileSize 返回文件字节数（取不到返回 0）
func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// humanBytes 格式化字节数
func humanBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}
