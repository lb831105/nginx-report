package accesslog

import "bytes"

// Fields 是一行访问日志解析后的通用字段。
// 所有值都指向原始行（或 Fields 内部的 scratch），调用方应在处理完当前行后
// 尽快复制需要长期保存的内容。
type Fields struct {
	Timestamp      []byte
	Client         []byte
	RealClient     []byte
	Method         []byte
	URI            []byte
	Request        []byte // 完整请求行，如 "GET /api?x=1 HTTP/1.1"
	Status         []byte
	Size           []byte
	UpstreamAddr   []byte
	UpstreamStatus []byte
	UpstreamTime   []byte
	ResponseTime   []byte
	Agent          []byte
	Referer        []byte
	Site           []byte

	scratch []byte
}

// Reset 清空字段，复用 scratch，避免每行重新分配。
func (f *Fields) Reset() {
	f.Timestamp = nil
	f.Client = nil
	f.RealClient = nil
	f.Method = nil
	f.URI = nil
	f.Request = nil
	f.Status = nil
	f.Size = nil
	f.UpstreamAddr = nil
	f.UpstreamStatus = nil
	f.UpstreamTime = nil
	f.ResponseTime = nil
	f.Agent = nil
	f.Referer = nil
	f.Site = nil
	f.scratch = f.scratch[:0]
}

type fieldID int

const (
	fieldIgnore fieldID = iota
	fieldTimestamp
	fieldClient
	fieldRealClient
	fieldMethod
	fieldURI
	fieldRequest
	fieldStatus
	fieldSize
	fieldUpstreamAddr
	fieldUpstreamStatus
	fieldUpstreamTime
	fieldResponseTime
	fieldAgent
	fieldReferer
	fieldSite
)

func (f *Fields) set(id fieldID, val []byte) {
	if id == fieldIgnore || len(val) == 0 {
		return
	}
	val = f.unescape(val)
	switch id {
	case fieldTimestamp:
		f.Timestamp = val
	case fieldClient:
		f.Client = val
	case fieldRealClient:
		f.RealClient = val
	case fieldMethod:
		f.Method = val
	case fieldURI:
		f.URI = val
	case fieldRequest:
		f.Request = val
	case fieldStatus:
		f.Status = val
	case fieldSize:
		f.Size = val
	case fieldUpstreamAddr:
		f.UpstreamAddr = val
	case fieldUpstreamStatus:
		f.UpstreamStatus = val
	case fieldUpstreamTime:
		f.UpstreamTime = val
	case fieldResponseTime:
		f.ResponseTime = val
	case fieldAgent:
		f.Agent = val
	case fieldReferer:
		f.Referer = val
	case fieldSite:
		f.Site = val
	}
}

// finish 把完整请求行拆成 method / URI（显式字段优先）。
func (f *Fields) finish() {
	if len(f.Request) == 0 {
		return
	}
	method, uri := splitRequest(f.Request)
	if len(f.Method) == 0 {
		f.Method = method
	}
	if len(f.URI) == 0 {
		f.URI = uri
	}
}

func splitRequest(req []byte) (method, uri []byte) {
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

// unescape 处理 nginx 默认 escape 和常见自定义转义。
// 没有反斜杠时直接返回原切片，不产生分配。
func (f *Fields) unescape(val []byte) []byte {
	if bytes.IndexByte(val, '\\') < 0 {
		return val
	}
	start := len(f.scratch)
	for i := 0; i < len(val); i++ {
		if val[i] != '\\' || i+1 >= len(val) {
			f.scratch = append(f.scratch, val[i])
			continue
		}
		i++
		switch val[i] {
		case 'n':
			f.scratch = append(f.scratch, '\n')
		case 'r':
			f.scratch = append(f.scratch, '\r')
		case 't':
			f.scratch = append(f.scratch, '\t')
		case 'b':
			f.scratch = append(f.scratch, '\b')
		case 'f':
			f.scratch = append(f.scratch, '\f')
		default:
			// \" \\ \/ 等：去掉反斜杠，保留字符本身
			f.scratch = append(f.scratch, val[i])
		}
	}
	return f.scratch[start:]
}
