package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

// ScriptValidator 脚本安全验证器
//
// 功能概述：
//   用于检测和阻止脚本中的恶意模式、危险命令和注入攻击
//   提供多层次的安全验证（脚本内容、命令行参数、标准输入）
//
// 验证能力：
//   1. 危险命令检测（rm -rf /、mkfs、fork炸弹等）
//   2. 危险模式匹配（base64编码载荷、下载执行等）
//   3. 参数注入检测（Shell操作符、命令替换等）
//   4. 网络访问检测（curl、wget、socket等）
//   5. 反弹Shell检测（/dev/tcp、bash -i等）
//   6. 标准输入注入检测
//
// 使用场景：
//   - 用户提交代码的安全预筛选
//   - CI/CD 流水线中的安全检查
//   - 作为沙箱系统的第一道防线
//   - 恶意脚本的静态分析
//
// 注意事项：
//   - 无法检测所有绕过技术（字符串拼接、变量混淆等）
//   - 可能存在误报，需要配合白名单机制
//   - 应与其他隔离技术（Docker、gVisor）配合使用
//   - 定期更新危险模式库以应对新型攻击

// ScriptValidator validates scripts and arguments for security
type ScriptValidator struct {
	// DangerousCommands are shell commands that should never be executed
	dangerousCommands []string // 危险命令列表（字符串匹配）
	// DangerousPatterns are regex patterns that indicate dangerous operations
	dangerousPatterns []*regexp.Regexp // 危险模式正则表达式
	// ArgPatterns are regex patterns to detect injection in arguments
	argInjectionPatterns []*regexp.Regexp // 参数注入模式正则
}

// ValidationError represents a security validation failure
type ValidationError struct {
	Type    string // 错误类型：dangerous_command、dangerous_pattern、arg_injection、shell_injection、reverse_shell、network_access等
	Pattern string // 触发验证失败的模式（命令字符串或正则表达式）
	Context string // 匹配时的上下文（前后20个字符）
	Message string // 人类可读的错误描述
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("security validation failed [%s]: %s (pattern: %s, context: %s)",
		e.Type, e.Message, e.Pattern, e.Context)
}

// ValidationResult contains all validation errors found
type ValidationResult struct {
	Valid  bool
	Errors []*ValidationError
}

// NewScriptValidator creates a new validator with default security rules
func NewScriptValidator() *ScriptValidator {
	v := &ScriptValidator{
		dangerousCommands: getDefaultDangerousCommands(),
	}
	v.dangerousPatterns = compilePatterns(getDefaultDangerousPatterns())
	v.argInjectionPatterns = compilePatterns(getDefaultArgInjectionPatterns())
	return v
}

// ValidateScript 验证脚本内容是否存在危险模式
//
// 检查项：
//   1. 危险命令（rm -rf /、mkfs、fork炸弹等）
//   2. 危险正则模式（base64解码、函数执行、eval调用等）
//   3. 网络访问尝试（curl、wget、socket连接等）
//   4. 反弹Shell模式（/dev/tcp、bash -i、pty.spawn等）

// ValidateScript validates script content for dangerous patterns
func (v *ScriptValidator) ValidateScript(content string) *ValidationResult {
	result := &ValidationResult{Valid: true, Errors: make([]*ValidationError, 0)}

	// Check for dangerous commands (use simple string matching for complex patterns)
	for _, cmd := range v.dangerousCommands {
		if strings.Contains(content, cmd) {
			result.Valid = false
			result.Errors = append(result.Errors, &ValidationError{
				Type:    "dangerous_command",
				Pattern: cmd,
				Context: extractContext(content, cmd),
				Message: fmt.Sprintf("Script contains dangerous command: %s", cmd),
			})
		}
	}

	// Check for dangerous patterns (case-insensitive matching is already in patterns)
	lowerContent := strings.ToLower(content)
	for _, pattern := range v.dangerousPatterns {
		if matches := pattern.FindString(lowerContent); matches != "" {
			result.Valid = false
			result.Errors = append(result.Errors, &ValidationError{
				Type:    "dangerous_pattern",
				Pattern: pattern.String(),
				Context: extractContext(content, matches),
				Message: fmt.Sprintf("Script contains dangerous pattern: %s", matches),
			})
		}
	}

	// Check for network access attempts
	if v.hasNetworkAccess(content) {
		result.Valid = false
		result.Errors = append(result.Errors, &ValidationError{
			Type:    "network_access",
			Pattern: "network commands",
			Context: "script content",
			Message: "Script attempts to access network resources",
		})
	}

	// Check for reverse shell patterns
	if v.hasReverseShellPattern(content) {
		result.Valid = false
		result.Errors = append(result.Errors, &ValidationError{
			Type:    "reverse_shell",
			Pattern: "reverse shell pattern",
			Context: "script content",
			Message: "Script contains potential reverse shell pattern",
		})
	}

	return result
}

// ValidateArgs 检查命令行参数，防止参数注入（Argument Injection）攻击。
//
// 它重点拦截以下字符及模式：
//   - Shell 操作符：分号 (;)、管道符 (|)、逻辑运算符 (&&/||) 等，防止命令拼接。
//   - 命令替换：反引号 (`) 和 $(...) 语法，防止在参数中嵌入子命令。
//   - 路径穿越：如 ../ 等试图访问授权范围外文件的行为。

// ValidateArgs validates command-line arguments for injection attempts
func (v *ScriptValidator) ValidateArgs(args []string) *ValidationResult {
	result := &ValidationResult{Valid: true, Errors: make([]*ValidationError, 0)}

	for i, arg := range args {
		// Check for command chaining operators
		if v.hasShellOperators(arg) {
			result.Valid = false
			result.Errors = append(result.Errors, &ValidationError{
				Type:    "shell_injection",
				Pattern: "shell operators",
				Context: fmt.Sprintf("arg[%d]: %s", i, truncate(arg, 50)),
				Message: "Argument contains shell command operators",
			})
		}

		// Check for backtick/subshell command execution
		if v.hasCommandSubstitution(arg) {
			result.Valid = false
			result.Errors = append(result.Errors, &ValidationError{
				Type:    "command_substitution",
				Pattern: "command substitution",
				Context: fmt.Sprintf("arg[%d]: %s", i, truncate(arg, 50)),
				Message: "Argument contains command substitution syntax",
			})
		}

		// Check for injection patterns
		for _, pattern := range v.argInjectionPatterns {
			if pattern.MatchString(arg) {
				result.Valid = false
				result.Errors = append(result.Errors, &ValidationError{
					Type:    "arg_injection",
					Pattern: pattern.String(),
					Context: fmt.Sprintf("arg[%d]: %s", i, truncate(arg, 50)),
					Message: "Argument matches injection pattern",
				})
			}
		}
	}

	return result
}

// ValidateStdin validates stdin content for injection attempts
func (v *ScriptValidator) ValidateStdin(stdin string) *ValidationResult {
	result := &ValidationResult{Valid: true, Errors: make([]*ValidationError, 0)}

	// Check for embedded shell commands
	if v.hasEmbeddedShellCommands(stdin) {
		result.Valid = false
		result.Errors = append(result.Errors, &ValidationError{
			Type:    "stdin_injection",
			Pattern: "embedded shell commands",
			Context: truncate(stdin, 100),
			Message: "Stdin contains embedded shell command patterns",
		})
	}

	return result
}

// ValidateAll 综合验证脚本、参数和标准输入
//
// 功能说明：
//   1. 调用 ValidateScript 验证脚本内容
//   2. 调用 ValidateArgs 验证命令行参数
//   3. 如果标准输入非空，调用 ValidateStdin 验证
//   4. 聚合所有验证错误
//
// 这是执行前的最终安全关卡，只有当所有部分都通过校验时，才允许进入沙箱运行。

// ValidateAll performs comprehensive validation on script, args, and stdin
func (v *ScriptValidator) ValidateAll(scriptContent string, args []string, stdin string) *ValidationResult {
	result := &ValidationResult{Valid: true, Errors: make([]*ValidationError, 0)}

	// Validate script content
	if scriptResult := v.ValidateScript(scriptContent); !scriptResult.Valid {
		result.Valid = false
		result.Errors = append(result.Errors, scriptResult.Errors...)
	}

	// Validate arguments
	if argsResult := v.ValidateArgs(args); !argsResult.Valid {
		result.Valid = false
		result.Errors = append(result.Errors, argsResult.Errors...)
	}

	// Validate stdin
	if stdin != "" {
		if stdinResult := v.ValidateStdin(stdin); !stdinResult.Valid {
			result.Valid = false
			result.Errors = append(result.Errors, stdinResult.Errors...)
		}
	}

	return result
}

// hasShellOperators 检测字符串是否包含Shell操作符
//
// 检测的操作符：
//   && - AND操作符
//   || - OR操作符
//   ;  - 命令分隔符
//   |  - 管道符
//   \n - 换行符（可用于命令注入）
//   \r - 回车符
//   $( - 命令替换
//   `  - 反引号命令替换
//   >  - 输出重定向
//   <  - 输入重定向
//   >> - 追加重定向
//   2> - 标准错误重定向
//   &> - 合并重定向
//
// 参数：
//   s - 待检测的字符串
//
// 返回值：
//   bool - true: 包含Shell操作符; false: 不包含

// hasShellOperators checks for shell command chaining operators
func (v *ScriptValidator) hasShellOperators(s string) bool {
	// Shell operators that could be used for command chaining
	operators := []string{
		"&&", // AND operator
		"||", // OR operator
		";",  // Command separator
		"|",  // Pipe
		"\n", // Newline (can be used to inject commands)
		"\r", // Carriage return
		"$(", // Command substitution
		"`",  // Backtick command substitution
		">",  // Output redirection
		"<",  // Input redirection
		">>", // Append redirection
		"2>", // Stderr redirection
		"&>", // Combined redirection
	}

	for _, op := range operators {
		if strings.Contains(s, op) {
			return true
		}
	}
	return false
}

// hasCommandSubstitution 检测命令替换模式
//
// 检测的模式：
//   $(command) - POSIX命令替换
//   `command`  - 传统反引号命令替换
//   ${...$(command) - 嵌套命令替换
//
// 参数：
//   s - 待检测的字符串
//
// 返回值：
//   bool - true: 包含命令替换; false: 不包含

// hasCommandSubstitution checks for command substitution patterns
func (v *ScriptValidator) hasCommandSubstitution(s string) bool {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\$\([^)]+\)`),   // $(command)
		regexp.MustCompile("`[^`]+`"),       // `command`
		regexp.MustCompile(`\$\{[^}]*\$\(`), // ${...$(command)
	}

	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// hasNetworkAccess 检测网络访问模式
//
// 检测的网络命令/库：
//   系统命令：curl、wget、nc、netcat、telnet、ssh、scp、rsync、ftp、sftp
//   Python库：socket.connect、urllib.request、requests.get/post、http.client
//   JavaScript：fetch、axios、XMLHttpRequest
//
// 参数：
//   content - 待检测的内容
//
// 返回值：
//   bool - true: 包含网络访问; false: 不包含

// hasNetworkAccess checks for network access patterns
func (v *ScriptValidator) hasNetworkAccess(content string) bool {
	patterns := []string{
		`\bcurl\b`,
		`\bwget\b`,
		`\bnc\b`,
		`\bnetcat\b`,
		`\btelnet\b`,
		`\bssh\b`,
		`\bscp\b`,
		`\brsync\b`,
		`\bftp\b`,
		`\bsftp\b`,
		`socket\.connect`,
		`urllib\.request`,
		`requests\.get`,
		`requests\.post`,
		`http\.client`,
		`httplib`,
		`fetch\s*\(`,
		`axios`,
		`XMLHttpRequest`,
	}

	for _, pattern := range patterns {
		if matched, _ := regexp.MatchString(`(?i)`+pattern, content); matched {
			return true
		}
	}
	return false
}

// hasReverseShellPattern 使用正则表达式识别常见的反弹 Shell（Reverse Shell）特征。
//
// 检测的模式：
//   - /dev/tcp/、/dev/udp/（TCP/UDP重定向）
//   - bash -i、sh -i（交互式Shell）
//   - python.*pty.spawn（Python PTY逃逸）
//   - perl.*-e.*socket（Perl Socket反弹）
//   - ruby.*-rsocket（Ruby Socket反弹）
//   - socat.*exec（Socat执行）
//   - mkfifo、mknod（命名管道）
//   - 文件描述符重定向（0<&196、196>&0）
//   - nc -e、ncat -e、netcat -e（NC带执行）
//
// 参数：
//   content - 待检测的内容
//
// 返回值：
//   bool - true: 包含反弹Shell模式; false: 不包含

// hasReverseShellPattern checks for common reverse shell patterns
func (v *ScriptValidator) hasReverseShellPattern(content string) bool {
	patterns := []string{
		`/dev/tcp/`,
		`/dev/udp/`,
		`bash\s+-i`,
		`sh\s+-i`,
		`/bin/bash\s+-i`,
		`/bin/sh\s+-i`,
		`python.*pty\.spawn`,
		`perl.*-e.*socket`,
		`ruby.*-rsocket`,
		`socat.*exec`,
		`mkfifo`,
		`mknod.*p`,
		`0<&196`, // File descriptor redirection trick
		`196>&0`,
		`/inet/tcp/`,
		`bash.*>&.*0>&1`,
		`nc.*-e`,
		`ncat.*-e`,
		`netcat.*-e`,
	}

	for _, pattern := range patterns {
		if matched, _ := regexp.MatchString(`(?i)`+pattern, content); matched {
			return true
		}
	}
	return false
}

// hasEmbeddedShellCommands 检测标准输入中的嵌入Shell命令
//
// 检测的模式：
//   $(...)   - 命令替换
//   `...`    - 反引号命令替换
//   \n[;&|]  - 换行后跟Shell操作符
//   \\n.*[;&|] - 转义换行后跟Shell操作符
//
// 参数：
//   content - 标准输入内容
//
// 返回值：
//   bool - true: 包含嵌入Shell命令; false: 不包含

// hasEmbeddedShellCommands checks stdin for embedded shell commands
func (v *ScriptValidator) hasEmbeddedShellCommands(content string) bool {
	patterns := []string{
		`\$\(.*\)`,   // Command substitution
		"`.*`",       // Backtick substitution
		`\n\s*[;&|]`, // Newline followed by shell operators
		`\\n.*[;&|]`, // Escaped newline followed by shell operators
	}

	for _, pattern := range patterns {
		if matched, _ := regexp.MatchString(pattern, content); matched {
			return true
		}
	}
	return false
}

// getDefaultDangerousCommands 返回默认的危险命令列表
//
// 包含的命令类别：
//   1. 系统破坏：rm -rf /、mkfs、dd破坏
//   2. Fork炸弹：:(){ :|:& };: 等各种变种
//   3. 进程控制：shutdown、reboot、killall
//   4. 权限提升：chmod 777、setuid、passwd
//   5. 凭证访问：/etc/passwd、/etc/shadow、SSH密钥
//   6. 环境操纵：export PATH=、LD_PRELOAD
//   7. 容器逃逸：docker、nsenter、unshare
//   8. 计划任务：crontab、/etc/cron
//   9. 内核模块：insmod、modprobe
//
// 返回值：
//   []string - 危险命令列表

// getDefaultDangerousCommands returns commands that should not appear in scripts
func getDefaultDangerousCommands() []string {
	return []string{
		// System modification - various forms of dangerous rm
		"rm -rf /",
		"rm -fr /",
		"rm -rf /", // with different spacing
		"rm -rf/*",
		"rm -rf *",

		// Filesystem destruction
		"mkfs",
		"dd if=/dev/zero",
		"dd if=/dev/random",

		// Fork bombs (various forms)
		":(){ :|:& };:",
		":(){:|:&};:",
		"bomb(){ bomb|bomb& };bomb",

		// Process and system control
		"shutdown",
		"reboot",
		"halt",
		"poweroff",
		"init 0",
		"init 6",
		"killall",
		"pkill",

		// Permission escalation
		"chmod 777 /",
		"chown root",
		"setuid",
		"setgid",
		"passwd",

		// Credential access
		"/etc/passwd",
		"/etc/shadow",
		"/etc/sudoers",
		".ssh/",
		"id_rsa",
		"id_ed25519",

		// Environment manipulation
		"export PATH=",
		"export LD_PRELOAD",
		"export LD_LIBRARY_PATH",

		// Cron manipulation
		"crontab",
		"/etc/cron",

		// Service manipulation
		"systemctl",
		"service",

		// Module/kernel manipulation
		"insmod",
		"modprobe",
		"rmmod",

		// Container escape attempts
		"docker",
		"kubectl",
		"nsenter",
		"unshare",
		"capsh",
	}
}

// getDefaultDangerousPatterns 返回默认的危险正则模式
//
// 包含的模式类别：
//   1. 编码载荷：base64解码、xxd解码
//   2. 下载执行：curl|wget.*|bash|sh
//   3. 代码执行：eval、exec、os.system
//   4. 反序列化：pickle.load、yaml.unsafe_load
//   5. Fork炸弹：函数递归模式
//   6. 危险rm：--no-preserve-root
//
// 返回值：
//   []string - 危险正则模式字符串列表

// getDefaultDangerousPatterns returns regex patterns for dangerous operations
func getDefaultDangerousPatterns() []string {
	return []string{
		// Base64 encoded payloads (often used to hide malicious code)
		`base64\s+(-d|--decode)`,
		`echo\s+.*\|\s*base64\s+-d`,

		// Hex encoded payloads
		`xxd\s+-r`,
		`echo\s+-e\s+.*\\x`,

		// Code download and execution
		`curl.*\|\s*(bash|sh)`,
		`wget.*\|\s*(bash|sh)`,
		`python.*http\.server`,

		// Eval and exec patterns (code injection)
		`eval\s*\(`,
		`exec\s*\(`,
		`os\.system\s*\(`,
		`subprocess\.call\s*\(.*shell\s*=\s*True`,
		`subprocess\.Popen\s*\(.*shell\s*=\s*True`,
		`os\.popen\s*\(`,
		`commands\.getoutput\s*\(`,
		`commands\.getstatusoutput\s*\(`,

		// History/log manipulation
		`history\s+-c`,
		`unset\s+HISTFILE`,
		`export\s+HISTSIZE=0`,

		// Python dangerous functions
		`__import__\s*\(`,
		`importlib\.import_module`,
		`compile\s*\(.*exec`,

		// Pickle deserialization (can execute arbitrary code)
		`pickle\.loads?\s*\(`,
		`cPickle\.loads?\s*\(`,

		// YAML unsafe loading
		`yaml\.load\s*\([^,]+\)`, // Without Loader argument
		`yaml\.unsafe_load`,

		// Fork bomb patterns (function recursion with backgrounding)
		`:\s*\(\s*\)\s*\{\s*:`,           // :() { : pattern
		`\(\)\s*\{\s*\w+\s*\|\s*\w+\s*&`, // () { x | x & pattern

		// Dangerous rm patterns
		`rm\s+-[rf]+\s+/`, // rm -rf / or rm -fr /
		`rm\s+--no-preserve-root`,
	}
}

// getDefaultArgInjectionPatterns 返回默认的参数注入模式
//
// 包含的模式：
//   - 路径遍历：../、..\
//   - 环境变量注入：${VAR}、$VAR
//   - 特殊字符：$(、`、\n、\r
//
// 返回值：
//   []string - 参数注入模式列表

// getDefaultArgInjectionPatterns returns patterns for argument injection
func getDefaultArgInjectionPatterns() []string {
	return []string{
		// Path traversal
		`\.\.\/`,
		`\.\.\\`,

		// Environment variable injection
		`\$\{[A-Z_]+\}`,
		`\$[A-Z_]+`,

		// Special shell characters
		`\$\(`,
		"`",
		`\n`,
		`\r`,
	}
}

// compilePatterns 编译字符串模式为正则表达式
//
// 功能：
//   1. 遍历模式字符串列表
//   2. 添加不区分大小写标志（(?i)）
//   3. 编译为正则表达式对象
//   4. 忽略编译失败的模式
//

// compilePatterns compiles string patterns to regex
func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if r, err := regexp.Compile(`(?i)` + p); err == nil {
			compiled = append(compiled, r)
		}
	}
	return compiled
}

// extractContext 提取匹配位置的上下文信息
//
// 功能：
//   1. 在内容中查找匹配字符串的位置
//   2. 提取匹配前后各20个字符
//   3. 在截断处添加"..."省略号
//
// 参数：
//   content - 原始内容
//   match   - 匹配到的字符串
//
// 返回值：
//   string - 匹配位置的前后20个字符（含匹配本身）
//
// 示例：
//   content := "echo 'hello world'"
//   match := "hello"
//   // 返回: "echo 'hello world'"

// extractContext extracts context around a match
func extractContext(content, match string) string {
	idx := strings.Index(strings.ToLower(content), strings.ToLower(match))
	if idx == -1 {
		return ""
	}

	start := idx - 20
	if start < 0 {
		start = 0
	}
	end := idx + len(match) + 20
	if end > len(content) {
		end = len(content)
	}

	context := content[start:end]
	if start > 0 {
		context = "..." + context
	}
	if end < len(content) {
		context = context + "..."
	}

	return context
}

// truncate 截断字符串到指定最大长度
//
// 参数：
//   s      - 待截断的字符串
//   maxLen - 最大长度
//
// 返回值：
//   string - 截断后的字符串（超出部分添加"..."）
//
// 示例：
//   truncate("hello world", 5) // 返回 "hello..."

// truncate truncates a string to max length
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
