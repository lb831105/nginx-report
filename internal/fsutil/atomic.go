// Package fsutil 提供文件写入的小工具。
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 原子写文件：先写同目录临时文件 → fsync → rename 覆盖。
//
// 为什么不用 os.WriteFile：报告 latest.html 可能正被 nginx/浏览器读取，
// 直接覆盖会读到"写了一半"的内容；进程崩溃或磁盘写满也会留下损坏文件。
// rename 在同一文件系统内是原子的，读方要么看到旧文件，要么看到新文件。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// rename 成功后临时文件已不存在，Remove 返回错误可忽略
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// 尽力而为地同步目录项，让 rename 也落盘（部分文件系统才需要；
	// Windows 上无法以只读方式打开目录，失败直接忽略）
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
