// Package accesslog 识别并解析 nginx 访问日志的常见格式：
// JSON（识别用，实际解析仍走 analyzer 快速路径）、combined、
// 自定义 log_format 模板、CSV。
//
// 识别只发生在文件首行（或前 20 个非空行），同一文件后续行复用 Format，
// 避免每行做格式判断。
package accesslog

import (
	"bytes"
	"fmt"
	"strings"
)

// Kind 日志格式类型
type Kind string

const (
	KindJSON     Kind = "json"
	KindCombined Kind = "combined"
	KindCustom   Kind = "custom"
	KindCSV      Kind = "csv"
)

// Format 是一份可复用的行解析器。
// combined/自定义模板尽量复用 scratch，不逐行分配；CSV 为了正确处理引号，
// 每行会分配少量切片，属于显式选择 CSV 后的可接受成本。
type Format struct {
	Kind Kind
	// Name 给日志/报告展示用，例如 custom、combined、csv。
	Name string
	// SkipHeader 为 true 时，第一行是 CSV 表头，调用方应跳过。
	SkipHeader bool
	// Warning 是识别阶段的可读提示（例如 combined 末尾字段含义不确定），
	// 由调用方决定是否写日志；不影响解析结果。
	Warning string

	parser lineParser
}

type lineParser interface {
	parse(line []byte, f *Fields) bool
}

// Parse 解析一行；fields 会被 Reset。
//
// 注意：KindJSON 走 analyzer 的 scanFields 快速路径，这里的 JSON parser 只是
// 占位，Parse 对 JSON 固定返回 false。通用调用方应先判断 Kind。
func (f *Format) Parse(line []byte, fields *Fields) bool {
	fields.Reset()
	if f == nil || f.parser == nil {
		return false
	}
	return f.parser.parse(line, fields)
}

// RecordComplete 判断一行是否已经构成完整记录。目前只有 CSV 可能因为
// 引号字段内部含换行而需要调用方继续累积；其它格式始终返回 true。
func (f *Format) RecordComplete(line []byte) bool {
	if f == nil || f.Kind != KindCSV {
		return true
	}
	return csvRecordComplete(line)
}

// RecordCompleteFrom 供跨行 CSV 累积使用：inQuotes 表示上一条物理行结束时
// 是否仍在引号字段内。返回当前累积是否已完整，以及下一行应使用的状态。
func (f *Format) RecordCompleteFrom(line []byte, inQuotes bool) (complete, nextInQuotes bool) {
	if f == nil || f.Kind != KindCSV {
		return true, false
	}
	return csvRecordCompleteFrom(line, inQuotes)
}

// DetectOptions 是 DetectWithOptions 的可扩展参数。
// Detect 保留原三参数签名，方便已有调用和测试。
type DetectOptions struct {
	// Configured 取值：
	//   - 空 / auto：自动识别 JSON、combined、带表头的 CSV
	//   - json / combined / csv：强制使用对应格式
	//   - 其它：按 nginx log_format 模板解析，必须包含 $time_local 或 $time_iso8601
	Configured string
	// CSVFields 仅在 Configured=csv 且没有表头时使用，例如：
	// time,client,method,uri,status,size,request_time,agent,referer
	CSVFields string
	// CSVSkipHeader 强制跳过 CSV 第一行（表头列名无法识别时很有用）。
	CSVSkipHeader bool
	// SiteField 指定站点字段名，用于 CSV 表头/自定义模板中非标准列名。
	SiteField string
}

// Detect 根据首行和用户配置识别日志格式。
func Detect(firstLine []byte, configured, csvFields string) (*Format, error) {
	return DetectWithOptions(firstLine, DetectOptions{Configured: configured, CSVFields: csvFields})
}

// DetectWithOptions 与 Detect 相同，但支持 CSV 表头等额外选项。
func DetectWithOptions(firstLine []byte, opt DetectOptions) (*Format, error) {
	line := bytes.TrimSpace(bytes.TrimPrefix(firstLine, []byte{0xEF, 0xBB, 0xBF}))
	if len(line) == 0 {
		return nil, fmt.Errorf("首行为空，无法识别日志格式")
	}
	cfg := strings.TrimSpace(opt.Configured)
	lower := strings.ToLower(cfg)

	switch {
	case cfg == "" || lower == "auto":
		return detectAuto(line, opt.SiteField)
	case lower == "json":
		return &Format{Kind: KindJSON, Name: "json", parser: jsonParserPlaceholder{}}, nil
	case lower == "combined" || lower == "common":
		return &Format{Kind: KindCombined, Name: "combined", parser: combinedParser{}}, nil
	case lower == "combined_host" || lower == "combined_server":
		return &Format{Kind: KindCombined, Name: lower, parser: combinedHostParser{}}, nil
	case lower == "csv":
		return newCSVFormat(opt.CSVFields, line, opt.CSVSkipHeader, opt.SiteField)
	default:
		return newTemplateFormat(cfg, opt.SiteField)
	}
}

// ValidateConfigured 在启动阶段校验 -log-format，避免等到第一个日志文件才报错。
// CSV 的列定义由 ValidateCSVFields 单独校验；auto/json/combined 无需额外校验。
func ValidateConfigured(configured string) error {
	cfg := strings.TrimSpace(configured)
	if cfg == "" {
		return nil
	}
	switch strings.ToLower(cfg) {
	case "auto", "json", "combined", "common", "csv", "combined_host", "combined_server":
		return nil
	}
	_, err := newTemplateFormat(cfg, "")
	return err
}

// ValidateCSVFields 在启动阶段校验 -csv-fields。
func ValidateCSVFields(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	_, err := parseCSVFieldSpec(spec)
	return err
}

func detectAuto(line []byte, siteField string) (*Format, error) {
	if line[0] == '{' {
		// JSON 解析仍由 analyzer 的快速路径完成；这里只负责识别。
		return &Format{Kind: KindJSON, Name: "json", parser: jsonParserPlaceholder{}}, nil
	}
	if looksLikeCSVHeader(line) {
		return newCSVFormat("", line, false, siteField)
	}
	// combined 解析器同时兼容 common log format；
	// 末尾常见扩展（request_time/upstream_*）也尝试识别，避免静默丢字段。
	var f Fields
	if p, ok := parseCombinedCore(line, &f); ok {
		if parseCombinedExtras(line[p:], &f) && len(f.Timestamp) > 0 {
			warning := ""
			if rest := bytes.Fields(line[p:]); len(rest) == 1 && looksFloatToken(rest[0]) {
				warning = "combined 末尾只有 1 个浮点字段，已按 $request_time 处理；若它是 $upstream_response_time，请用 -log-format 显式传模板"
			}
			return &Format{Kind: KindCombined, Name: "combined", Warning: warning, parser: combinedExtParser{}}, nil
		}
		return nil, fmt.Errorf("检测到 combined 基础字段，但末尾额外字段无法安全识别；请用 -log-format 传完整 nginx log_format 模板。首行示例: %s", sample(line))
	}
	return nil, fmt.Errorf("无法自动识别该行，请用 -log-format 指定 json / combined / csv，或传 nginx log_format 模板。首行示例: %s", sample(line))
}

// jsonParserPlaceholder 只是占位：JSON 由 analyzer 的 scanFields 快速路径解析。
type jsonParserPlaceholder struct{}

func (jsonParserPlaceholder) parse([]byte, *Fields) bool { return false }

func sample(line []byte) string {
	const max = 220
	truncated := false
	if len(line) > max {
		line = line[:max]
		truncated = true
	}
	// 只做最小限度的隐私处理：query 值可能带 token/session，日志报错时不应原样落盘。
	out := make([]byte, 0, len(line)+12)
	for i := 0; i < len(line); i++ {
		c := line[i]
		// 防止日志里的 ANSI/控制字符注入终端；保留空格语义即可。
		if c < 0x20 || c == 0x7f {
			c = ' '
		}
		out = append(out, c)
		if c != '?' {
			continue
		}
		for i+1 < len(line) && !isSampleDelimiter(line[i+1]) {
			i++
		}
		out = append(out, "<redacted>"...)
	}
	if truncated {
		out = append(out, '.', '.', '.')
	}
	return string(out)
}

func isSampleDelimiter(c byte) bool {
	switch c {
	case ' ', 0x09, 0x0a, 0x0d, '"', 0x27, ']', '}', ',':
		return true
	}
	return false
}
