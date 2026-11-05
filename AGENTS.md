# AGENTS.md

本文件是编码代理在本仓库工作的约定。先读 README 和相关代码；冲突时以代码/测试为准，并同步修正本文件。

## 项目

`nginx-report`：纯 Go、零第三方依赖、Go 1.21+、`CGO_ENABLED=0`。解析 nginx JSON / combined / 自定义 `log_format` / CSV（支持 `.gz`），输出客户报告、运维 `*_ops.txt/html`、PDF/XLSX、日记录/周汇总；可按域名/IP 拆分多站点报告。要求低 CPU/低内存、离线可用、数字可对账。

## 常用命令

```bash
go test ./...
go vet ./...
gofmt -w ./cmd ./internal

# 性能基准，别和重负载任务并行
go test ./internal/analyzer -run '^$' -bench . -benchmem

# 构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o nginx_weekly_report ./cmd/nginx-report
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o nginx_weekly_report_win.exe ./cmd/nginx-report
```

Windows PowerShell 用 `.\build.ps1 -Windows -Clean` 或 `.\build.ps1 -Clean`。`build.ps1` 必须保持纯 ASCII。

试跑用临时目录或 `test_reports/`：

```bash
go run ./cmd/nginx-report -logdir ./28888_access_json.log -reportdir ./test_reports -preview
```

## 包职责

| 路径 | 职责 |
|---|---|
| `cmd/nginx-report` | CLI、模式分发、文件选择、流程编排、落盘 |
| `internal/accesslog` | 格式识别；combined/模板/CSV 解析 |
| `internal/analyzer` | JSON 快速路径、`Agg` 累加/合并、直方图、节流、站点拆分 |
| `internal/site` | 站点归一化/别名/文件映射/文件名推断/安全目录 |
| `internal/staticasset` | 静态资源识别（解析和报告共用） |
| `internal/history` | 日记录/周汇总、旧格式兼容、清理 |
| `internal/detect` | 告警规则 |
| `internal/report` | `snapshot`、HTML/PDF/XLSX/Ops 渲染 |
| `internal/pdf` / `internal/xlsx` | 纯 Go 文件生成 |
| `internal/fsutil` | 原子写 |
| `internal/logger` | 分级日志、轮转 |
| `internal/scheduler` | `serve` 调度/重试 |
| `internal/sysctl` / `sysinfo` | 平台相关降载/资源统计 |

## 数据流

`main` 读配置 → 选文件 → `analyzer.ProcessFileWithOptions` → `ProcessReader`（每个文件只识别一次格式）→ 按天 `Agg`；站点开启时同时维护 `SiteStats` → `daily` 存 `history.Daily`（根记录 + `sites` 子记录），`weekly` 优先合并日记录、缺失日回退日志 → `report.buildSnapshot` → 全站报告 + 站点索引 + 每站点报告。所有报告/历史用 `fsutil.WriteFileAtomic`。

## 硬约束

### 解析

- JSON 默认字段必须继续走 `scanFields` 单趟扫描；别名只在 `jsonNeedsAlias` 需要时启用，最多检查前 20 个有效行。
- 不在 JSON 热路径加正则、逐字段扫描或 `time.Parse`；性能改动要 A/B。
- `accesslog.Fields` / `lineFields` 的值只在当前行有效，跨行保存必须复制。
- `scanFields` 与 `scanFieldsAlias` 必须同步；已有 `TestScanFieldsAliasMatchesDefault`。
- 首行 UTF-8 BOM 要兼容；同一文件保持同一 schema。
- `accesslog.Format.Parse` 对 JSON 固定返回 false；JSON 只能走 analyzer。
- CSV 支持表头、`-csv-fields`、`-csv-skip-header`、引号内逗号/`""`/换行；跨行逻辑在 `csvRecordComplete`，不要绕开。
- combined 单浮点扩展字段有歧义时输出 `Format.Warning`；非标准组合要求用户传 `-log-format`。
- 自定义模板必须含 `$time_local` 或 `$time_iso8601`；相邻变量无分隔符应报错。

### 统计口径

- `-skip-static` 是解析期过滤；静态资源单独计 `StaticSkipped`/`StaticBytes`，规则只能复用 `internal/staticasset`。
- 只有带耗时的请求进入平均响应、P50~P99、慢请求和 `TimeBuckets`；无耗时字段不能按 0ms 计。
- `NaN`/`Inf`/负数耗时必须丢弃或钳制；`Agg.add` 和解析层都要防护。
- `Agg.Timed()`：新记录用 `TimedCount`，旧记录兼容回退；整份日志无耗时时返回 0，不能伪造 P99。
- `TimeBuckets` 覆盖 0~10s，`TimeOverflow` 覆盖长尾；改桶边界要同步 analyzer/history/snapshot/报告。
- 慢请求严格 `> slowMS/1000`；阈值文案跟随配置。
- 默认 real IP：`real_client` 优先，空/`-` 回退 `client`；独立 IP 排除 `analyzer.OverflowKey`。
- `MinuteCnt` 是按天明细；文件级同一分钟相加无业务意义。

### 容量

- 已有上限：`maxMapKeys=20000`、`maxSecondKeys=200000`、`internLimit=4096`；新增高基数维度必须设计溢出桶 `(其他)`。
- URI/client/upstream/referer/status/method 有字段长度上限；CSV 有列数上限，防止内存放大。
- 合并日记录时也要执行上限保护。
- CPU 节流、跨日提前退出、文件间休眠不要默认去掉；保持单进程串行。

### 历史与报告

- `history.Daily` 新字段要同步 `NewDaily`/`ToAgg`/`foldLegacy`，用 `omitempty` 兼容旧记录。
- 新统计量必须同时支持 `Agg.add` 和 `Agg.MergeFrom`；补“日记录 → 周报”测试。
- 日记录不保存口径参数；改 `-slow-ms`/`-time-field`/`-real-ip`/`-exclude-bots`/`-skip-static` 后需重跑对应日期。
- 所有日志派生字符串写 HTML 前必须转义且只转义一次；纯文本/终端输出要清理 ASCII 控制字符。
- 空统计不得出现 `NaN`/`Inf`；无数据板块自动隐藏。
- 单文件模式只能写 `single_report_*`，不能覆盖 `weekly_report_latest*`。
- HTML 图表保持零依赖、离线可用；PDF/XLSX 不得依赖外部程序。

### 多站点

- 默认不开启：未显式传 `-site-*` / `-site` 时不得解析站点字段、不得建站点 map，保持旧路径零额外开销。
- 识别优先级：`-site-name` > 行内字段（`domain/host/http_host/server_name` 等）> `-site-map` > 文件名 > 目录名 > `(未识别站点)`；禁止用 URI/Referer/UA 猜域名。
- 站点模式下 `-pattern` 不含路径分隔符时递归扫描子目录（适配 `/data/_log/<domain>/access.log`）；非站点模式仍只扫顶层。
- 目录名推断只认合法域名/IP，`logs`、`backup`、日期目录等通用名不能当站点。
- 站点识别失败绝不丢行；全站 PV 必须等于各站点 PV 之和（含 unknown / other）。
- 同一行多个域名取第一个合法值，计 `MultiDomainLines`，不能重复计数。
- 行内 IP 默认 `separate` 单独归类，不混入域名；`inherit`/`ignore` 只改归属，不改总数。
- 站点 key 使用 `d:` / `i:` / `o:` / `u:` 前缀；目录用 `site.SafeDir`，禁止把原始域名直接拼路径。
- `SiteMax` 超出归 `o:other`；新增站点级高基数维度同样要溢出桶。
- 日记录站点子记录用 `omitempty`；旧记录没有 `sites` 时周报回退日志重算。
- 站点报告同样支持 HTML/PDF/XLSX/Ops，所有站点名必须转义；IP 直连默认不生成独立目录（`-site-ip-report` 控制）。

### CLI

- 新参数同步：`config` → `main` flag → 启动校验 → `processOptions` → `usage()` → README。
- `-format` 是报告格式；`-log-format` 是输入日志格式。
- `-site-report` 是站点报告范围；`-site-from` 是站点识别来源；`-site-ip-report` 只控制 IP 独立报告。`-site` 过滤仅允许 weekly / 单文件。
- `-from`/`-preview` 只允许 weekly / 单文件；daily/serve 不能用过滤后数据写历史。
- 参数错误启动即退出；目录模式只接受普通文件。
- `config.Default()` 是生产默认值；`analyzer.Options{}` 的零值不是生产默认值。

## 修改流程

1. 先补/定位测试，再做最小改动。
2. 新增日志字段：`accesslog/fields.go` → combined/template/csv 映射 → analyzer 的 `lineFields`/`scanFields`/`populateLineFields` → 测试。
3. 新增统计量：`Agg` 字段 → `NewAgg`/`add`/`MergeFrom` → `Daily`/`NewDaily`/`ToAgg` → `snapshot`/报告 → 回归测试。
4. 新增报告板块：`snapshot` 计算 → HTML/PDF/XLSX/Ops 同步 → 转义/空数据测试。
5. 格式/参数变化：更新 `usage()` 和 README。

## 验证

```bash
go test ./...
go vet ./...
```

性能改动至少跑一次基准；站点关闭时 95MB fixture 吞吐不得明显低于旧版。已知 95MB fixture 回归基线：PV `87399`、错误 `5149`、慢请求 `331`、静态排除 `47862 条 / 9.86 GB`、原始 `135261` 行；日志变化时以实际为准。

## 工作区

- 当前工作区可能没有 `.git`，不要依赖 `git diff/log`。
- `28888_access_json.log` 是只读 fixture；不要截断/覆盖。
- 根目录二进制、`reports/`、`test_reports/`、`*_report_*` 是产物，不要手改。
- 测试不要写生产 `history/`；用 `t.TempDir()` 或 `test_reports/`。
- 不依赖网络；PDF/XLSX/图表必须离线可用。
