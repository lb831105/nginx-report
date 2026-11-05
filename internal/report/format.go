package report

import (
	"fmt"
	"strings"
)

// 输出格式标识
const (
	FormatHTML = "html"
	FormatPDF  = "pdf"
	FormatXLSX = "xlsx"
)

// FormatAliases 允许用户写的别名 → 实际格式
// （很多人习惯说 xls，但真正的老式 .xls 是二进制格式，我们用 OOXML 的 .xlsx）
var formatAliases = map[string]string{
	"html":  FormatHTML,
	"htm":   FormatHTML,
	"pdf":   FormatPDF,
	"xlsx":  FormatXLSX,
	"xls":   FormatXLSX,
	"excel": FormatXLSX,
}

// Formats 解析 -format 参数，返回去重后的格式列表（保持用户书写顺序）。
// 空值或非法值回退到 html（默认行为不变）。
func Formats(spec string) []string {
	if strings.TrimSpace(spec) == "" {
		return []string{FormatHTML}
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		key := strings.ToLower(strings.TrimSpace(part))
		if key == "" {
			continue
		}
		if key == "all" {
			for _, f := range []string{FormatHTML, FormatPDF, FormatXLSX} {
				if !seen[f] {
					seen[f] = true
					out = append(out, f)
				}
			}
			continue
		}
		f, ok := formatAliases[key]
		if !ok {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{FormatHTML}
	}
	return out
}

// ValidateFormats 在启动阶段校验 -format；不能让拼写错误静默回退到 html。
func ValidateFormats(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		key := strings.ToLower(strings.TrimSpace(part))
		if key == "" || key == "all" {
			continue
		}
		if _, ok := formatAliases[key]; !ok {
			return fmt.Errorf("未知格式 %q（可选 html / pdf / xlsx / all，可用逗号组合）", strings.TrimSpace(part))
		}
	}
	return nil
}

// FormatNames 便于日志/帮助信息展示
func FormatNames() string {
	return fmt.Sprintf("%s（默认） / %s / %s（别名 xls, excel），可用逗号组合，或 all", FormatHTML, FormatPDF, FormatXLSX)
}
