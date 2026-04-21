package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LocalSandbox 没有使用 Linux Namespace 或 Control Groups (cgroups) 进行硬隔离，而是采用了应用级策略来降低风险。
//	- 白名单机制：只允许特定的解释器和路径。
//	- 资源限制：通过 context.WithTimeout 强制执行超时。
//	- 环境净化：剔除可能导致提权或注入的危险环境变量。
//
// 执行流程 (Execute)
//	- 预校验：检查脚本路径是否合法，并根据后缀名匹配解释器（如 .py 对应 python3）。
//	- 超时控制：使用 context.WithTimeout。如果执行超时，代码会通过 syscall.Kill 杀掉整个进程组（防止产生僵尸子进程）。
//	- 系统属性：设置 Setpgid: true，确保脚本及其派生的所有子进程都在同一个进程组内，方便统一清理。
//	- IO 捕获：使用 bytes.Buffer 捕获 stdout 和 stderr，以便返回给调用者。
//
// 软隔离实现逻辑
//	- 路径校验 (validateScript)：
//		必须是绝对路径。
//		如果配置了 AllowedPaths，脚本必须位于这些允许的目录之下（防止访问系统敏感文件如 /etc/passwd）。
//	- 解释器过滤 (isAllowedCommand)：
//		只有在白名单内的程序（如 bash, python3, node）才能运行。
//		防止用户通过脚本执行任意系统二进制文件。
//	- 环境净化 (buildEnvironment)：
//		硬编码了一个最小化的 $PATH 。
//		排除危险变量：重点拦截了 LD_PRELOAD（动态库预加载劫持）和 PYTHONPATH 等可能改变程序运行行为的环境变量。
//
// 优点：
// 	- 轻量级：无需 Docker 依赖，启动速度极快。
// 	- 清理彻底：使用了进程组信号 (-Pid)，能有效处理挂起的子进程。
// 	- 自动推断：支持根据后缀自动选择 .js, .py, .rb 等解释器。
//
// 缺点：
// 	- 隔离性弱：无法限制 CPU 和内存使用率，容易被 Fork 炸弹或内存溢出攻击。
// 	- 权限依赖：脚本运行权限等同于宿主机 Go 程序的权限，存在越权风险。
// 	- 竞态条件：os.Stat 到 exec.Command 之间存在文件被替换的理论风险 (TOCTOU)。
//
// 改进建议：如果需要更强的隔离，应考虑
// 	- 使用 Linux Namespaces（unshare, CLONE_NEWNS）
// 	- 使用 seccomp 限制系统调用
// 	- 使用 cgroups 限制资源
// 	- 考虑 Docker 作为主要沙箱，LocalSandbox 仅用于测试环境
//
// 使用示例
//
//	config := &Config{
//    	DefaultTimeout: 30 * time.Second,
//    	AllowedCommands: []string{"python3", "bash"},
//    	AllowedPaths:    []string{"/home/sandbox/"},
//	}
//
//	sandbox := NewLocalSandbox(config)
//
//	result, err := sandbox.Execute(ctx, &ExecuteConfig{
//    	Script:  "/home/sandbox/test.py",
//    	Args:    []string{"--verbose"},
//    	Stdin:   "input data",
//   	Timeout: 10 * time.Second,
//	})
//
//	fmt.Println(result.Stdout)
//	fmt.Println(result.ExitCode)

// LocalSandbox implements the Sandbox interface using local process isolation
// This is a fallback option when Docker is not available
// It provides basic isolation through:
// - Command whitelist validation
// - Working directory restriction
// - Timeout enforcement
// - Environment variable filtering
type LocalSandbox struct {
	config *Config
}

// NewLocalSandbox creates a new local process-based sandbox
func NewLocalSandbox(config *Config) *LocalSandbox {
	if config == nil {
		config = DefaultConfig()
	}
	return &LocalSandbox{
		config: config,
	}
}

// Type returns the sandbox type
func (s *LocalSandbox) Type() SandboxType {
	return SandboxTypeLocal
}

// IsAvailable checks if local sandbox is available
func (s *LocalSandbox) IsAvailable(ctx context.Context) bool {
	// Local sandbox is always available
	return true
}

// Execute 在本地进程中运行脚本，提供基本的隔离保护
//
// 功能说明：
//   1. 验证脚本路径的合法性（存在性、白名单、绝对路径等）
//   2. 根据文件扩展名自动选择对应的解释器（python3/bash/node等）
//   3. 对解释器命令进行白名单校验
//   4. 设置执行超时时间（防止脚本无限运行）
//   5. 配置独立的工作目录和环境变量
//   6. 将脚本及其所有子进程放入独立的进程组
//   7. 超时时强制终止整个进程树
//
// 安全机制：
//   - 路径白名单限制（只能执行指定目录下的脚本）
//   - 命令白名单限制（只允许预定义的解释器）
//   - 环境变量过滤（移除 LD_PRELOAD 等危险变量）
//   - 进程组隔离（超时后彻底清理）
//   - 最小权限环境（有限的 PATH 和 HOME）
//
// 参数：
//   ctx - 上下文对象，用于超时控制和取消执行
//   config - 执行配置，包含脚本路径、参数、环境变量等
//
// 返回值：
//   *ExecuteResult - 执行结果，包含标准输出、标准错误、退出码、执行时长等
//   error - 执行过程中的错误（路径无效、命令不允许、超时等）
//
// 注意事项：
//   - 脚本路径必须使用绝对路径
//   - 该方法不能替代完整的容器级隔离，仅适用于可信环境
//   - 超时后会发送 SIGKILL 信号强制终止，脚本无法捕获该信号
//   - 不会对脚本参数进行清洗，调用方需自行确保参数安全
//
// 示例：
//   config := &ExecuteConfig{
//       Script:  "/home/sandbox/test.py",
//       Args:    []string{"--verbose"},
//       Timeout: 10 * time.Second,
//       Stdin:   "input data",
//   }
//   result, err := sandbox.Execute(ctx, config)
//   if err != nil {
//       log.Printf("执行失败: %v", err)
//   }
//   fmt.Printf("输出: %s\n退出码: %d\n", result.Stdout, result.ExitCode)

// Execute runs a script locally with basic isolation
func (s *LocalSandbox) Execute(ctx context.Context, config *ExecuteConfig) (*ExecuteResult, error) {
	if config == nil {
		return nil, ErrInvalidScript
	}

	// Validate the script path
	if err := s.validateScript(config.Script); err != nil {
		return nil, err
	}

	// Determine interpreter
	interpreter := s.getInterpreter(config.Script)
	if !s.isAllowedCommand(interpreter) {
		return nil, fmt.Errorf("interpreter not allowed: %s", interpreter)
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

	// Build command
	args := append([]string{config.Script}, config.Args...)
	cmd := exec.CommandContext(execCtx, interpreter, args...)

	// Set working directory
	if config.WorkDir != "" {
		cmd.Dir = config.WorkDir
	} else {
		cmd.Dir = filepath.Dir(config.Script)
	}

	// Setup minimal environment
	cmd.Env = s.buildEnvironment(config.Env)

	// Setup process group for cleanup
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if config.Stdin != "" {
		cmd.Stdin = strings.NewReader(config.Stdin)
	}

	startTime := time.Now()
	err := cmd.Run()
	duration := time.Since(startTime)

	result := &ExecuteResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			// Kill the process group
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
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

// validateScript 验证脚本路径的合法性和安全性
//
// 检查项包括：
//   1. 脚本文件是否存在且可访问
//   2. 路径指向的是文件而非目录
//   3. 路径必须是绝对路径（防止路径遍历攻击）
//   4. 路径必须在配置的白名单目录内（如果配置了白名单）
//
// 安全说明：
//   - 使用 filepath.Abs 规范化路径后再进行白名单匹配
//   - 采用前缀匹配方式，允许子目录下的脚本
//   - 白名单为空时跳过路径检查（不推荐用于生产环境）
//
// 已知限制：
//   - 不解析符号链接，可能被软链接绕过
//   - 存在 TOCTOU 竞态条件（检查与执行之间的时间窗口）
//   - 路径规范化不彻底，某些边界情况可能绕过检查
//
// 参数：
//   scriptPath - 待验证的脚本路径（必须是绝对路径）
//
// 返回值：
//   error - 验证失败时返回具体错误（路径不存在、不在白名单等）

// validateScript checks if the script path is valid and safe
func (s *LocalSandbox) validateScript(scriptPath string) error {
	// Check if script exists
	// 使用 os.Stat 获取文件元数据，区分 "文件不存在" 和 "无法访问" 两种情况
	info, err := os.Stat(scriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrScriptNotFound
		}
		return fmt.Errorf("failed to access script: %w", err)
	}

	// 确保执行的是文件，防止意外执行目录
	if info.IsDir() {
		return ErrInvalidScript
	}

	// Check path is absolute
	// 必须绝对路径
	//	- 防止路径遍历攻击（如 ../../etc/passwd）
	//	- 避免工作目录带来的歧义
	//	- 配合后续的路径白名单检查
	if !filepath.IsAbs(scriptPath) {
		return fmt.Errorf("script path must be absolute: %s", scriptPath)
	}

	// Validate against allowed paths if configured
	// 路径白名单验证（前缀匹配）
	if len(s.config.AllowedPaths) > 0 {
		allowed := false
		absPath, _ := filepath.Abs(scriptPath)
		for _, allowedPath := range s.config.AllowedPaths {
			absAllowed, _ := filepath.Abs(allowedPath)
			if strings.HasPrefix(absPath, absAllowed) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("script path not in allowed paths: %s", scriptPath)
		}
	}

	return nil
}

// getInterpreter returns the appropriate interpreter for a script
func (s *LocalSandbox) getInterpreter(scriptPath string) string {
	ext := strings.ToLower(filepath.Ext(scriptPath))
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
	case ".php":
		return "php"
	default:
		return "sh"
	}
}

// isAllowedCommand checks if a command is in the allowed list
func (s *LocalSandbox) isAllowedCommand(cmd string) bool {
	if len(s.config.AllowedCommands) == 0 {
		// Use default allowed commands
		defaults := defaultAllowedCommands()
		for _, allowed := range defaults {
			if cmd == allowed {
				return true
			}
		}
		return false
	}

	for _, allowed := range s.config.AllowedCommands {
		if cmd == allowed {
			return true
		}
	}
	return false
}

// buildEnvironment 构建安全的最小化环境变量集合
//
// 设计原则：
//   1. 默认拒绝：从零开始构建环境，不继承父进程的任何变量
//   2. 最小权限：只提供脚本运行所必需的最基础环境
//   3. 危险过滤：主动移除可能被用于攻击的环境变量
//
// 默认环境变量：
//   - PATH=/usr/local/bin:/usr/bin:/bin（有限的命令搜索路径）
//   - HOME=/tmp（临时家目录，避免访问用户文件）
//   - LANG/LC_ALL=en_US.UTF-8（固定语言环境）
//
// 危险变量黑名单：
//   - LD_PRELOAD/LD_LIBRARY_PATH：动态链接劫持
//   - PYTHONPATH/NODE_OPTIONS：语言级代码注入
//   - BASH_ENV/ENV：Shell 自动执行文件
//   - SHELL：可能影响子进程行为
//
// 参数：
//   extra - 用户提供的环境变量（会被过滤后添加）
//
// 返回值：
//   []string - 格式化后的环境变量列表（格式：KEY=value）
//
// 注意事项：
//   - 危险变量会被静默丢弃（不报错，避免信息泄露）
//   - 变量名大小写不敏感（LD_PRELOAD 和 ld_preload 都会被过滤）
//   - 不会对变量值进行验证或转义

// 危险环境变量
//	#变量#				#风险说明#							#攻击示例#
//	LD_PRELOAD			强制预加载共享库，可劫持系统调用			LD_PRELOAD=/tmp/malicious.so ./script 可拦截 open()、exec() 等
//	LD_LIBRARY_PATH		修改动态链接器搜索路径					加载恶意库替换标准库函数
//	PYTHONPATH			Python 模块导入路径					PYTHONPATH=/tmp/malicious python3 script.py 导入恶意模块
//	NODE_OPTIONS		Node.js 启动选项						NODE_OPTIONS="--require /tmp/malicious.js" 预加载恶意代码
//	BASH_ENV			Bash 启动时自动执行文件				在脚本运行前执行任意命令
//	ENV	 				POSIX shell 的自动执行文件			类似 BASH_ENV，影响 sh/ksh
//	SHELL				指定 shell 程序						可能影响子进程的 shell 行为

// buildEnvironment creates a safe environment for script execution
func (s *LocalSandbox) buildEnvironment(extra map[string]string) []string {
	// Start with minimal environment
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/tmp",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
	}

	// Dangerous environment variables to exclude
	dangerous := map[string]bool{
		"LD_PRELOAD":      true,
		"LD_LIBRARY_PATH": true,
		"PYTHONPATH":      true,
		"NODE_OPTIONS":    true,
		"BASH_ENV":        true,
		"ENV":             true,
		"SHELL":           true,
	}

	// Add extra environment variables (filtered)
	for key, value := range extra {
		upperKey := strings.ToUpper(key)
		if dangerous[upperKey] {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}

// Cleanup releases any resources
func (s *LocalSandbox) Cleanup(ctx context.Context) error {
	// Local sandbox doesn't need cleanup
	return nil
}
