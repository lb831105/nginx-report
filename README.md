# Nginx 日志周报系统（nginx-report）

按标准 Go 项目结构组织的 **Nginx 访问日志周报工具**。自动处理每天 00:00 切割压缩的访问日志，统计 QPS、响应性能、状态码、接口排行等指标，自动检测运行问题，生成可直接交付的 HTML 周报。

- 语言：Go 1.21+，**零第三方依赖**，静态编译单二进制
- 两种使用方式：**一次性执行**（cron 定时）或**常驻服务**（进程内自带调度）
- 产物：HTML 周报 + 日记录/周汇总 JSON（用于周报聚合与环比）+ 运维/开发图文报告（`*_ops.html` / `*_ops.txt`）

> 完整可复制使用样例（单文件、目录、多站点、CSV/自定义格式、常驻服务等）见 [USAGE.md](USAGE.md)。

---

## 一、快速开始

```bash
# 1) 一次性：统计昨天（日志已切割压缩后运行）
./nginx_weekly_report -mode daily -logdir /data/_log/backup -reportdir /data/reports

# 2) 一次性：汇总上一个完整周（周一~周日）
./nginx_weekly_report -mode weekly -logdir /data/_log/backup -reportdir /data/reports

# 3) 常驻服务：进程内调度，每天 00:10 统计昨天，每周一 08:00 汇总上周
./nginx_weekly_report -mode serve -logdir /data/_log/backup -reportdir /data/reports
```

打开报告：`/data/reports/weekly_report_latest.html`

---

## 二、运行模式

| 模式 | 用途 | 读什么 | 写什么 |
|------|------|--------|--------|
| `-mode daily` | 统计**头一天** | 昨天那 1 个已切割压缩的日志文件 | `history/daily/report_<日期>.json` |
| `-mode weekly` | 汇总**上一个完整周** | 7 份日记录（缺失的天回退读日志） | HTML 周报 + `history/report_<起>_to_<止>.json` |
| `-mode serve` | **常驻服务**，进程内调度 | 到点自动执行上面两个任务 | 同上 |

**`-logdir` 给目录还是给文件，行为不同：**

| `-logdir` 指向 | 含义 |
|------|------|
| **目录**（如 `/data/_log/backup`） | 按周/按天整体处理：用 `-pattern` 匹配目录下所有分片，按行内时间戳筛选区间 |
| **单个文件**（如 `./access.log`、`./access.log.gz`） | **只处理这一个文件**：统计它的全部数据，周期 = 文件内实际首末日期，只出报告不写历史（见 [7.7](#77-单文件模式直接给一个日志文件)） |

**两种部署方式，二选一：**

- **方式 A（推荐）：常驻服务** —— 一个进程全包，失败自动重试、重启自动补跑。见 [第八章](#八服务常驻用法)
- **方式 B：一次性执行 + 系统定时** —— 交给 cron 跑两条命令。见 [7.5 节](#75-用-crontab-定时一次性执行)

> 为什么拆成「日统计 + 周汇总」：每天只处理 1 个文件，压力最小；周报直接由日记录聚合，秒级完成，
> 且日志被轮转删除后依然能出周报。

---

## 三、目录结构

```
nginx-report/
├── go.mod                          # module nginx-report
├── build.ps1                       # Windows 构建脚本（-Windows / -Run / -Clean）
├── cmd/
│   └── nginx-report/
│       └── main.go                 # 入口：三种模式、参数、流程编排、报告输出
└── internal/                       # 内部包（外部项目不可导入）
    ├── config/config.go            # 配置结构与默认值
    ├── logger/logger.go            # 分级日志（DEBUG/INFO/WARN/ERROR，终端彩色）
    ├── analyzer/analyzer.go        # 日志解析与统计（字段直查、直方图、CPU 节流）
    ├── history/history.go          # 日记录 / 周汇总存取、环比加载、超期清理
    ├── detect/detect.go            # 运行问题自动检测
    ├── report/html.go              # HTML 周报渲染
    ├── scheduler/scheduler.go      # 常驻模式周期调度（到点执行/重试/幂等跳过）
    └── sysctl/nice_*.go            # 进程降优先级（Linux/macOS）
```

---

## 四、环境要求与日志格式

- **编译**：Go 1.21+（仅开发机需要）
- **运行**：Linux x86_64（服务器无需安装 Go）
- **日志轮转**：约定每天 **00:00** 切割压缩，文件名带日期
- **日志格式**：支持 JSON、nginx 默认 combined、自定义 `log_format`、CSV

处理每个文件时会先读首行**识别格式**，再逐行解析；同一文件后续行复用解析器。
识别结果会写到运行日志（`识别日志格式: json/combined/custom/csv`），目录里混用多种格式会告警。

### 4.1 JSON 访问日志

```json
{"@timestamp": "2026-10-29T10:34:55+08:00", "client": "[IP]", "size": "162", "responsetime": "0.007", "status": "200", "upstream_response_time": "0.006", "request_method": "POST", "request_uri": "/bbt-api/org/online/heartbeat", "upstream_addr": "[IP]:9510"}
```

JSON 走快速路径，按字段名直接定位取值（不依赖完整 JSON 解析），容错好、性能高；
解析失败的行计入 `skipped`，不影响整体。默认仍优先使用 JSON，现有部署无需改参数。

除了项目默认字段名（`@timestamp` / `client` / `size` / `status` / `request_uri` …），
也兼容 nginx 原生变量名和常见自定义命名：

| 用途 | 可用 JSON 键 |
|---|---|
| 时间 | `@timestamp`、`time_iso8601`、`time_local`、`timestamp`、`datetime`、`date`、`time` |
| 客户端 IP | `client`、`remote_addr`、`remote_ip`、`ip`、`addr` |
| 真实 IP | `real_client`、`http_x_forwarded_for`、`x_forwarded_for`、`http_x_real_ip` |
| 请求 | `request_uri`、`uri`、`path`、`request`、`url`（后两者会自动拆 method/URI） |
| method | `request_method`、`method` |
| 状态码 | `status`、`code`、`status_code` |
| 字节数 | `size`、`body_bytes_sent`、`bytes_sent`、`bytes` |
| 耗时 | `upstream_response_time`、`request_time`、`responsetime`、`response_time`、`rt` |
| UA / Referer | `agent`、`user_agent`、`http_user_agent` / `refer`、`referer`、`http_referer` |

如果字段名不在上表，用 `-json-fields` 做显式映射（源字段=目标，也兼容目标=源；
两边都像目标名时按左边的源字段处理）：

```bash
./nginx_weekly_report -mode weekly \
  -logdir /data/_log/backup -pattern '*.log*.gz' \
  -json-fields 'ts=time,cip=client,rc=real_client,m=method,u=uri,req=request,st=status,sz=size,ut=upstream_time,rt=response_time,ua=agent,ref=referer' \
  -reportdir /data/reports
```

目标名可用：`time`、`client`、`real_client`、`method`、`uri`、`request`、`status`、
`size`、`upstream_addr`、`upstream_status`、`upstream_time`、`response_time`、`agent`、`referer`。
`url`/`request` 的值可以是 `GET /api/x HTTP/1.1`，也可以只是 `/api/x`。

### 4.2 nginx 默认 combined / common

默认的 `combined`：

```nginx
log_format main '$remote_addr - $remote_user [$time_local] "$request" '
                '$status $body_bytes_sent "$http_referer" "$http_user_agent"';
```

日志示例：

```text
203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /api/x?y=1 HTTP/1.1" 200 123 "https://ref.example/a" "Mozilla/5.0 (Windows NT 10.0) Chrome/120"
```

**不需要额外参数**，首行会自动识别为 `combined`；common log format（没有 referer/UA）也兼容。
`$request` 会自动拆成 method + URI，`$time_local` 会按 `02/Jan/2006:15:04:05 -0700` 解析。

> combined 末尾的两个引号字段按顺序解释为 referer、user_agent。如果自定义格式只保留其中一个，
> 请用 `-log-format` 显式传模板，否则可能把 UA 误当成 referer。

### 4.3 自定义 log_format

nginx 的 `log_format` 可以随意调整字段顺序。把配置里的模板**原样**传给 `-log-format` 即可
（命令行用单引号，防止 shell 展开 `$`）：

```bash
./nginx_weekly_report -mode weekly \
  -logdir /data/_log/backup -pattern '*.log*.gz' \
  -log-format '$time_iso8601 $remote_addr "$request" $status $body_bytes_sent $request_time $upstream_response_time $upstream_addr $upstream_status "$http_referer" "$http_user_agent"' \
  -reportdir /data/reports
```

支持的变量：

| nginx 变量 | 用途 |
|---|---|
| `$time_local` / `$time_iso8601` | 日志时间（模板必须包含其中一个） |
| `$remote_addr` | 客户端 IP |
| `$realip_remote_addr` / `$http_x_forwarded_for` / `$http_x_real_ip` | 真实客户端 IP（配合 `-real-ip`） |
| `$request` | 完整请求行，自动拆 method / URI |
| `$request_method` / `$request_uri` / `$uri` | method / URI |
| `$status` | HTTP 状态码 |
| `$body_bytes_sent` / `$bytes_sent` | 响应字节数 |
| `$request_time` / `$upstream_response_time` | 响应耗时（配合 `-time-field`） |
| `$upstream_addr` / `$upstream_status` | 上游地址 / 上游状态 |
| `$http_referer` / `$http_user_agent` | 来源 / UA |

其它变量（`$remote_user`、`$host`、`$scheme`、`$connection` 等）会被解析但忽略，不影响模板匹配。
模板没有时间字段会直接报错，因为无法按天统计。

> 含空格的变量（`$request`、`$http_user_agent`、`$http_referer`）请按 nginx 配置用双引号包起来；
> `$time_local` 本身带空格，建议写成 `[$time_local]`，或改用 `$time_iso8601`。

### 4.4 CSV 日志

**带表头**（首行是字段名，自动识别并跳过）：

```text
time,client,method,uri,status,size,request_time,agent,referer
2026-10-30T10:00:00+08:00,1.2.3.4,GET,/api/x?y=1,200,123,0.12,"Mozilla/5.0, Mobile",https://ref.example/a
```

**没有表头**时显式指定列顺序：

```bash
./nginx_weekly_report -mode weekly \
  -logdir /data/_log/backup -pattern '*.csv' \
  -log-format csv -csv-fields 'time,client,method,uri,status,size,request_time,agent,referer' \
  -reportdir /data/reports
```

表头/列名支持常见别名：`time`/`timestamp`/`@timestamp`、`client`/`remote_addr`/`ip`、
`method`/`request_method`、`uri`/`request_uri`/`url`/`request`、`status`/`code`、
`size`/`body_bytes_sent`/`bytes`、`request_time`/`responsetime`、`agent`/`user_agent`/`ua`、
`referer`/`http_referer`。

### 4.5 自动识别与排错

- 首行以 `{` 开头 → JSON
- 首行是带时间/状态等字段名的 CSV 表头 → CSV（自动跳过表头）
- 首行符合 `IP - 用户 [时间] "请求" 状态 字节 "referer" "UA"` → combined
- combined 末尾只多 1 个浮点字段时按 `$request_time` 处理并输出 WARN；若实际是 `$upstream_response_time`，请用 `-log-format` 显式传模板
- 其它 → 报错，并打印首行示例，提示用 `-log-format` 显式指定
- 文件头部有少量 banner/空行时，会最多尝试 20 个非空行再报错
- 强制指定：`-log-format json` / `combined` / `csv` / `'<nginx 模板>'`

**日志文件是怎么被选中的**

1. 用 `-pattern`（默认 `28888_access_json.log*`）在 `-logdir` 下匹配文件
2. 文件名里带 `YYYY-MM-DD` 的，日期落在 `[start, end+1天]` 才作为候选
   （`+1天` 是为了兼容「按轮转日命名」和「按数据日命名」两种习惯）
3. 最终以**每行 `@timestamp`** 精确过滤到统计区间
4. 文件名不带日期的（当天正在写的那个文件）：默认一并扫描并按内容过滤；
   **只有当存在一个文件名日期正好等于统计日 start 的文件**时才跳过它，
   防止 copytruncate 式轮转把同一天的数据在两个文件里重复计入

**支持的日志文件形态**（可以混着放，一次跑完）

| 形态 | 例子 | 是否支持 |
|---|---|---|
| 明文、无日期 | `28888_access_json.log` | ✅ |
| 明文、按序号轮转 | `28888_access_json.log.1`、`.log.2` | ✅ |
| 明文、带日期 | `28888_access_json.log-2026-11-01`、`.log.2026-11-01` | ✅ |
| **gzip 压缩**（任意命名） | `....log-2026-11-01.gz`、`.log.1.gz` | ✅ 自动识别 `.gz` 后缀解压，读完同时关闭解压流与文件句柄 |
| 混合 | 一部分 `.gz`、一部分明文 | ✅ 逐个处理并合并 |

```bash
# 以 gz 为主（很多环境只留压缩包）
./nginx_weekly_report -mode weekly -logdir /data/_log/backup -pattern '*.log*.gz' ...

# 只用一个文件（明文或 gz 都行）
./nginx_weekly_report -mode daily -date 2026-11-01 -logdir /data/_log/backup/28888_access_json.log-2026-11-01.gz ...
```

> 压缩包的成本主要来自解压。实测同一份 98 MB 日志：明文 **190 ms**、gz **280 ms**
> （解压后等效吞吐 ≈ 350 MB/s，实际瓶颈是 gzip 而不是解析）。
> 建议保持默认或更低的 `-cpu-percent`，避免解压抢占业务 CPU。

> `-logdir` 既可以是**目录**，也可以直接是**某个日志文件**：传文件时会自动拆成「目录 + 模式」，
> 例：`-logdir D:/vscode/28888_access_json.log.2026-11-01.gz` → 目录 `D:/vscode`、模式 `28888_access_json.log*`。

---

## 五、编译

```powershell
cd nginx-report

# 1) 交叉编译 Linux 版（部署用）-> nginx_weekly_report
.\build.ps1

# 2) 编译 Windows 版（本机测试）-> nginx_weekly_report_win.exe
.\build.ps1 -Windows

# 3) 清理当前目标的旧产物后再编译
.\build.ps1 -Windows -Clean

# 4) 编译后用 mock 日志试跑（脚本自动从 mock 日志首行提取日期，跑一次 daily）
.\build.ps1 -Windows -Run
```

> `-Run` 会在**仓库的上一级目录**找 `mock_access.log`，找不到就跳过试跑；
> 试跑产物写到 `test_reports/`。
>
> 说明：`build.ps1` 特意写成**纯 ASCII（英文提示）**——Windows PowerShell 5.1 会把
> 无 BOM 的 UTF-8 脚本按 ANSI/GBK 解码，脚本里的中文会直接导致语法解析失败。

等价的 go 命令：

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o nginx_weekly_report ./cmd/nginx-report
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o nginx_weekly_report_win.exe ./cmd/nginx-report
```

本地开发运行：

```bash
go run ./cmd/nginx-report -mode daily -logdir /data/_log/backup -debug
```

---

## 六、命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-mode` | `weekly` | 运行模式：`daily` / `weekly` / `serve` |
| `-date` | `yesterday` | **daily** 的统计日期：`YYYY-MM-DD` / `today` / `yesterday` |
| `-start` | 自动 | **weekly** 开始日期（默认上一个完整周的周一） |
| `-end` | 自动 | **weekly** 结束日期（默认上一个完整周的周日） |
| `-logdir` | 自动 | 日志**目录**（按周/按天整体处理）；也可以直接给**某个日志文件**，此时只处理这一个文件 |
| `-uri-normalize` | `true` | 统计接口时去掉 `?query`（`/api/x?id=1` → `/api/x`），避免同一接口被参数拆成上万条 |
| `-title` | `系统运行周报` | 报告标题（HTML 的 h1、PDF 封面、Excel A1 共用同一来源） |
| `-format` | `html` | 报告格式：`html`（默认）/ `pdf` / `xlsx`（别名 `xls`、`excel`）；可逗号组合如 `html,pdf`，或写 `all` |
| `-ops` | `true` | 同时输出**运维/开发版报告**：纯文本 `*_ops.txt` + 带图表的 `*_ops.html`；`-ops=false` 关闭 |
| `-top` | `20` | 运维报告里所有 Top 榜显示条数（1-200） |
| `-preview` | `false` | 只出报告，不读写日记录/周汇总；配合 `-start/-end` 复盘某个 gz / 补算一次时用 |
| `-max-mem-mb` | `256` | 堆内存软上限（MB），超过后主动 GC；0=不限制 |
| `-static-exclude` | `true` | 接口 Top 表排除静态资源（css/js/图片/字体，含 `/assets/`、`/static/` 等目录），单独汇总一行 |
| `-skip-static` | `true` | **解析时直接丢弃静态资源记录**（css/js/图片/字体等）：不计入 PV、按分钟趋势、状态码、IP、流量、错误/慢请求；`-skip-static=false` 关闭 |
| `-real-ip` | `true` | 优先用 `real_client` 作为客户端 IP（CDN/代理场景），为空或 `-` 时回退 `client`；`-real-ip=false` 只用 `client` |
| `-time-field` | `auto` | 响应时间取字段：`auto`（上游优先，缺失回退 `responsetime`）/ `upstream` / `response` |
| `-slow-ms` | `1000` | 慢请求阈值（毫秒）；`-slow-ms 500` 表示 >500ms 算慢请求 |
| `-exclude-bots` | `false` | 按 UA 排除爬虫/探针/监控请求（Googlebot、curl、python-requests、Prometheus 等），默认关闭 |
| `-log-format` | `auto` | 输入日志格式：`auto`（默认）/ `json` / `combined` / `combined_host`（combined 末尾带 host）/ `csv` / 直接传 nginx `log_format` 模板 |
| `-csv-fields` | 空 | CSV 没有表头时的列顺序，如 `time,client,method,uri,status,size` |
| `-csv-skip-header` | `false` | CSV 首行是表头但列名无法识别时，配合 `-log-format csv -csv-fields` 强制跳过第一行 |
| `-json-fields` | 空 | 任意 JSON 字段名映射，如 `ts=time,cip=client,u=uri,st=status` |
| `-site-map` | 空 | 多站点映射文件；每行 `站点 = 文件模式`，相对 `-logdir`，支持通配符 |
| `-site-alias-map` | 空 | 站点别名映射文件；每行 `别名 = 主域名`，如 `www.example.com = example.com` |
| `-site-report` | `both` | 站点报告范围：`aggregate`（只出全站）/ `both`（全站 + 每站点，默认）/ `per`（只出每站点） |
| `-site-from` | `auto` | 站点来源：`auto`（字段 > 映射 > 文件名 > 目录名 > 未识别）/ `field` / `map` / `filename` / `dir` |
| `-site-field` | 空 | 行内站点字段名；空=自动尝试 `domain`/`host`/`http_host`/`server_name` 等别名 |
| `-site-name` | 空 | 强制所有日志归属该站点（单文件/单站点目录最省事） |
| `-site` | 空 | 只生成指定站点的独立报告，逗号分隔（全站报告仍展示全部站点）；仅 weekly / 单文件模式 |
| `-site-ip-mode` | `separate` | 行内 IP 直连：`separate` 单独站点 / `inherit` 继承文件站点 / `ignore` 忽略行内 IP |
| `-site-ip-report` | `none` | IP 直连报告：`none`（只在全站展示；`-site-name` 强制站点仍生成）/ `group`（合并一份）/ `per-ip`（每个 IP 一份） |
| `-site-strip-www` | `false` | 把 `www.example.com` 归并到 `example.com`（应用别名前） |
| `-site-max` | `1000` | 单个日志文件最大站点数；超出归入 `(其他站点)`，总数不丢 |
| `-site-min-pv` | `0` | 小于该 PV 的站点不生成独立报告（全站对比仍显示） |
| `-site-max-report` | `100` | 最多生成多少个独立站点报告 |
| `-pattern` | `28888_access_json.log*` | 日志文件名匹配模式（支持 `*`；`.gz` 压缩包自动解压）；站点模式下不含 `/` 时会递归子目录 |
| `-reportdir` | `/data/reports` | 报告输出目录（历史数据自动放其下 `history/`） |
| `-daily-at` | `00:10` | **serve**：每天统计头一天的时间 `HH:MM` |
| `-weekly-day` | `1` | **serve**：周汇总在周几（1=周一 … 7=周日） |
| `-weekly-at` | `08:00` | **serve**：周汇总时间 `HH:MM` |
| `-retry-min` | `10` | **serve**：任务失败重试间隔（分钟） |
| `-max-retry` | `12` | **serve**：单周期最多尝试次数，超过后放弃本周期 |
| `-today-every` | `0` | **serve**：每隔 N 分钟刷新一次「今天」的部分统计（0=关闭）|
| `-cpu-percent` | `50` | 解析日志的目标 CPU 占用百分比（1-100，100=不限速） |
| `-early-exit` | `2000` | 跨日提前退出阈值行数（0=关闭，见 [11.3](#113-跨日提前退出)） |
| `-from` | 空 | 只统计每天 `HH:MM` **之后**的记录（如 `11:30`），对应原 `analyze_log_fast.sh` 的 START_TIME；空=不限 |
| `-nice` | `19` | 进程 nice 值 0-19（0=不调整，仅 Linux/macOS） |
| `-logfile` | 自动 | 运行日志基准路径（按天轮转成 `app-YYYY-MM-DD.log`）；默认 `<reportdir>/logs/nginx-report.log`，`off`=关闭 |
| `-log-keep-days` | `30` | 运行日志保留天数（负数=不清理） |
| `-debug` | `false` | 输出 DEBUG 级别日志 |

> 日期默认值说明：
> - `daily` 不传 `-date` → 处理**昨天**
> - `weekly` 不传 `-start/-end` → 汇总**最近一个已经结束的整周**（周一~周日）。无论周几运行，结果都落在上一个完整周

### 6.1 多站点报告

默认（不传任何 `-site-*`）完全走旧路径，不解析站点、不建站点 map，性能与旧版一致。
显式传任意 `-site-*` 或 `-site` 才开启站点维度。

识别优先级：

1. `-site-name` 强制站点（单文件/单站点目录）
2. 行内 `domain` / `host` / `http_host` / `server_name` 等字段（`-site-field` 可指定字段名）
3. `-site-map` 文件映射
4. 文件名推断（`example.com_2026-10-30.log`、`1.2.3.4.log`；不会把 `access.log` 当域名）
5. 日志文件所在目录名（`/data/_log/ad.kmrd.cn/access.log`，`-site-from dir` 或 auto 自动尝试）
6. `(未识别站点)` 兜底

**站点映射文件** `sites.conf`：

```ini
# 站点 = 文件模式；相对 -logdir，支持 * ? []
example.com = example.com_*.log
api.example.com = /var/log/nginx/api_access.log
```

**别名文件** `aliases.conf`：

```ini
www.example.com = example.com
shop.example.com = example.com
```

复合场景都能处理：

- **一站点一个目录**：`/data/_log/<domain>/access.log`；站点模式下 `-pattern '*.log*'` 会递归扫描子目录，目录名自动作为站点名（也可显式 `-site-from dir`）。
- **一站点一个文件**：`-site-map` 或文件名推断；无日期的 `api_access.log` 也会在站点模式下处理，不会被“存在带日期文件”跳过。
- **共享日志带 domain/host 字段**：JSON、CSV 表头、自定义 `log_format` 的 `$host`/`$server_name` 都支持；同一行有多个域名时取第一个合法值。
- **共享日志没有 host 字段**：默认 combined 不猜测域名；整体归入 `-site-map`/文件名/`-site-name`，否则进入 `(未识别站点)`。
- **IP 直连**：默认单独归类为 `IP直连`，不混入域名报告；`-site-ip-mode inherit` 可归到文件站点。
- **未识别行**：绝不丢行，计入 `(未识别站点)`，全站 PV 始终等于各站点 PV 之和。

输出结构：

```text
reports/
├── weekly_report_<起>_to_<止>.html        # 全站报告（-site-report aggregate/both）
├── sites_index.html / sites_index.json   # 站点索引
└── sites/
    ├── domain/example.com/...            # 每域名一份客户版 + Ops 版
    ├── ip/203.0.113.10/...               # -site-ip-report per-ip 时
    └── ip/_group/...                     # -site-ip-report group 时（所有 IP 合并）
```

单文件模式同样输出 `single_report_*` 到对应站点目录。日记录会把各站点当天统计写入根记录 `sites` 字段；旧日记录没有站点明细时，周报会自动回退读日志重算。

> 站点识别规则、`sites.conf`/`aliases.conf` 和 `-site-*` 参数变化后，建议用 `-preview` 先跑一遍确认，再写正式历史。

---

## 七、一次性用法

适合用 cron / systemd timer / 手动执行，每次跑完进程退出。

### 7.1 每天统计头一天

```bash
./nginx_weekly_report -mode daily -logdir /data/_log/backup -reportdir /data/reports
```

输出示例：

```
[INFO] 开始生成 Nginx 周报（模式: daily）
[INFO] 日统计: 2026-11-01
[INFO] 待处理 1 个日志文件
[INFO] [1/1] 处理 28888_access_json.log.2026-11-01.gz ...
[INFO] 2026-11-01: 总请求 15890，峰值 3 req/s (18:43:08)，流量 38.77 MB，平均响应 7ms，错误 631 (3.97%)，慢请求 310
[WARN] 问题[error] 错误率偏高: 本周错误率 3.97%，超过 1.0% 警戒线
[INFO] 日记录已保存: /data/reports/history/daily/report_2026-11-01.json
[INFO] 完成！
```

### 7.2 汇总上一个完整周

```bash
./nginx_weekly_report -mode weekly -logdir /data/_log/backup -reportdir /data/reports
```

```
[INFO] 统计周期: 2026-10-26 ~ 2026-11-01（7 天）
[INFO] 检测到 1 个问题
[WARN] 问题[error] 错误率偏高: 本周错误率 2.22%，超过 1.0% 警戒线
[INFO] 报告已生成: /data/reports/weekly_report_2026-10-26_to_2026-11-01.html
```

> 日记录齐全时**完全不读日志**，秒级出报告；某天日记录缺失会自动回退读日志并补写日记录。
>
> 环比取「**紧邻的上一个等长周期**」：默认 7 天窗口就是上周一~上周日；
> 用 `-start/-end` 指定 3 天等自定义区间时，会自动跟前面同样长度的区间比，不会拿 3 天去比 7 天。

### 7.3 补算 / 重跑指定日期

```bash
# 重跑某一天（覆盖同名日记录，幂等）
./nginx_weekly_report -mode daily -date 2026-10-29 -logdir /data/_log/backup

# 重算某个周期（缺失的日会回退读日志并补写）
./nginx_weekly_report -mode weekly -start 2026-10-12 -end 2026-10-18 -logdir /data/_log/backup
```

### 7.4 统计当天（日志还在写，部分统计）

默认只处理已切割压缩的日志（昨天）。想随时看今天的情况，用 `-date today`：

```bash
# 统计今天 00:00 到现在（读正在写入的那个日志文件）
./nginx_weekly_report -mode daily -date today -logdir /data/_log/backup -reportdir /data/reports
```

输出：

```
[INFO] 日统计: 2026-11-02（当天，正在写入 → 部分统计）
[INFO] 2026-11-02: 总请求 15230，峰值 96 req/s (10:12:03)，流量 812.4 MB，平均响应 21ms，错误 143 (0.94%)，慢请求 12
[INFO] 日记录已保存: /data/reports/history/daily/report_2026-11-02.json（部分数据，当天结束后会自动重算覆盖）
```

**当天的记录会被标记为 `partial: true`**，这有两个好处：

- 周报聚合时，`partial` 记录**不会被当成最终数据**：优先用日志重算；重算不出来（日志已删）才退回使用并告警
- 第二天凌晨的 `daily` 任务会发现是 `partial`，**重算并覆盖成最终数据**

> 如果你只想看数据、不想留下记录，可以加 `-reportdir` 指到一个临时目录。

### 7.5 用 crontab 定时（一次性执行）

```bash
crontab -e
```

```cron
# 每天 00:10 统计头一天（只读 1 个已压缩文件，压力小）
10 0 * * * cd /opt/nginx-report && ./nginx_weekly_report -mode daily  -logdir /data/_log/backup -reportdir /data/reports -logfile /var/log/nginx-report/nginx-report.log -log-keep-days 30

# 每周一 08:00 汇总上周（周一~周日）
0 8 * * 1  cd /opt/nginx-report && ./nginx_weekly_report -mode weekly -logdir /data/_log/backup -reportdir /data/reports -logfile /var/log/nginx-report/nginx-report.log -log-keep-days 30
```

### 7.7 单文件模式（直接给一个日志文件）

```bash
# 只处理这一个文件，统计它包含的全部数据（不套"上周"窗口、不匹配同目录其他文件）
./nginx_weekly_report -logdir ./28888_access_json.log -reportdir ./reports

# 与原 analyze_log_fast.sh 完全同口径：只看 11:30 之后 + Top URL 保留 ?query
./nginx_weekly_report -logdir ./28888_access_json.log -reportdir ./reports \
    -from 11:30 -uri-normalize=false
```

```
[INFO] 单个日志文件: D:/vscode/nginx-report/28888_access_json.log（只处理该文件，不匹配同目录其他文件）
[INFO] 单文件模式: D:/vscode/nginx-report/28888_access_json.log
[INFO] 统计周期: 2026-07-16 ~ 2026-10-30（82 天，单文件）
[INFO] 28888_access_json.log: 总请求 135261，峰值 175 req/s (07-16 18:59:26)，流量 11.17 GB，…
[INFO] 报告已生成: ./reports/single_report_2026-07-16_to_2026-10-30.html
```

与「给目录」的区别：

| `-logdir` | 处理范围 | 输出 | 写历史 |
|---|---|---|---|
| **日志目录** | 上一个完整周（或 `-start/-end` 指定区间），按文件名 + 行内时间戳筛选 | `weekly_report_<起>_to_<止>.html` + `weekly_report_latest.html` + `*_ops.html` / `*_ops.txt` | ✅ 日记录 / 周汇总 / 超期清理 |
| **单个日志文件** | 该文件的**全部内容**，周期 = 文件内实际首末日期 | `single_report_<起>_to_<止>.html`（上报/客户）+ `single_report_<起>_to_<止>_ops.html` / `_ops.txt`（运维/开发） | ❌ 只出报告，避免覆盖正式历史 |

**每次都会出两个文件，各看各的：**

| 文件 | 给谁 | 内容 |
|---|---|---|
| `single_report_<起>_to_<止>.html`（或 `-format` 指定的 pdf/xlsx） | **上报 / 客户 / 领导** | 封面 + 健康度 + 执行摘要 + 核心指标 + 业务趋势 + 问题/明细（按需） |
| `single_report_<起>_to_<止>_ops.html` | **运维 / 开发** | HTML5 交互报告：KPI 卡片 + Canvas 折线图（**滚轮缩放 / 拖拽平移 / 双击复位 / 触屏双指缩放 / 悬停查看数值**）+ 状态占比、Top 榜横柱、响应分布、设备/浏览器等 |
| `single_report_<起>_to_<止>_ops.txt` | **运维 / 开发** | 原 `analyze_log_fast.sh` 深度分析版式：PV/流量分钟趋势、状态码、方法、URL 类型、Top N URL/IP/大流量 URL、404/5xx/400、响应时间分布、慢请求 URL、文件类型、设备/浏览器/Referer |

> 运维版报告是纯文本，`grep`/`diff`/贴工单都方便；两份报告用**同一份统计量**渲染，数字必然对得上。
> 单文件模式直接解析日志，所以按分钟趋势是**真正的逐分钟**；周报从新格式日记录聚合时读 `minute_cnt`，仍然是 **1 分钟**；
> 升级前的旧日记录没有分钟明细，自动退化为 5 分钟粒度，再退化为按小时展示，不会输出空表。

> 单文件模式是「看一份日志」用的：不读同目录其他文件、不写 `history/`、不覆盖正式周报的 `latest.html`。
> 想只统计文件里的某一天，用 `-mode weekly -logdir <文件> -start 2026-10-28 -end 2026-10-28 -preview`（只出报告，不写历史）；
> 想统计文件里的一段，把 `-start/-end` 换成区间即可。

#### 用 .gz 切割日志统计某一天

`.gz` **不需要先解压**，直接把文件路径给 `-logdir`，工具会自动识别并解压；
筛选始终按每行的 `@timestamp`，与 gz 文件名带不带日期、日期是轮转日还是数据日无关。

| 场景 | 命令 |
|---|---|
| gz 正好是某一天（最常见） | `./nginx_weekly_report -logdir /data/_log/backup/28888_access_json.log-2026-10-30.gz -reportdir /data/reports` |
| 同一个 gz 里有多天，只要 10-30 | `./nginx_weekly_report -mode weekly -logdir xxx.gz -start 2026-10-30 -end 2026-10-30 -preview -reportdir /data/reports` |
| 只要 10-30 的 08:00 之后 | 同上加 `-from 08:00` |
| 想把这天正式写进历史（供周报用） | 去掉 `-preview`（会写 `history/daily/report_2026-10-30.json` 和周汇总） |

说明：

- `-preview` = 只出报告，不读写日记录/周汇总；拿备份 gz 复盘时不会污染正式周报
- 不给 `-start/-end` 时，单文件模式统计**整个文件**；报告名里的日期 = 文件内实际首末日期
- 多天 gz 指定 `-start/-end` 后，会先按文件名/内容选出文件，再按行内时间精确过滤到该天
- 单文件模式输出 `single_report_<起>_to_<止>.html` + `_ops.html` / `_ops.txt`，不写 `history/`

### 7.6 退出码与日志

| 退出码 | 含义 |
|--------|------|
| `0` | 成功 |
| `1` | 失败：没找到日志文件 / 区间内没有有效记录 / 写报告失败等 |

失败时终端会输出明确原因（同时便于 cron 邮件告警）。重跑同一天是幂等的，可安全补算。

---

## 八、服务（常驻）用法

`serve` 模式在进程内自带调度器，一次启动长期运行。

### 8.1 前台试运行

```bash
./nginx_weekly_report -mode serve \
  -logdir /data/_log/backup \
  -reportdir /data/reports \
  -daily-at 00:10 -weekly-day 1 -weekly-at 08:00 \
  -today-every 30 \
  -cpu-percent 25 -nice 19

# 输出
# [INFO] 常驻模式已启动：每天 00:10 处理昨天；每周周一 08:00 汇总上周；失败重试间隔 10 分钟
# [INFO] 另有任务：每 30 分钟刷新一次「今天」的部分统计（用于实时查看）
# [INFO] 日志目录 /data/_log/backup（模式 28888_access_json.log*），报告目录 /data/reports（Ctrl+C 退出）
```

`Ctrl+C`（或 `SIGTERM`）平滑退出。

### 8.2 注册为 systemd 服务（推荐）

`/etc/systemd/system/nginx-report.service`：

```ini
[Unit]
Description=Nginx Weekly Report (resident scheduler)
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/nginx-report
ExecStart=/opt/nginx-report/nginx_weekly_report -mode serve \
  -logdir /data/_log/backup -reportdir /data/reports \
  -daily-at 00:10 -weekly-day 1 -weekly-at 08:00 \
  -cpu-percent 25 -nice 19 -retry-min 10 -max-retry 12 
  -logfile /var/log/nginx-report/nginx-report.log -log-keep-days 30
Restart=always
RestartSec=10
# 终端输出交给 journal（崩溃栈也能看到）；文件日志由程序自己按天轮转
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now nginx-report     # 开机自启并立即启动
systemctl status nginx-report
journalctl -u nginx-report -f           # 跟踪日志
systemctl restart nginx-report          # 改参数后重启
```

> `Restart=always` 保证进程异常退出后自动拉起；拉起后调度器立刻检查一次状态，缺什么补什么。

### 8.3 调度行为（重点）

| 行为 | 说明 |
|------|------|
| **启动即检查** | 进程启动时立刻判断：昨天日记录不存在 → 立即补跑；上周周报不存在 → 立即补生成 |
| **到点执行** | 每天 `daily-at` 统计昨天；每周 `weekly-day` 的 `weekly-at` 汇总上一完整周 |
| **失败重试** | 失败（如 00:00 切割还没完成）按 `retry-min` 重试，默认 10 分钟一次 |
| **重试上限** | 最多 `max-retry` 次（默认 12 ≈ 2 小时），仍失败则放弃本周期，等明天/下周再试，避免整天反复扫大文件 |
| **幂等** | 以「日记录文件 / 周报文件是否存在」判断是否完成，成功后同周期不再执行；重启也不会重复计算 |
| **平滑退出** | 收到 `SIGTERM` / `Ctrl+C` 后停止派发新任务，最多等 **5 秒**让正在跑的任务收尾；所有输出文件都是原子写，超时强退也不会留下半截文件 |
| **单线程串行** | 任务在独立 goroutine 里**依次**执行：既不会并发抢 CPU/IO，也不会因为某个任务耗时长而漏掉其他任务的到点检查 |
| **可选：今天实时统计** | 配了 `-today-every N` 时，每 N 分钟重算一次「今天」的部分统计（`partial`），不影响周报 |

**时序示例**（周一 00:10 切割完成后）：

```
周一 00:10  daily  →  stats 周日日志  → 写 history/daily/report_<周日>.json
周一 08:00  weekly →  读上周一~周日 7 份日记录  → 出 HTML 周报
```

### 8.4 常用运维命令

```bash
# 查看当前调度与执行情况
journalctl -u nginx-report -n 100 --no-pager

# 只补一天（不必重启服务，直接命令行跑）
/opt/nginx-report/nginx_weekly_report -mode daily -date 2026-10-29 \
  -logdir /data/_log/backup -reportdir /data/reports

# 手动触发一次周报重算（先删掉旧报告文件，服务会认为未完成而重跑）
rm /data/reports/weekly_report_2026-10-26_to_2026-11-01.html
systemctl restart nginx-report
```

---

## 九、输出产物与数据流

```
                       ┌─────────────────────────────────────────┐
/data/_log/backup/     │  daily：只读头一天那 1 个文件            │
  28888_....2026-11-01.gz ──►  统计  ──► history/daily/report_2026-11-01.json
  28888_....2026-10-31.gz ──►  统计  ──► history/daily/report_2026-10-31.json
        ...                                    │
                                               ▼
                       ┌─────────────────────────────────────────┐
                       │  weekly：聚合 7 份日记录（默认不读日志） │
                       └─────────────────────────────────────────┘
                                               │
                                               ▼
                    weekly_report_<起>_to_<止>.html  +  weekly_report_latest.html
                    history/report_<起>_to_<止>.json（供下次环比）
```

| 文件 | 说明 |
|------|------|
| `weekly_report_YYYY-MM-DD_to_YYYY-MM-DD.html` | 带日期的周报（历史保留） |
| `weekly_report_latest.html` | 最新周报固定链接 |
| `weekly_report_YYYY-MM-DD_to_YYYY-MM-DD_ops.html` | 运维/开发版图表报告（带 KPI、折线、堆叠条、Top 横柱） |
| `weekly_report_YYYY-MM-DD_to_YYYY-MM-DD_ops.txt` | 运维/开发版纯文本明细 |
| `single_report_YYYY-MM-DD_to_YYYY-MM-DD.html` | 单文件模式报告（`-logdir` 给的是某个文件时）|
| `single_report_YYYY-MM-DD_to_YYYY-MM-DD_ops.html` | 单文件模式的运维/开发版图表报告 |
| `single_report_YYYY-MM-DD_to_YYYY-MM-DD_ops.txt` | 单文件模式的运维/开发版纯文本明细 |
| `history/report_YYYY-MM-DD_to_YYYY-MM-DD.json` | 周汇总（用于下周环比） |
| `history/daily/report_YYYY-MM-DD.json` | 日记录（周报的原料；`partial: true` 表示当天未结束的部分统计） |
| `logs/nginx-report-YYYY-MM-DD.log` | **运行日志**（程序自身日志，按天轮转，默认保留 30 天）|

> 所有 JSON 与 HTML 都用「同目录临时文件 → fsync → rename」的**原子写**：
> 周报 `latest.html` 可能正被浏览器/nginx 打开，绝不会读到写了一半的内容；
> 进程被强杀或磁盘写满也不会留下损坏的历史文件。

日记录约 **几十 KB/天**；周汇总与日记录均按 `KeepDays=90` 天自动清理。

### 9.1 运行日志（按天轮转）

程序自身日志默认写在 `<reportdir>/logs/`，**按天轮转、默认保留 30 天**：

- 每天 0 点自动切到新文件（常驻进程也不会一直写同一个文件）
- 早于 30 天的日志自动删除（`-log-keep-days` 可调，负数=不清理）
- 文件内**不带 ANSI 颜色码**，格式 `2026-11-02 12:29:37 [INFO] ...`；终端仍彩色输出
- 自定义路径：`-logfile /var/log/nginx-report/nginx-report.log`；关闭：`-logfile off`

```bash
$ ls /data/reports/logs/
nginx-report-2026-10-04.log
nginx-report-2026-11-01.log
nginx-report-2026-11-02.log

$ tail -2 /data/reports/logs/nginx-report-2026-11-02.log
2026-11-02 08:00:00 [INFO] 统计周期: 2026-10-26 ~ 2026-11-01（7 天）
2026-11-02 08:00:00 [INFO] 报告已生成: /data/reports/weekly_report_2026-10-26_to_2026-11-01.html
```

> 常驻模式下 `systemctl status` 只能看到最近的终端输出，历史请直接看这里的日志文件；
> `journalctl -u nginx-report` 依旧可用（终端输出同时进 journal）。


#### 接口表怎么过滤静态资源

`接口 Top 15` / `错误接口 Top 10` 默认**排除静态资源**，避免 `/pro/assets/xxx.svg` 这类请求把业务接口挤到看不见：

- **按后缀**：`.js .css .map .svg .png .jpg .jpeg .gif .ico .webp .avif .woff .woff2 .ttf .eot .otf .mp4 .webm .mp3 .pdf`
- **按目录段**（前两段）：`assets` `static` `public` `dist` `media` `img` `images` `fonts`
  —— `/static/app.a1b2c3`（构建产物，没有后缀）也能识别
- `/bbt-api/assets/list` 这种「api 命名空间下的同名目录」仍算业务接口，不会误杀
- 被排除的部分汇总成一行：`(静态资源 7 个：js/css/图片/字体) 1.1 万 81.40% 2ms`

> 解析阶段默认已经用 `-skip-static=true` 把静态资源整行丢弃，所以正常情况下这里不会再出现静态资源；
> 需要排查某个静态资源（比如某张图 404/超时）时，用 `-skip-static=false -static-exclude=false` 把它们放回统计和表里。

### 9.2 HTML 周报结构（按受众分区）

报告一共 9 个板块，按「管理 / 甲方 → 运维 → 开发」的顺序组织，越往下越细：

| 板块 | 受众 | 内容 |
|------|------|------|
| 封面 | 全部 | 标题、周期、日志总量、独立 IP、**健康度等级（A+/A/B/C/D + 分数）** |
| 一、执行摘要 | 管理 / 甲方 | 自动生成 5 条结论：访问规模（含环比）、稳定性（5xx/4xx 拆分）、性能（P99/慢请求）、用户覆盖、总体结论与「该先修哪个接口」 |
| 二、核心指标 | 管理 / 甲方 | **8 张卡片全部带环比**：总访问量、峰值 QPS、总流量、平均响应、错误率、错误次数、慢请求、独立 IP |
| 三、本周问题发现 | 运维 / 开发 | 红（error）/ 黄（warning）/ 蓝（info）三级问题 |
| 四、业务趋势 | 管理 / 甲方 | ①近 8 周访问量趋势（含各周错误率）②每日柱图 + **工作日 vs 周末对比**（仅多天时显示）③**QPS 曲线：单天画一条大曲线；一周每天一行**（每格 5 分钟，横向 00:00–24:00，纵轴按全周最大值归一化），每行标「峰值 15:05 · 6.8 万次（≈226 req/s，占全天 1.98%）」——次数、每秒速率、占全天比例三个口径互相能对上账 ④**24 小时流量分布：单天看绝对值 + 最忙/最闲时段；一周每天一行**（每格 1 小时，峰值显示「15:00–16:00 · 3000 次（占全天 100%）」） |
| 五、每日运行明细 | 运维 | 日期、星期、访问量、峰值 QPS、流量、平均响应、错误数/率、慢请求、状态标签 |
| 六、运维视角 | 运维 | ①状态分组堆叠条（2xx/3xx/4xx/5xx 占比）②错误状态码明细（4xx/5xx，占错误总数、占总请求）③**上游服务健康度**（调用量、占比、平均耗时、上游错误率；nginx 重试产生的 `ip1, ip2` 列表已按「首跳地址 + 最终状态」拆分）④**客户端来源 Top 10** ⑤**24 小时分布：单天看绝对值（含最忙/最闲时段）；多天每天一行**，可直接看出工作日/周末节奏 |
| 七、开发视角 | 开发 | ①**接口 Top 15**（调用量、占比、平均耗时、慢请求、错误数/率，自动打「高错误 / 慢接口」标签；**静态资源单独汇总成一行**，不再把业务接口挤掉）②错误次数最多的接口 Top 10（空 URI 显示 `(无 URI)`）③响应时间分布（7 档直方图） |
| 八、响应性能指标 | 运维 / 开发 | 平均 / P50 / P90 / P95 / P99 / 慢请求 / **成功率(2xx+3xx)** / **服务端错误率(5xx)** + 请求方法分布；表格下方注明「响应时间仅统计有 upstream 计时的请求，占 X%」 |
| 九、指标口径说明 | 附录 | 错误、慢请求、百分位、峰值、独立 IP 的定义，方便甲方核对 |

**健康度评分公式**（报告内也写明，避免"黑盒打分"）：

```
分数 = 100
     − min(40, 错误率% × 4)            // 错误率 10% 扣满
     − min(25, 慢请求占比 × 200)        // 慢请求 12.5% 扣满
     − min(20, max(0, P99ms − 300) / 50)
     − min(15, max(0, 平均ms − 100) / 20)
等级：A+ ≥ 95　A ≥ 90　B ≥ 80　C ≥ 70　D < 70
```

**支撑这些板块的统计数据**（都在日记录里，周报只做聚合，不再读日志）：

| 数据 | 用途 | 体积 |
|------|------|------|
| `slot_cnt[288]` | 5 分钟粒度 QPS 曲线（按天） | ~1.5 KB/天 |
| `minute_cnt[≤1440]` | **1 分钟粒度**请求量趋势（尾部 0 截断） | ~1–6 KB/天 |
| `minute_bytes[≤1440]` | **1 分钟粒度**流量趋势 | ~1–9 KB/天 |
| `max_time / error_uri / device_cnt / browser_cnt / referer_cnt / file_type_cnt` | 深度分析：最大耗时、错误 URL Top、设备/浏览器/来源/文件类型 | 很小（按实际基数） |
| `hour_cnt[24]` | 24 小时分布（按天） | 很少 |
| `uri_stat`：`{calls, time_sum, errors, slows}` | 接口健康度表、慢接口、错误接口 | 每接口 1 条（合并后比以前省 3/4） |
| `upstream_stat`：`{calls, time_sum, time_count, errors}` | 上游健康度 | 每上游 1 条 |
| `time_buckets[1001]` | 百分位 + 响应时间分布图 | ~2 KB/天 |
| `client_cnt` | 独立 IP + 来源 Top 10 | 每 IP 1 条 |
| `method_cnt / status_cnt` | 方法、状态码分布 | 很少 |

> 日记录里 `uri_cnt / uri_time_sum / uri_error_cnt / uri_slow_cnt` 是**旧格式**，新版本会合并成
> `uri_stat` 并继续读取旧文件（升级不需要清理历史）。

> 日记录实测：**~10 KB/天**（小流量）～ **~35 KB/天**（2000 请求、2000 个不同 URI）～
> 加上 `minute_cnt` / `minute_bytes` 后大约再多 **2–15 KB/天**；忙时几十万请求可达 **数百 KB/天**，90 天保留通常在几十 MB 以内。
> 为防止恶意扫描/带随机参数的 URI 把统计表撑爆，`URI`、`client`、`upstream` 三张表有 **2 万 key 上限**，
> 超出部分统一记为 `(其他)`，既保护内存也不影响总量与错误数统计。
> 实测 140 万行 / 36 万个不同 URI（关闭 `-uri-normalize`）时日记录 **≈3 MB**；开启归一化后只有几 KB。

---

### 9.2.1 统计口径（避免"数字互相打架"）

| 指标 | 口径 | 为什么这样定 |
|---|---|---|
| 总访问量 / 错误数 / 慢请求 | 全部**业务**日志行（已按 `-skip-static` 丢弃 css/js/图片/字体，按 `@timestamp` 归日） | 和 nginx 日志行数一一对应，可对账；静态资源流量在 `_ops.txt` 单列 |
| **平均响应 / P50~P99** | **只统计有耗时字段的请求**（`-time-field` 决定取 `upstream_response_time` 还是 `responsetime`），分母是这些请求数 | nginx 对静态资源/本地直接返回写 `-`。把这些当成 0ms 会把平均响应压低几倍（实测：900 条静态 + 100 条 1.5s 接口，按旧口径平均 150ms、P50 6ms；正确值是 1500ms） |
| **P95/P99 长尾** | 0~10s 用 10ms 直方图；10~60s 按 1s、60~600s 按 10s、>600s 单独一桶 | 旧版超过 10s 全挤在最后一桶，P99 会永远显示约 10s；新版长尾请求按实际区间插值 |
| 慢请求占比 | 慢请求数 / **业务总请求数** | 业务含义是“全部业务请求里有多少慢”；默认已排除静态资源 |
| **成功率** | (2xx+3xx) / 总请求 | 4xx 是客户端/参数/扫描问题，不该算服务不可用 |
| **服务端错误率** | 5xx / 总请求 | 真正衡量服务可用性 |
| 峰值 QPS | 逐秒最大请求数（同日多文件同秒会累加） | — |
| 平均 QPS | 总请求 / **窗口天数** / 86400 | 缺口数据的周按 7 天算，才反映真实压力 |
| 独立 IP | 默认按 `real_client` 去重（为空回退 `client`），**不含**基数溢出桶 `(其他)` | CDN/代理后 `client` 是代理 IP，`real_client` 才是真实访客；`-real-ip=false` 可退回旧口径 |
| 接口平均耗时 | 该接口 `time_sum / calls` | 静态资源天然 0ms，混排接口会被略微低估（见上方覆盖率提示） |
| 健康度 | 100 − 错误率扣分 − 慢请求扣分 − P99 扣分 − 平均响应扣分 | 四项都用上面已修正的口径 |

**升级影响**：日记录新增 `timed_count` 字段（旧记录没有 → 自动按 `Total` 回退，不会算出 0）；新增 `time_overflow` 字段保存 10 秒以上的耗时分布（旧记录没有 → 长尾只能按旧版 10s 上限近似）。

> **换口径要重算历史**：日记录只保存统计结果，不保存 `-slow-ms` / `-time-field` / `-real-ip` / `-exclude-bots` / `-skip-static` 这些口径参数。
> 如果本周用 500ms 判慢、上周的日记录是 1s 口径，直接聚合出的周报会混用两种阈值。
> 改口径后请用 `-mode daily -date YYYY-MM-DD` 重跑对应日期（或删掉 `history/daily/report_<日期>.json` 让周报回退读日志）。

> `-skip-static` 是**解析期过滤**：升级前已经落盘的 `history/daily/*.json` 仍包含静态资源，
> 需要重跑对应日期（`-mode daily -date ...`）才会变成业务口径；单文件/回退读日志的报告不受影响。

### 9.3 输出格式：HTML（默认）/ PDF / Excel

用 `-format` 控制，一份数据可以同时出多种格式：

```bash
# 默认只出 HTML（行为不变）
./nginx_weekly_report -mode weekly -logdir /data/log/backup -reportdir /data/reports

# 只出 PDF，发给甲方/领导
./nginx_weekly_report ... -format pdf

# 一份周报同时出三种，运维看 HTML、领导看 PDF、分析师拉 Excel
./nginx_weekly_report ... -format all          # = html,pdf,xlsx
```

| 格式 | 文件名 | 适用场景 |
|---|---|---|
| `html`（默认） | `weekly_report_<起>_to_<止>.html` | 浏览器看，交互最全（9 个板块、柱图、sparkline） |
| `pdf` | 同名 `.pdf` | 打印、发邮件、发甲方；A4 竖版，自动分页、带页码，中文用 PDF 预定义字体（不嵌字体，文件只有几十 KB） |
| `xlsx` | 同名 `.xlsx` | 二次分析；8 个工作表：概览、每日明细、接口Top、上游健康度、状态码、客户端Top、时段分布、问题清单 |

要点：

- 三种格式**共用同一份中间数据**（`internal/report/snapshot.go`），所以同一周的数字必然一致，不会出现「HTML 和 Excel 对不上」
- Excel 每个表**冻结首行 + 开自动筛选**，可直接排序、透视；打印设置为 A4 横向按宽度缩放到一页
- `xls`/`excel` 会当作 `xlsx` 处理（真正的老式 .xls 是二进制格式，不再使用）
- 写错的格式名会**回退到 html**，不会因为参数写错就什么都不出
- 同时也会生成对应的 `*_latest.<ext>`，方便 nginx 挂一个固定链接
- PDF/Excel 由纯标准库实现（`internal/pdf`、`internal/xlsx`），**不引入任何第三方库，也不需要 LibreOffice/wkhtmltopdf**

> PDF 里中文使用 PDF 规范自带的 CJK 字体（STSong-Light + Adobe-GB1），Acrobat、Chrome/Edge、WPS、macOS 预览都正常。
> 极少数没装 CJK 字体支持的阅读器可能显示方块，那种环境请用 HTML 或 Excel。

### 9.4 运维/开发报告（`*_ops.html` / `*_ops.txt`）

除 HTML/PDF/Excel 外，每次生成报告还会额外落两份运维/开发报告（`-ops=false` 可关闭）：

- `*_ops.html`：HTML5 交互版（KPI 卡片 + Canvas 图表，**可缩放/平移**，悬停显示数值，无外部 CDN/JS 依赖），浏览器直接看
- `*_ops.txt`：纯文本版，`grep`/`diff`/贴工单用

两份内容一一对应，文件名都带 `_ops`，不会和客户/上报的 `single_report_*.html` / `weekly_report_*.html` 混淆。
它沿用原 `analyze_log_fast.sh`（深度分析版）的版式，给运维和开发排查问题用：

```text
=========================================
系统运行周报（运维 / 开发）
时间范围: 2026-10-30
时间过滤: 每天 08:00 之后（对应原脚本 START_TIME）
统计粒度: 分钟（1 分钟）
Top 列表显示: 20 条
日志文件: /data/_log/28888_access_json.log
已排除静态资源: 47862 条 / 9.86 GB（css/js/图片/字体等；原始行数 135261）
=========================================

【总PV】: 87399（仅业务请求，已排除静态资源）
【独立访客(IP)】: 96
【直接访问】: 2258 (1.3%)

【按 PV 趋势】
  08:00	530
  08:01	477
  ...

【按 流量趋势】
  08:00	8.87 MB
  08:01	5.60 MB
  ...

【状态码分布】
  200: 178206 (99.8%)
  400: 410 (0.2%)
  ...

【HTTP方法统计】
  GET: 94311 (52.8%)
  POST: 83930 (47.0%)

【URL类型分布】
  业务页面: 2132 (1.2%)
  其他: 176519 (98.8%)

【Top 20 URL】          【Top 20 客户端IP】     【Top 20 大流量URL】
  33374  /heartbeat      33810  [IP]             630.69  /sys/config/all
  ...                    ...                     ...

【错误请求】: 445 (4xx/5xx)
  400: 410 / 499: 30 / 404: 4 / 500: 1
【Top 20 404 URL】 / 【Top 20 5xx URL】 / 【Top 20 400 URL】

【响应时间统计】
  平均响应时间: 0.343 s
  最大响应时间: 182.319 s
  慢请求(>1s): 17926
  响应时间分布: <0.1s / 0.1-0.5s / 0.5-1s / 1-3s / 3-5s / >5s

【Top 20 慢请求URL】（慢请求平均耗时 / 慢请求次数 / URL）
【文件类型统计】xlsx / pdf / xls / mp4 / txt / json / html ...
【设备类型】移动端 / 桌面端 / 未知
【浏览器统计】Chrome / Edge / Firefox / Safari / 微信 ...
【Top 20 Referer域名】

【总流量】/【平均响应大小】/【平均QPS】
【响应性能】平均 / P50 / P90 / P95 / P99
【请求结果】成功率 / 服务端错误率 / 峰值QPS
```

口径说明：

- 默认**排除静态资源**：`-skip-static=true` 时 css/js/图片/字体等整行丢弃，不进入 PV / 分钟趋势 / 状态码 / IP / 流量 / 错误 / 慢请求；报告开头会写「已排除 N 条 / X MB，原始 M 条」方便对账
- **时间颗粒度固定为 1 分钟**：单日报告逐分钟输出 PV 与流量；跨多天每天一行（当天 PV / 流量 + 峰值分钟），附 24 小时分布
- `-from 08:00` 对应原脚本的 START_TIME（每天按 HH:MM 过滤，含边界）
  - 用 `-from` 时会**直接读日志重算**，不读取也不写回日记录/周汇总，避免把“半天数据”污染历史；因此也不做环比
  - `-from` 只允许用在 `weekly` / 单文件模式（`daily`、`serve` 会写历史，直接报错）
- `-top 20` 控制所有 Top 榜条数（默认 20，最大 200）
- 默认会去掉 URI 的 `?query`（同一接口不被随机参数拆成上万条）；要和原脚本一样保留原始 `request_uri`，加 `-uri-normalize=false`
- 响应耗时口径由 `-time-field` 决定：`auto`（默认，优先 `upstream_response_time`，没有才回退 `responsetime`）/ `upstream` / `response`；原脚本固定看 `responsetime`，要和脚本完全一致用 `-time-field response`
- 慢请求阈值由 `-slow-ms` 控制（默认 1000ms），报告里的「>1s / >500ms」会跟着变
- 客户端 IP 默认优先 `real_client`（CDN/代理场景）；日志里没有该字段或为空时自动回退 `client`，`-real-ip=false` 可关闭
- 「Top 慢请求URL」按**慢请求的平均耗时**排序，分母只算慢请求；这样偶发超时的接口不会被巨大的调用量摊薄
- `-exclude-bots` 默认关闭；开启后按 UA 整行丢弃爬虫/探针/监控请求，报告开头会单列排除条数（不会静默改数）
- 「文件类型统计」从**原始 request_uri（含 query）**识别 `name=xx.xlsx` 这类文件名，不依赖 URI 归一化；旧日记录没有该字段时退化为按 URL 路径后缀统计
- 「URL类型分布」：路径含 api 段的算「其他」，无扩展名 / `.html` 的算「业务页面」
- 分钟明细来源：直接读日志时当场统计；周报从日记录聚合时读 `minute_cnt`（新格式日记录已包含，**仍然是 1 分钟**）；
  升级前的旧日记录没有 `minute_cnt`，自动退化为 5 分钟粒度，更旧的再退化为小时
- `_ops.html` 的图表交互：滚轮缩放、拖拽平移、双击复位、`1小时 / 6小时 / 12小时 / 全天` 快捷键、悬停 tooltip；触屏支持单指拖动、双指缩放
- 页面为纯 HTML5（内联 CSS + Canvas + 原生 JS），**不依赖 ECharts/CDN/网络**，离线、内网、nginx 直接打开都能用
- 文件名带 `_ops.html` / `_ops.txt`，同时生成 `*_latest_ops.html` / `*_latest_ops.txt` 固定链接；HTML/PDF/Excel 仍是对外上报的那一份

## 十、问题检测规则

| 触发条件 | 级别 | 提示 |
|----------|------|------|
| 错误率 > `1.0%` | error | 错误率偏高 |
| 错误率 > `0.5%` 且 ≤ `1.0%` | warning | 错误率需关注 |
| **慢请求占比 > 5%** | error | 慢请求占比过高 |
| **慢请求占比 > 1%** | warning | 慢请求占比偏高 |
| 慢请求占比 > 0.1% 且次数 > `SlowAlert`(100) | info | 慢请求数量较多 |
| **平均响应 > 1000ms** | error | 平均响应时间过长 |
| 平均响应 > 500ms | warning | 平均响应偏慢 |
| **P99 > 5s** | warning | 长尾延迟严重 |
| P99 > 3s | info | 长尾延迟偏高 |
| **5xx 占比 > 0.5%** | error | 服务器错误（5xx）偏多 |
| 5xx 占比 > 0.1% 且 ≥ 10 次 | warning | 存在服务器错误（5xx） |
| 5xx ≥ 100 次（占比极低） | info | 服务器错误绝对值偏多 |
| 404 占比 > `5%` | info | 404 请求较多（死链/爬虫） |

> **慢请求按「占比」判定而不是次数**：日请求上千万的站点上"慢请求 100 次"毫无意义，
> 早期版本只要超过 100 次就报警，噪声太大（199 万次/8.3% 和 100 次/0.001% 报同一句话）。
> 次数阈值只在占比极低（<0.1%）时兜底使用。

阈值可在 `internal/detect/detect.go` 调整。错误统计口径：`status >= 400`。

---

## 十一、性能与低负载

### 11.0 资源占用实测（不会拖垮服务器）

设计前提是"和业务抢资源时必须让路"。所有指标都写进运行日志，随时可核对。

**① CPU：按时间片节流，只占一部分**

`-cpu-percent`（默认 50）每处理 4000 行检查一次，超出比例就 sleep。单天 140 万行 / 360MB 实测：

| `-cpu-percent` | wall | 进程 CPU 时间 | 说明 |
|---|---|---|---|
| `100` | 1.4 s | 1.4 s | 不限速，适合夜间低峰 |
| `50`（默认） | 2.8 s | 1.4 s | 占用正好 50% |
| `25`（生产推荐） | 6.1 s | 1.4 s | 占用约 25%，业务几乎无感 |

注意节流**不减少 CPU 总量**（都是 ~1.4 s），只是把同样的 CPU 摊到更长时间里 —— 峰值占用低、不和业务抢核、不触发 CPU 告警。

**② 内存：有硬上限，不随行数线性增长**

| 场景 | 峰值内存 | CPU 时间 |
|---|---|---|
| 单天 140 万行（daily 模式，出报告） | **42 MB** | 1.4 s |
| 单文件 140 万行（single 模式） | **30 MB** | 1.4 s |
| 单天 20 万行 / 2 万个不同接口（daily） | **35 MB** | 0.4 s |
| 一周日记录汇总（7 份日记录 → 周报） | **55 MB** | 0.5 s |
| 一周 7 天全量回退扫日志（7 × 20 万行） | **110 MB** | 2.3 s |

硬上限来自三处：接口/上游/客户端/状态每张表最多 **2 万 key**（超出归 `(其他)`）；逐秒峰值表最多 **20 万 key**（超出后改用 O(1) 流式计数，峰值依然准确）；字符串复用池最多 4096 条。

另外还设了 **`-max-mem-mb 256`**（堆内存软上限）：万一遇到畸形数据，Go 会在接近上限时主动 GC，而不是一路涨到把机器吃干净。正常数据只有几十 MB，碰不到这个限制。

> 周报的"回退扫日志"是唯一会把 7 天数据同时读进内存的路径（110 MB），正常生产周报走日记录（55 MB）。

**③ 优先级让路 + 串行执行**

- `-nice 19`（默认，Linux）：内核调度时业务进程永远优先
- 常驻模式任务**串行**，不会同时跑两个统计
- 多文件之间 `SleepSec`（0.5 s）间隔，给磁盘喘息
- 已算过的日期**幂等跳过**；周报默认读日记录、不读日志

**④ 多格式额外开销很小**

PDF/Excel 都是在内存里拼字节流，不需要外部程序：

| 输出 | 真实采样（6220 请求 / 5 天）| 说明 |
|---|---|---|
| HTML | 68 KB / <10 ms | 默认 |
| PDF | 70 KB / ~20 ms | A4 3 页 |
| XLSX | 15 KB / ~10 ms | 8 个工作表 |

**⑤ 日志里的资源账**

```
[INFO] big1400k.log 解析耗时 3.1s（118 MB/s，1400000 行）
[INFO] 完成！本次运行：1.7s CPU，42 MB 峰值内存，耗时 3.2s
[INFO] 定时任务 daily 完成（2026-11-01），累计消耗 1.8s CPU，42 MB 峰值内存
```

觉得还有压力就调 `-cpu-percent 20`，或把 `-daily-at` 挪到凌晨低峰。

### 11.1 实测数据（开发机 Ryzen 7 5800U，98MB 日志 ≈ 13.5 万行）

| 场景 | 优化前 | 现在 |
|---|---|---|
| 低基数日志（20k 行、无重复 URI/IP） | 369 MB/s | **≈654 MB/s（+77%）** |
| 真实形态（20k 行、每秒 200 行、300 接口） | 301 MB/s | **≈372 MB/s（+23%）** |
| 真实采样（98MB / 13.5 万行） | 354 MB/s | **531 MB/s** |
| 140 万行 / 36 万不同 URI / 360MB | 5.6 s（64 MB/s @50%） | **2.8 s（131 MB/s @50%）**，不限速 1.4 s（**290 MB/s**） |
| 单次运行 CPU 时间（360MB） | 1.7 s | **1.4 s** |
| 每行分配（20k 行样本） | 120,177 次 / 3.9 MB | **60,107 次 / 2.8 MB** |

优化点（都做了 A/B 实测，见下方"被否决的优化"）：

- **SIMD 定位字符串边界**：`scanFields` 里找字段名/值结束引号，从「逐字节循环」改成 `bytes.IndexByte`（AVX2 加速），遇到 `\"` 转义再往后找。字符串扫描从 37% CPU 降到 13%，整体 **+17%**（360MB 实测 251→293 MB/s）
- **`@timestamp` 行首快速路径**：nginx JSON 第一个字段就是它，直接按固定前缀取值，省掉一次全行 `bytes.Index` 搜索；形态不符时自动回退
- **单趟扫描**：一行只走一遍取全 `request_uri / status / client / method / size / upstream_*`，修掉 `\"` 转义截断
- **每行只写一张表**：扫描时只累加「当天」统计量，结束后汇总到文件级
- **接口/上游统计合并成一张表**：`map[string]*UriStat` 替代原来的 4 张表 —— 同一 URI 每行只哈希一次
- **小字段字符串复用**：`status / method / upstream_addr` 走 intern 表，每行少约 3 次分配
- **时间戳手写解析**：不用 `time.Parse`，时区缓存 `fixedZone`
- **读缓冲 1MB**：系统调用次数减少，Windows 上 profile 里 `cgocall` 占 15%，加大缓冲后 IO 占比下降约 2%
- 解析与 IO 分离（`ProcessReader`），使纯内存基准可复现，A/B 噪声从 ±15% 降到 ±3%

**被否决的优化（实测无收益，已回退）**：

| 候选 | 想法 | 实测 |
|---|---|---|
| 逐秒表「每秒写一次」 | 1.4M 行/天时把 map 写入从每行一次降到每秒一次 | 真实形态基准 14.66ms vs 14.33ms —— Go 的 `mapassign_fast64` 已经足够快，多出的分支反而抵消收益 |
| 接口统计一级缓存 | 复用上一行的 `*UriStat` 指针，命中时免哈希 | 14.66 vs 14.33ms 同样无收益；长 URI 的 `memcmp` 成本约等于一次哈希 |

结论：热点已经从"算法"转移到**内存读取本身**（`indexbytebody` 16% + 文件 IO 17%），继续优化需要换思路（如多核并行），但那会和"不拖垮服务器"的低负载目标冲突，因此停在这里。

### 11.2 CPU 节流（默认只占半个核）

每处理 4000 行结算一次，按目标占用率插入 sleep。**默认 `-cpu-percent 50`，即最多占用 0.5 个核**
（8 核机器上约占 6% 总量）。实测线性可控（单天 140 万行 / 360MB）：

| 参数 | wall | CPU 时间 | 实际占用 |
|---|---|---|---|
| `-cpu-percent 100`（不限速） | 1.5 s | 1.6 s | 100% |
| `-cpu-percent 50`（默认） | 3.6 s | 1.8 s | 50% |
| `-cpu-percent 25`（更保守） | 6.4 s | 1.7 s | 26% |

**节流不减少 CPU 总消耗，只是把它摊平**：同样 1.4 s 的 CPU，在 25% 档位下摊到 6.1 s 里，
业务随时能抢到核，也不会在监控上打出 CPU 尖峰。业务繁忙或核数少时建议 `-cpu-percent 20~30`。

### 11.3 跨日提前退出

日志通常按时间递增，扫描时连续 **2000 行**都「比结束日期更新」就停止扫描，避免为统计一天而读完整个
未切割的当天文件（实测该场景少扫 9 万行）。

> 如果日志**不是**时间有序（多 upstream 交错严重），用 `-early-exit 0` 关闭，避免漏数据。

### 11.4 其他低负载措施

1. **nice 19**：启动时 `setpriority`（Linux/macOS），与业务争 CPU 时主动让路
2. **文件间休眠**：每个文件处理完 sleep 0.5 秒
3. **单进程串行**：不并发，避免瞬时翻倍
4. **坏行容错**：解析失败计入 `skipped`，不中断
5. **周报不读日志**：默认只聚合日记录，IO 几乎为零
6. **常驻模式有重试上限**：不会整天反复扫大文件
7. **内存有硬上限**：接口/上游/客户端/状态每张表最多 2 万 key，逐秒峰值表最多 20 万 key，字符串复用池 4096 条；另有 `-max-mem-mb 256` 软上限兜底
8. **资源账写进日志**：每次运行结束打印「CPU 时间 / 峰值内存 / 耗时」，常驻模式每个任务结束也打印

### Q16：报告里的峰值数字怎么自查？

每个峰值都给了三种口径，互相能对上账：

- **每次运行日志**：`解析耗时 3.1s（118 MB/s，1400000 行）`、`本次运行：1.7s CPU，42 MB 峰值内存`
- **5 分钟峰值**：`峰值 15:05 · 6.8 万次（≈226 req/s，占全天 1.98%）`
  —— `6.8 万 ÷ 300 秒 = 226 req/s`，应当和顶部「峰值 QPS」卡片同一量级（卡片是逐秒峰值，只会更高）
- **小时峰值**：`峰值 15:00–16:00 · 12 万次（占全天 35.2%）`

如果「占全天比例」明显不合理（比如 40% 却只占 1/288 时间，或 5 分钟峰值折算的 req/s 比逐秒峰值还高），
说明日记录的时段数据有问题：删掉 `history/daily/<那天>.json` 重新 `-mode daily` 跑一次即可。

## 十二、常见问题（FAQ）

**Q1: 报「没有找到日志文件」？**
检查 `-logdir` 与 `-pattern`，先 `ls -lh /data/_log/backup/` 确认文件在、名字能匹配。

**Q2: 报「统计区间 X 没有任何有效记录」？**
说明文件在，但里面没有该日期的数据。错误信息会直接告诉你实际情况，例：

```
[ERROR] 统计区间 2026-11-02 没有任何有效记录（区间内 0 行，区间外 135261 行）；
        日志文件内实际记录范围 2026-07-16 ~ 2026-10-30
```

常见原因：日志还没切割完（等一会儿或把 `-daily-at` 调晚）、`-date` 选错日期（
按上面的实际范围选一天）、机器时区与日志时区不一致。

**Q3: 只统计某一天 / 今天怎么做？**
`-mode daily -date 2026-10-29`（默认是昨天）；统计当天用 `-date today`，
当天记录会标记 `partial`，不会污染周报。

**Q4: 环比显示「-」？**
上周的周汇总 JSON 不存在（首次运行或历史被清理）。连续运行一周后即可显示。

**Q5: 支持 `.tar.gz` 吗？**
只支持单个 `.gz`。`.tar.gz` 请先解压出 `.log` / `.log.*.gz` 再统计。

**Q6: 历史会无限增长吗？**
不会。周汇总与日记录都按 90 天（`KeepDays`）自动清理。

**Q7: 日志已被轮转删除，还能出周报吗？**
能。周报只依赖 `history/daily/`；只有日记录缺失的那几天才回退读日志。

**Q8: 周报里的 P99 准吗？**
响应时间按 10ms 一桶做直方图统计，P50/P90/P99 由桶内插值得到：0~10s 误差不超过 10ms；
10s 以上用溢出桶（10~60s 按 1s、60~600s 按 10s、>600s 单独一桶）继续插值，不会被截断成 10s。
平均响应是精确值。这样日记录体积小、周报可合并。

**Q9: 常驻模式会重复统计吗？**
不会。以「成果文件是否存在」判断完成，成功后同周期不再执行；手动 `-mode daily -date X` 是覆盖写，幂等。

**Q10: 常驻进程挂了怎么办？**
systemd 的 `Restart=always` 自动拉起；拉起后调度器立刻检查，缺日记录就补跑、缺周报就补生成。

**Q11: 常驻模式日志一直提示 daily 失败？**
按 `-retry-min` 重试到 `-max-retry` 次后会放弃本周期。检查：切割时间是否晚于 `-daily-at`（可把
`-daily-at` 调到 00:20）、`-pattern` 是否匹配、日志时间戳时区。

**Q12: 我把日志文件路径传给 `-logdir` 了，会怎么处理？**
只处理你这**一个文件**（不会去匹配同目录的其他文件），统计它里面的全部数据，
报告周期取文件内实际首末日期，输出 `single_report_<起>_to_<止>.html`；
不写历史、不覆盖 `weekly_report_latest.html`。详见 [7.7](#77-单文件模式直接给一个日志文件)。
给**目录**才是「按一周处理」。

**Q15: 日志被压缩成 .gz 了，还能统计吗？**
能。程序按文件后缀自动识别 `.gz` 并流式解压（不落临时文件），明文中转、gz、带日期、带序号、以及混合目录都支持；
`-pattern` 只要匹配到文件名即可，例如 `-pattern '*.log*.gz'`。详见 [四、日志文件是怎么被选中的](#四环境要求与日志格式)。

**Q14: 程序自己的运行日志在哪？会一直变大吗？**
默认 `<reportdir>/logs/nginx-report-YYYY-MM-DD.log`，**按天轮转、保留 30 天**，不会无限增长；
`-logfile` 改路径，`-log-keep-days` 改保留天数，`-logfile off` 关闭。详见 [9.1](#91-运行日志按天轮转)。

**Q13: 能看历史趋势吗？**
`history/` 下每次周汇总的 JSON 都在，可直接做二次分析（也可作为后续接入图表/数据库的数据源）。

---

## 十三、开发与扩展

```bash
go vet ./...
go test ./...                                   # 单元测试
go test ./internal/analyzer -bench . -benchmem   # 解析性能基准
```

### 13.1 包职责

| 包 | 职责 | 修改入口 |
|----|------|---------|
| `internal/accesslog` | 日志格式识别、combined / 自定义 log_format / CSV 行解析 | 新增日志字段/格式 |
| `internal/analyzer` | 日志解析（JSON 快速路径 + 文本适配）、`Agg` 累加、响应时间直方图、按天归集、CPU 节流 | 新增统计维度 |
| `internal/detect` | 问题检测规则 | 新增告警规则 |
| `internal/report` | HTML 模板与渲染 | 调整报告布局 |
| `internal/history` | 日记录/周汇总存取、环比、超期清理 | 调整存储策略 |
| `internal/scheduler` | 常驻模式周期调度 | 调整调度策略 |
| `internal/sysctl` | 进程优先级 | 调整降载策略 |
| `internal/logger` | 分级日志 | 调整日志格式 |
| `internal/config` | 配置默认值 | 调整阈值/路径 |

### 13.2 新增一个统计维度

1. `internal/analyzer` 的 `Agg` 加字段，并在 `Agg.add` / `Agg.MergeFrom` 中统计与合并
2. 若要新增**日志字段**（如 `http_user_agent`）：
   - JSON：在 `fieldKindCount` 前加下标常量，在 `lineFields.setByKey` 里加一条 `case 长度`，`scanFields` 一次扫描就能取到（不要新增"每个字段扫一遍"的写法）
   - combined/自定义/CSV：在 `internal/accesslog/fields.go` 加字段，在 `template.go` 的 `nginxVarField`、`csv.go` 的 `headerField` 加映射，再在 `populateLineFields` 里接到 analyzer
3. `internal/history` 的 `Daily` 加同名 JSON 字段，并在 `NewDaily` / `ToAgg` 中带上
4. `internal/report` 的 `report.Input` 不需要改，直接在 `HTML` 里渲染新维度；缺数据时记得自动隐藏该板块

> 关键一：新维度必须同时支持「累加」与「合并」，否则「日记录 → 周报」会丢数据。
>
> 关键二：所有来自日志的字符串（URI、IP、上游地址、UA）渲染前必须 `html.EscapeString`，
> `internal/report/html_test.go` 里有注入用例，新增表格时请一并覆盖。

### 13.3 部署到服务器

```bash
# 1. 上传二进制
scp nginx_weekly_report root@服务器IP:/opt/nginx-report/

# 2. 赋执行权限
chmod +x /opt/nginx-report/nginx_weekly_report

# 3. 试运行一次（建议先补最近一周的日记录）
/opt/nginx-report/nginx_weekly_report -mode daily -date 2026-10-29 -logdir /data/_log/backup -reportdir /data/reports

# 4. 注册 systemd 服务（见 8.2）或配置 crontab（见 7.4）
```

---

## 十四、License

内部运维工具，供项目组使用。
