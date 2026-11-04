package accesslog

import "bytes"

// combinedParser 解析 nginx 默认 combined 格式（也兼容 common log format）：
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"
//
// 显式指定 -log-format combined 时，末尾多出来的字段会被忽略。
// 注意：两个引号字段按顺序解释为 referer、user_agent；如果自定义格式只保留
// 其中一个，请用 -log-format 显式传模板，否则可能把 UA 当成 referer。
type combinedParser struct{}

func (combinedParser) parse(line []byte, f *Fields) bool {
	if _, ok := parseCombinedCore(line, f); !ok {
		return false
	}
	f.finish()
	return len(f.Timestamp) > 0
}

// combinedExtParser 自动识别 combined 时使用：
// 在标准字段之后，额外识别常见的 $request_time / $upstream_response_time /
// $upstream_addr / $upstream_status，避免把 combined + 耗时字段的日志
// 误当成普通 combined 而静默丢掉耗时。
type combinedExtParser struct{}

func (combinedExtParser) parse(line []byte, f *Fields) bool {
	p, ok := parseCombinedCore(line, f)
	if !ok {
		return false
	}
	if !parseCombinedExtras(line[p:], f) {
		return false
	}
	f.finish()
	return len(f.Timestamp) > 0
}

// combinedHostParser 解析 combined + 一个域名/IP 字段，供 -log-format
// combined_host / combined_server 使用。字段可以是带引号或裸值。
type combinedHostParser struct{}

func (combinedHostParser) parse(line []byte, f *Fields) bool {
	p, ok := parseCombinedCore(line, f)
	if !ok {
		return false
	}
	p = skipSpaces(line, p)
	if p >= len(line) {
		return false
	}
	var site []byte
	if line[p] == '"' {
		site, p, ok = takeQuoted(line, p)
	} else {
		site, p, ok = nextToken(line, p)
	}
	if !ok || len(site) == 0 {
		return false
	}
	p = skipSpaces(line, p)
	if p != len(line) {
		return false
	}
	f.set(fieldSite, site)
	f.finish()
	return len(f.Timestamp) > 0
}

func parseCombinedCore(line []byte, f *Fields) (int, bool) {
	p := skipSpaces(line, 0)

	// remote_addr
	client, p, ok := nextToken(line, p)
	if !ok {
		return p, false
	}
	f.set(fieldClient, client)

	// ident / authuser（- 或用户名），只跳过
	if _, p, ok = nextToken(line, p); !ok {
		return p, false
	}
	if _, p, ok = nextToken(line, p); !ok {
		return p, false
	}

	// [time_local]
	p = skipSpaces(line, p)
	if p >= len(line) || line[p] != '[' {
		return p, false
	}
	end := bytes.IndexByte(line[p+1:], ']')
	if end < 0 {
		return p, false
	}
	f.set(fieldTimestamp, line[p+1:p+1+end])
	p += end + 2

	// "request"
	p = skipSpaces(line, p)
	req, p, ok := takeQuoted(line, p)
	if !ok {
		return p, false
	}
	f.set(fieldRequest, req)

	// status
	status, p, ok := nextToken(line, p)
	if !ok {
		return p, false
	}
	f.set(fieldStatus, status)

	// body_bytes_sent
	size, p, ok := nextToken(line, p)
	if !ok {
		return p, false
	}
	f.set(fieldSize, size)

	// 可选："referer" "user_agent"
	p = skipSpaces(line, p)
	if p < len(line) && line[p] == '"' {
		if ref, np, ok := takeQuoted(line, p); ok {
			f.set(fieldReferer, ref)
			p = np
		}
	}
	p = skipSpaces(line, p)
	if p < len(line) && line[p] == '"' {
		if ua, np, ok := takeQuoted(line, p); ok {
			f.set(fieldAgent, ua)
			p = np
		}
	}

	return p, true
}

// parseCombinedExtras 识别 combined 之后最常见的追加字段。
// 只接受能明确分类的组合；遇到未知字段返回 false，让调用方提示用户用模板。
func parseCombinedExtras(rest []byte, f *Fields) bool {
	// 不用 bytes.Fields：它会为每行分配一个 [][]byte。这里最多只认 4 个
	// 扩展字段，用固定数组原地切分，combined 大文件可少一次逐行分配。
	var tokens [4][]byte
	n := 0
	for p := 0; p < len(rest); {
		p = skipSpaces(rest, p)
		if p >= len(rest) {
			break
		}
		if n >= len(tokens) {
			return false
		}
		start := p
		for p < len(rest) && rest[p] != ' ' && rest[p] != '	' {
			p++
		}
		tokens[n] = rest[start:p]
		n++
	}
	if n == 0 {
		return true
	}
	isFloat := func(i int) bool { return looksFloatToken(tokens[i]) }
	isDash := func(i int) bool { return len(tokens[i]) == 1 && tokens[i][0] == '-' }
	isTime := func(i int) bool { return isFloat(i) || isDash(i) }

	switch n {
	case 1:
		// combined + $request_time 是最常见的扩展
		if !isFloat(0) {
			return false
		}
		f.set(fieldResponseTime, tokens[0])
		return true
	case 2:
		if !isFloat(0) || !isTime(1) {
			return false
		}
		f.set(fieldResponseTime, tokens[0])
		f.set(fieldUpstreamTime, tokens[1])
		return true
	case 3:
		if !isFloat(0) || !isTime(1) {
			return false
		}
		f.set(fieldResponseTime, tokens[0])
		f.set(fieldUpstreamTime, tokens[1])
		if looksStatusToken(tokens[2]) {
			f.set(fieldUpstreamStatus, tokens[2])
			return true
		}
		if looksAddrToken(tokens[2]) {
			f.set(fieldUpstreamAddr, tokens[2])
			return true
		}
		return false
	case 4:
		if !isFloat(0) || !isTime(1) || !looksAddrToken(tokens[2]) || !looksStatusToken(tokens[3]) {
			return false
		}
		f.set(fieldResponseTime, tokens[0])
		f.set(fieldUpstreamTime, tokens[1])
		f.set(fieldUpstreamAddr, tokens[2])
		f.set(fieldUpstreamStatus, tokens[3])
		return true
	default:
		return false
	}
}

func looksFloatToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	dot, digit := false, false
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c == '.':
			if dot {
				return false
			}
			dot = true
		default:
			return false
		}
	}
	return digit
}

func looksStatusToken(b []byte) bool {
	if len(b) != 3 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func looksAddrToken(b []byte) bool {
	if len(b) == 1 && b[0] == '-' {
		return true
	}
	// IP:port、域名:port、逗号重试列表
	return bytes.IndexByte(b, ':') >= 0 || bytes.IndexByte(b, ',') >= 0
}

func skipSpaces(b []byte, p int) int {
	for p < len(b) && (b[p] == ' ' || b[p] == '\t') {
		p++
	}
	return p
}

// nextToken 读取到下一个空白为止；返回 token 和 token 之后的位置。
func nextToken(b []byte, p int) ([]byte, int, bool) {
	p = skipSpaces(b, p)
	start := p
	for p < len(b) && b[p] != ' ' && b[p] != '\t' {
		p++
	}
	if p == start {
		return nil, p, false
	}
	return b[start:p], p, true
}

// takeQuoted 读取以 " 开头的字段，返回内容（不含引号）和结束引号之后的位置。
// 反斜杠转义的引号不会提前结束。
func takeQuoted(b []byte, p int) ([]byte, int, bool) {
	p = skipSpaces(b, p)
	if p >= len(b) || b[p] != '"' {
		return nil, p, false
	}
	start := p + 1
	end := scanQuoted(b, start)
	if end < 0 {
		return nil, p, false
	}
	return b[start:end], end + 1, true
}

// scanQuoted 从 start 开始找未转义的结束引号，返回其下标；找不到返回 -1。
func scanQuoted(b []byte, start int) int {
	for i := start; i < len(b); i++ {
		if b[i] == '\\' {
			i++ // 跳过被转义的字符
			continue
		}
		if b[i] == '"' {
			return i
		}
	}
	return -1
}
