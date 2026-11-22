//go:build windows

package hostupdate

// checkBackupDiskSpace 在 Windows 本地开发环境跳过磁盘空间预检。
// 磁盘预检仅服务 Linux 服务器部署时的在线更新器，Windows 本地开发用不到。
func checkBackupDiskSpace(string) error {
	return nil
}

// checkImageDiskSpace 在 Windows 本地开发环境跳过镜像空间预检。
func checkImageDiskSpace(string) error {
	return nil
}
