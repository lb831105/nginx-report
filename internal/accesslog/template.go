package accesslog

import (
	"bytes"
	"fmt"
	"strings"
)

// part 是 nginx log_format 模板的一段：literal 或 $variable。
type part struct {
	lit   []byte
	name  string
	field fieldID
}

type templateParser struct {
	parts []part
}

func newTemplateFormat(spec string, siteFields ...string) (*Format, error) {
	siteField := ""
	if len(siteFields) > 0 {
		siteField = strings.TrimSpace(siteFields[0])
	}
	parts, hasTime, err := compileTemplate(spec, siteField)
	if err != nil {
		return nil, err
	}
	if !hasTime {
		return nil, fmt.Errorf("自定义 log_format 必须包含 $time_local 或 $time_iso8601（否则无法按天统计）")
	}
	return &Format{Kind: KindCustom, Name: "custom", parser: &templateParser{parts: parts}}, nil
}

func compileTemplate(spec string, siteField string) ([]part, bool, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, false, fmt.Errorf("自定义 log_format 不能为空")
	}
	var parts []part
	var lit strings.Builder
	hasTime := false
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, part{lit: []byte(lit.String())})
			lit.Reset()
		}
	}

	for i := 0; i < len(spec); {
		if spec[i] != '$' {
			lit.WriteByte(spec[i])
			i++
			continue
		}
		name, next := templateVar(spec, i)
		if name == "" {
			lit.WriteByte('$')
			i++
			continue
		}
		flush()
		id := nginxVarField(name)
		if id == fieldIgnore && siteField != "" && name == siteField {
			id = fieldSite
		}
		parts = append(parts, part{name: name, field: id})
		if id == fieldTimestamp {
			hasTime = true
		}
		i = next
	}
	flush()
	if len(parts) == 0 {
		return nil, false, fmt.Errorf("自定义 log_format 没有可解析内容")
	}
	for i := 1; i < len(parts); i++ {
		if parts[i-1].name != "" && parts[i].name != "" {
			return nil, false, fmt.Errorf("自定义 log_format 中 $%s$%s 之间缺少分隔符，无法可靠拆分", parts[i-1].name, parts[i].name)
		}
	}
	return parts, hasTime, nil
}

// templateVar 解析 $name 或 ${name}，返回变量名和变量后的下标。
func templateVar(spec string, i int) (string, int) {
	if i+1 >= len(spec) {
		return "", i + 1
	}
	if spec[i+1] == '{' {
		end := strings.IndexByte(spec[i+2:], '}')
		if end < 0 {
			return "", i + 1
		}
		name := spec[i+2 : i+2+end]
		if !validVarName(name) {
			return "", i + 1
		}
		return name, i + 3 + end
	}
	j := i + 1
	for j < len(spec) && isVarChar(spec[j]) {
		j++
	}
	if j == i+1 {
		return "", i + 1
	}
	return spec[i+1 : j], j
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isVarChar(s[i]) {
			return false
		}
	}
	return true
}

func isVarChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func nginxVarField(name string) fieldID {
	switch name {
	case "time_local", "time_iso8601":
		return fieldTimestamp
	case "remote_addr":
		return fieldClient
	case "realip_remote_addr", "real_client", "http_x_forwarded_for", "http_x_real_ip", "proxy_add_x_forwarded_for":
		return fieldRealClient
	case "request_method":
		return fieldMethod
	case "request_uri", "uri", "document_uri":
		return fieldURI
	case "request":
		return fieldRequest
	case "status":
		return fieldStatus
	case "body_bytes_sent", "bytes_sent":
		return fieldSize
	case "upstream_addr":
		return fieldUpstreamAddr
	case "upstream_status":
		return fieldUpstreamStatus
	case "upstream_response_time":
		return fieldUpstreamTime
	case "request_time":
		return fieldResponseTime
	case "http_user_agent":
		return fieldAgent
	case "http_referer":
		return fieldReferer
	case "domain", "server_name", "host", "http_host", "vhost", "site", "website", "site_name", "server", "request_host":
		return fieldSite
	default:
		return fieldIgnore
	}
}

func (p *templateParser) parse(line []byte, f *Fields) bool {
	pos := 0
	for i := 0; i < len(p.parts); i++ {
		part := p.parts[i]
		if len(part.lit) > 0 {
			if pos+len(part.lit) > len(line) || !bytes.HasPrefix(line[pos:], part.lit) {
				return false
			}
			pos += len(part.lit)
		}
		if part.name == "" {
			continue
		}

		var nextLit []byte
		if i+1 < len(p.parts) {
			nextLit = p.parts[i+1].lit
		}
		var val []byte
		if len(nextLit) == 0 {
			val = line[pos:]
			pos = len(line)
		} else if len(part.lit) > 0 && part.lit[len(part.lit)-1] == '"' && nextLit[0] == '"' {
			// 模板里是 "$var"：按带转义的引号扫描，避免 UA 里的 \" 提前截断
			end := scanQuoted(line, pos)
			if end < 0 {
				return false
			}
			val = line[pos:end]
			pos = end
		} else {
			idx := bytes.Index(line[pos:], nextLit)
			if idx < 0 {
				return false
			}
			val = line[pos : pos+idx]
			pos += idx
		}
		f.set(part.field, val)
	}

	// 模板末尾允许有空白（日志行本身可能带 \r）
	if len(bytes.TrimSpace(line[pos:])) > 0 {
		return false
	}
	f.finish()
	return true
}
