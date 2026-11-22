//go:build !windows

package hostupdate

import (
	"fmt"
	"syscall"
)

// checkBackupDiskSpace 确保备份目录至少有 2 GiB 可用空间。
func checkBackupDiskSpace(directory string) error {
	available, err := availableDiskSpace(directory)
	if err != nil {
		return fmt.Errorf("检查备份磁盘空间：%w", err)
	}
	if available < 2<<30 {
		return fmt.Errorf("备份目录可用空间不足 2 GiB：当前 %d MiB", available>>20)
	}
	return nil
}

// checkImageDiskSpace 确保 Docker 数据目录能容纳即将拉取的镜像层。
// 空间不足时 Docker 会写入截断的层，之后每次提交都报摘要不符，
// 因此要在停服前拦下，而不是等 pull 阶段失败。
func checkImageDiskSpace(directory string) error {
	available, err := availableDiskSpace(directory)
	if err != nil {
		return fmt.Errorf("检查镜像存储磁盘空间：%w", err)
	}
	if available < minImageDiskFreeBytes {
		return fmt.Errorf("Docker 数据目录可用空间不足 %d MiB（%s）：当前 %d MiB，请先清理镜像或扩容", minImageDiskFreeBytes>>20, directory, available>>20)
	}
	return nil
}

func availableDiskSpace(directory string) (int64, error) {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(directory, &disk); err != nil {
		return 0, err
	}
	return int64(disk.Bavail) * int64(disk.Bsize), nil
}
