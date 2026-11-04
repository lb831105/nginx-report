//go:build linux || darwin

package sysinfo

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuTime 通过 getrusage 取进程累计 CPU 时间
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// peakRSSKB 读取 Linux 的 VmHWM（进程生命周期内的峰值常驻内存）。
// 该值由内核维护，比"采样当前 RSS"更准确，且不需要额外依赖。
func peakRSSKB() int64 {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "VmHWM:") {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				break
			}
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return v
			}
		}
	}
	// 非 Linux：退回 getrusage 的 maxrss（darwin 单位是字节，linux 是 KB）
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		if ru.Maxrss > 1<<20 { // 看起来是字节
			return ru.Maxrss / 1024
		}
		return ru.Maxrss
	}
	return 0
}
