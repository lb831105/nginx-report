package site

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rule 一条站点文件映射。
type Rule struct {
	Pattern string
	Info    Info
	order   int
	exact   bool
}

// Resolver 站点文件映射 + 别名 + 强制站点。
type Resolver struct {
	LogDir   string
	StripWWW bool
	Mode     string // auto / field / map / filename / dir
	Rules    []Rule
	Aliases  map[string]string
	Forced   *Info
}

// NewResolver 创建空解析器。
func NewResolver(logDir string, stripWWW bool) *Resolver {
	return &Resolver{LogDir: logDir, StripWWW: stripWWW, Mode: "auto", Aliases: make(map[string]string)}
}

// LoadMap 读取映射文件，格式：
//
//	example.com = example.com*.log
//	api.example.com = /var/log/nginx/api_access.log
//
// 左侧是站点，右侧是文件路径/通配符；相对路径基于 logDir。
func (r *Resolver) LoadMap(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开站点映射失败 %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 允许注释跟在后面
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d 映射格式应为 站点 = 文件模式", path, lineNo)
		}
		info, ok := Normalize(strings.TrimSpace(k), r.StripWWW)
		if !ok {
			return fmt.Errorf("%s:%d 站点 %q 无效", path, lineNo, strings.TrimSpace(k))
		}
		pat := strings.TrimSpace(v)
		if pat == "" {
			return fmt.Errorf("%s:%d 文件模式为空", path, lineNo)
		}
		if !filepath.IsAbs(pat) {
			pat = filepath.Join(r.LogDir, pat)
		}
		pat = filepath.Clean(pat)
		if _, err := filepath.Match(pat, "x"); err != nil {
			return fmt.Errorf("%s:%d 文件模式 %q 无效: %w", path, lineNo, pat, err)
		}
		info.Source = "map"
		r.Rules = append(r.Rules, Rule{
			Pattern: pat, Info: info,
			order: lineNo,
			exact: !strings.ContainsAny(pat, "*?["),
		})
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取站点映射失败: %w", err)
	}
	// 精确路径优先；同类型按模式长度降序，再按文件顺序。
	sort.SliceStable(r.Rules, func(i, j int) bool {
		a, b := r.Rules[i], r.Rules[j]
		if a.exact != b.exact {
			return a.exact
		}
		if len(a.Pattern) != len(b.Pattern) {
			return len(a.Pattern) > len(b.Pattern)
		}
		return a.order < b.order
	})
	return nil
}

// LoadAliases 读取别名文件：alias.example.com = example.com
func (r *Resolver) LoadAliases(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开站点别名失败 %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d 别名格式应为 别名 = 主站点", path, lineNo)
		}
		a, ok1 := Normalize(strings.TrimSpace(k), r.StripWWW)
		t, ok2 := Normalize(strings.TrimSpace(v), r.StripWWW)
		if !ok1 || !ok2 || a.Kind != KindDomain || t.Kind != KindDomain {
			return fmt.Errorf("%s:%d 别名/主站点必须是合法域名", path, lineNo)
		}
		if r.Aliases == nil {
			r.Aliases = make(map[string]string)
		}
		r.Aliases[a.Name] = t.Name
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取站点别名失败: %w", err)
	}
	return nil
}

// SetForced 强制所有文件归属该站点。
func (r *Resolver) SetForced(raw string) error {
	info, ok := Normalize(raw, r.StripWWW)
	if !ok {
		return fmt.Errorf("站点 %q 无效", raw)
	}
	if info.Kind == KindDomain {
		info = ApplyAlias(info, r.Aliases)
	}
	info.Source = "name"
	r.Forced = &info
	return nil
}

// Resolve 归一化字段值并应用别名。
func (r *Resolver) Resolve(raw string) (Info, bool) {
	info, ok := Normalize(raw, r.StripWWW)
	if !ok {
		return Info{}, false
	}
	if info.Kind == KindDomain {
		info = ApplyAlias(info, r.Aliases)
	}
	return info, true
}

// ResolveFile 返回文件的默认站点。
// auto：强制站点 > map > 文件名 > 目录名 > 未识别；
// map / filename / dir 只使用对应来源，field 不使用文件级来源。
func (r *Resolver) ResolveFile(path string) Info {
	if r.Forced != nil {
		return *r.Forced
	}
	mode := r.Mode
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "map":
		if info, ok := r.resolveMap(path); ok {
			return r.withAlias(info)
		}
		return Unknown()
	case "filename":
		if info, ok := r.resolveFilename(path); ok {
			return r.withAlias(info)
		}
		return Unknown()
	case "dir":
		if info, ok := r.resolveDir(path); ok {
			return r.withAlias(info)
		}
		return Unknown()
	case "field":
		return Unknown()
	}
	if info, ok := r.resolveMap(path); ok {
		return r.withAlias(info)
	}
	if info, ok := r.resolveFilename(path); ok {
		return r.withAlias(info)
	}
	if info, ok := r.resolveDir(path); ok {
		return r.withAlias(info)
	}
	return Unknown()
}

func (r *Resolver) withAlias(info Info) Info {
	if info.Kind == KindDomain {
		info = ApplyAlias(info, r.Aliases)
	}
	return info
}

func (r *Resolver) resolveMap(path string) (Info, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, rule := range r.Rules {
		if ok, _ := filepath.Match(rule.Pattern, abs); ok {
			return rule.Info, true
		}
	}
	return Info{}, false
}

func (r *Resolver) resolveFilename(path string) (Info, bool) {
	info, ok := FromFilename(path, r.StripWWW)
	if ok {
		info.Source = "filename"
	}
	return info, ok
}

func (r *Resolver) resolveDir(path string) (Info, bool) {
	info, ok := FromDir(path, r.LogDir, r.StripWWW)
	if ok {
		info.Source = "dir"
	}
	return info, ok
}

// ParseFilter 把 -site 的值归一化成 key 集合。
func ParseFilter(raw string, stripWWW bool, aliases map[string]string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		info, ok := Normalize(part, stripWWW)
		if !ok {
			return nil, fmt.Errorf("站点过滤值 %q 无效", part)
		}
		if info.Kind == KindDomain {
			info = ApplyAlias(info, aliases)
		}
		out[info.Key] = true
	}
	return out, nil
}
