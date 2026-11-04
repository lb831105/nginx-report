package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nginx-report/internal/analyzer"
)

func sampleAgg() *analyzer.Agg {
	a := analyzer.NewAgg()
	for i := 0; i < 100; i++ {
		a.Total++
		a.SizeTotal += 10
		a.TimeSum += 0.01
		a.TimeBuckets[analyzer.TimeBucketMS]++
		a.HourCnt[10]++
		a.StatusCnt["200"]++
		a.Uri("/api/a").Calls++
		a.ClientCnt["1.1.1.1"]++
	}
	a.Uri("/api/a").Slows += 3
	a.SlotCnt[6] += 5
	a.MinuteCnt[10*60] = 7
	a.MinuteBytes[10*60] = 1024
	a.MaxTime = 2.5
	a.DeviceCnt["移动端"] = 1
	a.BrowserCnt["Chrome"] = 1
	a.RefererCnt["ref.example:80"] = 1
	a.DirectCnt = 2
	a.ErrorURI["500"] = map[string]int64{"/api/a": 1}
	a.Uri("/api/a").Bytes = 4096
	a.Upstream("10.0.0.1:80").Calls++
	a.Upstream("10.0.0.1:80").TimeC++
	a.Upstream("10.0.0.1:80").TimeS += 0.02
	a.Upstream("10.0.0.1:80").Errors += 2
	a.StatusCnt["500"]++
	a.ErrorCount++
	a.ErrorStatus["500"]++
	a.SlowCount += 2
	peak, _ := time.Parse("2006-01-02 15:04:05", "2026-09-01 10:00:03")
	a.SecondCnt[peak.Unix()] = 12
	return a
}

func TestDailyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	agg := sampleAgg()

	if err := SaveDaily(dir, "2026-09-01", NewDaily("2026-09-01", agg, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(DailyDir(dir), "report_2026-09-01.json")); err != nil {
		t.Fatalf("日记录文件不存在: %v", err)
	}

	d, err := LoadDaily(dir, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if d.Total != agg.Total || d.ErrorCount != agg.ErrorCount || d.SlowCount != agg.SlowCount {
		t.Errorf("标量字段不一致: %+v", d)
	}
	if d.AvgTime != agg.AvgTime() {
		t.Errorf("AvgTime=%v, want %v", d.AvgTime, agg.AvgTime())
	}

	restored := d.ToAgg()
	if restored.Total != agg.Total {
		t.Errorf("Total=%d, want %d", restored.Total, agg.Total)
	}
	if restored.AvgTime() != agg.AvgTime() {
		t.Errorf("AvgTime=%v, want %v", restored.AvgTime(), agg.AvgTime())
	}
	if restored.Percentile(0.5) != agg.Percentile(0.5) {
		t.Errorf("P50=%v, want %v", restored.Percentile(0.5), agg.Percentile(0.5))
	}
	if restored.StatusCnt["200"] != 100 || restored.StatusCnt["500"] != 1 {
		t.Errorf("StatusCnt=%v", restored.StatusCnt)
	}
	if restored.HourCnt[10] != 100 {
		t.Errorf("HourCnt=%v", restored.HourCnt)
	}
	if restored.MinuteCnt[10*60] != 7 {
		t.Errorf("MinuteCnt=%v", restored.MinuteCnt[10*60])
	}
	if restored.MinuteBytes[10*60] != 1024 {
		t.Errorf("MinuteBytes=%v", restored.MinuteBytes[10*60])
	}
	if restored.MaxTime != 2.5 {
		t.Errorf("MaxTime=%v", restored.MaxTime)
	}
	if restored.DeviceCnt["移动端"] != 1 || restored.BrowserCnt["Chrome"] != 1 ||
		restored.RefererCnt["ref.example:80"] != 1 || restored.DirectCnt != 2 {
		t.Errorf("设备/浏览器/referer 未正确还原: %+v", restored)
	}
	if restored.ErrorURI["500"]["/api/a"] != 1 {
		t.Errorf("ErrorURI=%v", restored.ErrorURI)
	}
	if restored.UriStat["/api/a"].Bytes != 4096 {
		t.Errorf("UriStat.Bytes=%v", restored.UriStat["/api/a"].Bytes)
	}
	if restored.ClientCnt["1.1.1.1"] != 100 {
		t.Errorf("ClientCnt=%v", restored.ClientCnt)
	}
	if _, cnt := restored.Peak(); cnt != 12 {
		t.Errorf("峰值 QPS=%d, want 12", cnt)
	}
	// 新增维度也要能往返
	if len(d.SlotCnt) != 7 {
		t.Errorf("SlotCnt 长度=%d, want 7（尾部零值被裁剪）", len(d.SlotCnt))
	}
	if restored.UriStat["/api/a"].Slows != 3 {
		t.Errorf("UriStat.Slows=%v", restored.UriStat)
	}
	if restored.UpstreamStat["10.0.0.1:80"].Errors != 2 {
		t.Errorf("UpstreamStat.Errors=%v", restored.UpstreamStat)
	}
	if got := restored.SlotCnt[6]; got != 5 {
		t.Errorf("SlotCnt[6]=%d, want 5", got)
	}
}

// LoadRecent 只取当前周之前的历史周，按时间升序，并限制条数
func TestLoadRecent(t *testing.T) {
	dir := t.TempDir()
	weeks := []struct{ start, end string }{
		{"2026-10-05", "2026-10-11"},
		{"2026-10-12", "2026-10-18"},
		{"2026-10-19", "2026-10-25"},
		{"2026-10-26", "2026-11-01"}, // 当前周，应被排除
	}
	for i, w := range weeks {
		sum := &Summary{StartDate: w.start, EndDate: w.end, Total: int64(i + 1)}
		if err := Save(dir, w.start, w.end, sum); err != nil {
			t.Fatal(err)
		}
	}

	got := LoadRecent(dir, "2026-10-26", 8)
	if len(got) != 3 {
		t.Fatalf("应返回 3 个历史周，实际 %d", len(got))
	}
	if got[0].StartDate != "2026-10-05" || got[2].StartDate != "2026-10-19" {
		t.Errorf("排序或过滤错误: %v, %v", got[0].StartDate, got[2].StartDate)
	}
	if len(LoadRecent(dir, "2026-10-26", 2)) != 2 {
		t.Error("n 限制未生效")
	}
	if len(LoadRecent(dir, "2026-10-26", 0)) != 0 {
		t.Error("n<=0 应返回空")
	}
	if len(LoadRecent(filepath.Join(dir, "nope"), "2026-10-26", 8)) != 0 {
		t.Error("目录不存在应返回空而不是 panic")
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(DailyDir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -200).Format("2006-01-02")
	recent := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	files := map[string]string{
		filepath.Join(dir, "report_2026-01-05_to_"+old+".json"):    "{}",
		filepath.Join(DailyDir(dir), "report_"+old+".json"):        "{}",
		filepath.Join(dir, "report_2026-08-01_to_"+recent+".json"): "{}",
		filepath.Join(DailyDir(dir), "report_"+recent+".json"):     "{}",
		filepath.Join(dir, "unrelated.json"):                       "{}",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := Cleanup(dir, 90); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{
		filepath.Join(dir, "report_2026-01-05_to_"+old+".json"),
		filepath.Join(DailyDir(dir), "report_"+old+".json"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("应删除但存在: %s", gone)
		}
	}
	for _, kept := range []string{
		filepath.Join(dir, "report_2026-08-01_to_"+recent+".json"),
		filepath.Join(DailyDir(dir), "report_"+recent+".json"),
		filepath.Join(dir, "unrelated.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("应保留但丢失: %s", kept)
		}
	}
}

func TestIsDailyFinal(t *testing.T) {
	dir := t.TempDir()
	agg := sampleAgg()

	// 不存在 → false
	if IsDailyFinal(dir, "2026-09-01") {
		t.Error("记录不存在时应返回 false")
	}

	// partial 记录 → false（当天数据未完整）
	if err := SaveDaily(dir, "2026-09-01", NewDaily("2026-09-01", agg, true)); err != nil {
		t.Fatal(err)
	}
	if IsDailyFinal(dir, "2026-09-01") {
		t.Error("partial 记录不应视为最终数据")
	}
	d, err := LoadDaily(dir, "2026-09-01")
	if err != nil || !d.Partial {
		t.Fatalf("partial 标记未持久化: %+v", d)
	}

	// 覆盖为最终记录 → true
	if err := SaveDaily(dir, "2026-09-01", NewDaily("2026-09-01", agg, false)); err != nil {
		t.Fatal(err)
	}
	if !IsDailyFinal(dir, "2026-09-01") {
		t.Error("最终记录应返回 true")
	}
}

// 环比取「上一个等长周期」：7 天窗口取上周；自定义 3 天窗口取前 3 天
func TestLoadPreviousWindow(t *testing.T) {
	dir := t.TempDir()
	save := func(start, end string, total int64) {
		if err := Save(dir, start, end, &Summary{StartDate: start, EndDate: end, Total: total}); err != nil {
			t.Fatal(err)
		}
	}
	save("2026-10-19", "2026-10-25", 111) // 上周（7 天窗口用）
	save("2026-10-23", "2026-10-25", 222) // 前 3 天（3 天窗口用）

	s, err := LoadPrevious(dir, "2026-10-26", "2026-11-01")
	if err != nil || s.Total != 111 {
		t.Fatalf("7 天窗口环比应取 10-19~10-25: %+v err=%v", s, err)
	}
	s, err = LoadPrevious(dir, "2026-10-26", "2026-10-28")
	if err != nil || s.Total != 222 {
		t.Fatalf("3 天窗口环比应取 10-23~10-25: %+v err=%v", s, err)
	}
	if _, err := LoadPrevious(dir, "bad-date", "x"); err == nil {
		t.Error("非法日期应返回错误")
	}
	// 没有历史数据时返回错误（调用方据此显示"无对比"）
	if _, err := LoadPrevious(dir, "2026-12-07", "2026-12-13"); err == nil {
		t.Error("缺失历史应返回错误")
	}
}

// 峰值时刻必须带时区往返：日志是 +08 时，装配回周报仍是 09:44 而不是 01:44
func TestDailyPeakTimeKeepsZone(t *testing.T) {
	agg := analyzer.NewAgg()
	loc := time.FixedZone("UTC+8", 8*3600)
	ts := time.Date(2026, 10, 27, 9, 44, 32, 0, loc)
	for i := 0; i < 5; i++ {
		agg.SecondCnt[ts.Unix()]++
	}
	agg.SetLoc(loc)

	dir := t.TempDir()
	if err := SaveDaily(dir, "2026-10-27", NewDaily("2026-10-27", agg, false)); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaily(dir, "2026-10-27")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.PeakTime, "+08:00") {
		t.Errorf("PeakTime 未包含时区偏移: %s", d.PeakTime)
	}
	got, cnt := d.ToAgg().Peak()
	if cnt != 5 {
		t.Errorf("峰值=%d, want 5", cnt)
	}
	if got.Format("15:04:05") != "09:44:32" {
		t.Errorf("峰值时刻=%s, want 09:44:32", got.Format("15:04:05"))
	}
	if _, off := got.Zone(); off != 8*3600 {
		t.Errorf("峰值时区偏移=%d, want 28800", off)
	}

	// 旧格式（无偏移）仍要能读
	d.PeakTime = "2026-10-27 09:44:32"
	if _, cnt := d.ToAgg().Peak(); cnt != 5 {
		t.Errorf("旧格式解析失败，峰值=%d", cnt)
	}
}

// 旧版日记录（4 张 URI 表 / 4 张上游表）要能自动折叠进新结构
func TestLegacyDailyFold(t *testing.T) {
	d := &Daily{
		Day:            "2026-10-01",
		Total:          10,
		UriCnt:         map[string]int64{"/a": 6, "/b": 4},
		UriTimeSum:     map[string]float64{"/a": 1.2, "/b": 0.4},
		UriErrorCnt:    map[string]int64{"/a": 2},
		UriSlowCnt:     map[string]int64{"/a": 1},
		UpstreamCnt:    map[string]int64{"10.0.0.1:80": 7},
		UpstreamTimeS:  map[string]float64{"10.0.0.1:80": 0.7},
		UpstreamTimeC:  map[string]int64{"10.0.0.1:80": 7},
		UpstreamErrCnt: map[string]int64{"10.0.0.1:80": 3},
	}
	a := d.ToAgg()
	if u := a.UriStat["/a"]; u == nil || u.Calls != 6 || u.TimeS != 1.2 || u.Errors != 2 || u.Slows != 1 {
		t.Fatalf("旧 URI 字段未正确折叠: %+v", a.UriStat["/a"])
	}
	if u := a.UriStat["/b"]; u == nil || u.Calls != 4 {
		t.Fatalf("/b 折叠失败: %+v", a.UriStat["/b"])
	}
	if up := a.UpstreamStat["10.0.0.1:80"]; up == nil || up.Calls != 7 || up.TimeC != 7 || up.Errors != 3 {
		t.Fatalf("旧上游字段未正确折叠: %+v", a.UpstreamStat["10.0.0.1:80"])
	}
}

// 10 秒以上的耗时桶要能写入日记录并还原，否则周报 P99 会退回到 10s
func TestDailyTimeOverflowRoundTrip(t *testing.T) {
	line := `{"@timestamp": "2026-09-01T10:00:00+08:00", "status": "200", "upstream_response_time": "12.5", "request_uri": "/slow"}`
	st, err := analyzer.ProcessReader(strings.NewReader(line), analyzer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDaily("2026-09-01", &st.Agg, false)
	got := d.ToAgg()
	if p := got.Percentile(0.99); p < 12 || p > 13 {
		t.Errorf("日记录还原后 P99=%v, want ~12.5", p)
	}
}

func TestDailySitesRoundTrip(t *testing.T) {
	child := NewDaily("2026-10-30", analyzer.NewAgg(), false)
	child.Total = 3
	child.Site = "d:example.com"
	child.SiteKind = "domain"
	child.SiteName = "example.com"
	child.StaticSkipped = 2
	child.StaticBytes = 1024
	child.BotSkipped = 1
	child.SiteSourceCnt = map[string]int64{"map": 2, "field": 1}
	root := NewDaily("2026-10-30", analyzer.NewAgg(), false)
	root.Total = 3
	root.Sites = map[string]*Daily{"d:example.com": child}

	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var got Daily
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sites) != 1 || got.Sites["d:example.com"] == nil {
		t.Fatalf("sites = %+v", got.Sites)
	}
	c := got.Sites["d:example.com"]
	if c.Site != "d:example.com" || c.SiteKind != "domain" || c.StaticSkipped != 2 || c.StaticBytes != 1024 || c.BotSkipped != 1 {
		t.Fatalf("child = %+v", c)
	}
	if c.SiteSourceCnt["map"] != 2 || c.SiteSourceCnt["field"] != 1 {
		t.Fatalf("source = %+v", c.SiteSourceCnt)
	}
}
