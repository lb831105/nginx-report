package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nginx-report/internal/analyzer"
	"nginx-report/internal/config"
)

func TestLastWeekWindow(t *testing.T) {
	cases := []struct {
		now       string
		wantStart string
		wantEnd   string
		explain   string
	}{
		{"2026-11-02", "2026-10-26", "2026-11-01", "周一运行"},
		{"2026-11-03", "2026-10-26", "2026-11-01", "周二运行"},
		{"2026-11-07", "2026-10-26", "2026-11-01", "周六运行"},
		{"2026-11-08", "2026-10-26", "2026-11-01", "周日运行"},
		{"2026-11-09", "2026-11-02", "2026-11-08", "下周一运行"},
		{"2026-01-01", "2025-12-22", "2025-12-28", "跨年"},
	}
	for _, c := range cases {
		now, err := time.Parse("2006-01-02", c.now)
		if err != nil {
			t.Fatal(err)
		}
		gotStart, gotEnd := lastWeekWindow(now)
		if gotStart != c.wantStart || gotEnd != c.wantEnd {
			t.Errorf("%s(%s): got %s ~ %s, want %s ~ %s",
				c.explain, c.now, gotStart, gotEnd, c.wantStart, c.wantEnd)
		}
		// 结果必须正好 7 天
		s, _ := time.Parse("2006-01-02", gotStart)
		e, _ := time.Parse("2006-01-02", gotEnd)
		if e.Sub(s) != 6*24*time.Hour {
			t.Errorf("%s(%s): 周期不是 7 天: %s ~ %s", c.explain, c.now, gotStart, gotEnd)
		}
		// 结束日必须是周日
		if e.Weekday() != time.Sunday {
			t.Errorf("%s(%s): 结束日不是周日: %s", c.explain, c.now, gotEnd)
		}
	}
}

func TestDateRange(t *testing.T) {
	got := dateRange("2026-10-26", "2026-11-01")
	if len(got) != 7 {
		t.Fatalf("len=%d, want 7", len(got))
	}
	if got[0] != "2026-10-26" || got[6] != "2026-11-01" {
		t.Fatalf("range=%v", got)
	}
	if d := dateRange("2026-11-02", "2026-11-01"); len(d) != 0 {
		t.Fatalf("非法范围应返回空, got %v", d)
	}
}

func TestResolveDay(t *testing.T) {
	now := time.Now()
	y := now.AddDate(0, 0, -1).Format("2006-01-02")
	tod := now.Format("2006-01-02")

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", y, false},
		{"yesterday", y, false},
		{" Yesterday ", y, false},
		{"today", tod, false},
		{"TODAY", tod, false},
		{"2026-10-29", "2026-10-29", false},
		{"2026-13-01", "", true},
		{"2026/10/29", "", true},
	}
	for _, c := range cases {
		got, err := resolveDay(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolveDay(%q) 应该报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("resolveDay(%q)=%q,%v want %q", c.in, got, err, c.want)
		}
	}
}

func TestApplyLogTarget(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "28888_access_json.log")
	if err := os.WriteFile(file, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 传目录
	cfg := config.Default()
	if err := applyLogTarget(cfg, dir, false); err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != dir {
		t.Errorf("目录模式: LogDir=%q want %q", cfg.LogDir, dir)
	}
	if cfg.LogPattern != config.Default().LogPattern {
		t.Errorf("目录模式不应改模式: %q", cfg.LogPattern)
	}
	if cfg.SingleFile != "" {
		t.Errorf("目录模式不应进入单文件模式: %q", cfg.SingleFile)
	}

	// 传文件：记录为单文件模式，不改匹配模式（不匹配同目录其他文件）
	cfg = config.Default()
	if err := applyLogTarget(cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != dir {
		t.Errorf("文件模式: LogDir=%q want %q", cfg.LogDir, dir)
	}
	if cfg.SingleFile != file {
		t.Errorf("文件模式: SingleFile=%q want %q", cfg.SingleFile, file)
	}
	if cfg.LogPattern != config.Default().LogPattern {
		t.Errorf("文件模式不应改匹配模式: %q", cfg.LogPattern)
	}

	// 传文件 + 显式 -pattern：仍然只处理该文件（SingleFile 优先）
	cfg = config.Default()
	if err := applyLogTarget(cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.SingleFile != file {
		t.Errorf("显式 pattern 时仍应是单文件模式: %q", cfg.SingleFile)
	}

	// 路径不存在：不报错（周报可能只用日记录），但要按目录记下并告警
	missing := filepath.Join(dir, "nope")
	cfg = config.Default()
	if err := applyLogTarget(cfg, missing, false); err != nil {
		t.Errorf("不存在的路径不应直接失败: %v", err)
	}
	if cfg.LogDir != missing {
		t.Errorf("不存在的路径应按目录处理: LogDir=%q want %q", cfg.LogDir, missing)
	}
}

// findLogFiles：gz / 明文 / 无日期文件都要能挑出来
func TestFindLogFilesMixedFormats(t *testing.T) {
	dir := t.TempDir()
	touch := func(names ...string) {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := config.Default()
	cfg.LogDir = dir
	cfg.LogPattern = "access.log*"

	// 1) 有日期文件（含 gz）+ 无日期文件，但当天没有自己的文件 → 都读
	touch("access.log", "access.log.1.gz", "access.log-2026-10-29.gz", "access.log-2026-10-30")
	files, fb, err := findLogFiles(cfg, "2026-10-28", "2026-10-28")
	if err != nil {
		t.Fatal(err)
	}
	if fb {
		t.Error("有带日期文件时不应标记为回退")
	}
	if len(files) != 3 { // 10-29.gz（窗口放宽 +1 天）+ 两个无日期文件
		t.Errorf("应读到 3 个文件，实际 %d: %v", len(files), files)
	}

	// 2) 当天自己就有轮转文件（copytruncate 场景）→ 跳过无日期文件，避免重复计数
	files, _, err = findLogFiles(cfg, "2026-10-29", "2026-10-29")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 { // 10-29.gz + 10-30（窗口放宽）
		t.Errorf("应读到 2 个带日期文件，实际 %d: %v", len(files), files)
	}
	for _, f := range files {
		if analyzer.DateInFilename(filepath.Base(f)) == "" {
			t.Errorf("存在当天文件时不应再读无日期文件: %v", files)
		}
	}

	// 3) 全是无日期文件 → 全部扫描并标记回退
	dir2 := t.TempDir()
	cfg2 := config.Default()
	cfg2.LogDir = dir2
	cfg2.LogPattern = "access.log*"
	for _, n := range []string{"access.log", "access.log.1.gz", "access.log.2.gz"} {
		if err := os.WriteFile(filepath.Join(dir2, n), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	files, fb, err = findLogFiles(cfg2, "2026-10-29", "2026-10-29")
	if err != nil {
		t.Fatal(err)
	}
	if !fb || len(files) != 3 {
		t.Errorf("无日期文件应回退全量扫描: fallback=%v files=%d", fb, len(files))
	}

	// 4) 没有匹配文件 → 报错
	cfg3 := config.Default()
	cfg3.LogDir = t.TempDir()
	cfg3.LogPattern = "nope*"
	if _, _, err := findLogFiles(cfg3, "2026-10-29", "2026-10-29"); err == nil {
		t.Error("没有匹配文件时应返回错误")
	}

	// 5) 单文件模式：同目录有再多文件也只返回这一个
	cfgS := config.Default()
	cfgS.LogDir = dir
	cfgS.LogPattern = "access.log*"
	cfgS.SingleFile = filepath.Join(dir, "access.log-2026-10-29.gz")
	got, fb, err := findLogFiles(cfgS, "2026-10-29", "2026-10-29")
	if err != nil {
		t.Fatal(err)
	}
	if fb || len(got) != 1 || got[0] != cfgS.SingleFile {
		t.Errorf("单文件模式应只返回该文件: %v fallback=%v", got, fb)
	}

	// 6) 单文件不存在 / 不是普通文件 → 明确报错
	cfgS.SingleFile = filepath.Join(dir, "missing.log")
	if _, _, err := findLogFiles(cfgS, "", ""); err == nil {
		t.Error("单文件不存在时应报错")
	}
	cfgS.SingleFile = dir
	if _, _, err := findLogFiles(cfgS, "", ""); err == nil {
		t.Error("单文件是目录时应报错")
	}
}

func TestParseClockMinute(t *testing.T) {
	ok := map[string]int{"00:00": 0, "8:30": 510, "11:30": 690, "23:59": 1439}
	for s, want := range ok {
		got, err := parseClockMinute(s)
		if err != nil || got != want {
			t.Errorf("parseClockMinute(%q)=%d,%v want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"", "11", "24:00", "11:60", "aa:bb", "-1:00"} {
		if _, err := parseClockMinute(bad); err == nil {
			t.Errorf("parseClockMinute(%q) 应报错", bad)
		}
	}
}

func TestFindLogFilesSkipsNonRegularFallback(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "access.log")
	if err := os.WriteFile(regular, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LogDir = dir
	cfg.LogPattern = "*"
	files, fallback, err := findLogFiles(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !fallback || len(files) != 1 || files[0] != regular {
		t.Fatalf("files=%v fallback=%v", files, fallback)
	}
}

func TestFindLogFilesSiteRecursive(t *testing.T) {
	root := t.TempDir()
	must := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(root, "top.log"))
	must(filepath.Join(root, "ad.kmrd.cn", "access.log"))
	must(filepath.Join(root, "api.kmrd.cn", "access.log.gz"))

	cfg := config.Default()
	cfg.LogDir = root
	cfg.LogPattern = "*.log*"
	cfg.SiteEnabled = true
	files, _, err := findLogFiles(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("site recursive files=%v", files)
	}

	cfg.SiteEnabled = false
	files, _, err = findLogFiles(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "top.log" {
		t.Fatalf("non-site files=%v", files)
	}
}
