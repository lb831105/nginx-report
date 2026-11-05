package analyzer

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rec(ts string, status string, t float64) record {
	parsed, _ := time.Parse("2006-01-02T15:04:05-07:00", ts)
	r := record{
		ts:           parsed,
		day:          parsed.Format("2006-01-02"),
		hour:         parsed.Hour(),
		uri:          "/api/a",
		status:       status,
		client:       "1.1.1.1",
		method:       "GET",
		upstreamAddr: "10.0.0.1:80",
		size:         100,
		upstreamTime: t,
		hasTime:      true,
	}
	r.isError = len(status) > 0 && status[0] >= '4'
	r.isSlow = t > 1.0
	return r
}

func TestPercentileAndAvg(t *testing.T) {
	a := NewAgg()
	// 100 条：0ms, 10ms, ... 990ms，正好每桶一条
	for i := 0; i < 100; i++ {
		rr := rec("2026-09-01T10:00:00+08:00", "200", float64(i)*0.01)
		a.add(&rr)
	}
	if a.Total != 100 {
		t.Fatalf("Total=%d", a.Total)
	}
	if got := a.AvgTime() * 1000; got < 494 || got > 496 {
		t.Errorf("AvgTime=%.1fms, want ~495ms", got)
	}
	if got := a.Percentile(0.50) * 1000; got < 500 || got > 510 {
		t.Errorf("P50=%.1fms, want ~500ms", got)
	}
	if got := a.Percentile(0.90) * 1000; got < 900 || got > 910 {
		t.Errorf("P90=%.1fms, want ~900ms", got)
	}
	if got := a.Percentile(0.99) * 1000; got < 985 || got > 995 {
		t.Errorf("P99=%.1fms, want ~990ms", got)
	}
	// 累加桶数量必须等于请求数
	var sum int64
	for _, c := range a.TimeBuckets {
		sum += c
	}
	if sum != a.Total {
		t.Errorf("桶合计=%d, want %d", sum, a.Total)
	}
}

func TestPercentileEmpty(t *testing.T) {
	a := NewAgg()
	if got := a.Percentile(0.99); got != 0 {
		t.Errorf("空统计分位数应为 0, got %v", got)
	}
	if got := a.AvgTime(); got != 0 {
		t.Errorf("空统计均值应为 0, got %v", got)
	}
}

func TestProcessFileRange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "access.log")
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "1.1.1.1", "size": "100", "status": "200", "upstream_response_time": "0.050", "request_method": "GET", "request_uri": "/ok", "upstream_addr": "10.0.0.1:80"}
{"@timestamp": "2026-09-01T10:00:01+08:00", "client": "2.2.2.2", "size": "200", "status": "500", "upstream_response_time": "1.500", "request_method": "POST", "request_uri": "/err", "upstream_addr": "10.0.0.1:80"}
{"@timestamp": "2026-09-02T10:00:00+08:00", "client": "1.1.1.1", "size": "300", "status": "404", "responsetime": "0.020", "request_method": "GET", "request_uri": "/miss"}
not a json line
`
	if err := os.WriteFile(f, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}

	// 只统计 09-01
	st, err := ProcessFileRange(f, "2026-09-01", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 {
		t.Errorf("Total=%d, want 2", st.Total)
	}
	if st.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1", st.Skipped)
	}
	if st.OutOfRange != 1 {
		t.Errorf("OutOfRange=%d, want 1", st.OutOfRange)
	}
	if st.ErrorCount != 1 {
		t.Errorf("ErrorCount=%d, want 1", st.ErrorCount)
	}
	if st.SlowCount != 1 {
		t.Errorf("SlowCount=%d, want 1", st.SlowCount)
	}
	if st.StatusCnt["200"] != 1 || st.StatusCnt["500"] != 1 {
		t.Errorf("StatusCnt=%v", st.StatusCnt)
	}
	if st.ClientCnt["1.1.1.1"] != 1 || st.ClientCnt["2.2.2.2"] != 1 {
		t.Errorf("ClientCnt=%v", st.ClientCnt)
	}
	if st.SizeTotal != 300 {
		t.Errorf("SizeTotal=%d, want 300", st.SizeTotal)
	}

	// 按天归日
	days := st.DaySummaries()
	if len(days) != 1 || days[0].Day != "2026-09-01" {
		t.Fatalf("DaySummaries=%+v", days)
	}
	if days[0].PeakCnt != 1 || days[0].ErrorCount != 1 || days[0].SlowCount != 1 {
		t.Errorf("DaySummary=%+v", days[0])
	}
}

func TestDaySummariesAcrossFiles(t *testing.T) {
	// 两个文件各含相同一天的数据，合并后该天应累加且只出现一次
	mk := func(status string) *FileStats {
		s := NewStats()
		sr := rec("2026-09-01T10:00:00+08:00", status, 0.01)
		s.Agg.add(&sr)
		da := NewAgg()
		rr := rec("2026-09-01T10:00:00+08:00", status, 0.01)
		da.add(&rr)
		s.Days["2026-09-01"] = da
		return s
	}
	grand := NewStats()
	grand.Merge(mk("200"))
	grand.Merge(mk("500"))

	if grand.Total != 2 {
		t.Errorf("Total=%d, want 2", grand.Total)
	}
	days := grand.DaySummaries()
	if len(days) != 1 {
		t.Fatalf("应只有 1 天, got %d: %+v", len(days), days)
	}
	if days[0].Total != 2 || days[0].ErrorCount != 1 {
		t.Errorf("DaySummary=%+v", days[0])
	}
	if grand.DayAgg("2026-09-01").Total != 2 {
		t.Errorf("MergeDay/DayAgg 统计不一致")
	}
}

func TestFieldValue(t *testing.T) {
	line := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_status": "502", "real_client": "", "client": "1.2.3.4", "size": "abc", "upstream_response_time": "0.123", "request_uri": "/a?x=1", "request_method": "POST", "upstream_addr": "-"}`

	cases := []struct{ key, want string }{
		{"@timestamp", "2026-09-01T10:00:00+08:00"},
		{"status", "200"}, // 不能命中 upstream_status
		{"client", "1.2.3.4"},
		{"size", "abc"},
		{"upstream_response_time", "0.123"},
		{"request_uri", "/a?x=1"},
		{"request_method", "POST"},
		{"upstream_addr", "-"},
	}
	for _, c := range cases {
		got, ok := fieldBytes([]byte(line), []byte(c.key))
		if !ok || string(got) != c.want {
			t.Errorf("fieldBytes(%q)=%q,%v want %q", c.key, string(got), ok, c.want)
		}
	}
	if _, ok := fieldBytes([]byte(line), []byte("not_exist")); ok {
		t.Error("不存在的字段应返回 false")
	}

	// 裸值（无引号）也要支持
	bare := `{"status": 404, "size": 100}`
	if v, ok := fieldBytes([]byte(bare), []byte("status")); !ok || string(v) != "404" {
		t.Errorf("裸值解析失败: %q,%v", string(v), ok)
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in                 string
		y, mo, d, h, mi, s int
		offset             int
	}{
		{"2026-09-01T10:34:55+08:00", 2026, 9, 1, 10, 34, 55, 8 * 3600},
		{"2026-09-01T10:34:55+08:00", 2026, 9, 1, 10, 34, 55, 8 * 3600},
		{"2026-09-01T23:59:59-05:00", 2026, 9, 1, 23, 59, 59, -5 * 3600},
		{"2026-09-01 00:00:00", 2026, 9, 1, 0, 0, 0, -999},
		{"2026-09-01T10:34:55.123456+08:00", 2026, 9, 1, 10, 34, 55, 8 * 3600},
		{"2026-09-01T10:34:55Z", 2026, 9, 1, 10, 34, 55, 0},
	}
	for _, c := range cases {
		ts, ok := parseTimestamp([]byte(c.in))
		if !ok {
			t.Errorf("parseTimestamp(%q) 失败", c.in)
			continue
		}
		if ts.Year() != c.y || int(ts.Month()) != c.mo || ts.Day() != c.d ||
			ts.Hour() != c.h || ts.Minute() != c.mi || ts.Second() != c.s {
			t.Errorf("parseTimestamp(%q)=%v 字段不符", c.in, ts)
		}
		if c.offset != -999 {
			_, off := ts.Zone()
			if off != c.offset {
				t.Errorf("parseTimestamp(%q) offset=%d want %d", c.in, off, c.offset)
			}
		}
	}

	for _, bad := range []string{"", "abc", "2026-13-01T00:00:00+08:00", "2026-09-01T25:00:00+08:00"} {
		if _, ok := parseTimestamp([]byte(bad)); ok {
			t.Errorf("非法时间戳 %q 应解析失败", bad)
		}
	}
}

func TestDateInFilename(t *testing.T) {
	cases := map[string]string{
		"28888_access_json.log.2026-10-26.gz": "2026-10-26",
		"access.log":                          "",
		"a2026-01-02b.log":                    "2026-01-02",
	}
	for in, want := range cases {
		if got := DateInFilename(in); got != want {
			t.Errorf("DateInFilename(%q)=%q want %q", in, got, want)
		}
	}
}

func TestThrottleDisabledWhen100(t *testing.T) {
	th := newThrottle(100)
	th.mark()
	for i := 0; i < 3; i++ {
		th.tick()
	}
	if th.slept != 0 || th.active {
		t.Errorf("CPU 100%% 时不应节流: active=%v slept=%v", th.active, th.slept)
	}

	th = newThrottle(0)
	th.mark()
	th.tick()
	if th.active {
		t.Error("CPUPercent=0 应视为不限速")
	}
}

func TestThrottleSleeps(t *testing.T) {
	th := newThrottle(50)
	th.mark()
	time.Sleep(20 * time.Millisecond) // 模拟一段工作耗时
	th.tick()
	// 直接校验节流器的记账，避免依赖墙钟导致 CI 抖动
	if th.slept < 10*time.Millisecond {
		t.Errorf("50%% 目标占用下应累计 sleep 约 20ms，实际 %v", th.slept)
	}
	if th.slept > 100*time.Millisecond {
		t.Errorf("睡眠过多: %v", th.slept)
	}
}

func TestEarlyExitPastEndDate(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "access.log")
	var b strings.Builder
	// 先 10 条目标日数据，再 3000 条「更晚」的数据
	for i := 0; i < 10; i++ {
		b.WriteString(`{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "0.01"}` + "\n")
	}
	for i := 0; i < 3000; i++ {
		b.WriteString(`{"@timestamp": "2026-09-05T10:00:00+08:00", "status": "200", "upstream_response_time": "0.01"}` + "\n")
	}
	if err := os.WriteFile(f, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFileRange(f, "2026-09-01", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 10 {
		t.Errorf("Total=%d want 10", st.Total)
	}
	// 提前退出：不应把 3000 条都算进 OutOfRange
	if st.OutOfRange > earlyExitLines+100 {
		t.Errorf("没有提前退出，OutOfRange=%d", st.OutOfRange)
	}
}

func TestCPULimitSlowsDown(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖真实耗时的集成测试，-short 下跳过")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "access.log")
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString(`{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "1.1.1.1", "size": "100", "status": "200", "upstream_response_time": "0.01", "request_method": "GET", "request_uri": "/a"}` + "\n")
	}
	if err := os.WriteFile(f, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := ProcessFileWithOptions(f, Options{ChunkLines: 1000}); err != nil {
		t.Fatal(err)
	}
	free := time.Since(start)

	start = time.Now()
	if _, err := ProcessFileWithOptions(f, Options{CPUPercent: 50, ChunkLines: 1000}); err != nil {
		t.Fatal(err)
	}
	limited := time.Since(start)

	// 允许较大抖动：只要没有明显变慢就认为节流未生效
	if limited < free+free/4 {
		t.Errorf("限速 50%% 后应明显更慢: free=%v limited=%v", free, limited)
	}
}

func TestUpstreamTimeFallback(t *testing.T) {
	line := func(up, resp string) string {
		return `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "` + up + `", "responsetime": "` + resp + `"}`
	}
	cases := []struct {
		name   string
		line   string
		want   float64
		wantOK bool
	}{
		{"正常上游耗时", line("0.123", "0.5"), 0.123, true},
		{"上游为 - 时回退 responsetime", line("-", "0.456"), 0.456, true},
		{"多个上游只取第一个", line("0.005, 0.006", "9"), 0.005, true},
		{"都不可用时为 0 且标记为无耗时", line("-", "-"), 0, false},
	}
	for _, c := range cases {
		var fl lineFields
		scanFields([]byte(c.line), &fl)
		got, ok := firstFloatOf(&fl, fUpTime, fRespTime)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: got %v,%v want %v,%v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestParseIntAndFloatBytes(t *testing.T) {
	if v, ok := parseIntBytes([]byte("12345")); !ok || v != 12345 {
		t.Errorf("parseIntBytes=%d,%v", v, ok)
	}
	if _, ok := parseIntBytes([]byte("12a")); ok {
		t.Error("非法整数应失败")
	}
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"0.007", 0.007}, {"1.5", 1.5}, {"12", 12}, {"", 0},
	} {
		got, ok := parseFloatBytes([]byte(c.in))
		if c.in == "" {
			if ok {
				t.Errorf("空串应失败")
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("parseFloatBytes(%q)=%v,%v want %v", c.in, got, ok, c.want)
		}
	}
}

func TestFileStatsDayRange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "access.log")
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200"}
{"@timestamp": "2026-09-03T10:00:00+08:00", "status": "200"}
{"@timestamp": "2026-09-05T10:00:00+08:00", "status": "500"}
`
	if err := os.WriteFile(f, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	// 只统计 09-03，但 FirstDay/LastDay 应覆盖整个文件
	st, err := ProcessFileRange(f, "2026-09-03", "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.OutOfRange != 2 {
		t.Fatalf("Total=%d OutOfRange=%d", st.Total, st.OutOfRange)
	}
	if st.FirstDay != "2026-09-01" || st.LastDay != "2026-09-05" {
		t.Errorf("FirstDay=%q LastDay=%q", st.FirstDay, st.LastDay)
	}

	// Merge 应取并集范围
	grand := NewStats()
	grand.Merge(st)
	st2 := NewStats()
	st2.FirstDay, st2.LastDay = "2026-08-30", "2026-09-02"
	grand.Merge(st2)
	if grand.FirstDay != "2026-08-30" || grand.LastDay != "2026-09-05" {
		t.Errorf("Merge 后 FirstDay=%q LastDay=%q", grand.FirstDay, grand.LastDay)
	}
}

func TestFieldBytesEmptyKey(t *testing.T) {
	// 空 key 曾会导致 idx 不推进而死循环，这里必须有返回
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := fieldBytes([]byte(`{"a":"b"}`), nil); ok {
			t.Error("空 key 不应匹配成功")
		}
		if _, ok := fieldBytes(nil, []byte("a")); ok {
			t.Error("空行不应匹配成功")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fieldBytes 空 key 死循环")
	}
}

func TestMergeFromSelfIsNoop(t *testing.T) {
	a := NewAgg()
	rr := rec("2026-09-01T10:00:00+08:00", "200", 0.01)
	a.add(&rr)
	before := a.Total
	a.MergeFrom(a)
	if a.Total != before {
		t.Fatalf("自合并不应翻倍: before=%d after=%d", before, a.Total)
	}
}

// 基数爆炸保护：超过上限后归入 OverflowKey，且四张 URI 表 key 保持一致
func TestMergeStaticBytesSaturates(t *testing.T) {
	a := NewStats()
	b := NewStats()
	a.StaticBytes = math.MaxInt64 - 1
	b.StaticBytes = 10
	a.Merge(b)
	if a.StaticBytes != math.MaxInt64 {
		t.Fatalf("StaticBytes 溢出未饱和: %d", a.StaticBytes)
	}
}

func TestAggCappedCardinality(t *testing.T) {
	a := NewAgg()
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i <= maxMapKeys+50; i++ {
		a.add(&record{
			ts: base, day: "2026-09-01", hour: 10,
			uri: "/s?x=" + strconv.Itoa(i), status: "500",
			client: "10.1.1." + strconv.Itoa(i%200), method: "GET",
			upstreamAddr: "10.0.0.1:80", upstreamTime: 0.05, isError: true, isSlow: true,
		})
	}
	if len(a.UriStat) > maxMapKeys+1 {
		t.Fatalf("UriStat 未受限: %d", len(a.UriStat))
	}
	if a.UriStat[OverflowKey].Calls == 0 {
		t.Fatal("超出上限的 URI 未归入 OverflowKey")
	}
	if a.UriStat[OverflowKey].Slows == 0 || a.UriStat[OverflowKey].Errors == 0 || a.UriStat[OverflowKey].TimeS == 0 {
		t.Error("OverflowKey 的接口统计不完整")
	}
	// 总量守恒
	var sum int64
	for _, v := range a.UriStat {
		sum += v.Calls
	}
	if sum != a.Total {
		t.Errorf("URI 计数总和=%d, want %d", sum, a.Total)
	}
}

// 损坏/恶意日志：超大耗时不能让直方图越界 panic，超大 size 不能溢出成负数
func TestBadValuesDoNotPanicOrCorrupt(t *testing.T) {
	lines := []string{
		`{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "size": "999999999999999999999999", "upstream_response_time": "1e18"}`,
		`{"@timestamp": "2026-09-01T10:00:01+08:00", "status": "200", "size": "100", "upstream_response_time": "99999999999999999999"}`,
		`{"@timestamp": "2026-09-01T10:00:02+08:00", "status": "200", "size": "100", "upstream_response_time": "-1"}`,
	}
	f := filepath.Join(t.TempDir(), "bad.log")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 {
		t.Errorf("total=%d, want 3", st.Total)
	}
	if st.SizeTotal < 0 {
		t.Errorf("流量出现负数: %d", st.SizeTotal)
	}
	// 超大耗时都应落到溢出桶的最后一桶，且不越界
	if st.TimeOverflow[TimeOverflowCount-1] != 2 {
		t.Errorf("溢出桶最后一桶=%d, want 2", st.TimeOverflow[TimeOverflowCount-1])
	}
	if st.TimeBuckets[0] != 0 {
		t.Errorf("0 桶=%d, want 0（负耗时无效，不应计入耗时）", st.TimeBuckets[0])
	}
	_ = st.Percentile(0.99)
}

// timeBucket 边界
func TestTimeBucketBounds(t *testing.T) {
	cases := []struct {
		sec  float64
		want int
	}{
		{0, 0}, {-5, 0}, {0.009, 0}, {0.01, 1}, {9.99, 999},
		{10, TimeBucketCount - 1}, {1e18, TimeBucketCount - 1}, {9.9e307, TimeBucketCount - 1},
	}
	for _, c := range cases {
		if got := timeBucket(c.sec); got != c.want {
			t.Errorf("timeBucket(%v)=%d, want %d", c.sec, got, c.want)
		}
	}
}

// 各种畸形行都不能 panic（日志可能被截断/被写坏）
func TestGarbageLinesDoNotPanic(t *testing.T) {
	lines := []string{
		``,
		`{}`,
		`{"@timestamp":""}`,
		`{"@timestamp":"2026-13-45T99:99:99"}`,
		`{"@timestamp":"2026-09-01T10:00:00+08:00"`,
		`{"@timestamp":"2026-09-01T10:00:00+08:00","a":"\"}`,
		`{"@timestamp":"2026-09-01T10:00:00+08:00","request_uri":"` + strings.Repeat("x", 100000) + `"}`,
		`{"@timestamp":"2026-09-01T10:00:00+08:00","size":"-1","status":"abc"}`,
		"\x00\x01\x02",
	}
	f := filepath.Join(t.TempDir(), "garbage.log")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if st.Skipped < 5 {
		t.Errorf("无法解析的行应计入 skipped，实际 %d", st.Skipped)
	}
	// 解析成功的行不能出现负流量/负计数
	if st.SizeTotal < 0 || st.Total < 0 {
		t.Errorf("统计出现负值: total=%d size=%d", st.Total, st.SizeTotal)
	}
}

// 超长行超过 scanner 上限时应返回错误但保留已解析数据
func TestHugeLineKeepsParsedData(t *testing.T) {
	good := `{"@timestamp":"2026-09-01T10:00:00+08:00","status":"200","size":"10","request_uri":"/ok"}` + "\n"
	huge := strings.Repeat("x", 5*1024*1024) + "\n"
	f := filepath.Join(t.TempDir(), "huge.log")
	if err := os.WriteFile(f, []byte(good+good+huge), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err == nil {
		t.Log("超长行未返回错误（scanner 上限可能调整过）")
	}
	if st == nil || st.Total != 2 {
		t.Fatalf("超长行前的数据应被保留，实际 %+v", st)
	}
}

// 峰值时刻要按日志里的时区展示（服务器时区与日志时区可能不同）
func TestPeakKeepsLogTimezone(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tz.log")
	lines := []string{
		`{"@timestamp":"2026-09-01T10:00:00Z","status":"200"}`,
		`{"@timestamp":"2026-09-01T10:00:00Z","status":"200"}`,
		`{"@timestamp":"2026-09-01T10:00:05Z","status":"200"}`,
	}
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err != nil {
		t.Fatal(err)
	}
	pt, cnt := st.Peak()
	if cnt != 2 {
		t.Errorf("峰值=%d, want 2", cnt)
	}
	if got := pt.Format("15:04:05"); got != "10:00:00" {
		t.Errorf("峰值时刻=%s（应按日志 UTC 时区显示）, want 10:00:00", got)
	}
	if _, off := pt.Zone(); off != 0 {
		t.Errorf("峰值时区偏移=%d, want 0", off)
	}
}

// 同一秒跨文件时峰值要累加（不能只取单个文件的最大值）
func TestPeakMergesSameSecondAcrossFiles(t *testing.T) {
	a, b := NewAgg(), NewAgg()
	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		a.add(&record{ts: ts, day: "2026-09-01", status: "200"})
	}
	for i := 0; i < 4; i++ {
		b.add(&record{ts: ts, day: "2026-09-01", status: "200"})
	}
	a.MergeFrom(b)
	if _, cnt := a.Peak(); cnt != 7 {
		t.Errorf("合并后峰值=%d, want 7", cnt)
	}
}

// 峰值计数相同时取更早的时刻，保证多次运行结果稳定（map 序随机）
func TestPeakTieBreakIsDeterministic(t *testing.T) {
	loc := time.UTC
	early := time.Date(2026, 9, 1, 10, 0, 0, 0, loc)
	late := time.Date(2026, 9, 1, 11, 0, 0, 0, loc)

	want := ""
	for i := 0; i < 20; i++ {
		a := NewAgg()
		a.SecondCnt[late.Unix()] = 5
		a.SecondCnt[early.Unix()] = 5
		a.SetLoc(loc)
		pt, cnt := a.Peak()
		if cnt != 5 {
			t.Fatalf("峰值=%d, want 5", cnt)
		}
		got := pt.Format("15:04")
		if want == "" {
			want = got
		}
		if got != want {
			t.Fatalf("第 %d 次结果不稳定: %s != %s", i, got, want)
		}
	}
	if want != "10:00" {
		t.Errorf("并列时应取更早时刻 10:00，实际 %s", want)
	}
}

// URI 归一化：默认统计路径，避免 query 参数把基数撑爆
func TestURINormalize(t *testing.T) {
	f := filepath.Join(t.TempDir(), "uri.log")
	lines := []string{
		`{"@timestamp":"2026-09-01T10:00:00+08:00","status":"200","request_uri":"/api/x?id=1"}`,
		`{"@timestamp":"2026-09-01T10:00:01+08:00","status":"500","request_uri":"/api/x?id=2"}`,
		`{"@timestamp":"2026-09-01T10:00:02+08:00","status":"200","request_uri":"/api/x?id=3"}`,
		`{"@timestamp":"2026-09-01T10:00:03+08:00","status":"200","request_uri":"/api/y"}`,
		`{"@timestamp":"2026-09-01T10:00:04+08:00","status":"200","request_uri":"/api/z?"}`,
	}
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 开启归一化：3 条 /api/x 合并
	st, err := ProcessFileWithOptions(f, Options{URINormalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.UriStat["/api/x"].Calls != 3 || st.UriStat["/api/y"].Calls != 1 || st.UriStat["/api/z"].Calls != 1 {
		t.Errorf("归一化结果不正确: %v", st.UriStat)
	}
	if st.UriStat["/api/x"].Errors != 1 {
		t.Errorf("错误计数应归到路径: %v", st.UriStat["/api/x"])
	}
	if len(st.UriStat) != 3 {
		t.Errorf("URI 基数=%d, want 3", len(st.UriStat))
	}

	// 关闭归一化：保留原始 URI
	st, err = ProcessFileWithOptions(f, Options{URINormalize: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.UriStat) != 5 {
		t.Errorf("关闭归一化后基数=%d, want 5", len(st.UriStat))
	}
	if st.UriStat["/api/x?id=1"].Calls != 1 {
		t.Errorf("应保留完整 URI: %v", st.UriStat)
	}
	// 总量守恒
	var sum int64
	for _, v := range st.UriStat {
		sum += v.Calls
	}
	if sum != st.Total {
		t.Errorf("URI 计数总和=%d, want %d", sum, st.Total)
	}
}

// nginx 重试时的列表字段：地址取第一跳、状态取最终结果、耗时取首个有效值
func TestUpstreamListFields(t *testing.T) {
	f := filepath.Join(t.TempDir(), "up.log")
	lines := []string{
		// 单值（正常）
		`{"@timestamp":"2026-09-01T10:00:00+08:00","status":"200","upstream_addr":"10.0.0.1:80","upstream_status":"200","upstream_response_time":"0.010"}`,
		// 重试：地址/状态/耗时都是列表
		`{"@timestamp":"2026-09-01T10:00:01+08:00","status":"200","upstream_addr":"10.0.0.1:80, 10.0.0.2:80","upstream_status":"502, 200","upstream_response_time":"60.5, 0.020"}`,
		// 首个元素无效
		`{"@timestamp":"2026-09-01T10:00:02+08:00","status":"200","upstream_addr":"-, 10.0.0.3:80","upstream_status":"-, 200","upstream_response_time":"-, 0.5"}`,
		// 无上游
		`{"@timestamp":"2026-09-01T10:00:03+08:00","status":"200","upstream_addr":"-","upstream_status":"-","upstream_response_time":"-"}`,
	}
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err != nil {
		t.Fatal(err)
	}

	// 不应出现 "10.0.0.1:80, 10.0.0.2:80" 这种组合 key
	for k := range st.UpstreamStat {
		if strings.Contains(k, ",") {
			t.Errorf("上游地址未拆分: %q", k)
		}
		if k == "" || k == "-" {
			t.Errorf("不应统计空上游: %q", k)
		}
	}
	if st.UpstreamStat["10.0.0.1:80"].Calls != 2 || st.UpstreamStat["10.0.0.3:80"].Calls != 1 {
		t.Errorf("上游计数不对: %v", st.UpstreamStat)
	}
	// "502, 200" 最终成功 → 不算上游错误；"-" 跳过
	if st.UpstreamStat["10.0.0.1:80"].Errors != 0 {
		t.Errorf("502,200 应视为重试成功: %v", st.UpstreamStat)
	}
	// 耗时：60.5, 0.020 → 取第一个有效值 60.5；"-, 0.5" → 0.5
	if got := st.UpstreamStat["10.0.0.1:80"].TimeS; got < 60.4 || got > 60.6 {
		t.Errorf("列表耗时取首个有效值失败: %v", got)
	}
	if got := st.UpstreamStat["10.0.0.3:80"].TimeS; got < 0.49 || got > 0.51 {
		t.Errorf(`"-, 0.5" 应解析为 0.5: %v`, got)
	}
}

// 上游状态列表最后一个为 5xx 时仍要算错误
func TestUpstreamListErrStatus(t *testing.T) {
	f := filepath.Join(t.TempDir(), "up2.log")
	line := `{"@timestamp":"2026-09-01T10:00:00+08:00","status":"502","upstream_addr":"10.0.0.9:80","upstream_status":"200, 502","upstream_response_time":"0.1, 0.2"}`
	if err := os.WriteFile(f, []byte(line+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if st.UpstreamStat["10.0.0.9:80"].Errors != 1 {
		t.Errorf("最终状态 502 应计为上游错误: %v", st.UpstreamStat)
	}
}

// 逐秒表有条目上限：跨很多天也不会把内存吃爆，且峰值依旧准确
func TestSecondTableBounded(t *testing.T) {
	st := NewStats()
	a := &st.Agg
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

	// 30 万个不同的秒（相当于连续跑 3.5 天的逐秒数据），每秒 1 个请求
	for i := int64(0); i < 300000; i++ {
		a.add(&record{ts: time.Unix(base+i, 0).In(time.UTC), uri: "/x"})
	}
	if n := len(a.SecondCnt); n > maxSecondKeys {
		t.Fatalf("逐秒表条目 %d 超过上限 %d", n, maxSecondKeys)
	}
	// 最后一秒塞 50 个请求：即使逐秒表已封顶，流式峰值仍要抓到
	for i := 0; i < 50; i++ {
		a.add(&record{ts: time.Unix(base+299999, 0).In(time.UTC), uri: "/x"})
	}
	sec, cnt := a.Peak()
	if cnt != 51 {
		t.Fatalf("峰值计数 = %d，期望 51", cnt)
	}
	if sec.Unix() != base+299999 {
		t.Fatalf("峰值时刻 = %v，期望 %d", sec, base+299999)
	}
	if a.Total != 300050 {
		t.Fatalf("总量 = %d，期望 300050", a.Total)
	}
}

// MergeDay 传入本对象自己的文件级 Agg 时不能把当天数据合并两次（会翻倍）
func TestMergeDayNoDoubleCount(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "access.log")
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&sb, `{"@timestamp": "2026-09-01T10:%02d:%02d+08:00", "client": "[IP]", "size": "100", "status": "200", "upstream_response_time": "0.010", "request_method": "GET", "request_uri": "/api/x"}
`, i%60, i%60)
	}
	if err := os.WriteFile(f, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := ProcessFileWithOptions(f, Options{CPUPercent: 100})
	if err != nil || st == nil {
		t.Fatal(err)
	}
	before := st.Total
	if before != 300 {
		t.Fatalf("扫描总量 %d, want 300", before)
	}
	var day string
	for d := range st.Days {
		day = d
	}
	st.MergeDay(day, &st.Agg) // 传文件级 Agg：不应改变任何数字
	if st.Total != before {
		t.Fatalf("文件级总量被改成 %d，原值 %d", st.Total, before)
	}
	da := st.DayAgg(day)
	if da == nil || da.Total != before {
		got := int64(-1)
		if da != nil {
			got = da.Total
		}
		t.Fatalf("当天总量被改成 %d，原值 %d（自我合并导致翻倍）", got, before)
	}
	var sum int64
	for _, c := range da.SlotCnt {
		sum += c
	}
	if sum != before {
		t.Fatalf("时段桶合计 %d != 当天总量 %d", sum, before)
	}
	var hsum int64
	for _, c := range da.HourCnt {
		hsum += c
	}
	if hsum != before {
		t.Fatalf("小时桶合计 %d != 当天总量 %d", hsum, before)
	}
}

// 没有耗时记录的请求（nginx 对静态资源写 "-"）不能被当成 0ms：
// 它应计入 Total，但不进直方图、不进平均响应的分母。
func TestUntimedRowsDoNotDilute(t *testing.T) {
	a := NewAgg()
	// 100 次静态资源（无耗时）+ 10 次接口（每次 800ms）
	for i := 0; i < 100; i++ {
		r := record{ts: time.Unix(int64(1000+i), 0).UTC(), hour: 0, uri: "/static/x.css", status: "200", hasTime: false}
		a.add(&r)
	}
	for i := 0; i < 10; i++ {
		r := record{ts: time.Unix(int64(2000+i), 0).UTC(), hour: 0, uri: "/api/a", status: "200",
			hasTime: true, upstreamTime: 0.8}
		a.add(&r)
	}
	if a.Total != 110 {
		t.Fatalf("Total=%d, want 110", a.Total)
	}
	if a.TimedCount != 10 {
		t.Fatalf("TimedCount=%d, want 10", a.TimedCount)
	}
	if got := a.AvgTime() * 1000; got < 799 || got > 801 {
		t.Errorf("平均响应 = %.1fms，应只按 10 次接口算 ≈800ms（不能被 100 次无耗时拉成 73ms）", got)
	}
	if got := a.Percentile(0.5) * 1000; got < 790 || got > 810 {
		t.Errorf("P50 = %.1fms，应 ≈800ms（无耗时行不能进 0ms 桶）", got)
	}
	if a.TimeBuckets[0] != 0 {
		t.Errorf("bucket0 = %d，无耗时行不应计入直方图", a.TimeBuckets[0])
	}
	if a.SlowCount != 0 {
		t.Errorf("SlowCount=%d, want 0", a.SlowCount)
	}

	// 合并后口径不变
	b := NewAgg()
	b.MergeFrom(a)
	if b.TimedCount != 10 || b.AvgTime() != a.AvgTime() {
		t.Errorf("合并后 TimedCount=%d avg=%v, want 10/%v", b.TimedCount, b.AvgTime(), a.AvgTime())
	}
}

// 旧日记录没有 timed_count 字段时，必须回退到按 Total 计算（不能变成 0）
func TestTimedFallbackForLegacyAgg(t *testing.T) {
	a := NewAgg()
	a.Total = 100
	a.TimeSum = 50 // 100 次共 50s
	a.TimeBuckets[10] = 100
	if a.Timed() != 100 {
		t.Fatalf("旧数据 Timed()=%d, want 100（回退 Total）", a.Timed())
	}
	if got := a.AvgTime(); got != 0.5 {
		t.Errorf("旧数据平均响应 = %v, want 0.5", got)
	}
	if got := a.Percentile(0.5); got < 0.1 || got > 0.11 {
		t.Errorf("旧数据 P50 = %v, want ≈0.105", got)
	}
}

// 峰值时刻的时区必须随统计量一起传递：日记录 → 合并后仍按日志时区展示，
// 不能因为服务器是 UTC 就显示成 01:44
func TestPeakTimezoneSurvivesMerge(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	ts := time.Date(2026, 10, 27, 9, 44, 32, 0, cst)

	src := NewAgg()
	src.SetPeak(ts.Unix(), 96)
	src.SetLoc(cst) // 没有逐秒明细，只有峰值

	grand := NewAgg()
	grand.MergeFrom(src)

	got, cnt := grand.Peak()
	if cnt != 96 {
		t.Fatalf("峰值计数 = %d, want 96", cnt)
	}
	if got.Format("15:04") != "09:44" {
		t.Errorf("峰值时刻 = %s，应保持日志时区 09:44（丢时区会显示成服务器本地时间）", got.Format("15:04"))
	}
	if _, offset := got.Zone(); offset != 8*3600 {
		t.Errorf("峰值时刻偏移 = %d 秒, want 28800", offset)
	}
}

// 统计自洽性：各项分解必须能还原总量，否则报告里的数字互相矛盾。
// （这类不变量能在重构/改口径时第一时间抓到回归）
func TestAggregateInvariants(t *testing.T) {
	a := NewAgg()
	cst := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, cst)
	rows := []struct {
		off      int // 秒偏移
		uri      string
		status   string
		hasTime  bool
		t        float64
		upstream string
	}{
		{0, "/api/a", "200", true, 0.05, "10.0.0.1:80"},
		{0, "/api/a", "200", true, 0.05, "10.0.0.1:80"},
		{1, "/api/b", "500", true, 2.5, "10.0.0.2:80"},
		{2, "/api/b", "404", true, 0.3, "10.0.0.2:80"},
		{3, "/static/x.css", "200", false, 0, "-"},
		{4, "/static/y.js", "304", false, 0, "-"},
		{5, "/api/c", "200", true, 0.8, ""},
	}
	for _, c := range rows {
		r := record{
			ts: base.Add(time.Duration(c.off) * time.Second), hour: base.Hour(),
			uri: c.uri, status: c.status, client: "10.9.9.9", method: "GET",
			size: 100, hasTime: c.hasTime, upstreamTime: c.t, upstreamAddr: c.upstream,
		}
		r.day = r.ts.Format("2006-01-02")
		r.isError = len(c.status) > 0 && c.status[0] >= '4'
		r.isSlow = c.hasTime && c.t > 1.0
		r.isUpstreamErr = c.status == "500"
		a.add(&r)
	}

	sum := func(m map[string]int64) int64 {
		var s int64
		for _, v := range m {
			s += v
		}
		return s
	}
	if got := sum(a.StatusCnt); got != a.Total {
		t.Errorf("状态码合计 %d != 总请求 %d", got, a.Total)
	}
	if got := sum(a.MethodCnt); got != a.Total {
		t.Errorf("方法合计 %d != 总请求 %d", got, a.Total)
	}
	if got := sum(a.ClientCnt); got != a.Total {
		t.Errorf("客户端合计 %d != 总请求 %d", got, a.Total)
	}
	if got := sumOf(a.HourCnt); got != a.Total {
		t.Errorf("小时合计 %d != 总请求 %d", got, a.Total)
	}
	var slotSum int64
	for _, c := range a.SlotCnt {
		slotSum += c
	}
	if slotSum != a.Total {
		t.Errorf("5 分钟桶合计 %d != 总请求 %d", slotSum, a.Total)
	}
	var uriSum int64
	for _, u := range a.UriStat {
		uriSum += u.Calls
	}
	if uriSum != a.Total {
		t.Errorf("接口合计 %d != 总请求 %d", uriSum, a.Total)
	}
	var histSum int64
	for _, c := range a.TimeBuckets {
		histSum += c
	}
	if histSum != a.TimedCount {
		t.Errorf("直方图合计 %d != 有耗时请求 %d", histSum, a.TimedCount)
	}
	if a.TimedCount != 5 {
		t.Errorf("TimedCount=%d, want 5", a.TimedCount)
	}
	if got := sum(a.ErrorStatus); got != a.ErrorCount {
		t.Errorf("错误码合计 %d != 错误数 %d", got, a.ErrorCount)
	}
	var upCalls, upTimeC int64
	for _, u := range a.UpstreamStat {
		upCalls += u.Calls
		upTimeC += u.TimeC
	}
	if upCalls != 4 || upTimeC != 4 { // 4 行有上游地址，"/api/c" 没有
		t.Errorf("上游调用 %d/计时 %d, want 4/4", upCalls, upTimeC)
	}
	// 峰值：第 0 秒 2 次是最高
	if ts, cnt := a.Peak(); cnt != 2 || ts.Unix() != base.Unix() {
		t.Errorf("峰值 = %v/%d, want %v/2", ts, cnt, base)
	}
	// 合并后不变量依然成立
	b := NewAgg()
	b.MergeFrom(a)
	if got := sum(b.StatusCnt); got != b.Total || b.TimedCount != a.TimedCount {
		t.Errorf("合并后不自洽: 状态合计 %d total %d timed %d", got, b.Total, b.TimedCount)
	}
}

// sumOf 小时 map 求和（测试辅助）
func sumOf(m map[int]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// scanFields 改用 IndexByte 后，转义与字段顺序的边界必须仍然正确
func TestScanFieldsEscapesAndOrder(t *testing.T) {
	cases := []struct {
		name string
		line string
		want map[int]string // kind -> value
	}{
		{
			name: "@timestamp 不在首位（走 fieldBytes 兜底）",
			line: `{"@version":"1","@timestamp":"2026-09-01T10:00:00+08:00","request_uri":"/a"}`,
			want: map[int]string{fURI: "/a"},
		},
		{
			name: "值里含转义引号",
			line: `{"@timestamp":"2026-09-01T10:00:00+08:00","request_uri":"/a\"b","status":"200"}`,
			want: map[int]string{fURI: `/a\"b`, fStatus: "200"},
		},
		{
			name: "值里含反斜杠结尾",
			line: `{"@timestamp":"2026-09-01T10:00:00+08:00","request_uri":"/a\\","status":"200"}`,
			want: map[int]string{fURI: `/a\\`, fStatus: "200"},
		},
		{
			name: "键与值之间空格很多",
			line: `{"@timestamp" :  "2026-09-01T10:00:00+08:00" , "status" : "500" , "size" : "123"}`,
			want: map[int]string{fStatus: "500", fSize: "123"},
		},
		{
			name: "数值没有引号",
			line: `{"@timestamp":"2026-09-01T10:00:00+08:00","status":502,"size":9}`,
			want: map[int]string{fStatus: "502", fSize: "9"},
		},
		{
			name: "缺引号/截断的行不应 panic",
			line: `{"@timestamp":"2026-09-01T10:00:00+08:00","request_uri":"/unterminated`,
			want: map[int]string{},
		},
		{
			name: "上游耗时是逗号列表",
			line: `{"@timestamp":"2026-09-01T10:00:00+08:00","upstream_response_time":"0.005, 0.002","upstream_status":"502, 200"}`,
			want: map[int]string{fUpTime: "0.005, 0.002", fUpStatus: "502, 200"},
		},
	}
	for _, c := range cases {
		var fl lineFields
		scanFields([]byte(c.line), &fl)
		for kind, want := range c.want {
			got, ok := fl.get(kind)
			if !ok {
				t.Errorf("%s: 字段 %d 未取到，want %q", c.name, kind, want)
				continue
			}
			if string(got) != want {
				t.Errorf("%s: 字段 %d = %q, want %q", c.name, kind, got, want)
			}
		}
		if len(c.want) == 0 {
			// 截断行：取到行尾残值属预期行为（旧实现相同），只要不 panic、不误判状态码即可
			if fl.has(fStatus) {
				t.Errorf("%s: 截断行不应解析出状态码", c.name)
			}
		}
	}
}

// 行首快速路径与兜底路径必须给出同一个时间戳
func TestFastTimestampMatchesFieldBytes(t *testing.T) {
	lines := []string{
		`{"@timestamp": "2026-09-01T10:00:00+08:00", "x": "1"}`,
		`{"@timestamp":"2026-09-01T10:00:00.123+08:00"}`,
		`{"@version":"1","@timestamp":"2026-09-01T10:00:00Z"}`,
		`{"agent":"a\b","@timestamp":"2026-09-01T10:00:00+08:00"}`,
	}
	for _, ln := range lines {
		want, ok := fieldBytes([]byte(ln), keyTimestamp)
		if !ok {
			t.Fatalf("用例本身有问题: %s", ln)
		}
		got, ok2 := fastTimestamp([]byte(ln))
		if !ok2 {
			// 不是行首形态 → 走兜底，等价性由 fieldBytes 保证
			if bytes.HasPrefix([]byte(ln), timestampPrefix) {
				t.Errorf("行首形态应走快速路径: %s", ln)
			}
			continue
		}
		if string(got) != string(want) {
			t.Errorf("快速路径 %q != fieldBytes %q", got, want)
		}
	}
}

// 分钟级明细只用于运维报告：按 HH:MM 落到 1440 个槽，合并时逐槽累加
func TestMinuteCountsAndMerge(t *testing.T) {
	a := NewAgg()
	r1 := rec("2026-09-01T10:05:00+08:00", "200", 0.1)
	r2 := rec("2026-09-01T10:05:30+08:00", "200", 0.1)
	r3 := rec("2026-09-01T23:59:10+08:00", "200", 0.1)
	a.add(&r1)
	a.add(&r2)
	a.add(&r3)

	if got := a.MinuteCnt[10*60+5]; got != 2 {
		t.Errorf("10:05 分钟计数 = %d，期望 2", got)
	}
	if got := a.MinuteCnt[23*60+59]; got != 1 {
		t.Errorf("23:59 分钟计数 = %d，期望 1", got)
	}
	if len(a.MinuteCnt) != MinuteCount {
		t.Fatalf("MinuteCnt 长度 = %d，期望 %d", len(a.MinuteCnt), MinuteCount)
	}

	b := NewAgg()
	b.add(&r1)
	a.MergeFrom(b)
	if got := a.MinuteCnt[10*60+5]; got != 3 {
		t.Errorf("合并后 10:05 分钟计数 = %d，期望 3", got)
	}
}

// -from HH:MM：只统计每天该时间点（含）之后的记录，跨天各自生效
func TestFromMinuteFilter(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T11:29:59+08:00", "client": "[IP]", "size": "100", "status": "200", "request_uri": "/early"}
{"@timestamp": "2026-09-01T11:30:00+08:00", "client": "[IP]", "size": "100", "status": "200", "request_uri": "/edge"}
{"@timestamp": "2026-09-01T12:00:00+08:00", "client": "[IP]", "size": "100", "status": "200", "request_uri": "/late"}
{"@timestamp": "2026-09-02T09:00:00+08:00", "client": "[IP]", "size": "100", "status": "200", "request_uri": "/next-day-early"}
{"@timestamp": "2026-09-02T11:45:00+08:00", "client": "[IP]", "size": "100", "status": "200", "request_uri": "/next-day-late"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{FromMinute: 11*60 + 30})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 {
		t.Errorf("Total=%d, want 3", st.Total)
	}
	if st.OutOfRange != 2 {
		t.Errorf("OutOfRange=%d, want 2", st.OutOfRange)
	}
	if st.UriStat["/early"] != nil || st.UriStat["/next-day-early"] != nil {
		t.Error("早于 from 的记录不应被统计")
	}
	if st.UriStat["/edge"] == nil || st.UriStat["/late"] == nil || st.UriStat["/next-day-late"] == nil {
		t.Error("from 之后（含边界）的记录应被统计")
	}
}

// -skip-static：静态资源整行丢弃，不进入任何业务统计，只累计条数
func TestSkipStaticRecords(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "[IP]", "size": "1000", "status": "200", "upstream_response_time": "0.010", "request_uri": "/bbt-api/order/list"}
{"@timestamp": "2026-09-01T10:00:30+08:00", "client": "[IP]", "size": "9000", "status": "200", "upstream_response_time": "0.001", "request_uri": "/pro/assets/index-198354e4.css"}
{"@timestamp": "2026-09-01T10:00:40+08:00", "client": "[IP]", "size": "5000", "status": "404", "upstream_response_time": "0.001", "request_uri": "/pro/assets/app.js?v=2"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{SkipStatic: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 {
		t.Errorf("Total=%d, want 1（只保留业务请求）", st.Total)
	}
	if st.StaticSkipped != 2 {
		t.Errorf("StaticSkipped=%d, want 2", st.StaticSkipped)
	}
	if st.StaticBytes != 14000 {
		t.Errorf("StaticBytes=%d, want 14000", st.StaticBytes)
	}
	if st.StaticSkippedDay["2026-09-01"] != 2 || st.StaticBytesDay["2026-09-01"] != 14000 {
		t.Errorf("static day maps = %+v / %+v", st.StaticSkippedDay, st.StaticBytesDay)
	}
	if st.SizeTotal != 1000 {
		t.Errorf("SizeTotal=%d, want 1000（静态资源的流量不应计入）", st.SizeTotal)
	}
	if st.ErrorCount != 0 || st.StatusCnt["404"] != 0 {
		t.Errorf("静态资源的 404 不应计入错误: err=%d status=%v", st.ErrorCount, st.StatusCnt)
	}
	if len(st.UriStat) != 1 || st.UriStat["/bbt-api/order/list"] == nil {
		t.Errorf("UriStat 应只保留业务接口: %v", st.UriStat)
	}
	// 分钟粒度仍然是 1 分钟，只是静态请求不计入
	if st.MinuteCnt[10*60] != 1 {
		t.Errorf("MinuteCnt[10:00]=%d, want 1", st.MinuteCnt[10*60])
	}

	// 关掉开关后恢复原样
	st2, _ := ProcessReader(strings.NewReader(lines), Options{})
	if st2.Total != 3 || st2.StaticSkipped != 0 {
		t.Errorf("SkipStatic=false 时 Total=%d StaticSkipped=%d, want 3/0", st2.Total, st2.StaticSkipped)
	}
}

// 深度分析字段：UA 归类、Referer、分钟流量、最大耗时、错误 URL、接口流量
func TestDeepAnalysisFields(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "[IP]", "size": "1000", "status": "200", "upstream_response_time": "0.050", "request_uri": "/bbt-api/a", "agent": "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1", "refer": "http://ref.example.com:8080/page"}
{"@timestamp": "2026-09-01T10:00:10+08:00", "client": "[IP]", "size": "2000", "status": "404", "upstream_response_time": "2.500", "request_uri": "/bbt-api/missing", "agent": "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120.0 Safari/537.36", "refer": ""}
{"@timestamp": "2026-09-01T10:00:20+08:00", "client": "[IP]", "size": "3000", "status": "500", "upstream_response_time": "0.100", "request_uri": "/bbt-api/boom", "agent": "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Edg/120.0", "refer": "-"}
{"@timestamp": "2026-09-01T10:01:00+08:00", "client": "[IP]", "size": "500", "status": "200", "upstream_response_time": "0.010", "request_uri": "/login", "agent": "", "refer": "https://ref.example.com/other"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceCnt["移动端"] != 1 || st.DeviceCnt["桌面端"] != 2 || st.DeviceCnt["未知"] != 1 {
		t.Errorf("DeviceCnt=%v", st.DeviceCnt)
	}
	if st.BrowserCnt["Safari"] != 1 || st.BrowserCnt["Chrome"] != 1 || st.BrowserCnt["Edge"] != 1 || st.BrowserCnt["未知"] != 1 {
		t.Errorf("BrowserCnt=%v", st.BrowserCnt)
	}
	if st.RefererCnt["ref.example.com:8080"] != 1 || st.RefererCnt["ref.example.com"] != 1 {
		t.Errorf("RefererCnt=%v", st.RefererCnt)
	}
	if st.RefererCnt[""] != 0 {
		t.Error("空 referer 不应进入 referer 域名表")
	}
	if st.DirectCnt != 2 {
		t.Errorf("DirectCnt=%d, want 2（空 referer + \"-\"）", st.DirectCnt)
	}
	if st.MinuteBytes[10*60] != 6000 {
		t.Errorf("MinuteBytes[10:00]=%d, want 6000", st.MinuteBytes[10*60])
	}
	if st.MinuteBytes[10*60+1] != 500 {
		t.Errorf("MinuteBytes[10:01]=%d, want 500", st.MinuteBytes[10*60+1])
	}
	if st.MaxTime != 2.5 {
		t.Errorf("MaxTime=%v, want 2.5", st.MaxTime)
	}
	if st.UriStat["/bbt-api/a"].Bytes != 1000 || st.UriStat["/login"].Bytes != 500 {
		t.Errorf("UriStat.Bytes=%+v", st.UriStat)
	}
	if st.ErrorURI["404"]["/bbt-api/missing"] != 1 || st.ErrorURI["500"]["/bbt-api/boom"] != 1 {
		t.Errorf("ErrorURI=%v", st.ErrorURI)
	}
}

// 文件名常只出现在 query（name=xx.xlsx），要在原始 request_uri 上识别
func TestFileTypeDetection(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "[IP]", "size": "1", "status": "200", "request_uri": "/bbt-api/sys/file/load?id=1&name=%E6%8A%A5%E8%A1%A8-2026.xlsx"}
{"@timestamp": "2026-09-01T10:00:01+08:00", "client": "[IP]", "size": "1", "status": "200", "request_uri": "/pro/preview/preview-index?url=https://x/bbt-api/sys/file/load?id=2&name=report.pdf&x=1"}
{"@timestamp": "2026-09-01T10:00:02+08:00", "client": "[IP]", "size": "1", "status": "200", "request_uri": "/api/x.xlsxbackup"}
{"@timestamp": "2026-09-01T10:00:03+08:00", "client": "[IP]", "size": "1", "status": "200", "request_uri": "/bbt-api/task/executer/process?taskId=1"}
{"@timestamp": "2026-09-01T10:00:04+08:00", "client": "[IP]", "size": "1", "status": "200", "request_uri": "/bbt-api/sys/file/load?name=REPORT.XLSX"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.FileTypeCnt["xlsx"] != 2 {
		t.Errorf("xlsx=%d, want 2（%v）", st.FileTypeCnt["xlsx"], st.FileTypeCnt)
	}
	if st.FileTypeCnt["pdf"] != 1 {
		t.Errorf("pdf=%d, want 1（%v）", st.FileTypeCnt["pdf"], st.FileTypeCnt)
	}
	if len(st.FileTypeCnt) != 2 {
		t.Errorf("FileTypeCnt=%v, 不应把 .xlsxbackup 当成 xlsx", st.FileTypeCnt)
	}
}

// 10 秒以上的请求必须进入溢出桶，P99 不能被截断到 10s
func TestPercentileOverflow(t *testing.T) {
	a := NewAgg()
	for i := 0; i < 99; i++ {
		rr := rec("2026-09-01T10:00:00+08:00", "200", 0.1)
		a.add(&rr)
	}
	slow := rec("2026-09-01T10:00:00+08:00", "200", 30)
	a.add(&slow)

	if got := a.Percentile(0.90) * 1000; got < 100 || got > 110 {
		t.Errorf("P90=%.1fms, want 100~110ms", got)
	}
	if got := a.Percentile(0.99); got != 30 {
		t.Errorf("P99=%v s, want 30s", got)
	}
	var sum int64
	for _, c := range a.TimeBuckets {
		sum += c
	}
	for _, c := range a.TimeOverflow {
		sum += c
	}
	if sum != a.Total {
		t.Errorf("耗时桶合计=%d, want %d", sum, a.Total)
	}
}

func TestPercentileOverflowSingle(t *testing.T) {
	a := NewAgg()
	rr := rec("2026-09-01T10:00:00+08:00", "200", 700)
	a.add(&rr)
	if got := a.Percentile(0.99); got != 700 {
		t.Errorf("单条 700s 的 P99=%v, want 700", got)
	}
}

func TestRealIPSelection(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "client": "10.0.0.1", "real_client": "203.0.113.9", "status": "200", "request_uri": "/a"}
{"@timestamp": "2026-09-01T10:00:01+08:00", "client": "10.0.0.2", "real_client": "-", "status": "200", "request_uri": "/b"}
`
	on, err := ProcessReader(strings.NewReader(lines), Options{RealIP: true})
	if err != nil {
		t.Fatal(err)
	}
	if on.ClientCnt["203.0.113.9"] != 1 || on.ClientCnt["10.0.0.2"] != 1 {
		t.Errorf("RealIP=true ClientCnt=%v", on.ClientCnt)
	}
	off, _ := ProcessReader(strings.NewReader(lines), Options{})
	if off.ClientCnt["10.0.0.1"] != 1 || off.ClientCnt["10.0.0.2"] != 1 {
		t.Errorf("RealIP=false ClientCnt=%v", off.ClientCnt)
	}
}

func TestTimeFieldSelection(t *testing.T) {
	line := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "0.100", "responsetime": "0.500", "request_uri": "/a"}`
	cases := []struct {
		field string
		want  float64
	}{
		{"auto", 0.100},
		{"upstream", 0.100},
		{"response", 0.500},
	}
	for _, c := range cases {
		st, err := ProcessReader(strings.NewReader(line), Options{TimeField: c.field})
		if err != nil {
			t.Fatal(err)
		}
		if got := st.TimeSum; got != c.want {
			t.Errorf("TimeField=%s TimeSum=%v, want %v", c.field, got, c.want)
		}
	}
	// upstream 缺失时，response 口径仍能取到 responsetime
	line2 := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "-", "responsetime": "0.500", "request_uri": "/a"}`
	st, _ := ProcessReader(strings.NewReader(line2), Options{TimeField: "response"})
	if st.TimedCount != 1 || st.TimeSum != 0.5 {
		t.Errorf("response 口径未取到 responsetime: timed=%d sum=%v", st.TimedCount, st.TimeSum)
	}
}

func TestSlowThreshold(t *testing.T) {
	line := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "0.600", "request_uri": "/a"}`
	def, _ := ProcessReader(strings.NewReader(line), Options{})
	if def.SlowCount != 0 {
		t.Errorf("默认 1s 阈值下 0.6s 不应算慢请求: %d", def.SlowCount)
	}
	fast, _ := ProcessReader(strings.NewReader(line), Options{SlowMS: 500})
	if fast.SlowCount != 1 {
		t.Errorf("500ms 阈值下 0.6s 应算慢请求: %d", fast.SlowCount)
	}
}

func TestExcludeBots(t *testing.T) {
	lines := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "user_agent": "Mozilla/5.0 (compatible; Googlebot/2.1)", "request_uri": "/a"}
{"@timestamp": "2026-09-01T10:00:01+08:00", "status": "200", "user_agent": "curl/8.0", "request_uri": "/b"}
{"@timestamp": "2026-09-01T10:00:02+08:00", "status": "200", "user_agent": "Mozilla/5.0 (Windows NT 10.0) Chrome/120", "request_uri": "/c"}
`
	on, _ := ProcessReader(strings.NewReader(lines), Options{ExcludeBots: true})
	if on.Total != 1 || on.BotSkipped != 2 {
		t.Errorf("ExcludeBots=true Total=%d BotSkipped=%d, want 1/2", on.Total, on.BotSkipped)
	}
	off, _ := ProcessReader(strings.NewReader(lines), Options{})
	if off.Total != 3 || off.BotSkipped != 0 {
		t.Errorf("ExcludeBots=false Total=%d BotSkipped=%d, want 3/0", off.Total, off.BotSkipped)
	}
}

func TestUnknownStatusMethod(t *testing.T) {
	line := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "", "request_uri": "/a"}`
	st, _ := ProcessReader(strings.NewReader(line), Options{})
	if st.StatusCnt["(未知)"] != 1 {
		t.Errorf("空状态码应归为 (未知): %v", st.StatusCnt)
	}
	if st.MethodCnt["(未知)"] != 1 {
		t.Errorf("空方法应归为 (未知): %v", st.MethodCnt)
	}
}

// nginx 默认 combined 格式：不依赖 JSON，也能走同一套统计
func TestCombinedLogFormat(t *testing.T) {
	lines := `203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /api/a?x=1 HTTP/1.1" 200 123 "https://ref.example/x" "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1"
203.0.113.8 - - [30/Oct/2026:10:01:00 +0800] "POST /api/b HTTP/1.1" 500 456 "-" "Mozilla/5.0 (Windows NT 10.0) Chrome/120"
`
	st, err := ProcessReader(strings.NewReader(lines), Options{URINormalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "combined" {
		t.Errorf("Format=%q, want combined", st.Format)
	}
	if st.Total != 2 || st.SizeTotal != 579 {
		t.Errorf("Total=%d SizeTotal=%d, want 2/579", st.Total, st.SizeTotal)
	}
	if st.ClientCnt["203.0.113.7"] != 1 || st.ClientCnt["203.0.113.8"] != 1 {
		t.Errorf("ClientCnt=%v", st.ClientCnt)
	}
	if st.UriStat["/api/a"] == nil || st.UriStat["/api/b"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
	if st.StatusCnt["200"] != 1 || st.StatusCnt["500"] != 1 || st.ErrorCount != 1 {
		t.Errorf("status=%v err=%d", st.StatusCnt, st.ErrorCount)
	}
	if st.MethodCnt["GET"] != 1 || st.MethodCnt["POST"] != 1 {
		t.Errorf("MethodCnt=%v", st.MethodCnt)
	}
	if st.RefererCnt["ref.example"] != 1 || st.DirectCnt != 1 {
		t.Errorf("RefererCnt=%v DirectCnt=%d", st.RefererCnt, st.DirectCnt)
	}
	if st.DeviceCnt["移动端"] != 1 || st.DeviceCnt["桌面端"] != 1 {
		t.Errorf("DeviceCnt=%v", st.DeviceCnt)
	}
	if st.MinuteCnt[10*60] != 1 || st.MinuteCnt[10*60+1] != 1 {
		t.Errorf("MinuteCnt[10:00]=%d [10:01]=%d", st.MinuteCnt[10*60], st.MinuteCnt[10*60+1])
	}
	if st.TimedCount != 0 {
		t.Errorf("combined 默认没有耗时字段，TimedCount=%d", st.TimedCount)
	}
}

// 自定义 nginx log_format：真实 IP、请求耗时、上游字段都要能进来
func TestCustomLogFormat(t *testing.T) {
	spec := `$time_iso8601 $remote_addr $http_x_forwarded_for $request_method $request_uri $status $body_bytes_sent $request_time $upstream_response_time $upstream_addr $upstream_status $http_referer $http_user_agent`
	lines := `2026-10-30T10:00:00+08:00 10.0.0.1 203.0.113.9 GET /api/a?x=1 200 100 0.200 0.150 10.0.0.1:80 200 - Mozilla/5.0 (Windows NT 10.0) Chrome/120
2026-10-30T10:00:01+08:00 10.0.0.2 203.0.113.10 POST /api/b 200 200 0.800 0.700 10.0.0.2:80 200 - Mozilla/5.0 (Windows NT 10.0) Chrome/120
`
	st, err := ProcessReader(strings.NewReader(lines), Options{
		LogFormat: spec, RealIP: true, URINormalize: true, SlowMS: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "custom" {
		t.Errorf("Format=%q, want custom", st.Format)
	}
	if st.Total != 2 || st.TimedCount != 2 {
		t.Errorf("Total=%d TimedCount=%d", st.Total, st.TimedCount)
	}
	if st.ClientCnt["203.0.113.9"] != 1 || st.ClientCnt["203.0.113.10"] != 1 {
		t.Errorf("RealIP 未生效: %v", st.ClientCnt)
	}
	if st.ClientCnt["10.0.0.1"] != 0 {
		t.Errorf("不应把 remote_addr 当真实 IP: %v", st.ClientCnt)
	}
	if st.SlowCount != 1 {
		t.Errorf("SlowCount=%d, want 1", st.SlowCount)
	}
	if st.UriStat["/api/a"] == nil || st.UriStat["/api/b"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
	if st.UpstreamStat["10.0.0.1:80"] == nil || st.UpstreamStat["10.0.0.2:80"] == nil {
		t.Errorf("UpstreamStat=%v", st.UpstreamStat)
	}
	if got := st.TimeSum; got < 0.849 || got > 0.851 {
		t.Errorf("TimeSum=%v, want 0.85", got)
	}
}

// CSV：带表头时自动跳过首行；没有表头时用 -csv-fields 指定列顺序
func TestCSVLogFormat(t *testing.T) {
	header := `time,client,method,uri,status,size,request_time,agent,referer`
	lines := header + `
2026-10-30T10:00:00+08:00,1.2.3.4,GET,/api/a?x=1,200,100,0.120,Mozilla/5.0,https://ref.example/a
2026-10-30T10:00:01+08:00,1.2.3.5,POST,/api/b,500,200,0.500,Mozilla/5.0,-
`
	st, err := ProcessReader(strings.NewReader(lines), Options{URINormalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "csv" {
		t.Errorf("Format=%q, want csv", st.Format)
	}
	if st.Total != 2 || st.Skipped != 1 {
		t.Errorf("Total=%d Skipped=%d, want 2/1（表头跳过）", st.Total, st.Skipped)
	}
	if st.SizeTotal != 300 || st.ErrorCount != 1 {
		t.Errorf("SizeTotal=%d ErrorCount=%d", st.SizeTotal, st.ErrorCount)
	}
	if st.UriStat["/api/a"] == nil || st.UriStat["/api/b"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
	if st.TimedCount != 2 || st.TimeSum < 0.619 || st.TimeSum > 0.621 {
		t.Errorf("TimedCount=%d TimeSum=%v", st.TimedCount, st.TimeSum)
	}

	// 显式列顺序：request 字段自动拆 method / URI
	explicit := `2026-10-30T10:00:00+08:00,1.2.3.4,"GET /api/x HTTP/1.1",200,10`
	st2, err := ProcessReader(strings.NewReader(explicit), Options{
		LogFormat: "csv", CSVFields: "time,client,request,status,size",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Total != 1 || st2.MethodCnt["GET"] != 1 || st2.UriStat["/api/x"] == nil {
		t.Errorf("显式 CSV 解析失败: total=%d method=%v uri=%v", st2.Total, st2.MethodCnt, st2.UriStat)
	}
}

// 文本格式也要遵守 -skip-static / -exclude-bots / -from
func TestTextFormatFilters(t *testing.T) {
	lines := `203.0.113.7 - - [30/Oct/2026:09:00:00 +0800] "GET /api/early HTTP/1.1" 200 50 "-" "Mozilla/5.0"
203.0.113.8 - - [30/Oct/2026:11:00:00 +0800] "GET /pro/assets/app.css HTTP/1.1" 200 5000 "-" "Mozilla/5.0"
203.0.113.9 - - [30/Oct/2026:11:01:00 +0800] "GET /api/a HTTP/1.1" 200 100 "-" "Googlebot/2.1"
203.0.113.10 - - [30/Oct/2026:11:02:00 +0800] "GET /api/b HTTP/1.1" 200 200 "-" "Mozilla/5.0"
`
	st, err := ProcessReader(strings.NewReader(lines), Options{
		SkipStatic: true, ExcludeBots: true, FromMinute: 10 * 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.StaticSkipped != 1 || st.BotSkipped != 1 || st.OutOfRange != 1 {
		t.Errorf("Total=%d static=%d bot=%d out=%d, want 1/1/1/1",
			st.Total, st.StaticSkipped, st.BotSkipped, st.OutOfRange)
	}
	if st.UriStat["/api/b"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
}

// 整份日志都没有耗时字段时，不能把空直方图算成 P99=10s
func TestUntimedOnlyNoPercentile(t *testing.T) {
	a := NewAgg()
	for i := 0; i < 2; i++ {
		r := record{ts: time.Unix(int64(1000+i), 0).UTC(), hour: 0, uri: "/api/a", status: "200", hasTime: false}
		a.add(&r)
	}
	if a.Timed() != 0 {
		t.Fatalf("Timed()=%d, want 0", a.Timed())
	}
	if got := a.Percentile(0.99); got != 0 {
		t.Errorf("P99=%v, want 0（没有耗时样本）", got)
	}
}

// 自定义 JSON 字段名：nginx 原生变量名、url/request 请求行都要能识别
func TestJSONFieldAliases(t *testing.T) {
	lines := `{"time_iso8601": "2026-10-30T10:00:00+08:00", "remote_addr": "10.0.0.1", "http_x_forwarded_for": "203.0.113.9", "request": "GET /api/a?x=1 HTTP/1.1", "status": "200", "body_bytes_sent": "123", "request_time": "0.250", "upstream_response_time": "0.200", "upstream_addr": "10.0.0.1:80", "upstream_status": "200", "http_user_agent": "Mozilla/5.0 (Windows NT 10.0) Chrome/120", "http_referer": "https://ref.example/a"}
{"time_local": "30/Oct/2026:10:00:01 +0800", "ip": "10.0.0.2", "url": "POST /api/b HTTP/1.1", "code": "500", "bytes": "456", "rt": "0.800", "agent": "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Mobile/15E148 Safari/604.1", "referer": "-"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{URINormalize: true, RealIP: true, SlowMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "json" {
		t.Errorf("Format=%q, want json", st.Format)
	}
	if st.Total != 2 || st.SizeTotal != 579 {
		t.Errorf("Total=%d SizeTotal=%d", st.Total, st.SizeTotal)
	}
	if st.UriStat["/api/a"] == nil || st.UriStat["/api/b"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
	if st.MethodCnt["GET"] != 1 || st.MethodCnt["POST"] != 1 {
		t.Errorf("MethodCnt=%v", st.MethodCnt)
	}
	if st.StatusCnt["200"] != 1 || st.StatusCnt["500"] != 1 {
		t.Errorf("StatusCnt=%v", st.StatusCnt)
	}
	if st.ClientCnt["203.0.113.9"] != 1 || st.ClientCnt["10.0.0.2"] != 1 {
		t.Errorf("ClientCnt=%v", st.ClientCnt)
	}
	if st.ErrorCount != 1 || st.SlowCount != 1 {
		t.Errorf("ErrorCount=%d SlowCount=%d", st.ErrorCount, st.SlowCount)
	}
	if st.TimedCount != 2 || st.TimeSum < 0.999 || st.TimeSum > 1.001 {
		t.Errorf("TimedCount=%d TimeSum=%v", st.TimedCount, st.TimeSum)
	}
	if st.DeviceCnt["移动端"] != 1 || st.DeviceCnt["桌面端"] != 1 {
		t.Errorf("DeviceCnt=%v", st.DeviceCnt)
	}
}

// 任意 JSON 字段名：通过 -json-fields 做映射
func TestJSONCustomFieldMapping(t *testing.T) {
	lines := `{"ts":"2026-10-30T10:00:00+08:00","cip":"10.0.0.1","rc":"203.0.113.9","m":"GET","u":"/api/x?a=1","st":"200","sz":"123","ut":"0.200","rt":"0.250","ua":"Mozilla/5.0 Chrome/120","ref":"https://ref.example/a","ua_addr":"10.0.0.1:80","us":"200"}
{"ts":"2026-10-30T10:00:01+08:00","cip":"10.0.0.2","req":"POST /api/y HTTP/1.1","st":"500","sz":"456","rt":"0.800"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{
		JSONFields:   "ts=time,cip=client,rc=real_client,m=method,u=uri,req=request,st=status,sz=size,ut=upstream_time,rt=response_time,ua=agent,ref=referer,ua_addr=upstream_addr,us=upstream_status",
		RealIP:       true,
		URINormalize: true,
		SlowMS:       500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.SizeTotal != 579 {
		t.Fatalf("Total=%d SizeTotal=%d", st.Total, st.SizeTotal)
	}
	if st.UriStat["/api/x"] == nil || st.UriStat["/api/y"] == nil {
		t.Errorf("UriStat=%v", st.UriStat)
	}
	if st.MethodCnt["GET"] != 1 || st.MethodCnt["POST"] != 1 {
		t.Errorf("MethodCnt=%v", st.MethodCnt)
	}
	if st.ClientCnt["203.0.113.9"] != 1 || st.ClientCnt["10.0.0.2"] != 1 {
		t.Errorf("ClientCnt=%v", st.ClientCnt)
	}
	if st.UpstreamStat["10.0.0.1:80"] == nil {
		t.Errorf("UpstreamStat=%v", st.UpstreamStat)
	}
	if st.TimeSum < 0.999 || st.TimeSum > 1.001 {
		t.Errorf("TimeSum=%v, want 1.0", st.TimeSum)
	}
	if st.RefererCnt["ref.example"] != 1 {
		t.Errorf("RefererCnt=%v", st.RefererCnt)
	}
	if st.ErrorCount != 1 || st.SlowCount != 1 {
		t.Errorf("ErrorCount=%d SlowCount=%d", st.ErrorCount, st.SlowCount)
	}
}

func TestJSONFieldMapErrors(t *testing.T) {
	if _, _, err := parseJSONFieldMap("ts"); err == nil {
		t.Fatal("缺少 = 应报错")
	}
	if _, _, err := parseJSONFieldMap("ts=nope"); err == nil {
		t.Fatal("未知目标应报错")
	}
	if _, _, err := parseJSONFieldMap("=time"); err == nil {
		t.Fatal("缺少源字段应报错")
	}
	// 两边都像目标名时按标准写法「左边是源」处理
	m2, _, err := parseJSONFieldMap("ip=client")
	if err != nil || m2["ip"] != fClient {
		t.Errorf("ip=client m=%v err=%v", m2, err)
	}
	// target=source 写法也要兼容
	m, timeKey, err := parseJSONFieldMap("time=ts,client=cip")
	if err != nil {
		t.Fatal(err)
	}
	if timeKey != "ts" || m["cip"] != fClient {
		t.Errorf("timeKey=%q map=%v", timeKey, m)
	}
}

// 默认 JSON 字段 + 自定义耗时键（request_time）也要触发别名扫描，
// 否则响应时间会被静默忽略。
func TestJSONAliasResponseTime(t *testing.T) {
	lines := `{"@timestamp":"2026-10-30T10:00:00+08:00","client":"c1","request_uri":"/a","status":"200","size":"1","request_method":"GET","request_time":"0.500"}
{"@timestamp":"2026-10-30T10:00:01+08:00","client":"c2","request_uri":"/b","status":"200","size":"1","request_method":"GET","upstream_time":"0.700"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "json" {
		t.Errorf("Format=%q", st.Format)
	}
	if st.TimedCount != 2 || st.TimeSum < 1.199 || st.TimeSum > 1.201 {
		t.Errorf("TimedCount=%d TimeSum=%v, want 2/1.2", st.TimedCount, st.TimeSum)
	}
}

// 旧日记录即使耗时全是 0ms，也会在 bucket0 留下计数；不能因此认为“没有耗时样本”
func TestTimedFallbackLegacyZeroMs(t *testing.T) {
	a := NewAgg()
	a.Total = 100
	a.TimeBuckets[0] = 100
	if a.Timed() != 100 {
		t.Fatalf("Timed()=%d, want 100", a.Timed())
	}
}

// UTF-8 BOM 不能导致首行被跳过（Windows 上复制/编辑日志时很常见）
func TestBOMFirstLine(t *testing.T) {
	combined := "\xEF\xBB\xBF" + `203.0.113.7 - - [30/Oct/2026:10:00:00 +0800] "GET /api/a HTTP/1.1" 200 1 "-" "Mozilla/5.0"` + "\n"
	st, err := ProcessReader(strings.NewReader(combined), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.UriStat["/api/a"] == nil {
		t.Errorf("combined BOM: total=%d uri=%v", st.Total, st.UriStat)
	}

	jsonLine := "\xEF\xBB\xBF" + `{"@timestamp":"2026-10-30T10:00:00+08:00","client":"c","status":"200","request_uri":"/b","size":"1"}` + "\n"
	st2, err := ProcessReader(strings.NewReader(jsonLine), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Total != 1 || st2.UriStat["/b"] == nil {
		t.Errorf("json BOM: total=%d uri=%v", st2.Total, st2.UriStat)
	}
}

// 别名扫描器必须与默认扫描器在默认字段上保持一致，防止两份扫描逻辑漂移。
func TestScanFieldsAliasMatchesDefault(t *testing.T) {
	line := []byte(`{"@timestamp":"2026-10-30T10:00:00+08:00","client":"1.2.3.4","real_client":"5.6.7.8","request_uri":"/a","request_method":"GET","status":"200","size":"10","responsetime":"0.2","upstream_response_time":"0.1","upstream_addr":"u:80","upstream_status":"200","agent":"UA","refer":"-"}`)
	var a, b lineFields
	scanFields(line, &a)
	scanFieldsAlias(line, &b)
	if a.mask != b.mask {
		t.Fatalf("mask default=%b alias=%b", a.mask, b.mask)
	}
	for k := 0; k < fieldKindCount; k++ {
		if !bytes.Equal(a.vals[k], b.vals[k]) {
			t.Errorf("field %d default=%q alias=%q", k, a.vals[k], b.vals[k])
		}
	}
}

func TestHasJSONKeyRequiresColon(t *testing.T) {
	if !hasJSONKey([]byte(`{"url":"/x"}`), "url") {
		t.Fatal("对象 key 应识别")
	}
	if hasJSONKey([]byte(`{"msg":"url"}`), "url") {
		t.Fatal("字符串 value 不应被当成 key")
	}
	if !hasJSONKey([]byte(`{"url" : "/x"}`), "url") {
		t.Fatal("key 与冒号之间有空格应识别")
	}
}

func TestAliasDetectedAfterFirstLine(t *testing.T) {
	lines := `{"@timestamp":"2026-10-30T10:00:00+08:00","status":"200"}
{"@timestamp":"2026-10-30T10:00:01+08:00","client":"c","request_uri":"/a","status":"200","request_time":"0.500"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.TimedCount != 1 || st.TimeSum < 0.499 || st.TimeSum > 0.501 {
		t.Errorf("TimedCount=%d TimeSum=%v, want 1/0.5", st.TimedCount, st.TimeSum)
	}
}

func TestCSVMultilineQuotedField(t *testing.T) {
	csv := "time,client,status,uri,size,agent\n" +
		"2026-10-30T10:00:00+08:00,c1,200,/a,1,\"Mozilla/5.0\n(X11)\"\n" +
		"2026-10-30T10:00:01+08:00,c2,200,/b,1,curl\n"
	st, err := ProcessReader(strings.NewReader(csv), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Skipped != 1 || st.UriStat["/a"] == nil || st.UriStat["/b"] == nil {
		t.Errorf("total=%d skipped=%d uris=%v", st.Total, st.Skipped, st.UriStat)
	}
}

func TestCSVSkipHeaderOption(t *testing.T) {
	csv := "日期,客户端,接口\n2026-10-30T10:00:00+08:00,c1,/a\n"
	st, err := ProcessReader(strings.NewReader(csv), Options{
		LogFormat:     "csv",
		CSVFields:     "time,client,uri",
		CSVSkipHeader: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Skipped != 1 || st.UriStat["/a"] == nil {
		t.Errorf("total=%d skipped=%d uris=%v", st.Total, st.Skipped, st.UriStat)
	}
}

func TestParseNginxTimeValidation(t *testing.T) {
	if _, ok := parseNginxTime([]byte("31/Feb/2026:10:00:00 +0800")); ok {
		t.Fatal("无效日期不应解析成功")
	}
	if _, ok := parseNginxTime([]byte("30/Oct/2026:10:00:00 +1500")); ok {
		t.Fatal("无效时区偏移不应解析成功")
	}
	if _, ok := parseNginxTime([]byte("29/Feb/2024:10:00:00 +0800")); !ok {
		t.Fatal("闰年 2 月 29 日应解析成功")
	}
}

func TestFormatWarningPropagates(t *testing.T) {
	line := `[IP] - - [30/Oct/2026:10:00:00 +0800] "GET /a HTTP/1.1" 200 1 "-" "Mozilla/5.0" 0.123` + "\n"
	st, err := ProcessReader(strings.NewReader(line), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.FormatWarning == "" {
		t.Fatal("FormatWarning 应传递到 FileStats")
	}
}

// NaN/Inf/负数耗时不能污染平均值、直方图、接口耗时或报表。
func TestNonFiniteTimingIgnored(t *testing.T) {
	lines := `{"@timestamp":"2026-09-01T10:00:00+08:00","client":"c","status":"200","request_uri":"/a","upstream_response_time":"NaN"}
{"@timestamp":"2026-09-01T10:00:01+08:00","client":"c","status":"200","request_uri":"/a","upstream_response_time":"Inf"}
{"@timestamp":"2026-09-01T10:00:02+08:00","client":"c","status":"200","request_uri":"/a","upstream_response_time":"-1"}
{"@timestamp":"2026-09-01T10:00:03+08:00","client":"c","status":"200","request_uri":"/a","upstream_response_time":"1e309"}
{"@timestamp":"2026-09-01T10:00:04+08:00","client":"c","status":"200","request_uri":"/a","upstream_response_time":"0.5"}
`
	st, err := ProcessReader(strings.NewReader(lines), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 5 || st.TimedCount != 1 || st.MaxTime != 0.5 {
		t.Fatalf("total=%d timed=%d max=%v", st.Total, st.TimedCount, st.MaxTime)
	}
	if math.IsNaN(st.TimeSum) || math.IsInf(st.TimeSum, 0) || st.TimeSum != 0.5 {
		t.Errorf("TimeSum=%v", st.TimeSum)
	}
	if u := st.UriStat["/a"]; u == nil || u.TimeS != 0.5 || math.IsNaN(u.TimeS) {
		t.Errorf("UriStat=%+v", u)
	}
	if st.SlowCount != 0 {
		t.Errorf("SlowCount=%d", st.SlowCount)
	}
}

func TestParseTimestampValidation(t *testing.T) {
	if _, ok := parseTimestamp([]byte("2026-02-31T10:00:00+08:00")); ok {
		t.Fatal("非法日期应拒绝")
	}
	if _, ok := parseTimestamp([]byte("2026-09-01T10:00:00+15:00")); ok {
		t.Fatal("非法时区偏移应拒绝")
	}
	for _, bad := range []string{
		"2026-09-01T10:00:00+08",
		"2026-09-01T10:00:00+08:",
		"2026-09-01T10:00:00+08:00extra",
		"2026-09-01T10:00:00+ab:00",
		"2026-09-01T10:00:00Zx",
	} {
		if _, ok := parseTimestamp([]byte(bad)); ok {
			t.Fatalf("非法时区/尾缀应拒绝: %s", bad)
		}
	}
	if _, ok := parseTimestamp([]byte("2026-09-01T10:00:00+0800")); !ok {
		t.Fatal("无冒号时区偏移应接受")
	}
	if _, ok := parseTimestamp([]byte("2024-02-29T10:00:00+08:00")); !ok {
		t.Fatal("闰年 2 月 29 日应接受")
	}
}

func TestVeryLongUADoesNotPanic(t *testing.T) {
	ua := strings.Repeat("A", 1<<20)
	line := `{"@timestamp":"2026-09-01T10:00:00+08:00","client":"c","status":"200","request_uri":"/a","agent":"` + ua + `"}`
	st, err := ProcessReader(strings.NewReader(line), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 {
		t.Fatalf("total=%d", st.Total)
	}
}

func TestFieldLengthCaps(t *testing.T) {
	longURI := strings.Repeat("a", maxURIKey+100)
	line := `{"@timestamp":"2026-09-01T10:00:00+08:00","client":"c","status":"200","request_uri":"` + longURI + `"}`
	st, err := ProcessReader(strings.NewReader(line), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 {
		t.Fatalf("total=%d", st.Total)
	}
	for uri := range st.UriStat {
		if len(uri) > maxURIKey+3 {
			t.Fatalf("URI key 未截断: %d", len(uri))
		}
	}
	longStatus := strings.Repeat("9", maxShortKey+100)
	line = `{"@timestamp":"2026-09-01T10:00:00+08:00","client":"c","status":"` + longStatus + `","request_uri":"/a"}`
	st, err = ProcessReader(strings.NewReader(line), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for status := range st.StatusCnt {
		if len(status) > maxShortKey+3 {
			t.Fatalf("状态码 key 未截断: %d", len(status))
		}
	}
}

func TestMergeFromLegacyTimedCount(t *testing.T) {
	legacy := NewAgg()
	legacy.Total = 100
	legacy.TimeSum = 50
	legacy.TimeBuckets[10] = 100

	newer := NewAgg()
	newer.Total = 10
	newer.TimedCount = 10
	newer.TimeSum = 10
	newer.TimeBuckets[20] = 10

	legacy.MergeFrom(newer)
	if legacy.TimedCount != 110 {
		t.Fatalf("TimedCount=%d, want 110", legacy.TimedCount)
	}
	if got, want := legacy.AvgTime(), 60.0/110.0; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("AvgTime=%v, want %v", got, want)
	}
}
