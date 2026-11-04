package accesslog

import (
	"bytes"
	"fmt"
	"strings"
)

// maxCSVColumns 单行 CSV 最大列数，防止恶意逗号串把 [][]byte 撑爆。
const maxCSVColumns = 4096

// csvParser 按表头或 -csv-fields 的列顺序解析 CSV。
// 支持 RFC4180 常见的双引号包裹、字段内逗号、"" 转义。
type csvParser struct {
	fields []fieldID
}

func newCSVFormat(spec string, firstLine []byte, forceSkipHeader bool, siteFields ...string) (*Format, error) {
	siteField := ""
	if len(siteFields) > 0 {
		siteField = strings.TrimSpace(siteFields[0])
	}
	if strings.TrimSpace(spec) != "" {
		fields, err := parseCSVFieldSpec(spec, siteField)
		if err != nil {
			return nil, err
		}
		return &Format{
			Kind:       KindCSV,
			Name:       "csv",
			SkipHeader: forceSkipHeader || looksLikeCSVHeader(firstLine),
			parser:     &csvParser{fields: fields},
		}, nil
	}

	header := splitCSV(firstLine)
	fields := make([]fieldID, len(header))
	hasTime := false
	recognized := 0
	for i, name := range header {
		id := headerFieldWithSite(normalizeHeader(name), siteField)
		fields[i] = id
		if id == fieldTimestamp {
			hasTime = true
		}
		if id != fieldIgnore {
			recognized++
		}
	}
	if !hasTime || recognized < 3 {
		return nil, fmt.Errorf("CSV 缺少表头或表头无法识别；请把首行写成 time,client,method,uri,status,size,...，或用 -csv-fields 指定列顺序")
	}
	return &Format{
		Kind:       KindCSV,
		Name:       "csv",
		SkipHeader: true,
		parser:     &csvParser{fields: fields},
	}, nil
}

func parseCSVFieldSpec(spec string, siteFields ...string) ([]fieldID, error) {
	siteField := ""
	if len(siteFields) > 0 {
		siteField = strings.TrimSpace(siteFields[0])
	}
	names := strings.Split(spec, ",")
	fields := make([]fieldID, len(names))
	hasTime := false
	for i, name := range names {
		id := headerFieldWithSite(normalizeHeader([]byte(name)), siteField)
		fields[i] = id
		if id == fieldTimestamp {
			hasTime = true
		}
	}
	if !hasTime {
		return nil, fmt.Errorf("-csv-fields 必须包含 time / timestamp / @timestamp 之一")
	}
	return fields, nil
}

func looksLikeCSVHeader(line []byte) bool {
	cells := splitCSV(line)
	if len(cells) < 3 {
		return false
	}
	hasTime, recognized := false, 0
	for _, c := range cells {
		id := headerField(normalizeHeader(c))
		if id == fieldTimestamp {
			hasTime = true
		}
		if id != fieldIgnore {
			recognized++
		}
	}
	return hasTime && recognized >= 3
}

func (p *csvParser) parse(line []byte, f *Fields) bool {
	cells := splitCSV(line)
	for i, id := range p.fields {
		if i >= len(cells) {
			break
		}
		f.set(id, cells[i])
	}
	f.finish()
	return len(f.Timestamp) > 0
}

func normalizeHeader(b []byte) string {
	s := strings.ToLower(strings.TrimSpace(string(b)))
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func headerField(name string) fieldID {
	return headerFieldWithSite(name, "")
}

func headerFieldWithSite(name, siteField string) fieldID {
	if siteField != "" && name == normalizeHeader([]byte(siteField)) {
		return fieldSite
	}
	switch name {
	case "@timestamp", "timestamp", "time", "time_local", "time_iso8601", "date", "datetime":
		return fieldTimestamp
	case "client", "remote_addr", "remote_ip", "ip", "addr":
		return fieldClient
	case "real_client", "realip_remote_addr", "x_forwarded_for", "http_x_forwarded_for", "http_x_real_ip":
		return fieldRealClient
	case "method", "request_method":
		return fieldMethod
	case "uri", "request_uri", "path":
		return fieldURI
	case "url", "request":
		return fieldRequest
	case "status", "code", "status_code":
		return fieldStatus
	case "size", "body_bytes_sent", "bytes_sent", "bytes":
		return fieldSize
	case "upstream_addr":
		return fieldUpstreamAddr
	case "upstream_status":
		return fieldUpstreamStatus
	case "upstream_response_time", "upstream_time":
		return fieldUpstreamTime
	case "responsetime", "request_time", "response_time", "rt":
		return fieldResponseTime
	case "agent", "user_agent", "http_user_agent", "ua":
		return fieldAgent
	case "referer", "referrer", "http_referer", "ref":
		return fieldReferer
	case "domain", "server_name", "host", "http_host", "vhost", "site", "website", "site_name", "server", "request_host":
		return fieldSite
	default:
		return fieldIgnore
	}
}

// csvRecordComplete 判断一行 CSV 是否已经构成完整记录。
// 只有引号字段内部的换行才需要继续累积；普通字段里的单个引号不会误判。
func csvRecordComplete(line []byte) bool {
	complete, _ := csvRecordCompleteFrom(line, false)
	return complete
}

// csvRecordCompleteFrom 在“上一条物理行结束时是否仍在引号内”的状态上继续判断。
// 调用方累积多行 CSV 时只扫描新增行，避免每行都重扫整个缓冲区（O(n²)）。
func csvRecordCompleteFrom(line []byte, inQuotes bool) (complete, nextInQuotes bool) {
	if !inQuotes && bytes.IndexByte(line, '"') < 0 {
		return true, false
	}
	fieldStart := !inQuotes
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuotes {
			if c == '"' {
				if i+1 < len(line) && line[i+1] == '"' {
					i++
					continue
				}
				inQuotes = false
				fieldStart = false
			}
			continue
		}
		switch c {
		case ',':
			fieldStart = true
		case '"':
			if fieldStart {
				inQuotes = true
			}
		case ' ', 0x09:
			// 保留 fieldStart，兼容字段前有少量空白的写法
		default:
			fieldStart = false
		}
	}
	return !inQuotes, inQuotes
}

// splitCSV 拆分一行 CSV，去掉字段外层引号并处理 "" 转义。
// CSV 是显式选择/带表头才启用的格式，允许这里为转义字段分配内存。
func splitCSV(line []byte) [][]byte {
	out := make([][]byte, 0, 16)
	start := 0
	inQuotes := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			if inQuotes && i+1 < len(line) && line[i+1] == '"' {
				i++ // 字段内的 ""，跳过第二个引号
				continue
			}
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				if len(out) >= maxCSVColumns {
					return nil
				}
				out = append(out, unquoteCSV(line[start:i]))
				start = i + 1
			}
		}
	}
	if len(out) >= maxCSVColumns {
		return nil
	}
	out = append(out, unquoteCSV(line[start:]))
	return out
}

func unquoteCSV(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return b
	}
	b = b[1 : len(b)-1]
	if bytes.IndexByte(b, '"') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '"' && i+1 < len(b) && b[i+1] == '"' {
			out = append(out, '"')
			i++
			continue
		}
		out = append(out, b[i])
	}
	return out
}
