package hostupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxReleaseResponseBytes = 4 << 20

// DefaultRepository 是更新器默认读取 Release、Compose 和 Host Updater 二进制的仓库。
// 容器镜像包名固定为 genvie-canvas-*，因此这里要指向实际发布该包的仓库。
const DefaultRepository = "empty616165/genvie"

type Config struct {
	Repository  string
	InstallDir  string
	ComposeFile string
	EnvFile     string
	StateDir    string
	BackupDir   string
	HealthURL   string
	GitHubToken string
	// GitHubProxy 是 gh_proxy 来源使用的加速前缀，必须带协议与结尾斜杠。
	// 公共代理域名会失效或返回 403，宿主机可用 CANVAS_UPDATER_GITHUB_PROXY 覆盖。
	GitHubProxy string
	// ImageMirror 是 nju 来源使用的容器镜像仓库主机名。
	// 可用 CANVAS_UPDATER_IMAGE_MIRROR 指向自建代理或其它透明镜像源。
	ImageMirror string
	// ReleaseAttemptTimeout 是单个 GitHub 出口的请求预算，主要用于测试缩短等待。
	ReleaseAttemptTimeout time.Duration
	// ImagePullTimeout 是单个镜像的拉取预算。公共镜像源在大层上可能只有
	// 100~200 KB/s，按三镜像共享 StepTimeout 会必然超时，因此独立配置。
	ImagePullTimeout time.Duration
	StableWindow     time.Duration
	StepTimeout      time.Duration
	BinaryPath       string
	ServiceName      string
	SelfUpdate       bool
}

// DefaultGitHubProxy 与 DefaultImageMirror 只提供实测可用的回退值；
// 任一公共代理随时可能限流或下线，正式部署应通过环境变量固定可用地址。
const (
	DefaultGitHubProxy = "https://gh-proxy.org/"
	DefaultImageMirror = "ghcr.nju.edu.cn"
)

type commandRunner interface {
	Run(context.Context, string, []string, []string, io.Writer, io.Writer) error
}

type execRunner struct{ dir string }

func (r execRunner) Run(ctx context.Context, name string, args, environment []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = r.dir
	command.Env = append(os.Environ(), environment...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type persistedState struct {
	Sources         Sources      `json:"sources"`
	SourceProbe     *SourceProbe `json:"sourceProbe,omitempty"`
	LatestRelease   *Release     `json:"latestRelease,omitempty"`
	LastBackup      *Backup      `json:"lastBackup,omitempty"`
	RollbackVersion string       `json:"rollbackVersion,omitempty"`
	Operation       Operation    `json:"operation"`
}

type Manager struct {
	mu         sync.Mutex
	config     Config
	runner     commandRunner
	httpClient *http.Client
	state      persistedState
}

func NewManager(config Config) (*Manager, error) {
	config.Repository = strings.TrimSpace(config.Repository)
	if config.Repository == "" {
		config.Repository = DefaultRepository
	}
	if config.InstallDir == "" {
		config.InstallDir = "/opt/open-ai-canvas"
	}
	if config.ComposeFile == "" {
		config.ComposeFile = "docker-compose.deploy.yml"
	}
	if config.EnvFile == "" {
		config.EnvFile = ".env"
	}
	if config.StateDir == "" {
		config.StateDir = "/var/lib/open-ai-canvas-updater"
	}
	if config.BackupDir == "" {
		config.BackupDir = filepath.Join(config.InstallDir, "backups")
	}
	if config.GitHubProxy == "" {
		config.GitHubProxy = DefaultGitHubProxy
	}
	if config.ImageMirror == "" {
		config.ImageMirror = DefaultImageMirror
	}
	config.GitHubProxy = normalizeProxyPrefix(config.GitHubProxy)
	config.ImageMirror = normalizeImageHost(config.ImageMirror)
	if err := validateProxyPrefix(config.GitHubProxy); err != nil {
		return nil, err
	}
	if err := validateImageHost(config.ImageMirror); err != nil {
		return nil, err
	}
	if config.StableWindow <= 0 {
		config.StableWindow = 30 * time.Second
	}
	if config.StepTimeout <= 0 {
		config.StepTimeout = 20 * time.Minute
	}
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建更新器状态目录：%w", err)
	}
	if err := os.MkdirAll(config.BackupDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建备份目录：%w", err)
	}
	manager := &Manager{
		config: config,
		runner: execRunner{dir: config.InstallDir},
		// Client.Timeout 会连 body 读取一起计时，而 Release 二进制可达数 MB；
		// 总时长交给每个调用方的 context 控制，这里只限制建立连接与等待响应头。
		// 该网络下 HTTP/2 会在等待响应头时反复超时，实测 HTTP/1.1 明显更稳，
		// 因此关闭 HTTP/2 并禁用空闲连接复用，让每次尝试都走全新 TCP。
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 25 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
			ForceAttemptHTTP2:     false,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		}},
		state: persistedState{Operation: Operation{Phase: PhaseIdle, Logs: []LogEntry{}}},
	}
	if err := manager.loadState(); err != nil {
		return nil, err
	}
	if manager.state.Sources == (Sources{}) {
		manager.state.Sources = Sources{GitHubSource: "direct", ImageSource: "ghcr"}
	}
	if err := manager.state.Sources.Validate(); err != nil {
		return nil, fmt.Errorf("读取更新来源配置：%w", err)
	}
	if probe := manager.state.SourceProbe; probe != nil && probe.Status == "running" {
		now := time.Now().UTC()
		probe.Status, probe.FinishedAt = "failed", &now
		for i := range probe.Checks {
			if probe.Checks[i].Status == "pending" {
				probe.Checks[i].Status, probe.Checks[i].Detail = "failed", "更新器已重启，请重新检测"
			}
		}
		if err := manager.saveStateLocked(); err != nil {
			return nil, err
		}
	}
	if manager.state.Operation.Phase == PhaseChecking {
		manager.state.Operation.Phase = PhaseIdle
		manager.state.LatestRelease = nil
		manager.appendLogLocked(PhaseIdle, "更新器已重启，请重新检查版本")
		_ = manager.saveStateLocked()
	} else if manager.state.Operation.Phase.Active() {
		manager.state.Operation.Phase = PhaseManualIntervention
		manager.state.Operation.Error = "更新器进程在操作期间退出，请检查容器与数据库状态后手动处理"
		manager.appendLogLocked(PhaseManualIntervention, manager.state.Operation.Error)
		_ = manager.saveStateLocked()
	}
	return manager, nil
}

func (m *Manager) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Manager) Check(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.busyLocked() {
		status := m.snapshotLocked()
		m.mu.Unlock()
		return status, ErrBusy
	}
	sources := m.state.Sources
	previous := m.state
	m.state.Operation.Phase = PhaseChecking
	m.state.Operation.Error = ""
	m.state.LatestRelease = nil
	m.appendLogLocked(PhaseChecking, "正在读取 GitHub Release："+sources.descriptionFor(m))
	if err := m.saveStateLocked(); err != nil {
		m.state = previous
		status := m.snapshotLocked()
		m.mu.Unlock()
		return status, fmt.Errorf("保存版本检查任务：%w", err)
	}
	m.mu.Unlock()

	release, err := m.latestRelease(ctx, sources)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.state.Operation.Phase = PhaseFailed
		m.state.Operation.Error = err.Error()
		m.appendLogLocked(PhaseFailed, "检查更新失败")
		_ = m.saveStateLocked()
		return m.snapshotLocked(), err
	}
	m.state.LatestRelease = release
	current, currentErr := m.currentVersion()
	if currentErr != nil {
		m.state.Operation.Phase = PhaseFailed
		m.state.Operation.Error = currentErr.Error()
		_ = m.saveStateLocked()
		return m.snapshotLocked(), currentErr
	}
	if CompareVersions(current, release.Version) < 0 {
		m.state.Operation.Phase = PhaseReady
		if IsReleaseVersion(current) {
			m.appendLogLocked(PhaseReady, fmt.Sprintf("发现新版本 %s", release.Version))
		} else {
			m.appendLogLocked(PhaseReady, fmt.Sprintf("当前部署为 %s，可更新到正式版 %s", current, release.Version))
		}
	} else {
		m.state.Operation.Phase = PhaseNoUpdate
		m.appendLogLocked(PhaseNoUpdate, "当前已是最新版本")
	}
	_ = m.saveStateLocked()
	return m.snapshotLocked(), nil
}

func (m *Manager) StartUpdate(targetVersion string) (Status, error) {
	targetVersion = strings.TrimSpace(targetVersion)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busyLocked() {
		return m.snapshotLocked(), ErrBusy
	}
	if m.state.LatestRelease == nil || m.state.LatestRelease.Version != targetVersion {
		return m.snapshotLocked(), errors.New("目标版本与最近一次检查结果不一致，请重新检查更新")
	}
	current, err := m.currentVersion()
	if err != nil {
		return m.snapshotLocked(), err
	}
	if CompareVersions(current, targetVersion) >= 0 {
		return m.snapshotLocked(), errors.New("目标版本必须高于当前版本")
	}
	now := time.Now().UTC()
	sources := m.state.Sources
	previous := m.state.Operation
	m.state.Operation = Operation{
		Sources:       &sources,
		ID:            randomID(),
		Phase:         PhasePreflight,
		FromVersion:   current,
		TargetVersion: targetVersion,
		StartedAt:     &now,
		Logs:          []LogEntry{{At: now, Phase: PhasePreflight, Message: "开始更新前检查，来源：" + sources.descriptionFor(m)}},
	}
	if err := m.saveStateLocked(); err != nil {
		m.state.Operation = previous
		return m.snapshotLocked(), fmt.Errorf("保存更新任务：%w", err)
	}
	go m.runUpdate(current, targetVersion, sources)
	return m.snapshotLocked(), nil
}

func (m *Manager) StartRollback(reason string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busyLocked() {
		return m.snapshotLocked(), ErrBusy
	}
	if m.state.RollbackVersion == "" || m.state.LastBackup == nil {
		return m.snapshotLocked(), errors.New("没有可用的回退版本或已校验备份")
	}
	current, err := m.currentVersion()
	if err != nil {
		return m.snapshotLocked(), err
	}
	now := time.Now().UTC()
	target := m.state.RollbackVersion
	m.state.Operation = Operation{
		ID:            randomID(),
		Phase:         PhaseRollingBack,
		FromVersion:   current,
		TargetVersion: target,
		StartedAt:     &now,
		Logs:          []LogEntry{{At: now, Phase: PhaseRollingBack, Message: "管理员发起人工回退：" + strings.TrimSpace(reason)}},
	}
	_ = m.saveStateLocked()
	backup := *m.state.LastBackup
	go m.runRollback(target, backup, false)
	return m.snapshotLocked(), nil
}

func (m *Manager) runUpdate(fromVersion, targetVersion string, sources Sources) {
	nextCompose, err := m.prepareTargetCompose(targetVersion, sources)
	if err != nil {
		m.failWithoutRollback(PhaseFailed, err)
		return
	}
	defer os.Remove(nextCompose)
	if err := m.preflight(nextCompose, targetVersion, sources); err != nil {
		m.failWithoutRollback(PhaseFailed, err)
		return
	}
	updaterBinary, err := m.prepareUpdaterBinary(targetVersion, sources)
	if err != nil {
		m.failWithoutRollback(PhaseFailed, err)
		return
	}
	if updaterBinary != "" {
		defer os.Remove(updaterBinary)
	}

	m.setPhase(PhasePulling, "拉取目标版本镜像："+sources.descriptionFor(m))
	targetImages := sources.imagesWithin(m, m.config.Repository, targetVersion)
	pulledImages, err := m.pullTargetImages(nextCompose, targetVersion, targetImages, sources)
	if err != nil {
		m.failWithoutRollback(PhaseFailed, err)
		return
	}
	targetImages, err = m.verifyImages(pulledImages)
	if err != nil {
		m.failWithoutRollback(PhaseFailed, err)
		return
	}

	m.setPhase(PhaseDraining, "停止 Web、Backend 与 Agent，等待后台任务优雅退出")
	if err := m.compose(m.composePath(), fromVersion, m.config.StepTimeout, nil, "stop", "web", "backend", "yingce-agent"); err != nil {
		m.failBeforeMigration(err, fromVersion)
		return
	}

	// 最终备份必须包含旧服务退出前的全部写入；镜像下载不占用停服窗口。
	m.setPhase(PhaseBackingUp, "创建停服后的 PostgreSQL 与数据目录 ZIP 备份")
	backup, err := m.createBackup(fromVersion)
	if err != nil {
		m.failBeforeMigration(err, fromVersion)
		return
	}
	if err := replaceFile(m.composePath(), m.previousComposePath(), 0o600); err != nil {
		m.failBeforeMigration(fmt.Errorf("保存旧 Compose 配置：%w", err), fromVersion)
		return
	}
	if err := replaceFile(m.envPath(), m.previousEnvPath(), 0o600); err != nil {
		m.failBeforeMigration(fmt.Errorf("保存旧部署环境：%w", err), fromVersion)
		return
	}
	m.mu.Lock()
	m.state.LastBackup = &backup
	m.state.RollbackVersion = fromVersion
	_ = m.saveStateLocked()
	m.mu.Unlock()

	m.setPhase(PhaseMigrating, "执行目标版本数据库迁移")
	if err := m.composeWithImages(nextCompose, targetVersion, targetImages, m.config.StepTimeout, nil, "run", "--rm", "migrate"); err != nil {
		m.failWithRollback(err, fromVersion, backup)
		return
	}

	m.setPhase(PhaseSwitching, "切换 Compose 配置与镜像版本")
	if err := setDeploymentImages(m.envPath(), targetVersion, targetImages); err != nil {
		m.failWithRollback(err, fromVersion, backup)
		return
	}
	if err := replaceFile(nextCompose, m.composePath(), 0o600); err != nil {
		_ = setEnvValue(m.envPath(), "CANVAS_IMAGE_TAG", strings.TrimPrefix(fromVersion, "v"))
		m.failWithRollback(err, fromVersion, backup)
		return
	}
	if err := m.composeWithImages(m.composePath(), targetVersion, targetImages, m.config.StepTimeout, nil, "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", "600"); err != nil {
		m.failWithRollback(err, fromVersion, backup)
		return
	}

	m.setPhase(PhaseVerifying, "验证健康状态、运行版本和稳定窗口")
	if err := m.verifyHealthy(targetVersion); err != nil {
		m.failWithRollback(err, fromVersion, backup)
		return
	}
	if updaterBinary != "" {
		if err := replaceFile(updaterBinary, m.config.BinaryPath, 0o755); err != nil {
			m.mu.Lock()
			now := time.Now().UTC()
			m.state.Operation.Phase = PhaseManualIntervention
			m.state.Operation.FinishedAt = &now
			m.state.Operation.Error = "应用已更新，但 Host Updater 自更新失败：" + safeOperationError(err)
			m.appendLogLocked(PhaseManualIntervention, "应用版本已切换，更新器二进制需要人工更新")
			_ = m.saveStateLocked()
			m.mu.Unlock()
			return
		}
	}
	m.mu.Lock()
	now := time.Now().UTC()
	m.state.Operation.Phase = PhaseSucceeded
	m.state.Operation.FinishedAt = &now
	m.state.Operation.Error = ""
	m.appendLogLocked(PhaseSucceeded, fmt.Sprintf("已成功更新到 %s", targetVersion))
	if updaterBinary != "" {
		m.appendLogLocked(PhaseSucceeded, "Host Updater 二进制已同步到目标 Release")
	}
	_ = m.saveStateLocked()
	m.mu.Unlock()
	if updaterBinary != "" && m.config.ServiceName != "" {
		go m.restartSelf()
	}
}

// pullTargetImages 按「所选镜像源 → 官方 ghcr.io」逐镜像拉取目标版本，
// 返回实际成功拉取的镜像引用（允许混合来源）。
//
// 为什么要逐镜像拉取并单独计时：StepTimeout 原先对整个 `docker compose pull`
// （三个镜像约 467 MB）生效。公共镜像源常在超大层上掉到 100~200 KB/s，按此速率
// 需要 40 分钟以上，必然撞上 20 分钟上限——这不是抖动，是算不出来。
//
// 为什么可以换源：各源的多架构 index digest 完全一致，同一 tag 对应同一份内容。
// 因此某镜像在所选源拉不动时，改从 ghcr.io 拉同一 digest 不会改变镜像内容；
// 之后 verifyImages 仍会校验本地仓库摘要。
func (m *Manager) pullTargetImages(composePath, version string, images deploymentImages, sources Sources) (deploymentImages, error) {
	chosen := []string{images.backend, images.web, images.agent}
	names := []string{"backend", "web", "agent"}

	// 所选来源不是 ghcr.io 时，官方直连作为同一 digest 的兜底来源。
	var fallbackRefs []string
	if sources.ImageSource != "ghcr" {
		direct := immutableImageRefs(m.config.Repository, version)
		fallbackRefs = []string{direct.backend, direct.web, direct.agent}
	}

	for index := range chosen {
		err := m.pullImage(chosen[index])
		if err == nil {
			continue
		}
		if len(fallbackRefs) == 0 || fallbackRefs[index] == chosen[index] {
			return deploymentImages{}, err
		}
		// 鉴权、404、架构不符属于确定性失败，换源也救不回来，直接报错。
		if !retryableImagePull(err) {
			return deploymentImages{}, err
		}
		m.appendLog(PhasePulling, fmt.Sprintf("%s 镜像在所选源拉取失败，改用 ghcr.io 同一 digest：%s", names[index], firstLine(err.Error())))
		// 先清掉所选源可能留下的半截引用，避免同一 tag 下内容混杂。
		m.removeTargetImages(deploymentImages{}.withAt(index, chosen[index]))
		if fallbackErr := m.pullImage(fallbackRefs[index]); fallbackErr != nil {
			return deploymentImages{}, fmt.Errorf("%s 镜像：所选源失败（%s）；ghcr.io 兜底也失败（%s）",
				names[index], firstLine(err.Error()), firstLine(fallbackErr.Error()))
		}
		chosen[index] = fallbackRefs[index]
	}

	result := deploymentImages{backend: chosen[0], web: chosen[1], agent: chosen[2]}
	if result.backend != images.backend || result.web != images.web || result.agent != images.agent {
		m.appendLog(PhasePulling, "部分镜像已改用 ghcr.io 获取，部署将使用实际拉取的引用")
	}
	return result, nil
}

// pullImage 拉取单个镜像并单独计时，预算由 ImagePullTimeout 控制。
func (m *Manager) pullImage(ref string) error {
	timeout := m.config.ImagePullTimeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stderr bytes.Buffer
	if err := m.runner.Run(ctx, "docker", []string{"pull", ref}, nil, io.Discard, &stderr); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[len(message)-1000:]
		}
		if message != "" {
			return fmt.Errorf("docker pull %s：%s", ref, explainDockerFailure(message))
		}
		return fmt.Errorf("docker pull %s：%w", ref, err)
	}
	return nil
}

// withAt 构造一个只在指定位置有值的镜像集合，供按需清理单个标签使用。
func (d deploymentImages) withAt(index int, ref string) deploymentImages {
	switch index {
	case 0:
		d.backend = ref
	case 1:
		d.web = ref
	case 2:
		d.agent = ref
	}
	return d
}

// retryableImagePull 只匹配可恢复的层写入/传输问题，避免对确定性失败重复拉取。
func retryableImagePull(err error) bool {
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unexpected commit digest",
		"failed precondition",
		"failed commit on ref",
		"unexpected eof",
		"connection reset",
		"i/o timeout",
		"tls handshake timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// removeTargetImages 只删除本次目标标签的本地引用，保留旧版本镜像以供回退。
func (m *Manager) removeTargetImages(images deploymentImages) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, image := range []string{images.backend, images.web, images.agent} {
		if strings.TrimSpace(image) == "" {
			continue
		}
		_ = m.runner.Run(ctx, "docker", []string{"image", "rm", "--force", image}, nil, io.Discard, io.Discard)
	}
}

func firstLine(message string) string {
	trimmed := strings.TrimSpace(message)
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		return strings.TrimSpace(trimmed[:index])
	}
	return trimmed
}

func (m *Manager) appendLog(phase Phase, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendLogLocked(phase, message)
	_ = m.saveStateLocked()
}

func (m *Manager) failWithoutRollback(phase Phase, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	m.state.Operation.Phase = phase
	m.state.Operation.Error = safeOperationError(err)
	m.state.Operation.FinishedAt = &now
	m.appendLogLocked(phase, "更新已中止，尚未修改运行版本")
	_ = m.saveStateLocked()
}

// 迁移前只有容器被停止，恢复旧容器即可，不能恢复更早的数据库快照。
func (m *Manager) failBeforeMigration(cause error, version string) {
	err := m.compose(m.composePath(), version, m.config.StepTimeout, nil, "start", "backend", "yingce-agent", "web")
	if err == nil {
		err = m.verifyHealthy(version)
	}
	if err != nil {
		m.failWithoutRollback(PhaseManualIntervention, errors.Join(cause, fmt.Errorf("恢复旧服务：%w", err)))
		return
	}
	m.failWithoutRollback(PhaseFailed, cause)
}

func (m *Manager) failWithRollback(cause error, targetVersion string, backup Backup) {
	m.mu.Lock()
	m.state.Operation.Error = safeOperationError(cause)
	m.state.Operation.AutomaticRollback = true
	m.state.Operation.Phase = PhaseRollingBack
	m.appendLogLocked(PhaseRollingBack, "更新失败，正在自动恢复旧版本和数据库备份")
	_ = m.saveStateLocked()
	m.mu.Unlock()
	m.runRollback(targetVersion, backup, true)
}

func (m *Manager) runRollback(targetVersion string, backup Backup, automatic bool) {
	var failures []error
	if err := m.compose(m.composePath(), targetVersion, 5*time.Minute, nil, "stop", "web", "backend"); err != nil {
		failures = append(failures, err)
	}
	if _, err := os.Stat(m.previousComposePath()); err == nil {
		if err := replaceFile(m.previousComposePath(), m.composePath(), 0o600); err != nil {
			failures = append(failures, fmt.Errorf("恢复旧 Compose 配置：%w", err))
		}
	}
	if _, err := os.Stat(m.previousEnvPath()); err == nil {
		if err := replaceFile(m.previousEnvPath(), m.envPath(), 0o600); err != nil {
			failures = append(failures, fmt.Errorf("恢复旧部署环境：%w", err))
		}
	}
	if err := m.restoreDatabase(backup); err != nil {
		failures = append(failures, err)
	}
	started := true
	if err := m.compose(m.composePath(), targetVersion, m.config.StepTimeout, nil, "up", "-d", "--pull", "never", "--remove-orphans", "--wait", "--wait-timeout", "600"); err != nil {
		started = false
		failures = append(failures, err)
	}
	if started {
		if err := m.verifyHealthy(targetVersion); err != nil {
			failures = append(failures, err)
		}
	}
	rollbackErr := errors.Join(failures...)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	m.state.Operation.FinishedAt = &now
	m.state.Operation.AutomaticRollback = automatic
	if rollbackErr != nil {
		m.state.Operation.Phase = PhaseManualIntervention
		m.state.Operation.RollbackError = safeOperationError(rollbackErr)
		m.appendLogLocked(PhaseManualIntervention, "自动回退未能完成，需要人工介入")
	} else {
		m.state.Operation.Phase = PhaseRolledBack
		m.state.Operation.RollbackError = ""
		m.appendLogLocked(PhaseRolledBack, fmt.Sprintf("已恢复到 %s", targetVersion))
	}
	_ = m.saveStateLocked()
}

func (m *Manager) snapshotLocked() Status {
	current, err := m.currentVersion()
	checks := m.checks(current, err)
	status := Status{
		Sources:         cloneSources(&m.state.Sources),
		SourceProbe:     cloneSourceProbe(m.state.SourceProbe),
		Supported:       true,
		Connected:       true,
		Repository:      m.config.Repository,
		Deployment:      "docker-compose-host-updater",
		GitHubProxy:     m.config.GitHubProxy,
		ImageMirror:     m.config.ImageMirror,
		CurrentVersion:  current,
		LatestRelease:   cloneRelease(m.state.LatestRelease),
		Checks:          checks,
		LastBackup:      cloneBackup(m.state.LastBackup),
		RollbackVersion: m.state.RollbackVersion,
		Operation:       cloneOperation(m.state.Operation),
	}
	if status.LatestRelease != nil && err == nil {
		status.UpdateAvailable = CompareVersions(current, status.LatestRelease.Version) < 0
	}
	return status
}

func (m *Manager) checks(current string, currentErr error) []Check {
	items := []Check{
		{Key: "updater", Label: "Host Updater", Status: "passed", Detail: runtime.GOOS + "/" + runtime.GOARCH, Blocking: true},
		{Key: "version", Label: "当前版本", Status: "passed", Detail: current, Blocking: true},
		{Key: "backup", Label: "数据库备份 ZIP", Status: "pending", Detail: "开始更新后自动创建并校验", Blocking: true},
		{Key: "images", Label: "目标镜像", Status: "pending", Detail: "拉取后校验仓库摘要", Blocking: true},
		{Key: "migration", Label: "数据库迁移", Status: "pending", Detail: "在旧服务完全停止后执行", Blocking: true},
	}
	if currentErr != nil {
		items[1].Status = "failed"
		items[1].Detail = currentErr.Error()
	} else if current != "" && !IsReleaseVersion(current) {
		items[1].Detail = current + " · 非正式镜像标签，可切换到正式 Release"
	}
	if m.state.LastBackup != nil {
		items[2].Status = "passed"
		items[2].Detail = m.state.LastBackup.ID + " · " + m.state.LastBackup.Checksum
	}
	return items
}

func (m *Manager) setPhase(phase Phase, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Operation.Phase = phase
	m.appendLogLocked(phase, message)
	_ = m.saveStateLocked()
}

func (m *Manager) appendLogLocked(phase Phase, message string) {
	m.state.Operation.Logs = append(m.state.Operation.Logs, LogEntry{At: time.Now().UTC(), Phase: phase, Message: message})
	if len(m.state.Operation.Logs) > 200 {
		m.state.Operation.Logs = append([]LogEntry(nil), m.state.Operation.Logs[len(m.state.Operation.Logs)-200:]...)
	}
}

func (m *Manager) loadState() error {
	data, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取更新器状态：%w", err)
	}
	if err := json.Unmarshal(data, &m.state); err != nil {
		return fmt.Errorf("解析更新器状态：%w", err)
	}
	if m.state.Operation.Phase == "" {
		m.state.Operation.Phase = PhaseIdle
	}
	return nil
}

func (m *Manager) saveStateLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.config.StateDir, ".state-*.json")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, m.statePath())
}

func (m *Manager) statePath() string { return filepath.Join(m.config.StateDir, "state.json") }
func (m *Manager) composePath() string {
	return filepath.Join(m.config.InstallDir, m.config.ComposeFile)
}
func (m *Manager) envPath() string { return filepath.Join(m.config.InstallDir, m.config.EnvFile) }
func (m *Manager) previousComposePath() string {
	return filepath.Join(m.config.StateDir, "previous-compose.yml")
}
func (m *Manager) previousEnvPath() string {
	return filepath.Join(m.config.StateDir, "previous.env")
}

func randomID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

func safeOperationError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1200 {
		message = message[:1200]
	}
	return message
}

func cloneRelease(value *Release) *Release {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneBackup(value *Backup) *Backup {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Path = ""
	return &cloned
}

func cloneOperation(value Operation) Operation {
	value.Sources = cloneSources(value.Sources)
	value.Logs = append([]LogEntry(nil), value.Logs...)
	return value
}

func cloneSources(value *Sources) *Sources {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneSourceProbe(value *SourceProbe) *SourceProbe {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Checks = append([]Check(nil), value.Checks...)
	return &cloned
}
