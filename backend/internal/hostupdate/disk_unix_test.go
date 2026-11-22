//go:build !windows

package hostupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 镜像空间预检必须在拉取前拦住空间不足，否则 Docker 会写入截断层，
// 之后每次提交都报摘要不符（unexpected commit digest）。
func TestCheckImageDiskSpaceRejectsMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	err := checkImageDiskSpace(missing)
	if err == nil {
		t.Fatal("missing directory was accepted")
	}
	if !strings.Contains(err.Error(), "检查镜像存储磁盘空间") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// 正常目录只要空间充足就应通过，不因预检本身产生误报。
func TestCheckImageDiskSpaceAcceptsWritableDirectory(t *testing.T) {
	directory := t.TempDir()
	if stat, err := os.Stat(directory); err != nil || !stat.IsDir() {
		t.Fatalf("fixture directory unavailable: %v", err)
	}
	if err := checkImageDiskSpace(directory); err != nil {
		t.Fatalf("sufficient space was rejected: %v", err)
	}
}

// 镜像空间阈值必须明显高于备份阈值，并覆盖三个镜像解压后的体积。
func TestImageDiskThresholdExceedsBackupThreshold(t *testing.T) {
	if minImageDiskFreeBytes <= 2<<30 {
		t.Fatalf("image threshold %d must exceed the 2 GiB backup threshold", minImageDiskFreeBytes)
	}
}

// docker info 不可用时 dockerDataDir 返回空串，调用方必须跳过预检；
// 若把空串当路径，Statfs 必然失败，会误拦截所有更新。
func TestDockerDataDirReturnsEmptyWhenDockerUnavailable(t *testing.T) {
	m := sourceManager(t)
	m.runner = sourceRunner(func(_ context.Context, _ string, _ []string, _ []string, _ io.Writer, _ io.Writer) error {
		return errors.New("docker daemon not running")
	})
	if directory := m.dockerDataDir(); directory != "" {
		t.Fatalf("expected empty directory when docker info fails, got %q", directory)
	}
}

// docker info 正常时必须使用它报告的真实数据根目录。
func TestDockerDataDirUsesReportedRoot(t *testing.T) {
	m := sourceManager(t)
	m.runner = sourceRunner(func(_ context.Context, _ string, args []string, _ []string, out, _ io.Writer) error {
		if len(args) >= 2 && args[0] == "info" {
			_, _ = io.WriteString(out, "/srv/docker\n")
		}
		return nil
	})
	if directory := m.dockerDataDir(); directory != "/srv/docker" {
		t.Fatalf("dockerDataDir = %q", directory)
	}
}
