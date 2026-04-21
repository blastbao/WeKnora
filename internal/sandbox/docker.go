package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DockerSandbox implements the Sandbox interface using Docker containers
type DockerSandbox struct {
	config *Config
}

// NewDockerSandbox creates a new Docker-based sandbox
func NewDockerSandbox(config *Config) *DockerSandbox {
	if config == nil {
		config = DefaultConfig()
	}
	if config.DockerImage == "" {
		config.DockerImage = DefaultDockerImage
	}
	return &DockerSandbox{
		config: config,
	}
}

// Type returns the sandbox type
func (s *DockerSandbox) Type() SandboxType {
	return SandboxTypeDocker
}

// IsAvailable 检查 Docker 是否可用
//
// 检查逻辑：
//   1. 执行 "docker version" 命令
//   2. 如果命令执行成功，说明 Docker 守护进程正在运行
//   3. 如果命令失败，返回 false
//
// 参数：
//   ctx - 上下文（支持超时控制）
//
// 返回值：
//   bool - true: Docker 可用; false: Docker 不可用
//
// 注意：
//   该函数只检查 Docker 命令是否可用，不检查镜像是否存在

// IsAvailable checks if Docker is available
func (s *DockerSandbox) IsAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "version")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// Execute 在 Docker 容器中运行脚本，提供高级别的资源隔离和安全防护。
//
// 该方法通过构建复杂的 `docker run` 参数来实现多维度安全限制：
//   - 资源隔离：强制限制 CPU 使用率、内存上限及禁用 Swap。
//   - 权限限制：以非 root 用户运行，剥夺所有 Capabilities，禁止权限提升。
//   - 文件系统：根文件系统只读，仅挂载必要的脚本目录并设为只读。
//   - 网络安全：默认禁用网络访问。
//   - 进程防护：设置 PID 限制，防止 Fork 炸弹攻击。
//
// 参数：
//   ctx - 生命周期上下文，用于控制整个 Docker 命令的执行时长。
//   config - 包含脚本内容、资源限制（CPU/内存）及环境配置。
//
// 返回：
//   *ExecuteResult - 包含容器输出、耗时及详细的退出状态。

// Execute runs a script in a Docker container
func (s *DockerSandbox) Execute(ctx context.Context, config *ExecuteConfig) (*ExecuteResult, error) {
	if config == nil {
		return nil, ErrInvalidScript
	}

	// Set default timeout
	timeout := config.Timeout
	if timeout == 0 {
		timeout = s.config.DefaultTimeout
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	// Create context with timeout
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build docker run command
	args := s.buildDockerArgs(config)

	startTime := time.Now()
	cmd := exec.CommandContext(execCtx, "docker", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if config.Stdin != "" {
		cmd.Stdin = strings.NewReader(config.Stdin)
	}

	err := cmd.Run()
	duration := time.Since(startTime)

	result := &ExecuteResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			result.Killed = true
			result.Error = ErrTimeout.Error()
			result.ExitCode = -1
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err.Error()
			result.ExitCode = -1
		}
	}

	return result, nil
}

// buildDockerArgs 构建 `docker run` 命令的参数列表，核心在于安全加固。
//
// 安全特性配置：
//   - --rm: 容器退出后立即自动清理，防止资源残留。
//   - --user: 指定 1000:1000 非特权用户运行。
//   - --cap-drop ALL: 移除所有内核能力，防止突破容器限制。
//   - --pids-limit: 限制进程总数，抵御恶意并发进程请求。
//   - --tmpfs: 使用内存临时文件系统替代磁盘写入，并设置 noexec 权限。
//   - --network none: 实现物理级的网络断开隔离。
//
// 示例输出：
//   ["run", "--rm", "--user", "1000:1000", "--cap-drop", "ALL",
//    "--memory", "536870912", "--cpus", "1.0", "--network", "none",
//    "-v", "/home/sandbox:/workspace:ro", "-w", "/workspace",
//    "sandbox:latest", "python3", "test.py", "--verbose"]
//
// docker run \
//  --rm \
//  --user 1000:1000 \
//  --cap-drop ALL \
//  --read-only \
//  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
//  --memory 536870912 \
//  --memory-swap 536870912 \
//  --cpus 1.0 \
//  --network none \
//  --pids-limit 100 \
//  --security-opt no-new-privileges \
//  -v /home/user/scripts:/workspace:ro \
//  -w /workspace \
//  -e DEBUG=true \
//  -e LOG_LEVEL=info \
//  sandbox:latest \
//  python3 test.py --arg1 value1

// buildDockerArgs constructs the docker run command arguments
func (s *DockerSandbox) buildDockerArgs(config *ExecuteConfig) []string {
	args := []string{"run", "--rm"}

	// Security: run as non-root user
	args = append(args, "--user", "1000:1000")

	// Security: drop all capabilities
	args = append(args, "--cap-drop", "ALL")

	// Security: read-only root filesystem (optional)
	if config.ReadOnlyRootfs {
		args = append(args, "--read-only")
		// Add writable tmp directory
		args = append(args, "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m")
	}

	// Resource limits
	memLimit := config.MemoryLimit
	if memLimit == 0 {
		memLimit = s.config.MaxMemory
	}
	if memLimit > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", memLimit))
		args = append(args, "--memory-swap", fmt.Sprintf("%d", memLimit)) // Disable swap
	}

	cpuLimit := config.CPULimit
	if cpuLimit == 0 {
		cpuLimit = s.config.MaxCPU
	}
	if cpuLimit > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.2f", cpuLimit))
	}

	// Network isolation
	if !config.AllowNetwork {
		args = append(args, "--network", "none")
	}

	// Security: disable privileged mode and limit PIDs
	args = append(args, "--pids-limit", "100")
	args = append(args, "--security-opt", "no-new-privileges")

	// Mount the script and working directory as read-only
	scriptDir := filepath.Dir(config.Script)
	args = append(args, "-v", fmt.Sprintf("%s:/workspace:ro", scriptDir))

	// Working directory
	args = append(args, "-w", "/workspace")

	// Environment variables
	for key, value := range config.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}

	// Image
	args = append(args, s.config.DockerImage)

	// Script execution command
	scriptName := filepath.Base(config.Script)
	interpreter := getInterpreter(scriptName)

	args = append(args, interpreter, scriptName)
	args = append(args, config.Args...)

	return args
}

// getInterpreter returns the appropriate interpreter for a script
func getInterpreter(scriptName string) string {
	ext := strings.ToLower(filepath.Ext(scriptName))
	switch ext {
	case ".py":
		return "python3"
	case ".sh", ".bash":
		return "bash"
	case ".js":
		return "node"
	case ".rb":
		return "ruby"
	case ".pl":
		return "perl"
	default:
		return "sh"
	}
}

// ImageExists checks if the configured Docker image exists locally
func (s *DockerSandbox) ImageExists(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", s.config.DockerImage)
	return cmd.Run() == nil
}

// EnsureImage 确保配置的 Docker 镜像在本地可用。
//
// 如果本地不存在该镜像，将尝试从远程仓库拉取（docker pull）。
// 该方法通常在系统启动阶段调用，以避免在脚本首次执行时因拉取镜像导致响应超时。

// EnsureImage pulls the Docker image if it doesn't exist locally.
// This is intended to be called during initialization so the image is
// ready before the first script execution.
func (s *DockerSandbox) EnsureImage(ctx context.Context) error {
	if s.ImageExists(ctx) {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "pull", s.config.DockerImage)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to pull image %s: %w (%s)", s.config.DockerImage, err, stderr.String())
	}
	return nil
}

// Cleanup removes any lingering resources
func (s *DockerSandbox) Cleanup(ctx context.Context) error {
	// Docker --rm flag should handle container cleanup
	// This is here for any additional cleanup if needed
	return nil
}
