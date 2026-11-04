// Package sysinfo 提供进程自身的资源占用信息（CPU 时间、峰值内存），
// 用于在日志里如实报告"这次跑动吃了多少服务器资源"。
package sysinfo

import (
	"runtime"
	"time"
)

// Usage 一次运行的资源占用
type Usage struct {
	// CPUTime 进程累计 CPU 时间（用户态+内核态）
	CPUTime time.Duration
	// PeakRSSKB 峰值常驻内存（KB），取不到时为 0
	PeakRSSKB int64
	// HeapSysMB Go 从操作系统申请的内存（MB），兜底指标
	HeapSysMB float64
}

// Read 读取当前进程的累计资源占用
func Read() Usage {
	var u Usage
	u.CPUTime = cpuTime()
	u.PeakRSSKB = peakRSSKB()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	u.HeapSysMB = float64(m.Sys) / (1 << 20)
	return u
}

// Human 便于日志阅读的字符串
func (u Usage) Human() string {
	s := ""
	if u.CPUTime > 0 {
		s += formatSeconds(u.CPUTime.Seconds()) + " CPU"
	}
	if u.PeakRSSKB > 0 {
		if s != "" {
			s += "，"
		}
		s += formatMB(float64(u.PeakRSSKB)/1024) + " 峰值内存"
	} else if u.HeapSysMB > 0 {
		if s != "" {
			s += "，"
		}
		s += formatMB(u.HeapSysMB) + " 堆内存"
	}
	return s
}

func formatSeconds(v float64) string {
	switch {
	case v >= 60:
		return itoa(int(v/60)) + "m" + itoa(int(v)%60) + "s"
	case v >= 1:
		return itoa(int(v)) + "." + itoa(int(v*10)%10) + "s"
	}
	return itoa(int(v*1000)) + "ms"
}

func formatMB(v float64) string {
	if v >= 1024 {
		return itoa(int(v/1024)) + "." + itoa(int(v/102.4)%10) + " GB"
	}
	return itoa(int(v)) + " MB"
}

func itoa(v int) string {
	if v < 0 {
		return "-" + itoa(-v)
	}
	if v < 10 {
		return string(rune('0' + v))
	}
	return itoa(v/10) + string(rune('0'+v%10))
}
