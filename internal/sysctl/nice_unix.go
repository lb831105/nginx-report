//go:build linux || darwin

// Package sysctl 提供进程层面的低优先级设置，避免统计任务抢占业务 CPU。
package sysctl

import "syscall"

// Nice 设置进程 nice 值（0-19，19 最低优先级）。
// 降低优先级（增大 nice 值）无需特权，符合生产环境降负载需求。
func Nice(level int) error {
	if level < 0 {
		level = 0
	}
	if level > 19 {
		level = 19
	}
	if level == 0 {
		return nil
	}
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, level)
}
