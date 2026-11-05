// Package site 负责多站点报告中的站点识别与归一化。
//
// 设计目标：兼容“文件名带域名”“日志里有 domain/host 字段”“直接用 IP 访问”
// 三类来源；识别失败时返回 (未识别站点)，绝不因为站点问题丢弃日志行。
package site

import (
	"hash/fnv"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Kind 站点类型。域名和 IP 直连必须分开统计，未知站点用于兜底。
type Kind string

const (
	KindDomain  Kind = "domain"
	KindIP      Kind = "ip"
	KindUnknown Kind = "unknown"
	KindOther   Kind = "other"
)

const (
	// UnknownKey 未识别站点。Key 使用前缀避免与真实域名冲突。
	UnknownKey = "u:unknown"
	// OtherKey 站点数超过上限后的溢出桶。
	OtherKey = "o:other"
	// UnknownName / OtherName 报告展示文案。
	UnknownName = "(未识别站点)"
	OtherName   = "(其他站点)"
)

// Info 一个站点标识。
type Info struct {
	Key    string
	Name   string
	Kind   Kind
	Source string // field/map/filename/name
}

// Unknown 返回未识别站点。
func Unknown() Info {
	return Info{Key: UnknownKey, Name: UnknownName, Kind: KindUnknown, Source: "unknown"}
}

// Other 返回站点溢出桶。
func Other() Info {
	return Info{Key: OtherKey, Name: OtherName, Kind: KindOther, Source: "overflow"}
}

// KeyOf 由类型和名称生成稳定 key。
func KeyOf(kind Kind, name string) string {
	return string(kind) + ":" + name
}

func keyPrefix(kind Kind) string {
	switch kind {
	case KindDomain:
		return "d:"
	case KindIP:
		return "i:"
	case KindOther:
		return "o:"
	default:
		return "u:"
	}
}

// ParseKey 还原 Info。未知 key 视为 unknown。
func ParseKey(key string) Info {
	switch {
	case strings.HasPrefix(key, "d:"):
		return Info{Key: key, Name: key[2:], Kind: KindDomain}
	case strings.HasPrefix(key, "i:"):
		return Info{Key: key, Name: key[2:], Kind: KindIP}
	case strings.HasPrefix(key, "o:"):
		return Info{Key: OtherKey, Name: OtherName, Kind: KindOther}
	default:
		return Unknown()
	}
}

// IsKnownKind 是否为需要生成独立报告的域名/IP 类型。
func IsKnownKind(k Kind) bool { return k == KindDomain || k == KindIP }

var (
	placeholderNames = map[string]bool{
		"": true, "-": true, "_": true, "null": true, "nil": true,
		"unknown": true, "none": true, "n/a": true, "na": true,
		"default": true, "default_server": true,
	}
	// 文件名里常见的非域名扩展名/TLD，避免把 access.log 当成 access.log 站点。
	fileLikeTLD = map[string]bool{
		"log": true, "gz": true, "txt": true, "json": true, "bak": true,
		"old": true, "out": true, "err": true, "error": true, "access": true,
		// app / zip 是真实 gTLD，不能当文件名后缀拒绝。
		"nginx": true, "debug": true, "tar": true,
	}
)

// Normalize 把日志字段/文件名里的站点值归一化。
// 支持域名、URL、host:port、IPv4、IPv6；多值取第一个合法值。
func Normalize(raw string, stripWWW bool) (Info, bool) {
	info, ok, _ := NormalizeWithCount(raw, stripWWW)
	return info, ok
}

// NormalizeWithCount 返回第一个合法站点，以及合法值的个数。
// 同一字段出现多个域名时取第一个，调用方据此累计 MultiDomainLines。
func NormalizeWithCount(raw string, stripWWW bool) (Info, bool, int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Info{}, false, 0
	}
	// 字段可能来自 server_name 的多值形式："example.com www.example.com"
	var first Info
	count := 0
	for _, part := range splitValues(raw) {
		if info, ok := normalizeOne(part, stripWWW); ok {
			if count == 0 {
				first = info
			}
			count++
		}
	}
	return first, count > 0, count
}

func splitValues(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ',', ';', ' ', '\t', '\r', '\n':
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func normalizeOne(raw string, stripWWW bool) (Info, bool) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")
	if s == "" {
		return Info{}, false
	}
	// URL / //host / host/path
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if strings.HasPrefix(s, "//") {
		s = s[2:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return Info{}, false
	}

	host := s
	// IPv6 带方括号和端口：[2001:db8::1]:443
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return Info{}, false
		}
		inside := host[1:end]
		if addr, err := netip.ParseAddr(inside); err == nil {
			return ipInfo(addr), true
		}
		return Info{}, false
	}
	// host:port（只处理一个冒号；IPv6 裸地址会走 ParseAddr）
	if strings.Count(host, ":") == 1 {
		if h, p, ok := strings.Cut(host, ":"); ok && p != "" {
			if _, err := strconv.Atoi(p); err == nil {
				host = h
			}
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return ipInfo(addr), true
	}

	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "*.")
	host = strings.TrimPrefix(host, ".")
	if stripWWW {
		host = strings.TrimPrefix(host, "www.")
	}
	if !strings.Contains(host, ".") && host != "localhost" {
		return Info{}, false
	}
	if lastLabelNumeric(host) {
		// 2026.10.30 这类日期目录不是域名；真实 TLD 不会是纯数字。
		return Info{}, false
	}
	if placeholderNames[host] || !validHost(host) {
		return Info{}, false
	}
	return Info{Key: keyPrefix(KindDomain) + host, Name: host, Kind: KindDomain}, true
}

func ipInfo(addr netip.Addr) Info {
	name := addr.String()
	return Info{Key: keyPrefix(KindIP) + name, Name: name, Kind: KindIP}
}

func lastLabelNumeric(host string) bool {
	i := strings.LastIndexByte(host, '.')
	if i < 0 || i+1 >= len(host) {
		return false
	}
	for _, c := range host[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, " \t\r\n/?#:@[]") {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r == '_' || r == '-' || r == '.' {
				continue
			}
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return false
			}
		}
	}
	return true
}

// ApplyAlias 把域名别名归一化到主域名。aliases 的 key 为归一化域名。
func ApplyAlias(info Info, aliases map[string]string) Info {
	if len(aliases) == 0 || info.Kind != KindDomain {
		return info
	}
	source := info.Source
	seen := 0
	for seen < 8 {
		target, ok := aliases[info.Name]
		if !ok || target == "" || target == info.Name {
			info.Source = source
			return info
		}
		next, ok := Normalize(target, false)
		if !ok {
			info.Source = source
			return info
		}
		info = next
		seen++
	}
	info.Source = source
	return info
}

var (
	fileDateRe = regexp.MustCompile(`(?:^|[._-])\d{4}-\d{2}-\d{2}(?:$|[._-])`)
	fileIPRe   = regexp.MustCompile(`(?:^|[_-])(\d{1,3}(?:\.\d{1,3}){3})(?:$|[_-])`)
)

// FromFilename 从文件名推断站点。只做高置信度推断：
//   - example.com.log
//   - www.example.com_2026-10-30.log
//   - 1.2.3.4.log
//
// access.log / error.log / 28888_access_json.log 等返回 false。
func FromFilename(name string, stripWWW bool) (Info, bool) {
	base := strings.ToLower(filepath.Base(name))
	// 去掉压缩和轮转后缀
	for {
		changed := false
		for _, ext := range []string{".gz", ".log", ".txt", ".json", ".bak", ".old"} {
			if strings.HasSuffix(base, ext) {
				base = strings.TrimSuffix(base, ext)
				changed = true
			}
		}
		// 只去掉日志轮转编号（access.log.1），不能把 IPv4 最后一段当成轮转号。
		if n := len(base); n > 2 && base[n-2] == '.' && base[n-1] >= '0' && base[n-1] <= '9' {
			prefix := strings.ToLower(base[:n-2])
			for _, ext := range []string{".log", ".json", ".txt", ".out", ".err"} {
				if strings.HasSuffix(prefix, ext) {
					base = prefix
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	// 用分隔符替换日期，而不是直接删除：example.com_2026-10-30_access.log
	// 不能变成 example.comaccess（会被误认为另一个合法域名）。
	base = fileDateRe.ReplaceAllString(base, "_")
	base = strings.Trim(base, "_-.")
	if base == "" {
		return Info{}, false
	}
	if addr, err := netip.ParseAddr(base); err == nil {
		return ipInfo(addr), true
	}
	// 形如 access-203.0.113.10 的文件名：提取明确 IP token
	if m := fileIPRe.FindStringSubmatch(base); len(m) == 2 {
		if addr, err := netip.ParseAddr(m[1]); err == nil {
			return ipInfo(addr), true
		}
	}
	// 去掉常见日志前缀/后缀，例如 access-example.com、example.com-access
	for _, prefix := range []string{"access-", "access_", "nginx-", "nginx_", "error-", "error_", "app-", "app_", "log-", "log_", "www-", "www_"} {
		base = strings.TrimPrefix(base, prefix)
	}
	// 不把 ".app" 当文件后缀：app 是真实 gTLD，example.app.log 是合法站点文件。
	for _, suffix := range []string{"-access", "_access", ".access", "-nginx", "_nginx", ".nginx", "-error", "_error", ".error", "-app", "_app", "-log", "_log"} {
		base = strings.TrimSuffix(base, suffix)
	}
	base = strings.Trim(base, "_-.")
	if base == "" {
		return Info{}, false
	}
	// 防止把 2026-10-30 / 28888_access_json 当成域名
	if fileDateRe.MatchString(base) || !strings.Contains(base, ".") {
		return Info{}, false
	}
	tld := base[strings.LastIndexByte(base, '.')+1:]
	if fileLikeTLD[tld] {
		return Info{}, false
	}
	return Normalize(base, stripWWW)
}

// FromDir 从日志文件所在目录推断站点。
// 适用于「一个域名一个目录」的部署：/data/_log/ad.kmrd.cn/access.log。
// 从最近父目录逐级向上，直到 root 为止，返回第一个合法域名/IP。
func FromDir(path, root string, stripWWW bool) (Info, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Info{}, false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		base := filepath.Base(dir)
		if base != "" && base != "." && base != string(filepath.Separator) {
			// 目录名 access.log / backup.json 是日志目录，不是站点；
			// 不要因为语法上像域名就生成一个假站点。
			if tld := base[strings.LastIndexByte(base, '.')+1:]; !fileLikeTLD[tld] {
				if info, ok := normalizeOne(base, stripWWW); ok && IsKnownKind(info.Kind) {
					return info, true
				}
			}
		}
		// root 本身也要检查：单文件模式 -logdir /data/_log/ad.kmrd.cn/access.log
		// 的根目录就是站点目录。检查完 root 后停止，避免把 /data 当站点。
		if samePath(dir, rootAbs) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return Info{}, false
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// SafeDir 返回可直接用于报告目录的安全相对路径。
// 域名和 IP 分开目录，避免同名冲突。
func SafeDir(key string) string {
	info := ParseKey(key)
	switch info.Kind {
	case KindIP:
		return filepath.Join("ip", safeName(info.Name))
	case KindDomain:
		return filepath.Join("domain", safeName(info.Name))
	case KindOther:
		return filepath.Join("other")
	default:
		return filepath.Join("unknown")
	}
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		out = "_"
	}
	if len(out) > 80 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(s))
		out = out[:70] + "-" + strconv.FormatUint(uint64(h.Sum32()), 16)
	}
	return out
}
