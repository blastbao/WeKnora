package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// 该工具允许 Agent 调用某个 Skill 目录下附带的脚本（如 Python、Shell 脚本等）。

// 这个工具的主要作用是在安全的沙箱环境中运行脚本。
//	- 输入：技能名称、脚本路径、命令行参数以及可选的标准输入数据。
//	- 处理：验证输入，检查权限，然后调用底层的 skills.Manager 来执行脚本。
//	- 输出：返回结构化的执行结果，包括标准输出（stdout）、标准错误（stderr）、退出码、执行时长以及是否被强制终止等信息。

// Tool name constant for execute_skill_script
//
// 工具定义，告诉 LLM（大语言模型）什么时候该用这个工具：
//	- 场景：当模型发现技能说明书中提到 “运行某某分析脚本” 时使用。
//	- 确定性：强调对于复杂的逻辑处理，运行脚本比模型直接生成代码更可靠。
//	- 安全性：明确告知模型这些脚本运行在隔离的沙箱中，没有网络权限，且只能访问该技能自己的文件夹。
//
// 输入参数结构 (ExecuteSkillScriptInput)
//	- SkillName: 必填，指定哪个技能。
//	- ScriptPath: 必填，脚本的相对路径。
//	- Args: 选填，命令行参数（如 --mode fast）。
//	- Input: 选填，通过标准输入 (stdin) 传递的数据，方便将内存中的 JSON 或大量文本直接“喂”给脚本。
//
// 执行逻辑 (Execute 函数)
//	- 参数解析与校验：检查 JSON 参数是否合法，确保 SkillName 和 ScriptPath 不为空。
//	- 状态检查：确认 skillManager 是否已启用。
//	- 核心调用：调用 skillManager.ExecuteScript。这会触发底层沙箱（Sandbox）启动环境、加载文件并运行。
//	- 结果格式化：将脚本运行的结果（输出、错误、耗时等）组装成一个易于阅读的 Markdown 字符串返回给模型。
//
// 设计细节
//	- 结果可视化：代码使用了 strings.Builder 来构建非常详细的报告，包括 === Script Execution === 标题和代码块包裹的输出。这有助于 LLM 理解脚本到底运行了什么，以及为什么失败。
//	- 安全性检查：在执行前，工具会打印日志记录执行详情，并通过 skillManager 确保脚本路径合法（防止路径穿越攻击）。
//	- 结构化数据返回：除了给模型看的 Output（Markdown 字符串），还返回了 Data（Map 结构）。这对于系统后台监控或后续自动化处理非常有用。

// AI 发送给工具的 JSON 数据：
//
//	{
//		"skill_name": "data_analyzer",
//		"script_path": "scripts/process_logs.py",
//		"args": ["--format", "json", "--verbose"],
//		"input": "{\"logs\": [\"error: connection timeout\", \"info: retrying...\"], \"threshold\": 5}"
//	}
//
// Output 字段
//
//	=== Script Execution: data_analyzer/scripts/process_logs.py ===
//
//	**Arguments**: [--format json --verbose]
//	**Exit Code**: 0
//	**Duration**: 1.24s
//
//	## Standard Output
//
//	```json
//	{
//	  "status": "completed",
//	  "processed_count": 2,
//	  "alerts": [
//		{
//		  "level": "warning",
//		  "message": "Connection timeout detected",
//		  "action": "retry_scheduled"
//		}
//	  ],
//	  "summary": "Log processing finished successfully."
//	}

var executeSkillScriptTool = BaseTool{
	name: ToolExecuteSkillScript,
	description: `Execute a script from a skill in a sandboxed environment.

## Usage
- Use this tool to run utility scripts bundled with a skill
- Scripts are executed in an isolated sandbox for security
- Only scripts from loaded skills can be executed

## When to Use
- When a skill's instructions reference a utility script (e.g., "Run scripts/analyze_form.py")
- When automation or data processing is needed as part of skill workflow
- For deterministic operations where script execution is more reliable than generating code

## Security
- Scripts run in a sandboxed environment with limited permissions
- Network access is disabled by default
- File access is restricted to the skill directory

## Returns
- Script stdout and stderr output
- Exit code indicating success (0) or failure (non-zero)`,
	schema: utils.GenerateSchema[ExecuteSkillScriptInput](),
}

// ExecuteSkillScriptInput defines the input parameters for the execute_skill_script tool
type ExecuteSkillScriptInput struct {
	SkillName  string   `json:"skill_name" jsonschema:"Name of the skill containing the script"`
	ScriptPath string   `json:"script_path" jsonschema:"Relative path to the script within the skill directory (e.g. scripts/analyze.py)"`
	Args       []string `json:"args,omitempty" jsonschema:"Optional command-line arguments to pass to the script. Note: if using --file flag, you must provide an actual file path that exists in the skill directory. If you have data in memory (not a file), use the 'input' parameter instead."`
	Input      string   `json:"input,omitempty" jsonschema:"Optional input data to pass to the script via stdin. Use this when you have data in memory (e.g. JSON string) that the script should process. This is equivalent to piping data: echo 'data' | python script.py"`
}

// ExecuteSkillScriptTool allows the agent to execute skill scripts in a sandbox
type ExecuteSkillScriptTool struct {
	BaseTool
	skillManager *skills.Manager
}

// NewExecuteSkillScriptTool creates a new execute_skill_script tool instance
func NewExecuteSkillScriptTool(skillManager *skills.Manager) *ExecuteSkillScriptTool {
	return &ExecuteSkillScriptTool{
		BaseTool:     executeSkillScriptTool,
		skillManager: skillManager,
	}
}

// Execute 执行技能脚本工具，在沙箱环境中运行技能目录下的脚本文件
//
// 功能说明:
//   - 解析并验证输入参数，提取技能名称、脚本路径、执行参数和输入数据
//   - 验证技能系统是否已启用
//   - 在隔离的沙箱环境中执行指定脚本（支持Python、Shell等）
//   - 捕获脚本的标准输出、标准错误、退出码和执行时长
//   - 处理脚本超时或被强制终止的情况
//   - 返回格式化的执行结果，包含完整的输出信息和执行状态
//
// 参数:
//   - ctx: 上下文对象，用于控制超时和日志记录
//   - args: JSON格式的原始参数，包含以下字段：
//     - SkillName: string 技能名称（必填），指定技能目录
//     - ScriptPath: string 脚本路径（必填），相对于技能目录的路径
//     - Args: []string 命令行参数（可选），传递给脚本的参数列表
//     - Input: string 标准输入数据（可选），通过stdin传递给脚本
//
// 返回值:
//   - *types.ToolResult: 工具执行结果，包含以下内容：
//     - Success: 脚本是否成功执行（true/false，由exit code判断）
//     - Output: 格式化的执行报告（Markdown格式），包含：
//         * 脚本标识（技能名/脚本路径）
//         * 执行参数（如有）
//         * 退出码和执行时长
//         * 终止警告（如被kill）
//         * 标准输出（Stdout）
//         * 标准错误（Stderr）
//         * 错误信息（如有）
//     - Data: 结构化数据（map[string]interface{}），包含：
//         * skill_name: string 技能名称
//         * script_path: string 脚本路径
//         * args: []string 执行参数
//         * exit_code: int 进程退出码
//         * stdout: string 标准输出内容
//         * stderr: string 标准错误内容
//         * duration_ms: int64 执行时长（毫秒）
//         * killed: bool 是否被强制终止
//     - Error: 错误信息（脚本执行失败时），优先使用result.Error，其次使用退出码信息
//   - error: 工具框架层面的错误（参数解析失败等），业务错误通过ToolResult返回
//
// 执行流程:
//   1. 记录执行开始日志
//   2. 解析JSON输入参数
//   3. 验证SkillName必填
//   4. 验证ScriptPath必填
//   5. 检查技能管理器是否可用（skillManager != nil && IsEnabled()）
//   6. 记录执行参数详情（技能名、脚本路径、参数、输入长度）
//   7. 调用skillManager.ExecuteScript在沙箱中执行脚本
//   8. 处理执行错误（沙箱启动失败、脚本不存在等）
//   9. 格式化输出报告（Markdown格式）
//   10. 判断执行成功状态（result.IsSuccess()基于exit code）
//   11. 组装结构化数据和错误信息
//   12. 记录执行完成日志（包含退出码）
//   13. 返回结果
//
// 沙箱执行环境:
//   - 隔离性：脚本在独立沙箱中运行，与主系统隔离
//   - 资源限制：CPU、内存、执行时长受限制
//   - 超时处理：超时时自动终止脚本（Killed=true）
//   - 输入输出：通过stdin/stdout/stderr与脚本交互
//   - 支持类型：Python、Shell、Node.js等（取决于沙箱配置）
//
// 成功判断标准:
//   - 调用result.IsSuccess()判断，通常exit code = 0为成功
//   - 非零退出码视为失败，即使stdout有内容
//   - 被强制终止（Killed=true）视为失败
//
// 输出格式化:
//   - 使用代码块（```）包裹stdout和stderr，保持格式
//   - 自动补全换行符（避免输出末尾无换行）
//   - 区分展示Stdout、Stderr、Error三个部分
//   - 被终止时添加警告提示
//
// 日志记录:
//   - Info: 执行开始、执行参数详情、执行完成（含退出码）
//   - Error: 参数解析失败、脚本执行失败
//
// 错误处理:
//   - 参数解析失败: 返回解析错误，记录Error日志
//   - SkillName为空: 返回"skill_name is required"
//   - ScriptPath为空: 返回"script_path is required"
//   - 技能系统未启用: 返回"Skills are not enabled"
//   - 脚本执行失败: 返回执行错误详情（沙箱错误、脚本不存在等）
//
// 安全机制:
//   - 沙箱隔离：脚本在受限环境中执行，无法访问主机系统
//   - 资源限制：防止恶意脚本耗尽资源
//   - 超时控制：防止无限循环或长时间挂起
//   - 输入验证：参数和输入数据经过验证后才传入
//
// 使用场景:
//   - Agent需要执行技能的自动化脚本（如数据处理、文件操作）
//   - 需要运行Python进行复杂计算或数据分析
//   - 需要调用外部命令完成特定任务
//   - 需要批量处理数据并获取执行结果
//
// 典型使用流程:
//   1. Agent通过read_skill了解技能能力
//   2. 发现需要执行scripts/process.py脚本
//   3. 准备执行参数和输入数据
//   4. 调用ExecuteSkillScript执行脚本
//   5. 根据exit code和stdout判断执行结果
//   6. 解析stdout获取处理结果
//   7. 如失败，根据stderr排查问题
//
// 与read_skill的配合:
//   - 先用read_skill查看技能说明书，了解有哪些可执行脚本
//   - 识别标注为"(script - can be executed)"的文件
//   - 再用ExecuteSkillScript执行目标脚本
//
// 示例:
//   result, err := tool.Execute(ctx, []byte(`{
//       "SkillName": "data_analysis",
//       "ScriptPath": "scripts/analyze.py",
//       "Args": ["--format", "json", "--verbose"],
//       "Input": "col1,col2\n1,2\n3,4\n"
//   }`))
//
// 成功返回示例:
//	  result = &types.ToolResult{
//		  Success: true,
//		  Output: "=== Script Execution: data_analysis/scripts/analyze.py ===\n\n" +
//				  "**Arguments**: [--format json --verbose]\n" +
//				  "**Exit Code**: 0\n" +
//				  "**Duration**: 1.234s\n\n" +
//				  "## Standard Output\n\n" +
//				  "```\n" +
//				  "{\"mean\": 2.5, \"std\": 1.12, \"count\": 2}\n" +
//				  "```\n\n",
//		  Data: map[string]interface{}{
//			  "skill_name":  "data_analysis",
//			  "script_path": "scripts/analyze.py",
//			  "args":        []string{"--format", "json", "--verbose"},
//			  "exit_code":   0,
//			  "stdout":      "{\"mean\": 2.5, \"std\": 1.12, \"count\": 2}\n",
//			  "stderr":      "",
//			  "duration_ms": 1234,
//			  "killed":      false,
//		  },
//		  Error: "",
//	  }
//
// 失败返回示例（脚本错误）:
//	  result = &types.ToolResult{
//		  Success: false,
//		  Output: "=== Script Execution: data_analysis/scripts/analyze.py ===\n\n" +
//				  "**Exit Code**: 1\n" +
//				  "**Duration**: 0.5s\n\n" +
//				  "## Standard Output\n\n" +
//				  "```\n" +
//				  "```\n\n" +
//				  "## Standard Error\n\n" +
//				  "```\n" +
//				  "Traceback (most recent call last):\n" +
//				  "  File \"analyze.py\", line 5, in <module>\n" +
//				  "    import pandas as pd\n" +
//				  "ModuleNotFoundError: No module named 'pandas'\n" +
//				  "```\n\n",
//		  Data: map[string]interface{}{
//			  "skill_name":  "data_analysis",
//			  "script_path": "scripts/analyze.py",
//			  "args":        []string{},
//			  "exit_code":   1,
//			  "stdout":      "",
//			  "stderr":      "Traceback (most recent call last):\n  File \"analyze.py\"...",
//			  "duration_ms": 500,
//			  "killed":      false,
//		  },
//		  Error: "Script exited with code 1",
//	  }
//
// 超时终止返回示例:
//	  result = &types.ToolResult{
//		  Success: false,
//		  Output: "=== Script Execution: data_analysis/scripts/heavy_compute.py ===\n\n" +
//				  "**Exit Code**: -1\n" +
//				  "**Duration**: 30s\n\n" +
//				  "**Warning**: Script was terminated (timeout or killed)\n\n" +
//				  "## Standard Output\n\n" +
//				  "```\n" +
//				  "Computing... (partial output)\n" +
//				  "```\n\n",
//		  Data: map[string]interface{}{
//			  "skill_name":  "data_analysis",
//			  "script_path": "scripts/heavy_compute.py",
//			  "args":        []string{},
//			  "exit_code":   -1,
//			  "stdout":      "Computing... (partial output)\n",
//			  "stderr":      "",
//			  "duration_ms": 30000,
//			  "killed":      true,
//		  },
//		  Error: "Script was terminated (timeout or killed)",
//	  }

// Execute executes the execute_skill_script tool
func (t *ExecuteSkillScriptTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Execute started")

	// Parse input
	var input ExecuteSkillScriptInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][ExecuteSkillScript] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, nil
	}

	// Validate required fields
	if input.SkillName == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "skill_name is required",
		}, nil
	}

	if input.ScriptPath == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "script_path is required",
		}, nil
	}

	// Check if skill manager is available
	if t.skillManager == nil || !t.skillManager.IsEnabled() {
		return &types.ToolResult{
			Success: false,
			Error:   "Skills are not enabled",
		}, nil
	}

	// Execute the script in sandbox
	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Executing script: %s/%s with args: %v, input length: %d",
		input.SkillName, input.ScriptPath, input.Args, len(input.Input))

	result, err := t.skillManager.ExecuteScript(ctx, input.SkillName, input.ScriptPath, input.Args, input.Input)
	if err != nil {
		logger.Errorf(ctx, "[Tool][ExecuteSkillScript] Script execution failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Script execution failed: %v", err),
		}, nil
	}

	// Build output
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("=== Script Execution: %s/%s ===\n\n", input.SkillName, input.ScriptPath))

	if len(input.Args) > 0 {
		builder.WriteString(fmt.Sprintf("**Arguments**: %v\n", input.Args))
	}

	builder.WriteString(fmt.Sprintf("**Exit Code**: %d\n", result.ExitCode))
	builder.WriteString(fmt.Sprintf("**Duration**: %v\n\n", result.Duration))

	if result.Killed {
		builder.WriteString("**Warning**: Script was terminated (timeout or killed)\n\n")
	}

	if result.Stdout != "" {
		builder.WriteString("## Standard Output\n\n")
		builder.WriteString("```\n")
		builder.WriteString(result.Stdout)
		if !strings.HasSuffix(result.Stdout, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("```\n\n")
	}

	if result.Stderr != "" {
		builder.WriteString("## Standard Error\n\n")
		builder.WriteString("```\n")
		builder.WriteString(result.Stderr)
		if !strings.HasSuffix(result.Stderr, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("```\n\n")
	}

	if result.Error != "" {
		builder.WriteString("## Error\n\n")
		builder.WriteString(result.Error)
		builder.WriteString("\n")
	}

	// Determine success based on exit code
	success := result.IsSuccess()

	resultData := map[string]interface{}{
		"skill_name":  input.SkillName,
		"script_path": input.ScriptPath,
		"args":        input.Args,
		"exit_code":   result.ExitCode,
		"stdout":      result.Stdout,
		"stderr":      result.Stderr,
		"duration_ms": result.Duration.Milliseconds(),
		"killed":      result.Killed,
	}

	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Script completed with exit code: %d", result.ExitCode)

	return &types.ToolResult{
		Success: success,
		Output:  builder.String(),
		Data:    resultData,
		Error: func() string {
			if !success {
				if result.Error != "" {
					return result.Error
				}
				return fmt.Sprintf("Script exited with code %d", result.ExitCode)
			}
			return ""
		}(),
	}, nil
}

// Cleanup releases any resources
func (t *ExecuteSkillScriptTool) Cleanup(ctx context.Context) error {
	return nil
}
