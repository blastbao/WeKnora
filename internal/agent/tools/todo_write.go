package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

var todoWriteTool = BaseTool{
	name: ToolTodoWrite,
	description: `Use this tool to create and manage a structured task list for retrieval and research tasks. This helps you track progress, organize complex retrieval operations, and demonstrate thoroughness to the user.

**CRITICAL - Focus on Retrieval Tasks Only**:
- This tool is for tracking RETRIEVAL and RESEARCH tasks (e.g., searching knowledge bases, retrieving documents, gathering information)
- DO NOT include summary or synthesis tasks in todo_write - those are handled by the thinking tool
- Examples of appropriate tasks: "Search for X in knowledge base", "Retrieve information about Y", "Compare A and B"
- Examples of tasks to EXCLUDE: "Summarize findings", "Generate final answer", "Synthesize results" - these are for thinking tool

## When to Use This Tool
Use this tool proactively in these scenarios:

1. Complex multi-step tasks - When a task requires 3 or more distinct steps or actions
2. Non-trivial and complex tasks - Tasks that require careful planning or multiple operations
3. User explicitly requests todo list - When the user directly asks you to use the todo list
4. User provides multiple tasks - When users provide a list of things to be done (numbered or comma-separated)
5. After receiving new instructions - Immediately capture user requirements as todos
6. When you start working on a task - Mark it as in_progress BEFORE beginning work. Ideally you should only have one todo as in_progress at a time
7. After completing a task - Mark it as completed and add any new follow-up tasks discovered during implementation

## When NOT to Use This Tool

Skip using this tool when:
1. There is only a single, straightforward task
2. The task is trivial and tracking it provides no organizational benefit
3. The task is purely conversational or informational

NOTE that you should not use this tool if there is only one trivial task to do. In this case you are better off just doing the task directly.

## Examples of When to Use the Todo List

<example>
User: Compare WeKnora with other RAG frameworks like LangChain and LlamaIndex.
Assistant: I'll help you compare WeKnora with other RAG frameworks. Let me create a retrieval plan to gather information about each framework.
*Creates a todo list with retrieval tasks: 1) Search knowledge base for WeKnora features and architecture, 2) Use web_search to find LangChain documentation and features, 3) Use web_search to find LlamaIndex documentation and features, 4) Retrieve detailed comparison points for each framework*
*Note: Summary and synthesis will be handled by thinking tool after all retrieval tasks are completed*
Assistant: Let me start by searching the knowledge base for detailed WeKnora information.

<reasoning>
The assistant used todo_write tool correctly because:
1. The task requires multiple retrieval operations (searching KB for WeKnora, web search for LangChain and LlamaIndex)
2. Each retrieval task is specific and actionable
3. The todo list focuses on WHAT to retrieve, not HOW to summarize
4. Summary and synthesis will be handled separately by thinking tool after all retrieval is complete
5. This separation ensures clear task boundaries and avoids duplication
</reasoning>
</example>

<example>
User: I need to research the latest developments in vector databases for RAG applications.
Assistant: I'll help you research vector databases for RAG. Let me create a retrieval plan to gather comprehensive information.
*Creates todo list with retrieval tasks: 1) Search knowledge base for vector database information, 2) Use web_search to find latest vector database technologies, 3) Retrieve information about performance comparisons, 4) Gather information about integration approaches*
*Note: After completing all retrieval tasks, thinking tool will synthesize the findings into a comprehensive answer*
Assistant: Let me start by searching the knowledge base for vector database information.

<reasoning>
The assistant used todo_write tool correctly because:
1. The research task requires multiple retrieval operations (KB search, web search for latest info)
2. Each task focuses on retrieving specific information
3. The todo list tracks retrieval progress, not synthesis
4. Summary and analysis will be handled by thinking tool after retrieval is complete
5. This approach separates retrieval (todo_write) from synthesis (thinking tool)
</reasoning>
</example>

## Examples of When NOT to Use the Todo List

<example>
User: How do I print 'Hello World' in Python?
Assistant: In Python, you can print "Hello World" with this simple code:

print("Hello World")

This will output the text "Hello World" to the console when executed.</assistant>

<reasoning>
The assistant did not use the todo list because this is a single, trivial task that can be completed in one step. There's no need to track multiple tasks or steps for such a straightforward request.
</reasoning>
</example>

<example>
User: What does the git status command do?
Assistant: The git status command shows the current state of your working directory and staging area. It displays which changes have been staged, which haven't, and which files aren't being tracked by Git. This is a fundamental command for understanding the current state of your repository before making commits.

<reasoning>
The assistant did not use the todo list because this is an informational request with no actual coding task to complete. The user is simply asking for an explanation, not for the assistant to perform multiple steps or tasks.
</reasoning>
</example>

## Task States and Management

1. **Task States**: Use these states to track progress:
  - pending: Task not yet started
  - in_progress: Currently working on (limit to ONE task at a time)
  - completed: Task finished successfully

2. **Task Management**:
  - Update task status in real-time as you work
  - Mark tasks complete IMMEDIATELY after finishing (don't batch completions)
  - Only have ONE task in_progress at any time
  - Complete current tasks before starting new ones
  - Remove tasks that are no longer relevant from the list entirely

3. **Task Completion Requirements**:
  - ONLY mark a task as completed when you have FULLY accomplished it
  - If you encounter errors, blockers, or cannot finish, keep the task as in_progress
  - When blocked, create a new task describing what needs to be resolved
  - Never mark a task as completed if:
    - Tests are failing
    - Implementation is partial
    - You encountered unresolved errors
    - You couldn't find necessary files or dependencies

4. **Task Breakdown**:
  - Create specific, actionable RETRIEVAL tasks
  - Break complex retrieval needs into smaller, manageable steps
  - Use clear, descriptive task names focused on what to retrieve or research
  - **DO NOT include summary/synthesis tasks** - those are handled separately by the thinking tool

**Important**: After completing all retrieval tasks in todo_write, use the thinking tool to synthesize findings and generate the final answer. The todo_write tool tracks WHAT to retrieve, while thinking tool handles HOW to synthesize and present the information.

When in doubt, use this tool. Being proactive with task management demonstrates attentiveness and ensures you complete all retrieval requirements successfully.`,
	schema: utils.GenerateSchema[TodoWriteInput](),
}

// TodoWriteTool implements a planning tool for complex tasks
// This is an optional tool that helps organize multi-step research
type TodoWriteTool struct {
	BaseTool
}

// TodoWriteInput defines the input parameters for todo_write tool
type TodoWriteInput struct {
	Task  string     `json:"task" jsonschema:"The complex task or question you need to create a plan for"`
	Steps []PlanStep `json:"steps" jsonschema:"Array of research plan steps with status tracking"`
}

// PlanStep represents a single step in the research plan
type PlanStep struct {
	ID          string `json:"id" jsonschema:"Unique identifier for this step (e.g., 'step1', 'step2')"`
	Description string `json:"description" jsonschema:"Clear description of what to investigate or accomplish in this step"`
	Status      string `json:"status" jsonschema:"Current status: pending (not started), in_progress (executing), completed (finished)"`
}

// NewTodoWriteTool creates a new todo_write tool instance
func NewTodoWriteTool() *TodoWriteTool {
	return &TodoWriteTool{
		BaseTool: todoWriteTool,
	}
}

// Execute 函数是 TodoWriteTool 的核心执行逻辑。

// Execute 函数的核心逻辑流，不仅涵盖了数据处理（解析、提取），还深刻理解了双重输出：
// 人类可读的报告用于引导 LLM 和展示给用户，机器可读的数据用于前端渲染和程序逻辑）的设计意图。
// 在 Agent 开发中，这种引导 LLM 的做法被称为 “交互式状态提示”，通过返回一段带有警告意味的文本，强行把 LLM 拉回到工作流中，防止它在收集齐信息之前就“跳步”去给用户写答案。

// 1. 参数解析
// 2. 提取步骤列表，每个步骤包含 ID、Description 和 Status（pending/in_progress/completed）。
// 3. 生成人类可读的报告，包含：
//	- 任务标题。
//	- 带状态图标（⏳/🔄/✅）的步骤列表。
//	- 进度统计（已完成 X 个，剩余 Y 个）。
//	- 动态指令: 如果任务没做完，它会用粗体警告“必须完成所有任务才能总结”，这是为了引导 LLM 在下一轮对话中继续执行任务。
// 4. 生成机器可读的结构化数据，包含：
//	- 任务名
//	- 任务步骤
//	- 任务步骤的 JSON 字符串
//	- 步骤总数
//	- 是否为新创建的计划
//	- 前端渲染提示

// TodoWriteTool 在什么时候被调用？
//
// 它不是由代码逻辑“硬编码”触发的，而是由大模型根据对话上下文自主决定什么时候调用的。
//
//	1. 任务启动：创建规划（复杂任务开始时）
//	当用户提出的问题无法通过一次搜索解决，需要拆解时。
//
//	用户输入：“我想了解 DeepSeek-V3 的技术架构，并将其与 GPT-4o 进行对比。”
//	LLM 动作：调用 todo_write 创建初始清单。
//	调用参数示例：
//		{
//		 	"task": "对比 DeepSeek-V3 与 GPT-4o 的技术架构",
//		 	"steps": [
//		   		{"id": "s1", "description": "在知识库中搜索 DeepSeek-V3 的架构文档", "status": "in_progress"},
//		   		{"id": "s2", "description": "使用联网搜索获取 GPT-4o 的最新技术参数", "status": "pending"},
//		   		{"id": "s3", "description": "检索两者在 MoE 架构实现上的差异点", "status": "pending"}
//		 	]
//		}
//
//
//	2. 任务流转：标记进度（完成某一项检索后）
//	当 LLM 刚执行完一个搜索动作，并拿到了有效信息，需要“打勾”时。
//
//	场景回放：LLM 刚刚执行了 knowledge_search 并拿到了 DeepSeek-V3 的 PDF 摘要。
//	LLM 动作：调用 todo_write 更新状态，并开启下一个任务。
//	调用参数示例：
//		{
//		 	"task": "对比 DeepSeek-V3 与 GPT-4o 的技术架构",
//		 	"steps": [
//		   		{"id": "s1", "description": "在知识库中搜索 DeepSeek-V3 的架构文档", "status": "completed"},
//		   		{"id": "s2", "description": "使用联网搜索获取 GPT-4o 的最新技术参数", "status": "in_progress"},
//		   		{"id": "s3", "description": "检索两者在 MoE 架构实现上的差异点", "status": "pending"}
//		 	]
//		}
//
//
//	3. 动态调整：增加新任务（发现意外线索时）
//	当在检索过程中发现了一个之前没想到的关键点，需要临时加戏。
//
//	场景回放：在搜索 GPT-4o 时，LLM 发现其“推理成本”是个关键对比项，但原计划没写。
//	LLM 动作：调用 todo_write 在末尾插入一个新步骤。
//	调用参数示例：
//		{
//		 	"task": "对比 DeepSeek-V3 与 GPT-4o 的技术架构",
//		 	"steps": [
//		   		{"id": "s1", "description": "...", "status": "completed"},
//		   		{"id": "s2", "description": "...", "status": "completed"},
//		   		{"id": "s3", "description": "...", "status": "in_progress"},
//		   		{"id": "s4", "description": "专门检索两者的 Token 推理成本数据", "status": "pending"} （注：s4 是新加的）
//		 	]
//		}
//
//
//	4. 任务完结：最后确认（准备生成答案前）
//	当所有检索动作结束，LLM 在输出最终回答前，最后更新一次列表，给自己一个“可以总结”的信号。
//
//	场景回放：所有信息已到齐。
//	LLM 动作：调用 todo_write 将最后一项标为 completed。
//	工具返回：此时工具会返回 “✅ 所有任务已完成！现在可以综合所有发现”。
//	结果：LLM 看到这个返回，就会放心地开始写长篇大论的总结报告了。

// TodoWriteTool 好像啥也没做啊？
//
// 从代码逻辑上看，它确实“啥也没干”：它不查数据库、不调 AI 接口、不写文件，只是把传进来的参数换个格式又吐了回去。
// 在 Agent 系统中，它的价值不在于“干活”，而在于“控盘”。作为一个元工具（Meta-Tool），其核心作用是改变 LLM 的行为模式。
//
//
// 它有三个非常隐蔽但关键的特殊作用：
//
// 1. 它是一个“强制刹车片”
//	如果没有这个工具，大模型（LLM）有一个天性：急于求成。
//	不带 TodoTool：用户问一个复杂问题，LLM 可能会搜一下，然后就开始凭感觉胡思乱想，最后编一个答案给你。
//	带了 TodoTool：LLM 在执行动作前，必须先调用这个工具。工具返回的 Output（“还有 3 个任务未完成！”）像复读机一样钉在上下文里。这会强制 LLM 的下一轮预测进入“检索模式”而不是“总结模式”。
//
//	2. 它是 AI 的“外置短期记忆”
//	大模型虽然有很长的上下文窗口，但在处理多任务时容易“丢包”。
//
//	如果你让它执行 5 个搜索任务，搜到第 3 个时，它可能已经忘了后面 2 个是要干嘛了。
//	TodoWriteTool 强制让清单在每一轮对话中都“重现”一次。
//	这相当于给 AI 贴了一个便利贴，让它每走一步都能回头看一眼：“哦，我还没做完，还得继续搜。”
//
//	3. 它是前端的“渲染桩”
//	对于开发者（比如你）来说，这个工具最大的意义在于它的 display_type: "plan"：
//
//	如果后端只返回搜索到的结果，用户看到的只是屏幕上跳出几段文字。
//	有了这个工具，前端界面可以抓取到 steps 数组，在用户界面上画出一个漂亮的进度条、复选框或状态看板。
//	用户会觉得“这个 AI 好专业，它正在认真分步骤帮我查”，而不是在盲目等待。

// 大模型的任务执行状态，存在哪里？
//
// 从代码实现来看，TodoWriteTool 确实没有存储任何状态。
// 这是因为 LLM Agent 系统与传统后端应用不同，其状态没有存储在数据库里，而是存储在“对话历史”中。
// 在 Agent 的对话中，“上下文”就是它的硬盘。
//	第一轮：LLM 调用 todo_write 传了 3 个 pending 任务，工具返回了格式化的列表。
//	框架：这个列表被添加到对话历史中。
//	第二轮：当 LLM 完成了一个任务，它会翻看之前的对话历史，找到那个列表，自己脑补出更新后的版本，然后再次调用 todo_write 把全量更新后的列表发过来。
//	框架：这个新列表被添加到对话历史中。
//
// 这样，状态一直存在于 LLM 的“记忆”和对话历史中。
// 因此，TodoWriteTool 只是一个状态加工厂，而不是存储仓库。
//
// 这种设计的优缺点
//	优点
//		- 无状态服务（Stateless Service）：工具本身不需要连接数据库，不需要 Redis，部署极其简单，扩展性极强。
//		- 透明性（Transparency）：状态对 LLM 和用户都是完全可见的。LLM 不会“偷偷”修改状态，所有的状态变更都必须通过生成明确的文本来完成，这增加了可解释性。
//		- 自我修正：如果历史记录里的状态错了，LLM 可以通过阅读上下文发现矛盾，并主动调用工具修正它。
//	缺点
//		- 上下文长度限制：如果任务非常多，或者对话轮次非常多，历史记录会越来越长，最终可能超出 LLM 的 Context Window 限制，导致早期的状态信息被“遗忘”（截断）。
//		- 成本：每次调用都要把包含状态的历史记录重新传给 LLM，Token 消耗量大。
//		- 非持久化：一旦对话会话结束（Session Reset），状态就彻底消失了，除非外部系统专门把历史记录存下来。

// Execute executes the todo_write tool
func (t *TodoWriteTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input TodoWriteInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	if input.Task == "" {
		input.Task = "未提供任务描述"
	}

	// Parse plan steps
	planSteps := input.Steps

	// Generate formatted output
	output := generatePlanOutput(input.Task, planSteps)

	// Prepare structured data for response
	stepsJSON, _ := json.Marshal(planSteps)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"task":         input.Task,
			"steps":        planSteps,
			"steps_json":   string(stepsJSON),
			"total_steps":  len(planSteps),
			"plan_created": true,
			"display_type": "plan",
		},
	}, nil
}

// Helper function to safely get string field from map
func getStringField(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

// Helper function to safely get string array field from map
func getStringArrayField(m map[string]interface{}, key string) []string {
	if val, ok := m[key].([]interface{}); ok {
		result := make([]string, 0, len(val))
		for _, item := range val {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}
	// Handle legacy string format for backward compatibility
	if val, ok := m[key].(string); ok && val != "" {
		return []string{val}
	}
	return []string{}
}

// generatePlanOutput generates a formatted plan output
func generatePlanOutput(task string, steps []PlanStep) string {
	output := "计划已创建\n\n"
	output += fmt.Sprintf("**任务**: %s\n\n", task)

	if len(steps) == 0 {
		output += "注意：未提供具体步骤。建议创建3-7个检索任务以系统化研究。\n\n"
		output += "建议的检索流程（专注于检索任务，不包含总结）：\n"
		output += "1. 使用 grep_chunks 搜索关键词定位相关文档\n"
		output += "2. 使用 knowledge_search 进行语义搜索获取相关内容\n"
		output += "3. 使用 list_knowledge_chunks 获取关键文档的完整内容\n"
		output += "4. 使用 web_search 获取补充信息（如需要）\n"
		output += "\n注意：总结和综合由 thinking 工具处理，不要在此处添加总结任务。\n"
		return output
	}

	// Count task statuses
	pendingCount := 0
	inProgressCount := 0
	completedCount := 0
	for _, step := range steps {
		switch step.Status {
		case "pending":
			pendingCount++
		case "in_progress":
			inProgressCount++
		case "completed":
			completedCount++
		}
	}
	totalCount := len(steps)
	remainingCount := pendingCount + inProgressCount

	output += "**计划步骤**:\n\n"

	// Display all steps in order
	for i, step := range steps {
		output += formatPlanStep(i+1, step)
	}

	// Add summary and emphasis on remaining tasks
	output += "\n=== 任务进度 ===\n"
	output += fmt.Sprintf("总计: %d 个任务\n", totalCount)
	output += fmt.Sprintf("✅ 已完成: %d 个\n", completedCount)
	output += fmt.Sprintf("🔄 进行中: %d 个\n", inProgressCount)
	output += fmt.Sprintf("⏳ 待处理: %d 个\n", pendingCount)

	output += "\n=== ⚠️ 重要提醒 ===\n"
	if remainingCount > 0 {
		output += fmt.Sprintf("**还有 %d 个任务未完成！**\n\n", remainingCount)
		output += "**必须完成所有任务后才能总结或得出结论。**\n\n"
		output += "下一步操作：\n"
		if inProgressCount > 0 {
			output += "- 继续完成当前进行中的任务\n"
		}
		if pendingCount > 0 {
			output += fmt.Sprintf("- 开始处理 %d 个待处理任务\n", pendingCount)
			output += "- 按顺序完成每个任务，不要跳过\n"
		}
		output += "- 完成每个任务后，更新 todo_write 标记为 completed\n"
		output += "- 只有在所有任务完成后，才能生成最终总结\n"
	} else {
		output += "✅ **所有任务已完成！**\n\n"
		output += "现在可以：\n"
		output += "- 综合所有任务的发现\n"
		output += "- 生成完整的最终答案或报告\n"
		output += "- 确保所有方面都已充分研究\n"
	}

	return output
}

// formatPlanStep formats a single plan step for output
func formatPlanStep(index int, step PlanStep) string {
	statusEmoji := map[string]string{
		"pending":     "⏳",
		"in_progress": "🔄",
		"completed":   "✅",
		"skipped":     "⏭️",
	}

	emoji, ok := statusEmoji[step.Status]
	if !ok {
		emoji = "⏳"
	}

	output := fmt.Sprintf("  %d. %s [%s] %s\n", index, emoji, step.Status, step.Description)

	// if len(step.ToolsToUse) > 0 {
	// 	output += fmt.Sprintf("     工具: %s\n", strings.Join(step.ToolsToUse, ", "))
	// }

	return output
}
