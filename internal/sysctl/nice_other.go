//go:build !linux && !darwin

// Package sysctl 提供进程层面的低优先级设置。
// Windows 等平台暂不实现（不修改优先级）。
package sysctl

// Nice 在非 Unix 平台为空实现
func Nice(level int) error { return nil }
