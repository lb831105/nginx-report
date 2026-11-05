package accesslog

import "testing"

func TestCombinedHostParserSetsSite(t *testing.T) {
	line := `203.0.113.10 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 100 "-" "curl/8" "www.example.com"`
	f, err := DetectWithOptions([]byte(line), DetectOptions{Configured: "combined_host"})
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.Site) != "www.example.com" {
		t.Fatalf("site=%q fields=%+v", got.Site, got)
	}
}

func TestAutoDetectCombinedHost(t *testing.T) {
	line := `203.0.113.10 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 100 "-" "curl/8" "www.example.com"`
	f, err := DetectWithOptions([]byte(line), DetectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "combined_host" {
		t.Fatalf("format=%s", f.Name)
	}
}

func TestCSVHeaderSiteField(t *testing.T) {
	first := "time,client,custom_domain,status,request,size\n2026-10-30T10:00:00+08:00,203.0.113.10,shop.example.com,200,GET /a HTTP/1.1,100"
	f, err := DetectWithOptions([]byte(first), DetectOptions{SiteField: "custom_domain"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "csv" {
		t.Fatalf("format=%s", f.Name)
	}
	got := parseOne(t, f, "2026-10-30T10:00:00+08:00,203.0.113.10,shop.example.com,200,GET /a HTTP/1.1,100")
	if string(got.Site) != "shop.example.com" {
		t.Fatalf("site=%q", got.Site)
	}
}

func TestTemplateHostMapsSite(t *testing.T) {
	spec := `$time_iso8601 $remote_addr "$request" $status $body_bytes_sent "$host"`
	line := `2026-10-30T10:00:00+08:00 203.0.113.10 "GET /a HTTP/1.1" 200 100 "shop.example.com"`
	f, err := DetectWithOptions([]byte(line), DetectOptions{Configured: spec})
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.Site) != "shop.example.com" {
		t.Fatalf("site=%q", got.Site)
	}
}
