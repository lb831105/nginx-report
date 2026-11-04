// Package logger 提供分级日志输出（Debug / Info / Warn / Error）。
//
// 输出两端：
//   - 终端（stdout，可带 ANSI 颜色）
//   - 文件（无颜色），按天轮转，文件名形如 app-2026-11-02.log，保留 N 天
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 日志级别
type Level int

const (
	// LevelDebug 调试级别，输出最多
	LevelDebug Level = iota
	// LevelInfo 信息级别
	LevelInfo
	// LevelWarn 警告级别
	LevelWarn
	// LevelError 错误级别
	LevelError
)

var levelName = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// DefaultRetainDays 日志文件默认保留天数
const DefaultRetainDays = 30

// 打开日志文件失败后的重试间隔，避免每条日志都去重试 / 刷屏
const openRetryInterval = time.Minute

// Logger 分级日志器
type Logger struct {
	mu      sync.Mutex
	level   int32     // atomic: 与 SetLevel 并发安全
	out     io.Writer // 终端输出（带颜色）
	console bool      // 终端输出是否带颜色

	// 轮转文件日志
	fileBase string    // 基准路径，如 /var/log/nginx-report/app.log
	fileKeep int       // 保留天数，<=0 表示不清理
	file     *os.File  // 当前打开的日志文件
	fileDay  string    // 当前文件对应日期 YYYY-MM-DD
	fileTry  time.Time // 打开失败后的下次重试时间
	now      func() time.Time
}

var (
	std     *Logger
	stdOnce sync.Once
)

func stdLogger() *Logger {
	stdOnce.Do(func() {
		std = &Logger{
			level:   int32(LevelInfo),
			out:     os.Stdout,
			console: true,
			now:     time.Now,
		}
	})
	return std
}

// New 创建独立日志器（主要供测试使用），out 为 nil 时只写文件
func New(out io.Writer) *Logger {
	return &Logger{
		level:   int32(LevelInfo),
		out:     out,
		console: false,
		now:     time.Now,
	}
}

// SetLevel 设置全局日志级别（原子操作，可在运行期/并发下安全调用）
func SetLevel(level Level) {
	atomic.StoreInt32(&stdLogger().level, int32(level))
}

// SetDebug 便捷开关：true 开启调试日志，false 回到信息级别
func SetDebug(debug bool) {
	if debug {
		SetLevel(LevelDebug)
	} else {
		SetLevel(LevelInfo)
	}
}

// SetLogFile 开启文件日志：按天轮转，保留 keepDays 天（0=默认 30 天，负数=不清理）。
// base 例如 /var/log/nginx-report/app.log，实际写入 app-2026-11-02.log。
// 传空字符串表示关闭文件日志。
func SetLogFile(base string, keepDays int) error {
	return stdLogger().SetLogFile(base, keepDays)
}

// (l *Logger) SetLogFile 见包级 SetLogFile
func (l *Logger) SetLogFile(base string, keepDays int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.closeFileLocked()
	l.fileBase = ""
	l.fileDay = ""
	l.fileTry = time.Time{}

	if base == "" {
		return nil
	}
	// 0 = 用默认 30 天；负数 = 不清理
	if keepDays == 0 {
		keepDays = DefaultRetainDays
	}
	if keepDays < 0 {
		keepDays = 0
	}
	l.fileBase = base
	l.fileKeep = keepDays

	day := l.nowTime().Format("2006-01-02")
	if err := l.openLocked(day); err != nil {
		// 保留 fileBase，之后写日志时按 openRetryInterval 重试
		l.fileTry = l.nowTime().Add(openRetryInterval)
		return err
	}
	return nil
}

// CloseLogFile 关闭文件日志（进程退出前调用）
func CloseLogFile() {
	stdLogger().CloseLogFile()
}

// (l *Logger) CloseLogFile 见包级 CloseLogFile
func (l *Logger) CloseLogFile() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeFileLocked()
}

// LogFilePath 返回全局日志器当前日志文件路径（未开启或尚未打开时返回空串）
func LogFilePath() string { return stdLogger().LogFilePath() }

// LogFilePath 返回当前日志文件路径（未开启或尚未打开时返回空串）
func (l *Logger) LogFilePath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return ""
	}
	return l.file.Name()
}

// Debug 输出调试日志
func Debug(format string, args ...interface{}) { stdLogger().Debug(format, args...) }

// Info 输出信息日志
func Info(format string, args ...interface{}) { stdLogger().Info(format, args...) }

// Warn 输出警告日志
func Warn(format string, args ...interface{}) { stdLogger().Warn(format, args...) }

// Error 输出错误日志
func Error(format string, args ...interface{}) { stdLogger().Error(format, args...) }

// Debug 输出调试日志（实例方法）
func (l *Logger) Debug(format string, args ...interface{}) { l.log(LevelDebug, format, args...) }

// Info 输出信息日志（实例方法）
func (l *Logger) Info(format string, args ...interface{}) { l.log(LevelInfo, format, args...) }

// Warn 输出警告日志（实例方法）
func (l *Logger) Warn(format string, args ...interface{}) { l.log(LevelWarn, format, args...) }

// Error 输出错误日志（实例方法）
func (l *Logger) Error(format string, args ...interface{}) { l.log(LevelError, format, args...) }

// log 内部输出
func (l *Logger) log(level Level, format string, args ...interface{}) {
	if level < Level(atomic.LoadInt32(&l.level)) {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ts := l.nowTime().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	plain := fmt.Sprintf("%s [%s] %s\n", ts, levelName[level], msg)

	// 终端输出（可选颜色）
	if l.out != nil {
		if l.console {
			fmt.Fprintf(l.out, "%s\033[%dm[%s]\033[0m %s\n",
				ts, colorCode(level), levelName[level], msg)
		} else {
			io.WriteString(l.out, plain)
		}
	}

	// 文件输出（无颜色，按天轮转）
	l.writeFileLocked(plain)
}

// ---------------------------------------------------------------- 文件轮转

func (l *Logger) nowTime() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

func (l *Logger) closeFileLocked() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// writeFileLocked 写入当前日期对应的文件；跨天自动切换
func (l *Logger) writeFileLocked(msg string) {
	if l.fileBase == "" {
		return
	}
	now := l.nowTime()
	day := now.Format("2006-01-02")

	if l.file == nil {
		if now.Before(l.fileTry) {
			return // 刚失败过，稍后再试
		}
		if err := l.openLocked(day); err != nil {
			l.fileTry = now.Add(openRetryInterval)
			fmt.Fprintf(os.Stderr, "[WARN] 打开日志文件失败（将稍后重试）: %v\n", err)
			return
		}
	} else if l.fileDay != day {
		// 跨天：关闭旧文件，打开新一天的文件并清理超期文件
		l.closeFileLocked()
		if err := l.openLocked(day); err != nil {
			l.fileTry = now.Add(openRetryInterval)
			fmt.Fprintf(os.Stderr, "[WARN] 轮转日志文件失败（将稍后重试）: %v\n", err)
			return
		}
	}

	if _, err := io.WriteString(l.file, msg); err != nil {
		// 磁盘满 / 文件被外部删除 / 权限变化：关掉句柄，下次写入时按重试间隔重开
		l.closeFileLocked()
		l.fileTry = now.Add(openRetryInterval)
		fmt.Fprintf(os.Stderr, "[WARN] 写日志文件失败（将稍后重试）: %v\n", err)
	}
}

// openLocked 打开 day 对应的日志文件，并清理超期文件
func (l *Logger) openLocked(day string) error {
	dir := filepath.Dir(l.fileBase)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.pathForDay(day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	l.file = f
	l.fileDay = day
	l.fileTry = time.Time{}
	l.cleanupLocked(day)
	return nil
}

// pathForDay 生成某天的日志文件名：app.log + 2026-11-02 -> app-2026-11-02.log
func (l *Logger) pathForDay(day string) string {
	dir, stem, ext := splitLogPath(l.fileBase)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", stem, day, ext))
}

// splitLogPath 拆分基准路径：/x/app.log -> (/x, app, .log)；无扩展名时补 .log
func splitLogPath(base string) (dir, stem, ext string) {
	dir = filepath.Dir(base)
	name := filepath.Base(base)
	ext = filepath.Ext(name)
	stem = strings.TrimSuffix(name, ext)
	if ext == "" {
		ext = ".log"
	}
	return dir, stem, ext
}

// cleanupLocked 删除早于「今天 - keepDays」的历史日志文件
func (l *Logger) cleanupLocked(today string) {
	if l.fileKeep <= 0 {
		return
	}
	now, err := time.Parse("2006-01-02", today)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -l.fileKeep)

	dir, stem, ext := splitLogPath(l.fileBase)
	prefix := stem + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		dayStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
		d, err := time.Parse("2006-01-02", dayStr)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// colorCode 返回级别对应的 ANSI 前景色码（如 31 红色）
func colorCode(level Level) int {
	switch level {
	case LevelDebug:
		return 37
	case LevelInfo:
		return 32
	case LevelWarn:
		return 33
	case LevelError:
		return 31
	}
	return 0
}
