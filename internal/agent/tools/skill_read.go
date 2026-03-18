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
