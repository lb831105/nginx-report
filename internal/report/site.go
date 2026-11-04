package report

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

// SiteRow 全站报告里的站点对比行。
type SiteRow struct {
	Key        string
	Name       string
	Kind       string
	Total      int64
	Size       int64
	ErrorCount int64
	SlowCount  int64
	ErrorRate  float64
	SlowRate   float64
	AvgMs      float64
	P95        float64
	P99        float64
	Sources    map[string]int64
	StaticSkip int64
	BotSkip    int64
}

// SiteIndexEntry 站点索引页条目。
type SiteIndexEntry struct {
	SiteRow
	Dir     string
	Base    string
	Formats []string
}

// SiteRows 复制并排序站点行，按 PV 降序。
func SiteRows(rows []SiteRow) []SiteRow {
	out := append([]SiteRow(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// siteKindLabel 站点类型展示。
func siteKindLabel(k string) string {
	switch k {
	case "domain":
		return "域名"
	case "ip":
		return "IP直连"
	case "other":
		return "溢出"
	default:
		return "未识别"
	}
}

// writeSiteComparisonHTML 输出全站报告的站点对比表。
func writeSiteComparisonHTML(b *strings.Builder, rows []SiteRow, total int64) {
	if len(rows) == 0 {
		return
	}
	b.WriteString(`<div class="section"><h2>站点对比<span class="aud">全部站点</span></h2><table><thead><tr><th>类型</th><th>站点</th><th class="num">PV</th><th class="num">占比</th><th class="num">流量</th><th class="num">错误率</th><th class="num">慢请求</th><th class="num">P95</th><th class="num">P99</th></tr></thead><tbody>`)
	for _, r := range rows {
		name := html.EscapeString(cleanText(r.Name))
		if r.Key == "u:unknown" {
			name = `(未识别站点)`
		}
		fmt.Fprintf(b, `<tr><td>%s</td><td class="mono">%s</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%d</td><td class="num">%.0fms</td><td class="num">%.0fms</td></tr>`,
			siteKindLabel(r.Kind), name, humanNum(r.Total), pct(r.Total, total), humanBytes(float64(r.Size)),
			r.ErrorRate, r.SlowCount, r.P95, r.P99)
	}
	b.WriteString(`</tbody></table></div>`)
}

// OpsSiteComparison 运维文本里的站点对比。
func OpsSiteComparison(rows []SiteRow, total int64) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【站点对比】\n")
	for _, r := range rows {
		name := cleanText(r.Name)
		if r.Key == "u:unknown" {
			name = "(未识别站点)"
		}
		fmt.Fprintf(&b, "  %-8s\t%-28s\t%d\t%.2f%%\t%s\t错误 %.2f%%\t慢 %d\tP95 %.0fms\n",
			siteKindLabel(r.Kind), name, r.Total, pct(r.Total, total), humanBytes(float64(r.Size)),
			r.ErrorRate, r.SlowCount, r.P95)
	}
	b.WriteString("\n")
	return b.String()
}

// SitesIndexHTML 生成站点索引页。
func SitesIndexHTML(rows []SiteIndexEntry, start, end, title string) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"UTF-8\"><title>站点索引</title><style>")
	b.WriteString("body{font-family:-apple-system,'Microsoft YaHei',sans-serif;background:#f0f2f5;padding:24px;color:#333}.box{max-width:1200px;margin:auto;background:#fff;padding:24px;border-radius:10px}table{width:100%;border-collapse:collapse;font-size:13px}th,td{padding:8px;border-bottom:1px solid #eee;text-align:left}th{background:#f8fafc}.num{text-align:right}.mono{font-family:Consolas,monospace}a{color:#3949ab;text-decoration:none}a:hover{text-decoration:underline}.muted{color:#888;font-size:12px}")
	b.WriteString("</style></head><body><div class=\"box\"><h1>")
	b.WriteString(html.EscapeString(cleanText(title)))
	b.WriteString("</h1><div class=\"muted\">")
	b.WriteString(html.EscapeString(start + " ~ " + end))
	b.WriteString("</div><table><thead><tr><th>类型</th><th>站点</th><th class=\"num\">PV</th><th class=\"num\">流量</th><th class=\"num\">错误率</th><th class=\"num\">慢请求</th><th>报告</th></tr></thead><tbody>")
	for _, r := range rows {
		name := html.EscapeString(cleanText(r.Name))
		links := make([]string, 0, len(r.Formats))
		for _, f := range r.Formats {
			href := r.Dir + "/" + r.Base + "." + f
			links = append(links, fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(href), strings.ToUpper(f)))
		}
		fmt.Fprintf(&b, `<tr><td>%s</td><td class="mono">%s</td><td class="num">%d</td><td class="num">%s</td><td class="num">%.2f%%</td><td class="num">%d</td><td>%s</td></tr>`,
			siteKindLabel(r.Kind), name, r.Total, humanBytes(float64(r.Size)), r.ErrorRate, r.SlowCount, strings.Join(links, " "))
	}
	b.WriteString(`</tbody></table></div></body></html>`)
	return []byte(b.String())
}
