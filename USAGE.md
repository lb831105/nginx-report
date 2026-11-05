# nginx-report 使用样例

本文按使用场景列出可直接复制的命令。示例按 Linux/macOS bash 编写；Windows 将 `./nginx_weekly_report` 换成 `nginx_weekly_report_win.exe` 即可。

```bash
BIN=./nginx_weekly_report
LOG=/data/_log
REPORT=/data/reports
PATTERN='*.log*'
```

> 建议先加 `-preview` 试跑：只出报告，不读写历史。

---

## 一、单文件

### 1.1 统计一个日志文件

```bash
$BIN -logdir /data/_log/ad.kmrd.cn/access.log \
  -reportdir $REPORT
```

输出：

```text
single_report_<起>_to_<止>.html
single_report_<起>_to_<止>_ops.html
single_report_<起>_to_<止>_ops.txt
```

单文件模式不会覆盖 `weekly_report_latest*`，也不会写日记录/周汇总。

### 1.2 统计一个 `.gz`

```bash
$BIN -logdir /data/_log/backup/access.log-2026-11-04.gz \
  -reportdir $REPORT
```

### 1.3 一个文件只统计某一天

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup/access.log-2026-11-04.gz \
  -start 2026-11-04 -end 2026-11-04 \
  -preview \
  -reportdir $REPORT
```

### 1.4 单文件只统计每天 08:00 之后

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup/access.log \
  -start 2026-11-04 -end 2026-11-04 \
  -from 08:00 -preview \
  -reportdir $REPORT
```

### 1.5 单文件指定站点名

适用于 `/tmp/access.log` 这种父目录不是域名的文件：

```bash
$BIN -logdir /tmp/access.log \
  -site-name ad.kmrd.cn \
  -site-report per \
  -reportdir $REPORT
```

### 1.6 单文件从父目录推断站点

```bash
$BIN -logdir /data/_log/ad.kmrd.cn/access.log \
  -site-from dir \
  -site-report per \
  -reportdir $REPORT
```

输出：

```text
$REPORT/sites/domain/ad.kmrd.cn/single_report_<起>_to_<止>.html
```

---

## 二、目录 / 多天日志

### 2.1 daily：统计昨天并写日记录

```bash
$BIN -mode daily \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT
```

### 2.2 daily：指定日期

```bash
$BIN -mode daily \
  -date 2026-11-04 \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT
```

### 2.3 weekly：默认统计上一个完整周

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT
```

### 2.4 weekly：指定日期范围

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -start 2026-10-26 -end 2026-11-01 \
  -reportdir $REPORT
```

### 2.5 weekly：直接读日志，不读写历史

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -start 2026-10-26 -end 2026-11-01 \
  -preview \
  -reportdir $REPORT
```

### 2.6 weekly：只统计每天 11:30 之后

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -start 2026-10-26 -end 2026-11-01 \
  -from 11:30 \
  -preview \
  -reportdir $REPORT
```

### 2.7 目录模式只看今天正在写入的日志

```bash
$BIN -mode weekly \
  -logdir /data/_log \
  -pattern '*.log*' \
  -start "$(date +%F)" -end "$(date +%F)" \
  -preview \
  -reportdir $REPORT
```

---

## 三、日志格式

### 3.1 JSON 自动识别

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT
```

### 3.2 自定义 JSON 字段名

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -json-fields 'ts=time,cip=client,u=uri,st=status,sz=size,rt=response_time,dom=domain' \
  -reportdir $REPORT
```

常用目标字段：

```text
time client real_client method uri request status size
upstream_addr upstream_status upstream_time response_time
agent referer site/domain
```

### 3.3 nginx 默认 combined 自动识别

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern 'access.log*' \
  -reportdir $REPORT
```

### 3.4 combined + 末尾 host

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern 'access.log*' \
  -log-format combined_host \
  -site-from field \
  -reportdir $REPORT
```

### 3.5 自定义 `log_format`

把 nginx 配置里的格式原样传入：

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -log-format '$time_iso8601 $remote_addr "$request" $status $body_bytes_sent $request_time $upstream_response_time "$http_referer" "$http_user_agent" "$host"' \
  -reportdir $REPORT
```

模板必须包含 `$time_local` 或 `$time_iso8601`。变量含空格时要用双引号包住。

### 3.6 CSV：带表头

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.csv*' \
  -log-format csv \
  -reportdir $REPORT
```

表头示例：

```csv
time,client,method,uri,status,size,request_time,referer,agent,domain
```

### 3.7 CSV：没有表头，用 `-csv-fields`

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.csv*' \
  -log-format csv \
  -csv-fields 'time,client,method,uri,status,size,upstream_time,referer,agent,domain' \
  -reportdir $REPORT
```

### 3.8 CSV：首行是表头但列名无法识别

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.csv*' \
  -log-format csv \
  -csv-fields 'time,client,uri,status,size' \
  -csv-skip-header \
  -reportdir $REPORT
```

---

## 四、多站点

不传任何 `-site-*` / `-site` 时，完全走旧的全站统计路径。

站点识别优先级：

```text
-site-name
  > 行内 domain/host/http_host/server_name 等字段
  > -site-map
  > 文件名推断
  > 目录名推断
  > (未识别站点)
```

### 4.1 一个域名一个目录

目录结构：

```text
/data/_log/
├── ad.kmrd.cn/access.log
├── app.kmrd.cn/access.log
└── www.kmrd.cn/access.log
```

命令：

```bash
$BIN -mode weekly \
  -logdir /data/_log \
  -pattern '*.log*' \
  -start 2026-11-04 -end 2026-11-04 \
  -site-from dir \
  -site-report per \
  -site-ip-report group \
  -preview \
  -reportdir $REPORT
```

站点模式下 `-pattern` 不含 `/` 时会递归扫描子目录。

输出：

```text
$REPORT/sites/domain/ad.kmrd.cn/weekly_report_<起>_to_<止>.html
$REPORT/sites/domain/app.kmrd.cn/weekly_report_<起>_to_<止>.html
$REPORT/sites/domain/www.kmrd.cn/weekly_report_<起>_to_<止>.html
$REPORT/sites_index.html
$REPORT/sites_index.json
```

### 4.2 一个域名一个文件，文件名带域名

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-from filename \
  -site-report per \
  -reportdir $REPORT
```

文件名示例：

```text
ad.kmrd.cn_2026-11-04.log
app.kmrd.cn_2026-11-04.log
1.2.3.4.log
```

### 4.3 用 `sites.conf` 映射

`/data/reports/sites.conf`：

```ini
ad.kmrd.cn = ad.kmrd.cn*.log
app.kmrd.cn = /data/_log/app_access.log
www.kmrd.cn = /data/_log/www/*.log
```

相对路径基于 `-logdir`，支持 `* ? []`。

命令：

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-map /data/reports/sites.conf \
  -site-report both \
  -site-ip-report per-ip \
  -reportdir $REPORT
```

### 4.4 站点别名

`/data/reports/aliases.conf`：

```ini
www.kmrd.cn = kmrd.cn
shop.kmrd.cn = kmrd.cn
```

命令：

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-map /data/reports/sites.conf \
  -site-alias-map /data/reports/aliases.conf \
  -site-strip-www \
  -reportdir $REPORT
```

### 4.5 JSON 行内有 domain/host

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-from auto \
  -site-field domain \
  -site-report both \
  -site-ip-report group \
  -reportdir $REPORT
```

自动尝试字段：

```text
domain host http_host server_name vhost site
website site_name server request_host
```

### 4.6 强制所有日志归到一个站点

```bash
$BIN -mode weekly \
  -logdir /data/_log/ad.kmrd.cn \
  -pattern '*.log*' \
  -site-name ad.kmrd.cn \
  -site-report per \
  -reportdir $REPORT
```

### 4.7 IP 直连分开统计

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-from auto \
  -site-ip-mode separate \
  -site-ip-report per-ip \
  -reportdir $REPORT
```

### 4.8 IP 直连合并成一份报告

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-from auto \
  -site-ip-report group \
  -reportdir $REPORT
```

输出：

```text
$REPORT/sites/ip/_group/...
```

### 4.9 IP 直连归到文件站点

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-map /data/reports/sites.conf \
  -site-ip-mode inherit \
  -reportdir $REPORT
```

### 4.10 忽略行内 IP

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*' \
  -site-from field \
  -site-field host \
  -site-ip-mode ignore \
  -reportdir $REPORT
```

### 4.11 只生成指定站点的独立报告

仅支持 weekly / 单文件；全站报告仍展示所有站点，只有 `sites/` 下的独立报告按此过滤：

```bash
$BIN -mode weekly \
  -logdir /data/_log \
  -pattern '*.log*' \
  -start 2026-11-04 -end 2026-11-04 \
  -site-from dir \
  -site ad.kmrd.cn,www.kmrd.cn \
  -site-report per \
  -reportdir $REPORT
```

### 4.12 站点报告范围

只出全站：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -site-from dir \
  -site-report aggregate \
  -reportdir $REPORT
```

全站 + 每站点：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -site-from dir \
  -site-report both \
  -reportdir $REPORT
```

只出每站点：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -site-from dir \
  -site-report per \
  -reportdir $REPORT
```

### 4.13 限制站点数量和独立报告数量

```bash
$BIN -mode weekly \
  -logdir /data/_log \
  -pattern '*.log*' \
  -site-from dir \
  -site-max 500 \
  -site-min-pv 100 \
  -site-max-report 50 \
  -site-report per \
  -reportdir $REPORT
```

### 4.14 站点模式单文件

```bash
$BIN -logdir /data/_log/ad.kmrd.cn/access.log \
  -site-from dir \
  -site-report both \
  -site-ip-report per-ip \
  -reportdir $REPORT
```

---

## 五、输出格式

### 5.1 默认 HTML

```bash
$BIN -mode weekly -logdir $LOG -pattern $PATTERN -reportdir $REPORT
```

### 5.2 HTML + PDF + XLSX

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -format all \
  -reportdir $REPORT
```

### 5.3 只出 PDF

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -format pdf \
  -reportdir $REPORT
```

### 5.4 只出 XLSX

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -format xlsx \
  -reportdir $REPORT
```

### 5.5 同时出 HTML + PDF

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -format html,pdf \
  -reportdir $REPORT
```

### 5.6 不出运维报告

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -ops=false \
  -reportdir $REPORT
```

### 5.7 Ops 报告 Top 50

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -ops \
  -top 50 \
  -reportdir $REPORT
```

### 5.8 自定义报告标题

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -title '某某系统运行周报' \
  -reportdir $REPORT
```

### 5.9 多站点 + 全格式

```bash
$BIN -mode weekly \
  -logdir /data/_log \
  -pattern '*.log*' \
  -site-from dir \
  -site-report both \
  -site-ip-report group \
  -format all \
  -ops \
  -reportdir $REPORT
```

输出包含：

```text
全站 HTML/PDF/XLSX
每站点 HTML/PDF/XLSX
每站点 Ops txt/html
sites_index.html
sites_index.json
```

---

## 六、统计口径

### 6.1 CDN/代理后取真实 IP

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -real-ip=true \
  -reportdir $REPORT
```

只用 `client`：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -real-ip=false \
  -reportdir $REPORT
```

### 6.2 按端到端耗时

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -time-field response \
  -reportdir $REPORT
```

只用上游耗时：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -time-field upstream \
  -reportdir $REPORT
```

### 6.3 500ms 判慢

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -slow-ms 500 \
  -reportdir $REPORT
```

### 6.4 排除爬虫/探针

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -exclude-bots \
  -reportdir $REPORT
```

### 6.5 不排除静态资源

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -skip-static=false \
  -reportdir $REPORT
```

### 6.6 接口 Top 表包含静态资源

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -static-exclude=false \
  -reportdir $REPORT
```

### 6.7 保留 URI 的 query

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -uri-normalize=false \
  -reportdir $REPORT
```

### 6.8 综合精准口径

```bash
$BIN -mode weekly \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -real-ip=true \
  -time-field response \
  -slow-ms 500 \
  -exclude-bots \
  -skip-static=true \
  -top 50 \
  -format all \
  -reportdir $REPORT
```

> 修改 `-slow-ms`、`-time-field`、`-real-ip`、`-exclude-bots`、`-skip-static` 后，
> 已经从日记录生成的历史口径不会自动改变，需要重跑对应日期。

---

## 七、常驻服务

### 7.1 默认调度

```bash
$BIN -mode serve \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT
```

默认：每天 `00:10` 统计昨天，每周一 `08:00` 汇总上周。

### 7.2 自定义每日/每周时间

```bash
$BIN -mode serve \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT \
  -daily-at 00:10 \
  -weekly-day 1 \
  -weekly-at 08:00
```

### 7.3 失败重试

```bash
$BIN -mode serve \
  -logdir /data/_log/backup \
  -pattern '*.log*.gz' \
  -reportdir $REPORT \
  -retry-min 10 \
  -max-retry 12
```

### 7.4 每 5 分钟刷新今天部分统计

```bash
$BIN -mode serve \
  -logdir /data/_log \
  -pattern '*.log*' \
  -reportdir $REPORT \
  -today-every 5
```

### 7.5 常驻 + 多站点

```bash
$BIN -mode serve \
  -logdir /data/_log \
  -pattern '*.log*' \
  -site-from dir \
  -site-report both \
  -site-ip-report group \
  -reportdir $REPORT
```

---

## 八、性能 / 资源

### 8.1 限制 CPU 占用

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -cpu-percent 30 \
  -reportdir $REPORT
```

`100` 表示不限速：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -cpu-percent 100 \
  -reportdir $REPORT
```

### 8.2 限制内存

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -max-mem-mb 128 \
  -reportdir $REPORT
```

### 8.3 调整跨日提前退出

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -early-exit 5000 \
  -reportdir $REPORT
```

关闭：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -early-exit 0 \
  -reportdir $REPORT
```

### 8.4 设置 nice 值

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -nice 19 \
  -reportdir $REPORT
```

### 8.5 控制运行日志

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -logfile /var/log/nginx-report.log \
  -log-keep-days 30 \
  -reportdir $REPORT
```

关闭运行日志：

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -logfile off \
  -reportdir $REPORT
```

### 8.6 调试

```bash
$BIN -mode weekly \
  -logdir $LOG -pattern $PATTERN \
  -debug \
  -preview \
  -reportdir $REPORT
```

---

## 九、构建

### 9.1 Linux 静态编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
go build -trimpath -ldflags "-s -w" \
  -o nginx_weekly_report \
  ./cmd/nginx-report
```

### 9.2 Windows

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
go build -trimpath -ldflags "-s -w" \
  -o nginx_weekly_report_win.exe \
  ./cmd/nginx-report
```

### 9.3 Windows PowerShell

```powershell
.\build.ps1 -Windows -Clean
```

### 9.4 本地试跑

```bash
go run ./cmd/nginx-report \
  -logdir ./28888_access_json.log \
  -reportdir ./test_reports \
  -preview
```

---

## 十、最常用组合速查

```bash
# 单文件
$BIN -logdir /path/access.log -reportdir /data/reports

# 昨日日统计
$BIN -mode daily -logdir /data/_log/backup -pattern '*.log*.gz' -reportdir /data/reports

# 上周周报
$BIN -mode weekly -logdir /data/_log/backup -pattern '*.log*.gz' -reportdir /data/reports

# 复盘某一天
$BIN -mode weekly -logdir /data/_log/backup -pattern '*.log*.gz' \
  -start 2026-11-04 -end 2026-11-04 -preview -reportdir /data/reports

# 多域名目录，每域名一份报告
$BIN -mode weekly -logdir /data/_log -pattern '*.log*' \
  -start 2026-11-04 -end 2026-11-04 \
  -site-from dir -site-report per -site-ip-report group \
  -preview -reportdir /data/reports

# 完整报告
$BIN -mode weekly -logdir /data/_log/backup -pattern '*.log*.gz' \
  -format all -ops -top 50 -reportdir /data/reports
```

---

## 十一、参数速查

| 参数 | 说明 |
|---|---|
| `-mode` | `daily` / `weekly` / `serve` |
| `-date` | daily 日期：`YYYY-MM-DD` / `today` / `yesterday` |
| `-start` / `-end` | weekly/单文件日期区间 |
| `-from` | 只统计每天 `HH:MM` 之后；仅 weekly/单文件 |
| `-preview` | 只出报告，不读写历史；仅 weekly/单文件 |
| `-logdir` | 日志目录或单个日志文件 |
| `-pattern` | 文件名匹配模式；站点模式下不含 `/` 时递归 |
| `-reportdir` | 报告目录 |
| `-format` | `html` / `pdf` / `xlsx` / `all`，可逗号组合 |
| `-ops` | 是否输出 `*_ops.txt/html` |
| `-top` | Ops 报告 Top 条数 |
| `-title` | 报告标题 |
| `-log-format` | `auto` / `json` / `combined` / `combined_host` / `csv` / nginx 模板 |
| `-json-fields` | JSON 字段映射 |
| `-csv-fields` | CSV 列顺序 |
| `-csv-skip-header` | 强制跳过 CSV 表头 |
| `-real-ip` | 优先 `real_client` |
| `-time-field` | `auto` / `upstream` / `response` |
| `-slow-ms` | 慢请求阈值毫秒 |
| `-exclude-bots` | 排除爬虫/探针 |
| `-skip-static` | 解析期排除静态资源 |
| `-static-exclude` | 接口 Top 表排除静态资源 |
| `-uri-normalize` | 去掉 URI query |
| `-site-map` | 站点文件映射 |
| `-site-alias-map` | 站点别名映射 |
| `-site-name` | 强制站点 |
| `-site-from` | `auto` / `field` / `map` / `filename` / `dir` |
| `-site-field` | 行内站点字段名 |
| `-site-report` | `aggregate` / `both` / `per` |
| `-site` | 只生成指定站点的独立报告（全站报告仍展示全部站点）；仅 weekly/单文件 |
| `-site-ip-mode` | `separate` / `inherit` / `ignore` |
| `-site-ip-report` | `none` / `group` / `per-ip` |
| `-site-strip-www` | `www.example.com` 归并到 `example.com` |
| `-site-max` | 最大站点数 |
| `-site-min-pv` | 独立报告最小 PV |
| `-site-max-report` | 最多独立站点报告数 |
| `-cpu-percent` | 目标 CPU 占用 |
| `-max-mem-mb` | 堆内存软上限 |
| `-early-exit` | 跨日提前退出阈值 |
| `-nice` | 进程 nice 值 |
| `-logfile` | 运行日志路径，`off` 关闭 |
| `-log-keep-days` | 运行日志保留天数 |
| `-debug` | DEBUG 日志 |

完整参数以命令行为准：

```bash
$BIN -h
```
