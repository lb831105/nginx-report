// Package staticasset 识别静态资源请求（css/js/图片/字体等）。
//
// 这份规则同时被「解析日志时排除静态记录」和「报告里的接口榜过滤」使用，
// 放在独立包里避免两处逻辑漂移：同一个 /pro/assets/x.svg 不可能在一个地方算静态、
// 在另一个地方又算业务接口。
package staticasset

import (
	"bytes"
	"strings"
)

// staticExts 静态资源后缀：这类请求量往往极大，会把「接口 Top」挤满，
// 也会让业务 PV / 错误率失真。
var staticExts = map[string]bool{
	".js": true, ".css": true, ".map": true, ".svg": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".webp": true, ".avif": true, ".woff": true, ".woff2": true,
	".ttf": true, ".eot": true, ".otf": true,
	".mp4": true, ".webm": true, ".mp3": true, ".pdf": true,
}

// staticDirs 静态资源目录名（路径里出现这些「整段」就认为是静态资源）。
var staticDirs = map[string]bool{
	"assets": true, "static": true, "public": true, "dist": true,
	"media": true, "img": true, "images": true, "fonts": true,
}

// Is 判断 URI 是否为静态资源（忽略 ?query）。
func Is(uri string) bool {
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	ext := pathExtString(uri)
	if ext != "" && (staticExts[ext] || staticExts[strings.ToLower(ext)]) {
		return true
	}
	// 按路径段判断（只认前两段，避免 /a/b/c/assets/... 这种深层路径误判）：
	//   /pro/assets/x.svg、/static/app.abc123 都算静态；
	//   但 /bbt-api/assets/list 这类 "api 命名空间下的同名目录" 仍当业务接口。
	segs := strings.Split(strings.TrimPrefix(uri, "/"), "/")
	if len(segs) > 2 {
		segs = segs[:2]
	}
	for _, seg := range segs {
		ls := strings.ToLower(seg)
		if strings.Contains(ls, "api") {
			return false
		}
		if staticDirs[ls] {
			return true
		}
	}
	return false
}

// IsBytes 同 Is，但直接吃日志行里的 []byte，供解析热路径使用。
//
// 解析期每行都会调用；这里用栈上小缓冲做 ASCII 小写化，避免
// strings.ToLower(string(seg)) 在热路径产生临时字符串。
func IsBytes(uri []byte) bool {
	if i := bytes.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	if ext := pathExtBytes(uri); len(ext) > 0 {
		var buf [maxStaticKeyLen]byte
		if len(ext) <= len(buf) {
			if staticExts[string(lowerASCIITo(buf[:], ext))] {
				return true
			}
		}
	}
	rest := uri
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	for i := 0; i < 2 && len(rest) > 0; i++ {
		seg := rest
		if j := bytes.IndexByte(rest, '/'); j >= 0 {
			seg, rest = rest[:j], rest[j+1:]
		} else {
			rest = nil
		}
		if len(seg) == 0 {
			continue
		}
		if len(seg) <= maxStaticKeyLen {
			var buf [maxStaticKeyLen]byte
			ls := lowerASCIITo(buf[:], seg)
			if bytes.Contains(ls, apiSegment) {
				return false
			}
			if staticDirs[string(ls)] {
				return true
			}
		} else if containsFoldASCII(seg, "api") {
			// 超长路径段不可能是 staticDirs 里的目录名，但仍要识别 api 命名空间。
			return false
		}
	}
	return false
}

const maxStaticKeyLen = 16

var apiSegment = []byte("api")

// lowerASCIITo 把 src 的 ASCII 大写字母写入 dst，返回 dst[:len(src)]。
// 调用方保证 len(src) <= len(dst)；非 ASCII 字节原样保留。
func lowerASCIITo(dst, src []byte) []byte {
	for i, c := range src {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst[i] = c
	}
	return dst[:len(src)]
}

// containsFoldASCII 对 ASCII 字符串做大小写不敏感的子串查找，不分配内存。
func containsFoldASCII(b []byte, s string) bool {
	n := len(s)
	if n == 0 {
		return true
	}
	if n > len(b) {
		return false
	}
	for i := 0; i+n <= len(b); i++ {
		match := true
		for j := 0; j < n; j++ {
			c, d := b[i+j], s[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if d >= 'A' && d <= 'Z' {
				d += 'a' - 'A'
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// pathExtString 取 URI 里的扩展名（含点），没有返回空串
func pathExtString(uri string) string {
	for i := len(uri) - 1; i >= 0; i-- {
		switch uri[i] {
		case '/':
			return ""
		case '.':
			return uri[i:]
		}
	}
	return ""
}

// pathExtBytes 同 pathExtString
func pathExtBytes(uri []byte) []byte {
	for i := len(uri) - 1; i >= 0; i-- {
		switch uri[i] {
		case '/':
			return nil
		case '.':
			return uri[i:]
		}
	}
	return nil
}
