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

// 在一个复杂的 Agent 系统中，我们不能把所有的 skill 说明（可能成千上万行）一次性塞进 LLM 的 Prompt 里，否则会造成：
//	- Token 浪费：上下文窗口会瞬间爆满。
//	- 干扰严重：无关的指令会降低 AI 遵循核心任务的能力。

// 所以，在 System Prompt 上下文有限的情况下，系统提示词通常只包含 skill 的“简介”，
// 具体的执行步骤、参数要求、注意事项等详细内容存储在独立的 skill 目录中（通常是 SKILL.md）。
// Agent 需要执行具体 skill 时，调用这个工具来获取该 skill 的完整信息。

// Tool name constant for read_skill

// `按需读取技能内容，以学习专业化能力。
//
//	## 使用方法
//	- 当用户请求与可用技能的描述相匹配时，请使用此工具。
//	- 提供 skill_name 以加载技能的完整指令（SKILL.md 的内容）。
//	- 可选提供 file_path 以读取技能目录下的其他文件。
//
//	## 适用场景
//	- 当系统提示词中显示的可用技能与用户请求相匹配时。
//	- 在执行与技能描述相符的任务之前。
//	- 用于阅读技能目录内的其他文档或参考文件。
//
//	## 返回结果
//	- 完成任务所需的技能指令和指南。
//	- 如果指定了 file_path，则返回该文件的具体内容。`

var readSkillTool = BaseTool{
	name: ToolReadSkill,
	description: `Read skill content on demand to learn specialized capabilities.

## Usage
- Use this tool when a user request matches an available skill's description
- Provide the skill_name to load the skill's full instructions (SKILL.md content)
- Optionally provide file_path to read additional files within the skill directory

## When to Use
- When the system prompt shows an available skill that matches the user's request
- Before performing tasks that match a skill's description
- To read additional documentation or reference files within a skill

## Returns
- Skill instructions and guidance for completing the task
- File content if file_path is specified`, // 告诉 LLM 什么时候用这个工具
	schema: utils.GenerateSchema[ReadSkillInput](), // 定义输入参数的 JSON Schema
}

// ReadSkillInput defines the input parameters for the read_skill tool
type ReadSkillInput struct {
	SkillName string `json:"skill_name" jsonschema:"Name of the skill to read"`
	FilePath  string `json:"file_path,omitempty" jsonschema:"Optional relative path to a specific file within the skill directory"`
}

// ReadSkillTool allows the agent to read skill content on demand
type ReadSkillTool struct {
	BaseTool
	skillManager *skills.Manager
}

// NewReadSkillTool creates a new read_skill tool instance
func NewReadSkillTool(skillManager *skills.Manager) *ReadSkillTool {
	return &ReadSkillTool{
		BaseTool:     readSkillTool,
		skillManager: skillManager,
	}
}

// 当 AI 想知道某个技能具体怎么用时，这段代码负责去文件夹里把“说明书”读出来，
// 顺便告诉 AI 这个技能文件夹里还有哪些“辅助文件”，最后整理成一段通顺的文字发给 AI。
//
// 输入：技能名字（比如 "data_cleaner"）。
// 处理：读文件 -> 列目录 -> 拼文字。
// 输出：一段包含 “怎么做” 和 “有哪些文件” 的 Markdown 文本。

// 内部包含两个分支：
//	第一段 (if)：是“精读”。用户指定了具体文件名，只读那一个文件的内容（比如只看某个脚本代码）。
//	第二段 (else)：是“概览”。用户没指定文件，默认读取技能的主说明书，并顺便列出目录下还有哪些其他文件。
//
// AI 总是先走 else 分支，获取“操作指南”和“文件列表”。
// AI 看到列表后，如果发现有用的附件，它才会走 if 分支，去读取“文件内容”。
// 这样，不需要一次性把所有东西塞给 AI，而是让 AI 先看大纲，觉得有需要再自己去翻细节。
// 这样，既节省了 Token，又让 AI 的逻辑更清晰。

// Execute 执行读取技能工具，获取技能的详细信息或特定文件内容
//
// 功能说明:
//   - 解析并验证输入参数，提取技能名称和可选的文件路径
//   - 验证技能系统是否已启用
//   - 支持两种读取模式：
//     * 读取技能说明书（SKILL.md）：获取技能的元数据、描述和操作指令
//     * 读取特定文件：获取技能目录下的脚本、配置或其他文件内容
//   - 列出技能目录中的可用文件（排除SKILL.md），标注可执行脚本
//   - 返回格式化的技能信息，供Agent理解和使用
//
// 参数:
//   - ctx: 上下文对象，用于日志记录和超时控制
//   - args: JSON格式的原始参数，包含以下字段：
//     - SkillName: string 技能名称（必填）
//     - FilePath: string 文件路径（可选），指定时读取特定文件，否则读取技能说明书
//
// 返回值:
//   - *types.ToolResult: 工具执行结果，包含以下内容：
//     - Success: 是否成功读取（true/false）
//     - Output: 格式化的技能信息文本（Markdown格式）
//     - Data: 结构化数据（map[string]interface{}），两种模式：
//
//       模式1 - 读取特定文件（FilePath指定）:
//         * skill_name: string 技能名称
//         * file_path: string 文件路径
//         * content: string 文件内容
//         * content_length: int 内容长度
//
//       模式2 - 读取技能说明书（FilePath为空）:
//         * skill_name: string 技能名称
//         * description: string 技能描述
//         * instructions: string 操作指令（详细的使用说明）
//         * instructions_length: int 指令长度
//         * files: []string 技能目录下的文件列表（包含SKILL.md）
//
//     - Error: 错误信息（执行失败时）
//   - error: 工具框架层面的错误，业务逻辑错误通过ToolResult.Error返回
//
// 执行流程:
//   1. 记录执行开始日志
//   2. 解析JSON输入参数
//   3. 验证SkillName必填
//   4. 检查技能管理器是否可用（skillManager != nil && IsEnabled()）
//   5. 判断读取模式：
//      - FilePath指定：调用ReadSkillFile读取特定文件
//      - FilePath为空：调用LoadSkill读取说明书，ListSkillFiles列出文件
//   6. 格式化输出内容（Markdown格式）
//   7. 组装结构化数据
//   8. 记录执行成功日志
//   9. 返回结果
//
// 读取模式说明:
//
// 模式1 - 读取特定文件:
//   用途：获取技能目录下的脚本、配置、文档等具体内容
//   调用：指定FilePath参数，如"config.yaml"、"scripts/setup.sh"
//   输出：文件原始内容，带文件路径标识
//   适用：Agent需要查看或执行技能的具体文件时
//
// 模式2 - 读取技能说明书:
//   用途：了解技能的功能描述和使用方法
//   调用：不指定FilePath，仅提供SkillName
//   输出：技能概况（名称、描述）+ 操作指南（Instructions）+ 附件清单
//   适用：Agent首次使用某技能，需要了解其能力和调用方式
//
// 文件类型识别:
//   - 脚本文件（.py, .sh等）：在文件列表中标注"(script - can be executed)"
//   - 其他文件：普通列出
//   - SKILL.md：不重复列出（已在Instructions中展示）
//
// 技能系统依赖:
//   - skillManager: 技能管理器，负责技能的加载、文件读取、文件列表
//   - 必须已启用（IsEnabled()返回true）
//   - 技能以目录形式组织，每个技能一个独立文件夹
//   - SKILL.md是每个技能的入口文件（说明书）
//
// 日志记录:
//   - Info: 执行开始、执行成功（包含技能名称）
//   - Error: 参数解析失败、技能加载失败、文件读取失败
//
// 错误处理:
//   - 参数解析失败: 返回解析错误
//   - SkillName为空: 返回"skill_name is required"
//   - 技能系统未启用: 返回"Skills are not enabled"
//   - 技能加载失败: 返回加载错误详情
//   - 文件读取失败: 返回读取错误详情
//   - 列出文件失败: 非致命错误，返回空列表继续
//
// 使用场景:
//   - Agent首次接触某技能，需要阅读说明书了解功能
//   - Agent需要查看技能的具体脚本内容
//   - Agent需要获取技能的配置文件
//   - Agent需要确认技能目录下有哪些可用资源
//
// 典型使用流程:
//   1. Agent发现需要使用某技能（如"data_analysis"）
//   2. 调用ReadSkill（不指定FilePath）获取说明书
//   3. 阅读Description和Instructions，了解技能能力
//   4. 查看Available Files列表，发现需要"scripts/analyze.py"
//   5. 再次调用ReadSkill（指定FilePath="scripts/analyze.py"）获取脚本内容
//   6. 根据脚本逻辑执行分析任务
//
// 示例:
//   // 读取技能说明书
//   result, err := tool.Execute(ctx, []byte(`{
//       "SkillName": "data_analysis"
//   }`))
//
//   // 读取特定文件
//   result, err := tool.Execute(ctx, []byte(`{
//       "SkillName": "data_analysis",
//       "FilePath": "scripts/analyze.py"
//   }`))
//
// 返回示例（读取说明书）:
//	  result = &types.ToolResult{
//		  Success: true,
//		  Output: "=== Skill: data_analysis ===\n\n" +
//				  "**Description**: 数据分析技能，支持清洗、统计、可视化\n\n" +
//				  "## Instructions\n\n" +
//				  "1. 使用 read_data 工具加载数据文件\n" +
//				  "2. 使用 analyze 工具进行统计分析\n" +
//				  "3. 使用 visualize 工具生成图表...\n\n" +
//				  "## Available Files\n\n" +
//				  "The following files are available...\n\n" +
//				  "- `scripts/analyze.py` (script - can be executed)\n" +
//				  "- `templates/chart.html`\n" +
//				  "- `config.yaml`",
//		  Data: map[string]interface{}{
//			  "skill_name":          "data_analysis",
//			  "description":         "数据分析技能，支持清洗、统计、可视化",
//			  "instructions":        "1. 使用 read_data 工具加载数据文件\n2. ...",
//			  "instructions_length": 256,
//			  "files":               []string{"SKILL.md", "scripts/analyze.py", "templates/chart.html", "config.yaml"},
//		  },
//		  Error: "",
//	  }
//
// 返回示例（读取特定文件）:
//	  result = &types.ToolResult{
//		  Success: true,
//		  Output: "=== Skill File: data_analysis/scripts/analyze.py ===\n\n" +
//				  "import pandas as pd\n\n" +
//				  "def analyze_data(file_path):\n" +
//				  "    df = pd.read_csv(file_path)\n" +
//				  "    return df.describe()\n",
//		  Data: map[string]interface{}{
//			  "skill_name":     "data_analysis",
//			  "file_path":      "scripts/analyze.py",
//			  "content":        "import pandas as pd\n\ndef analyze_data...",
//			  "content_length": 156,
//		  },
//		  Error: "",
//	  }

// Execute executes the read_skill tool
func (t *ReadSkillTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][ReadSkill] Execute started")

	// Parse input
	var input ReadSkillInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][ReadSkill] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, nil
	}

	// Validate skill name
	if input.SkillName == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "skill_name is required",
		}, nil
	}

	// Check if skill manager is available
	if t.skillManager == nil || !t.skillManager.IsEnabled() {
		return &types.ToolResult{
			Success: false,
			Error:   "Skills are not enabled",
		}, nil
	}

	var builder strings.Builder
	var resultData = make(map[string]interface{})

	if input.FilePath != "" {
		// Read a specific file from the skill directory
		content, err := t.skillManager.ReadSkillFile(ctx, input.SkillName, input.FilePath)
		if err != nil {
			logger.Errorf(ctx, "[Tool][ReadSkill] Failed to read skill file: %v", err)
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("Failed to read skill file: %v", err),
			}, nil
		}

		builder.WriteString(fmt.Sprintf("=== Skill File: %s/%s ===\n\n", input.SkillName, input.FilePath))
		builder.WriteString(content)

		resultData["skill_name"] = input.SkillName
		resultData["file_path"] = input.FilePath
		resultData["content"] = content
		resultData["content_length"] = len(content)

	} else {
		// 主要工作：
		//  读取说明书：获取 skill 的元数据，包含名称、简介和操作指令。
		//  列出相关文件：扫描 skill 文件夹，看看除了说明书 (SKILL.md) 外，还有没有脚本 (.py, .sh)、配置文件 (.yaml) 或其他文档。
		//  拼接成给 AI 看的文本：技能概况、操作指南、附件清单

		// Read the main skill instructions (SKILL.md)
		skill, err := t.skillManager.LoadSkill(ctx, input.SkillName)
		if err != nil {
			logger.Errorf(ctx, "[Tool][ReadSkill] Failed to load skill: %v", err)
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("Failed to load skill: %v", err),
			}, nil
		}

		// List available files in the skill directory
		files, err := t.skillManager.ListSkillFiles(ctx, input.SkillName)
		if err != nil {
			files = []string{} // Non-fatal error
		}

		builder.WriteString(fmt.Sprintf("=== Skill: %s ===\n\n", skill.Name))
		builder.WriteString(fmt.Sprintf("**Description**: %s\n\n", skill.Description))
		builder.WriteString("## Instructions\n\n")
		builder.WriteString(skill.Instructions)

		// Add available files section
		if len(files) > 1 { // More than just SKILL.md
			builder.WriteString("\n\n## Available Files\n\n")
			builder.WriteString("The following files are available in this skill directory. Use `read_skill` with `file_path` to read them:\n\n")
			for _, file := range files {
				if file != skills.SkillFileName { // Don't list SKILL.md again
					if skills.IsScript(file) {
						builder.WriteString(fmt.Sprintf("- `%s` (script - can be executed)\n", file))
					} else {
						builder.WriteString(fmt.Sprintf("- `%s`\n", file))
					}
				}
			}
		}

		resultData["skill_name"] = skill.Name
		resultData["description"] = skill.Description
		resultData["instructions"] = skill.Instructions
		resultData["instructions_length"] = len(skill.Instructions)
		resultData["files"] = files
	}

	logger.Infof(ctx, "[Tool][ReadSkill] Successfully read skill: %s", input.SkillName)

	return &types.ToolResult{
		Success: true,
		Output:  builder.String(),
		Data:    resultData,
	}, nil
}

// Cleanup releases any resources (implements Tool interface if needed)
func (t *ReadSkillTool) Cleanup(ctx context.Context) error {
	return nil
}
