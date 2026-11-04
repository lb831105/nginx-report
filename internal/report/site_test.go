package report

import (
	"strings"
	"testing"
)

func TestSiteComparisonEscapesName(t *testing.T) {
	rows := []SiteRow{{Key: "d:x", Name: `<script>alert(1)</script>`, Kind: "domain", Total: 1, ErrorRate: 100}}
	var b strings.Builder
	writeSiteComparisonHTML(&b, rows, 1)
	out := b.String()
	if strings.Contains(out, "<script>") || !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("site name not escaped: %s", out)
	}
}

func TestOpsSiteComparisonCleansControl(t *testing.T) {
	rows := []SiteRow{{Key: "d:x", Name: "a\x1b[31mb", Kind: "domain", Total: 2, P95: 10}}
	out := OpsSiteComparison(rows, 2)
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("control char leaked: %q", out)
	}
}

func TestSitesIndexEscapesName(t *testing.T) {
	rows := []SiteIndexEntry{{SiteRow: SiteRow{Name: `<x>`, Kind: "domain"}, Dir: `sites/domain/x`, Base: "b", Formats: []string{"html"}}}
	out := string(SitesIndexHTML(rows, "2026-10-30", "2026-10-30", `<title>evil`))
	if strings.Contains(out, "<x>") || strings.Contains(out, "<title>evil") {
		t.Fatalf("index not escaped: %s", out)
	}
}
