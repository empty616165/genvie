package hostupdate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// pullRunner 记录每次 docker pull 的镜像与结果，便于断言回退顺序。
type pullRunner struct {
	attempts []string
	// failWith 对指定镜像返回错误；返回空表示成功。
	failWith func(ref string) error
	removed  []string
}

func (p *pullRunner) run(_ context.Context, _ string, args, _ []string, _ io.Writer, stderr io.Writer) error {
	switch {
	case len(args) >= 2 && args[0] == "pull":
		ref := args[1]
		p.attempts = append(p.attempts, ref)
		if p.failWith != nil {
			if err := p.failWith(ref); err != nil {
				_, _ = io.WriteString(stderr, err.Error())
				return err
			}
		}
		return nil
	case len(args) >= 4 && args[0] == "image" && args[1] == "rm":
		p.removed = append(p.removed, args[3])
		return nil
	}
	return nil
}

// 镜像加速的核心可用性：所选源拉不动某个镜像时，改用 ghcr.io 同一 digest，
// 而不是在同一个仓库里反复重试到超时。
func TestPullFallsBackToGHCRWhenMirrorFails(t *testing.T) {
	m := sourceManager(t)
	runner := &pullRunner{failWith: func(ref string) error {
		if strings.HasPrefix(ref, "ghcr.nju.edu.cn/") {
			// 模拟大层传输停滞导致层提交失败。
			return errors.New(`failed commit on ref "layer-sha256:abc": commit failed: unexpected commit digest: failed precondition`)
		}
		return nil
	}}
	m.runner = sourceRunner(runner.run)

	sources := Sources{GitHubSource: "direct", ImageSource: "nju"}
	requested := sources.imagesWithin(m, m.config.Repository, "v2.0.0")
	pulled, err := m.pullTargetImages(m.composePath(), "v2.0.0", requested, sources)
	if err != nil {
		t.Fatalf("mirror failure should fall back to ghcr.io: %v", err)
	}

	// 每个镜像先试镜像源，再退到 ghcr.io，共 6 次尝试。
	if len(runner.attempts) != 6 {
		t.Fatalf("attempts = %v", runner.attempts)
	}
	for i := 0; i < len(runner.attempts); i += 2 {
		if !strings.HasPrefix(runner.attempts[i], "ghcr.nju.edu.cn/") {
			t.Errorf("attempt %d should hit the mirror first: %s", i, runner.attempts[i])
		}
		if !strings.HasPrefix(runner.attempts[i+1], "ghcr.io/") {
			t.Errorf("attempt %d should fall back to ghcr.io: %s", i+1, runner.attempts[i+1])
		}
	}
	// 返回的引用必须是实际成功拉取的那一份，否则后续 migrate/up 会去拉镜像源标签。
	for _, ref := range []string{pulled.backend, pulled.web, pulled.agent} {
		if !strings.HasPrefix(ref, "ghcr.io/") {
			t.Fatalf("returned image should be the fallback ref: %s", ref)
		}
	}
	if len(runner.removed) != 3 {
		t.Fatalf("should clean the failed mirror tags, got %v", runner.removed)
	}
}

// 镜像源正常时不应退到 ghcr.io，避免无谓的跨源下载。
func TestPullUsesSelectedMirrorWhenItSucceeds(t *testing.T) {
	m := sourceManager(t)
	runner := &pullRunner{}
	m.runner = sourceRunner(runner.run)

	sources := Sources{GitHubSource: "direct", ImageSource: "nju"}
	requested := sources.imagesWithin(m, m.config.Repository, "v2.0.0")
	pulled, err := m.pullTargetImages(m.composePath(), "v2.0.0", requested, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.attempts) != 3 {
		t.Fatalf("expected one attempt per image, got %v", runner.attempts)
	}
	for _, ref := range runner.attempts {
		if !strings.HasPrefix(ref, "ghcr.nju.edu.cn/") {
			t.Fatalf("unexpected source: %s", ref)
		}
	}
	if pulled != requested {
		t.Fatalf("pulled refs changed: %+v", pulled)
	}
}

// 鉴权、404、架构不符属于确定性失败，换源只会拖长停服前的等待，应立即中止。
func TestPullDoesNotFallbackOnDeterministicFailure(t *testing.T) {
	m := sourceManager(t)
	runner := &pullRunner{failWith: func(ref string) error {
		return errors.New("denied: requested access to the resource is denied")
	}}
	m.runner = sourceRunner(runner.run)

	sources := Sources{GitHubSource: "direct", ImageSource: "nju"}
	requested := sources.imagesWithin(m, m.config.Repository, "v2.0.0")
	if _, err := m.pullTargetImages(m.composePath(), "v2.0.0", requested, sources); err == nil {
		t.Fatal("deterministic failure should abort")
	}
	if len(runner.attempts) != 1 {
		t.Fatalf("should stop after the first deterministic failure, got %v", runner.attempts)
	}
}

// 两个来源都失败时，错误必须同时保留两边的失败原因，便于判断是限流、断链还是空间不足。
func TestPullReportsBothSourcesWhenFallbackAlsoFails(t *testing.T) {
	m := sourceManager(t)
	runner := &pullRunner{failWith: func(ref string) error {
		if strings.HasPrefix(ref, "ghcr.nju.edu.cn/") {
			return errors.New("failed commit on ref: unexpected commit digest")
		}
		return errors.New("no space left on device")
	}}
	m.runner = sourceRunner(runner.run)

	sources := Sources{GitHubSource: "direct", ImageSource: "nju"}
	requested := sources.imagesWithin(m, m.config.Repository, "v2.0.0")
	_, err := m.pullTargetImages(m.composePath(), "v2.0.0", requested, sources)
	if err == nil {
		t.Fatal("expected failure")
	}
	message := err.Error()
	if !strings.Contains(message, "所选源失败") || !strings.Contains(message, "ghcr.io 兜底也失败") {
		t.Fatalf("error did not describe both sources: %s", message)
	}
}

// 镜像源本来就是 ghcr.io 时不存在兜底来源，失败应直接返回。
func TestPullWithoutMirrorHasNoFallback(t *testing.T) {
	m := sourceManager(t)
	runner := &pullRunner{failWith: func(string) error {
		return errors.New("connection reset by peer")
	}}
	m.runner = sourceRunner(runner.run)

	sources := Sources{GitHubSource: "direct", ImageSource: "ghcr"}
	requested := sources.imagesWithin(m, m.config.Repository, "v2.0.0")
	if _, err := m.pullTargetImages(m.composePath(), "v2.0.0", requested, sources); err == nil {
		t.Fatal("expected failure")
	}
	if len(runner.attempts) != 1 {
		t.Fatalf("ghcr source must not retry itself as a fallback: %v", runner.attempts)
	}
}
