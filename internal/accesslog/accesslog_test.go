package accesslog

import (
	"strings"
	"testing"
)

func parseOne(t *testing.T, format *Format, line string) *Fields {
	t.Helper()
	var f Fields
	if !format.Parse([]byte(line), &f) {
		t.Fatalf("Parse failed for %q", line)
	}
	return &f
}

func TestDetectJSON(t *testing.T) {
	f, err := Detect([]byte(`{"@timestamp": "2026-10-30T10:00:00+08:00"}`), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindJSON {
		t.Fatalf("Kind=%s, want json", f.Kind)
	}
}

func TestDetectAndParseCombined(t *testing.T) {
	line := `203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /api/x?y=1 HTTP/1.1" 200 123 "https://ref.example/a" "Mozilla/5.0 (X11; Linux) \"quoted\" UA"`
	f, err := Detect([]byte(line), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindCombined {
		t.Fatalf("Kind=%s, want combined", f.Kind)
	}
	got := parseOne(t, f, line)
	checks := []struct {
		name string
		got  []byte
		want string
	}{
		{"timestamp", got.Timestamp, "30/Oct/2026:10:00:00 +0800"},
		{"client", got.Client, "203.0.113.7"},
		{"method", got.Method, "GET"},
		{"uri", got.URI, "/api/x?y=1"},
		{"status", got.Status, "200"},
		{"size", got.Size, "123"},
		{"referer", got.Referer, "https://ref.example/a"},
		{"agent", got.Agent, `Mozilla/5.0 (X11; Linux) "quoted" UA`},
	}
	for _, c := range checks {
		if string(c.got) != c.want {
			t.Errorf("%s=%q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestCombinedCommonLogFormat(t *testing.T) {
	line := `127.0.0.1 - frank [10/Oct/2000:13:55:36 -0700] "GET /apache_pb.gif HTTP/1.0" 200 2326`
	f, err := Detect([]byte(line), "combined", "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.URI) != "/apache_pb.gif" || string(got.Status) != "200" || string(got.Size) != "2326" {
		t.Errorf("fields=%+v", got)
	}
}

func TestCustomTemplate(t *testing.T) {
	spec := `$time_iso8601 $remote_addr $request_method $request_uri $status $body_bytes_sent $request_time $upstream_response_time $upstream_addr $upstream_status $http_referer $http_user_agent`
	line := `2026-10-30T10:00:00+08:00 1.2.3.4 GET /api/x?y=1 200 123 0.123 0.100 10.0.0.1:80 200 - Mozilla/5.0`
	f, err := Detect([]byte(line), spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindCustom {
		t.Fatalf("Kind=%s, want custom", f.Kind)
	}
	got := parseOne(t, f, line)
	if string(got.Timestamp) != "2026-10-30T10:00:00+08:00" ||
		string(got.Client) != "1.2.3.4" ||
		string(got.Method) != "GET" ||
		string(got.URI) != "/api/x?y=1" ||
		string(got.Status) != "200" ||
		string(got.Size) != "123" ||
		string(got.ResponseTime) != "0.123" ||
		string(got.UpstreamTime) != "0.100" ||
		string(got.UpstreamAddr) != "10.0.0.1:80" ||
		string(got.UpstreamStatus) != "200" ||
		string(got.Agent) != "Mozilla/5.0" {
		t.Errorf("fields=%+v", got)
	}
}

func TestCustomTemplateQuotedRequest(t *testing.T) {
	spec := `$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"`
	line := `203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "POST /api/create?x=1 HTTP/1.1" 201 456 "https://ref.example/a" "Mozilla/5.0 (Windows NT 10.0) Chrome/120"`
	f, err := Detect([]byte(line), spec, "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.Method) != "POST" || string(got.URI) != "/api/create?x=1" ||
		string(got.Referer) != "https://ref.example/a" || string(got.Agent) != "Mozilla/5.0 (Windows NT 10.0) Chrome/120" {
		t.Errorf("fields=%+v", got)
	}
}

func TestTemplateExplicitURIPrecedence(t *testing.T) {
	spec := `$time_iso8601 $remote_addr "$request" $request_uri $status $body_bytes_sent`
	line := `2026-10-30T10:00:00+08:00 1.2.3.4 "GET /from-request HTTP/1.1" /from-uri 200 1`
	f, err := Detect([]byte(line), spec, "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.Method) != "GET" || string(got.URI) != "/from-uri" {
		t.Errorf("method=%q uri=%q, want GET /from-uri", got.Method, got.URI)
	}
}

func TestCSVHeader(t *testing.T) {
	header := `time,client,method,uri,status,size,request_time,agent,referer`
	line := `2026-10-30T10:00:00+08:00,1.2.3.4,GET,/api/x?y=1,200,123,0.12,"Mozilla/5.0, Mobile",https://ref.example/a`
	f, err := Detect([]byte(header), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindCSV || !f.SkipHeader {
		t.Fatalf("Kind=%s SkipHeader=%v", f.Kind, f.SkipHeader)
	}
	got := parseOne(t, f, line)
	if string(got.Timestamp) != "2026-10-30T10:00:00+08:00" ||
		string(got.Client) != "1.2.3.4" ||
		string(got.Method) != "GET" ||
		string(got.URI) != "/api/x?y=1" ||
		string(got.Status) != "200" ||
		string(got.Size) != "123" ||
		string(got.ResponseTime) != "0.12" ||
		string(got.Agent) != "Mozilla/5.0, Mobile" ||
		string(got.Referer) != "https://ref.example/a" {
		t.Errorf("fields=%+v", got)
	}
}

func TestCSVExplicitFields(t *testing.T) {
	spec := `time,client,request,status,size,upstream_response_time,user_agent`
	line := `2026-10-30T10:00:00+08:00,1.2.3.4,"GET /api/x HTTP/1.1",200,123,0.5,Mozilla/5.0`
	f, err := Detect([]byte(line), "csv", spec)
	if err != nil {
		t.Fatal(err)
	}
	if f.SkipHeader {
		t.Fatal("数据行不应被当成表头")
	}
	got := parseOne(t, f, line)
	if string(got.Method) != "GET" || string(got.URI) != "/api/x" ||
		string(got.UpstreamTime) != "0.5" || string(got.Agent) != "Mozilla/5.0" {
		t.Errorf("fields=%+v", got)
	}
}

func TestCSVHeaderWithQuotedComma(t *testing.T) {
	header := `"time","remote_addr","request_method","request_uri","status","body_bytes_sent"`
	line := `"2026-10-30T10:00:00+08:00","1.2.3.4","GET","/a,b","200","1"`
	f, err := Detect([]byte(header), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.URI) != "/a,b" || string(got.Client) != "1.2.3.4" {
		t.Errorf("fields=%+v", got)
	}
}

func TestDetectInvalid(t *testing.T) {
	if _, err := Detect([]byte(`this is not a log line`), "auto", ""); err == nil {
		t.Fatal("应返回无法识别的错误")
	}
	if _, err := Detect([]byte(`x`), `$remote_addr $status`, ""); err == nil {
		t.Fatal("自定义模板缺少时间字段应报错")
	}
	if _, err := Detect([]byte(`x`), `$time_iso8601$remote_addr`, ""); err == nil {
		t.Fatal("相邻变量缺少分隔符应报错")
	}
}

func TestTemplateVarBraces(t *testing.T) {
	spec := `${time_iso8601} ${remote_addr} "${request}" ${status} ${body_bytes_sent}`
	line := `2026-10-30T10:00:00+08:00 1.2.3.4 "GET /a HTTP/1.1" 200 12`
	f, err := Detect([]byte(line), spec, "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseOne(t, f, line)
	if string(got.URI) != "/a" || string(got.Status) != "200" {
		t.Errorf("fields=%+v", got)
	}
}

func TestSampleTruncates(t *testing.T) {
	s := sample([]byte(strings.Repeat("x", 300)))
	if len(s) <= 220 || !strings.HasSuffix(s, "...") {
		t.Errorf("sample len=%d s=%q", len(s), s)
	}
}

func TestCombinedWithRequestTimeAuto(t *testing.T) {
	lines := []string{
		`203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" 0.123`,
		`203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" 0.123 0.100`,
		`203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" 0.123 0.100 10.0.0.1:80 200`,
	}
	for _, line := range lines {
		f, err := Detect([]byte(line), "auto", "")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		got := parseOne(t, f, line)
		if string(got.ResponseTime) != "0.123" {
			t.Errorf("ResponseTime=%q, want 0.123", got.ResponseTime)
		}
	}
}

func TestCombinedAmbiguousExtraFailsAuto(t *testing.T) {
	line := `203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" some-extra-field`
	if _, err := Detect([]byte(line), "auto", ""); err == nil {
		t.Fatal("无法分类的额外字段应报错，提示用 -log-format")
	}
}

func TestSampleRedactsQuery(t *testing.T) {
	line := `[IP] - - [30/Oct/2026:10:00:00 +0800] "GET /a?token=super-secret HTTP/1.1" 200 1 "-" "UA"`
	s := sample([]byte(line))
	if strings.Contains(s, "super-secret") || !strings.Contains(s, "<redacted>") {
		t.Fatalf("sample=%q", s)
	}
}

func TestCSVSkipHeaderOption(t *testing.T) {
	first := "日期,客户端,接口\n"
	f, err := DetectWithOptions([]byte(first), DetectOptions{
		Configured:    "csv",
		CSVFields:     "time,client,uri",
		CSVSkipHeader: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.SkipHeader {
		t.Fatal("CSVSkipHeader 应强制跳过首行")
	}
}

func TestCSVRecordCompleteFromMatchesFull(t *testing.T) {
	parts := [][]byte{[]byte(`a,"b`), []byte(`c`), []byte(`d"`)}
	var buf []byte
	inQuotes := false
	for i, part := range parts {
		if i > 0 {
			buf = append(buf, 0x0a)
		}
		buf = append(buf, part...)
		wantComplete := csvRecordComplete(buf)
		gotComplete, next := csvRecordCompleteFrom(part, inQuotes)
		if gotComplete != wantComplete {
			t.Fatalf("第 %d 段 complete=%v，整段扫描=%v", i+1, gotComplete, wantComplete)
		}
		if next != !wantComplete {
			t.Fatalf("第 %d 段 nextInQuotes=%v，期望 %v", i+1, next, !wantComplete)
		}
		inQuotes = next
	}
}

func TestCSVRecordComplete(t *testing.T) {
	if csvRecordComplete([]byte(`a,"b`)) {
		t.Fatal("未闭合引号不应视为完整记录")
	}
	if !csvRecordComplete([]byte("a,\"b\nc\"")) {
		t.Fatal("引号闭合后应视为完整记录")
	}
	if !csvRecordComplete([]byte(`a,O'Brien,c`)) {
		t.Fatal("普通字段中的单引号不应触发跨行")
	}
}

func TestCombinedSingleFloatWarning(t *testing.T) {
	line := `[IP] - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" 0.123`
	f, err := Detect([]byte(line), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Warning == "" {
		t.Fatal("单个浮点扩展字段应给出歧义提示")
	}
}

func TestValidateConfigured(t *testing.T) {
	if err := ValidateConfigured("auto"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfigured("$remote_addr $status"); err == nil {
		t.Fatal("缺少时间字段的模板应报错")
	}
	if err := ValidateCSVFields("time,client"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCSVFields("client,uri"); err == nil {
		t.Fatal("CSV 列定义缺少时间字段应报错")
	}
}

func TestCSVSplitColumnCap(t *testing.T) {
	line := strings.Repeat(",", maxCSVColumns+10)
	if got := splitCSV([]byte(line)); got != nil {
		t.Fatalf("超限 CSV 应返回 nil，实际 %d 列", len(got))
	}
}

func TestSampleStripsControlChars(t *testing.T) {
	s := sample([]byte("a\x1b[31mred\x00b"))
	if strings.ContainsAny(s, "\x1b\x00") {
		t.Fatalf("sample 仍含控制字符: %q", s)
	}
}
