package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", p, err)
	}
	return string(b)
}

func TestFileLoggingDailyName(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")

	l := New(nil)
	l.now = fixedClock(time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local))
	if err := l.SetLogFile(base, 30); err != nil {
		t.Fatal(err)
	}
	defer l.CloseLogFile()

	l.Info("hello %s", "world")
	l.Error("boom")

	path := filepath.Join(dir, "app-2026-11-02.log")
	got := readFile(t, path)
	if !strings.Contains(got, "[INFO] hello world") || !strings.Contains(got, "[ERROR] boom") {
		t.Fatalf("日志内容不符合预期:\n%s", got)
	}
	// 文件里不应出现 ANSI 颜色码
	if strings.Contains(got, "\033[") {
		t.Fatal("日志文件里出现了 ANSI 颜色码")
	}
	if !strings.HasPrefix(got, "2026-11-02 10:00:00 [INFO]") {
		t.Fatalf("缺少时间戳前缀: %q", got)
	}
	if LogFilePath() == "" {
		t.Log("提示：LogFilePath 只反映全局 logger，这里是独立实例，忽略")
	}
	if l.file == nil || l.file.Name() != path {
		t.Fatalf("当前文件路径不符: %v", l.file)
	}
}

func TestRotateOnDayChange(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")

	l := New(nil)
	day1 := time.Date(2026, 11, 1, 23, 59, 0, 0, time.Local)
	day2 := time.Date(2026, 11, 2, 0, 1, 0, 0, time.Local)
	cur := day1
	l.now = func() time.Time { return cur }

	if err := l.SetLogFile(base, 30); err != nil {
		t.Fatal(err)
	}
	defer l.CloseLogFile()

	l.Info("day1 line")

	cur = day2
	l.Info("day2 line")
	l.Info("day2 line2")

	d1 := readFile(t, filepath.Join(dir, "app-2026-11-01.log"))
	d2 := readFile(t, filepath.Join(dir, "app-2026-11-02.log"))
	if !strings.Contains(d1, "day1 line") || strings.Contains(d1, "day2") {
		t.Fatalf("11-01 文件内容不对:\n%s", d1)
	}
	if !strings.Contains(d2, "day2 line") || strings.Contains(d2, "day1") {
		t.Fatalf("11-02 文件内容不对:\n%s", d2)
	}
	if l.fileDay != "2026-11-02" {
		t.Fatalf("fileDay=%q", l.fileDay)
	}
}

func TestCleanupKeepsConfiguredDays(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")

	// 预置历史文件：40 天前 / 35 天前 / 5 天前
	today := time.Date(2026, 11, 2, 8, 0, 0, 0, time.Local)
	for _, off := range []int{-40, -35, -5} {
		d := today.AddDate(0, 0, off).Format("2006-01-02")
		p := filepath.Join(dir, "app-"+d+".log")
		if err := os.WriteFile(p, []byte("old\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// 干扰项：不同前缀 / 不同扩展名，都不应被删除
	for _, name := range []string{"other-2026-01-01.log", "app-2026-01-01.txt", "app.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	l := New(nil)
	l.now = fixedClock(today)
	if err := l.SetLogFile(base, 30); err != nil {
		t.Fatal(err)
	}
	defer l.CloseLogFile()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}

	removed := []string{
		"app-" + today.AddDate(0, 0, -40).Format("2006-01-02") + ".log",
		"app-" + today.AddDate(0, 0, -35).Format("2006-01-02") + ".log",
	}
	for _, n := range removed {
		if names[n] {
			t.Errorf("超期日志未被清理: %s", n)
		}
	}
	kept := []string{
		"app-" + today.AddDate(0, 0, -5).Format("2006-01-02") + ".log",
		"app-2026-11-02.log",
		"other-2026-01-01.log",
		"app-2026-01-01.txt",
		"app.log",
	}
	for _, n := range kept {
		if !names[n] {
			t.Errorf("不应被删除: %s（当前目录: %v）", n, names)
		}
	}
}

func TestSetLogFileEmptyDisables(t *testing.T) {
	dir := t.TempDir()
	l := New(nil)
	if err := l.SetLogFile(filepath.Join(dir, "app.log"), 30); err != nil {
		t.Fatal(err)
	}
	if err := l.SetLogFile("", 30); err != nil {
		t.Fatal(err)
	}
	if l.file != nil || l.fileBase != "" {
		t.Fatal("SetLogFile(\"\") 应关闭文件输出")
	}
	l.Info("不应写文件")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(b), "不应写文件") {
			t.Fatal("关闭后仍在写文件")
		}
	}
}

func TestSetLogFileCreatesDirAndErrors(t *testing.T) {
	dir := t.TempDir()
	l := New(nil)
	// 自动创建多级目录
	base := filepath.Join(dir, "a", "b", "app.log")
	if err := l.SetLogFile(base, 30); err != nil {
		t.Fatalf("应自动创建目录: %v", err)
	}
	l.CloseLogFile()

	// 路径不可用（用已存在的文件当目录）→ 返回错误但不 panic
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	l2 := New(nil)
	if err := l2.SetLogFile(filepath.Join(f, "sub", "app.log"), 30); err == nil {
		t.Fatal("不可用路径应返回错误")
	}
	// 失败后继续写日志不应 panic（走重试逻辑）
	l2.Info("still ok")
}

func TestLevelFilteringStillAppliesToFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")

	l := New(nil)
	l.now = fixedClock(time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local))
	if err := l.SetLogFile(base, 30); err != nil {
		t.Fatal(err)
	}
	defer l.CloseLogFile()

	l.Debug("debug line") // 默认 INFO 级别，不应落盘
	l.Info("info line")

	got := readFile(t, filepath.Join(dir, "app-2026-11-02.log"))
	if strings.Contains(got, "debug line") {
		t.Fatal("DEBUG 级别不应写入")
	}
	if !strings.Contains(got, "info line") {
		t.Fatal("INFO 级别应写入")
	}
}

// 写文件失败（磁盘满/文件被外部删除）不能让日志静默消失：
// 应关闭句柄、到点后自动重开继续写。
func TestFileWriteFailureRecovers(t *testing.T) {
	dir := t.TempDir()
	l := New(nil)
	fixed := time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local)
	l.now = func() time.Time { return fixed }
	base := filepath.Join(dir, "app.log")
	if err := l.SetLogFile(base, 5); err != nil {
		t.Fatal(err)
	}

	// 模拟句柄被外部关掉（比如文件被删除后 fd 失效）
	l.mu.Lock()
	if err := l.file.Close(); err != nil {
		t.Fatal(err)
	}
	l.mu.Unlock()

	l.Info("第一次写失败") // 不 panic；内部会关句柄并记下重试时间
	if p := l.LogFilePath(); p != "" {
		t.Errorf("写失败后应关闭句柄，实际仍持有 %s", p)
	}

	// 把时钟推过重试间隔，下一次写应自动重开并成功落盘
	l.mu.Lock()
	l.now = func() time.Time { return fixed.Add(2 * openRetryInterval) }
	l.mu.Unlock()

	l.Info("重开后的日志")
	if p := l.LogFilePath(); p == "" {
		t.Fatal("重试后应重新打开日志文件")
	}
	l.CloseLogFile()

	data, err := os.ReadFile(filepath.Join(dir, "app-"+fixed.Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "重开后的日志") {
		t.Errorf("重开后的日志未落盘: %q", string(data))
	}
}
