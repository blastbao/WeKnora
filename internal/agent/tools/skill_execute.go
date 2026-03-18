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
