package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// SequentialThinkingTool 是一个元认知工具。
// 它不直接解决问题（如搜索网络或运行代码），而是为 AI 提供一个结构化的画板，让它能够慢下来，有条理地、可追溯地推演复杂问题的解决方案。
// 这对于提高 AI 在处理数学题、代码架构设计、逻辑推理等任务上的准确率至关重要。

// 通常 AI 是直接输出答案的，但对于复杂问题（如架构设计、多步故障排除），直接输出容易出错。
// 这个工具允许 AI 在执行动作前，先进行显式的、可修正的思考。
// 其核心逻辑是 Chain of Thought (思维链) 的工程化实现。

// SequentialThinkingInput 不仅仅是一个简单的输入包，它是一个状态机控制器。
//	通过 ThoughtNumber/TotalThoughts 管理进度。
//	通过 NextThoughtNeeded 控制生命周期。
//	通过 IsRevision/RevisesThought 实现自我纠错。
//	通过 Branch... 字段实现并行探索。
// 这种设计使得 AI 能够模拟人类专家在面对难题时那种“走一步看一步”、“随时调整策略”、“多角度验证”的高级认知过程。

// 和一般的“查询工具”（比如搜索、天气）不同，一般的工具是 AI 查资料，而这个工具是 AI 存档自己的大脑状态。
// AI 不是一次性调完这个工具就结束了，它通常会经历：想一步 -> 调工具记录 -> 工具回馈“请继续” -> 再想一步。

// 	场景模拟：AI 解决“如何计算 1 到 100 的和”
//	假设用户问：“请帮我算一下 1 加到 100 等于多少？”，AI 决定使用 sequential_thinking 工具来展示它的推导过程。
//
//  ==> 第 1 轮交互：启动思考
//	(a) AI 的内心活动: “这个问题很简单，但我应该展示步骤。我大概需要 3 步：1.识别公式，2.代入数值，3.计算结果。”
//
//	(b) AI 发出的工具调用 (Tool Call):
//		{
//  		"name": "sequential_thinking",
//  		"arguments": {
//    			"thought": "首先，我需要识别这是一个等差数列求和问题。高斯求和公式是 (首项 + 末项) * 项数 / 2。",
//    			"next_thought_needed": true,
//    			"thought_number": 1,
//    			"total_thoughts": 3
//  		}
//		}
//		(注意：next_thought_needed 是 true，表示还没完)
//
//
//	(c) Tool (Go 代码) 的执行:
//		1. 解析 JSON ，验证通过。
//		2. 将这条记录存入 thoughtHistory 列表（现在列表长度为 1）。
//		3. 检查：thought_number (1) 没有超过 total_thoughts (3)，需要继续思考。
//		4. 判断状态：因为 next_thought_needed 是 true，所以 incomplete = true。
//		5. 返回结果给 AI:
//
//		{
//		 	"success": true,
//		 	"output": "Thought process recorded - unfinished steps remain, continue exploring and calling tools",
//		 	"data": {
//		   		"thought_number": 1,
//		   		"total_thoughts": 3,
//		   		"thought_history_length": 1,
//		   		"incomplete_steps": true
//		 	}
//		}
//
//	(d) AI 收到反馈: “好的，第一步记下来了。系统提示我还有未完成步骤，我该进行第二步了。”
//
// 	==> 第 2 轮交互：深入与修正（关键点）
//	AI 的内心活动: “等等，我直接套公式是不是太跳跃了？我应该先确认项数是多少。而且，万一用户问的是 1 到 99 呢？我要修正一下我的思路，强调项数的确认。”
//	AI 发出的工具调用:
//		{
//		 	"name": "sequential_thinking",
//		 	"arguments": {
//		   		"thought": "修正一下：在套用公式前，必须明确项数 n=100。如果是 1 到 99，结果会完全不同。确认本题 n=100。",
//		   		"next_thought_needed": true,
//		   		"thought_number": 2,
//		   		"total_thoughts": 4,
//		   		"is_revision": true,
//		   		"revises_thought": 1
//		 	}
//		}
//		(注意：AI 发现需要多讲一点，把 total_thoughts 从 3 改成了 4。并且标记这是对第 1 步的修订)
//
//
//	Tool (Go 代码) 的执行:
//		解析 JSON。
//		动态调整逻辑触发: 代码检查 if input.ThoughtNumber (2) > input.TotalThoughts (4)? 不成立，保持 4。(如果 AI 写的是 total=1，这里会被代码强制改成 2)。
//		存入 thoughtHistory（现在长度为 2）。
//		检测到 is_revision: true，虽然代码主要做记录，但这个标记会让前端或后续逻辑知道这是在“回滚/修正”。
//		返回结果给 AI:
//			{
//			 	"success": true,
//			 	"output": "Thought process recorded - unfinished steps remain...",
//			 	"data": {
//			   		"thought_number": 2,
//			   		"total_thoughts": 4,
//			   		"thought_history_length": 2,
//			   		"incomplete_steps": true
//			 	}
//			}
//
//	AI 收到反馈: “收到，修订已记录。目前进度 2/4。继续。”
//
// 	==> 第 3 轮交互：计算与分支（可选）
//	AI 的内心活动: “好，现在代入公式：(1+100)*100/2 = 5050。为了保险，我可以用另一种方法（循环累加）验证一下吗？算了，太麻烦，直接出结果吧。”
//	AI 发出的工具调用:
//
//		{
//		 	"name": "sequential_thinking",
//		 	"arguments": {
//		   		"thought": "代入公式：(1 + 100) * 100 / 2 = 101 * 50 = 5050。计算无误。",
//		   		"next_thought_needed": false,
//		   		"thought_number": 3,
//		   		"total_thoughts": 4
//		 	}
//		}
//		(注意：next_thought_needed 变成了 false，表示 AI 认为想完了)
//
//	Tool (Go 代码) 的执行:
//		存入历史（长度 3）。
//		判断状态：
//			next_thought_needed = false
//			needs_more_thoughts = false (默认)
//			thought_number (3) < total_thoughts (4) -> True!
//		关键点: 虽然 AI 说“不需要下一步了”，但因为当前步数 (3) 还没达到它自己预估的总数 (4)，代码逻辑判定 incomplete 依然可能是 true 。
//		返回结果: "unfinished steps remain..." (系统提示：你计划了 4 步，才走了 3 步，真的结束了吗？)
//
//
// 	==> 第 4 轮交互：收尾总结
//	收到第三轮 tool 的回复之后，AI 可能会意识到：“哦，我计划了 4 步，但我只用了 3 步就想完了。我可以把总步数改成 3，或者再补一步总结。
//	假设 AI 选择补一步总结:
//
// 	AI 的内心活动: “既然还差一步，那我就做个最终总结吧。”
//	AI 发出的工具调用:
//		{
//		 	"name": "sequential_thinking",
//		 	"arguments": {
//		   		"thought": "总结：1 到 100 的和是 5050。思考过程完整。",
//		   		"next_thought_needed": false,
//		   		"thought_number": 4,
//		   		"total_thoughts": 4
//		 	}
//		}
//
//	Tool (Go 代码) 的执行:
//		存入历史（长度 4）。
//		判断状态：
//			next_thought_needed = false
//			thought_number (4) < total_thoughts (4) -> False
//		结果: incomplete = false。
//		返回结果:
//			{
//			 	"success": true,
//			 	"output": "Thought process recorded",
//			 	"data": {
//					"incomplete_steps": false
//			 	}
//			}
//
//	AI 收到反馈: “记录成功，没有未完成步骤了。”
//	AI 的最终行动: AI 看到 incomplete_steps: false，于是停止调用工具，直接向用户输出最终答案：“经过详细推导，1 加到 100 等于 5050。”

// 为什么 is_revision ？
//
//	大模型（LLM）有一个致命弱点：一旦它开始说话，就很难回头。
//	如果没有这个字段：如果 AI 在第 1 步想错了，它只能在第 2 步强行寻找理由去“圆”第 1 步的错误（这就是幻觉产生的原因）。
//	有了 is_revision：AI 可以在第 2 步理直气壮地说：“标记为修订：我发现第 1 步的假设有误，现在重新设定逻辑。”
//
//  is_revision 的核心作用是告诉系统：“我现在的这一步思考，不是在向前推进，而是在回头修正之前的某个错误。”
// 	它直译是修正，也可以理解为写作软件中的 “撤销 + 重写”，或者编程中的 “回滚” 操作。
//  它让思维过程从一条死板的直线，变成了一个可以不断自我优化的动态网络。
//
// 	在没有 is_revision 的普通线性思维中，AI 的思维链条是单向的：
//		第1步 -> 第2步 -> 第3步 (发现第1步错了) -> 第4步 (基于错误的第1步继续错下去...)
//	这会导致“一步错，步步错”。
//	有了 is_revision，AI 可以打断线性流程，跳回过去：
//		第1步 (错了) -> 第2步 -> 第3步 (发现第1步错了) -> 第4步 (标记为 revision，修正第1步) -> 第5步 (基于修正后的逻辑继续)
//
// 	当 AI 决定使用 is_revision 时，必须同时配合另一个字段 revises_thought。
//		- is_revision: true: 声明“我在修错”。
//		- revises_thought: <int>: 指定“我要修正的是第几步”。

// AI 如何决定何时调用这个 tool ？
//
// 	核心机制：System Prompt 中的“路由指令”
//		在构建 AI 应用时，开发者会在 System Prompt（系统预设指令）中明确写入类似以下的规则：
//
//		System Prompt 示例：
//			你是一个智能助手。
//			在回答用户问题之前，请先评估问题的复杂度：
//			  - 简单问题（如事实查询、简单问候、简短翻译）：直接回答，不要调用任何工具。
//			  - 复杂问题（如数学推理、代码生成、逻辑谜题、多步骤规划、需要自我纠错的任务）：必须先调用 sequential_thinking 工具。
//			在调用工具时，请拆解你的思路，逐步推导。如果你发现之前的推导有误，请使用 is_revision 进行修正。
//
//		AI 的工作流程如下：
//			接收用户输入：用户问了一个问题。
//			内部评估（隐式思考）：AI 会根据训练数据和 System Prompt，瞬间判断：“这个问题难不难？需不需要打草稿？”
//			 - 如果是“今天天气如何？” -> 判定为简单 -> 跳过工具，直接生成回答。
//			 - 如果是“帮我设计一个高并发秒杀系统” -> 判定为复杂 -> 触发工具调用。
//			生成工具调用请求：如果判定需要调用工具，AI 会停止生成最终答案，转而生成一个特殊的 JSON 块（Tool Call），启动 sequential_thinking。
//
//
// 	微调机制：除了 Prompt 工程，高级的 AI 系统还会通过微调来强化这种行为：
//		监督微调 (SFT)：使用大量“复杂问题 -> 调用工具 -> 完美答案”的数据对模型进行训练。让模型学会：“遇到这类问题，调用工具是‘正确’的行为”。
//		强化学习 (RLHF/RLAIF)：
//			- 如果模型直接回答复杂问题但出错了 -> 给予惩罚。
//			- 如果模型调用工具，经过多步思考后答对了 -> 给予奖励。
//		久而久之，模型会内化这种能力，即使没有显式的 Prompt 提醒，它在面对难题时也会“下意识”地想要调用工具。
//
//
// 	有时候模型会“偷懒”或者误判，直接判断为简单问题。这时候有两种补救措施：
//		- 后端拦截 (Middleware)：
//		  	系统检测到用户问题是复杂的（通过另一个小模型或规则分类器），但主模型没有调用工具。
//			系统可以强行中断回答，并回复：“检测到该问题较复杂，建议您引导我使用顺序思维工具进行推导。”，
//			或者直接自动帮用户补上一个“请先使用 sequential_thinking 工具”的指令。
//		- 动态 Prompt 注入：
//			在对话过程中，如果发现模型开始胡言乱语，系统可以动态插入一条 System Message：
//			“你现在的回答似乎缺乏逻辑，请暂停，调用 sequential_thinking 重新梳理思路。”

// AI 是如何“感知”复杂度的？
//
//	大模型经过海量数据训练，已经具备了很强的直觉判断力。它不需要显式地计算“难度分数”，而是通过模式匹配来决策：
//		- 关键词触发：看到“证明”、“设计”、“优化”、“调试”、“策划”、“多步骤”等词汇，模型会倾向于调用工具。
//		- 上下文长度与依赖：如果问题需要记住很多前置条件，或者步骤之间环环相扣，模型会意识到“内存不够用”或“容易出错”，从而主动寻求外部存储（即 Tool）。
//		- 不确定性检测：当模型在生成第一个 token 时，如果预测的概率分布比较平坦（即它不太确定下一个词是什么），这通常意味着它在“犹豫”。在这种状态下，配合良好的 Prompt 工程，模型更有可能选择“先思考再回答”。
//
// 具体来讲：
//	1. 语义特征匹配（关键词与意图识别）
//		AI 在处理输入的第一时间，会扫描文本中的“高复杂度特征”。
//		- 指令动词：如果用户用了“设计”、“重构”、“论证”、“诊断”、“对比”这类词，AI 内部的权重会立刻向“高难度任务”倾斜。
//		- 领域深度：如果问题涉及“分布式系统”、“量子物理”、“法律合规”等专业领域，AI 在训练中学习到的统计概率会告诉它：这类问题的关联 token 极其广泛，不能简单输出。
//		- 长度与约束：如果用户提出了很多限制条件（“既要...又要...且不能...”），这些约束在 AI 的注意力机制（Attention）中会产生复杂的相互作用，触发其“进入慎重模式”的概率。
//
// 	2. 预测的不确定性（熵值变化）
//		在 LLM 生成 Token 的过程中，有一个概念叫 “困惑度 (Perplexity)” 或 “熵 (Entropy)”。
//		- 低复杂度：当用户问“1+1等于几”或“你好”，AI 预测下一个 Token 的概率分布非常集中（比如 99% 的概率是“2”）。这时 AI 感到很“自信”，直接输出。
//		- 高复杂度：当面对复杂问题时，AI 发现接下来的路径有无数种可能（概率分布非常扁平），没有任何一个路径有绝对胜算。
//		这种预测上的“犹豫”会让它意识到：“我无法一步到位生成完美答案，我需要借助 sequential_thinking 来稳定我的预测路径。”
//
//	3. 系统提示词（System Prompt）的逻辑预设
//		这是 AI 感知复杂度的“外部传感器”。
//		开发者通常会在提示词里建立一套判定逻辑："如果问题包含 3 个以上的子任务，或者涉及跨学科知识，请务必先调用思维工具。"
//		AI 就像执行代码一样，会对用户问题进行前置扫描。
//		当它发现问题符合“复杂性定义”的条件，便会主动触发工具。
//
//	4. “贪婪”与“长程依赖”的博弈
//		大模型在生成答案时，是逐词生成的。
//		在生成第一个词之前，模型会进行一次全局扫描（KV Cache 处理）。
//		如果模型预见到（通过自注意力机制预测）要完成这个任务需要跨越很长的上下文依赖，或者逻辑链条极长，它会倾向于先开启一个“思考缓冲区”。
//		而 sequential_thinking 工具就是这个缓冲区。
//		AI 调用它，本质上是给自己腾出空间，把庞大的推理压力拆散到多个交互轮次中。

// 交互流程示例
//
//	假设用户问：“请用 Python 写一个快速排序算法，并解释其时间复杂度。”
//
//	阶段一：决策 (Decision)
//		User: (输入问题)
//		LLM (内部): “这是一个编程 + 理论解释的任务。直接写可能会漏掉边界条件，或者复杂度解释不清楚。根据 System Prompt，我应该先拆解思路。”
//		LLM (输出): [TOOL_CALL] sequential_thinking (而不是直接输出代码)
//
//	阶段二：执行 (Execution via Tool)
//		LLM: 发送 Thought 1 (“快速排序的核心是分治法...”)
//		Tool: 返回 “记录成功，请继续”
//		LLM: 发送 Thought 2 (“基准值的选择很重要，通常选中间值...”)
//		Tool: 返回 “记录成功”
//		LLM: 发送 Thought 3 (“等等，递归终止条件容易写错，我要仔细检查一下...”) -> 这里可能触发 revision
//		...
//		LLM: 发送 Thought N (“思路清晰了，可以开始写代码了”), next_thought_needed: false
//		Tool: 返回 “思考结束”
//
//	阶段三：生成最终答案 (Final Generation)
//		LLM: 接收到工具结束的反馈，结合刚才记录在上下文中的所有思维步骤，生成最终的、高质量的 Python 代码和解释。

var sequentialThinkingTool = BaseTool{
	name: ToolThinking,
	description: `A detailed tool for dynamic and reflective problem-solving through thoughts.

This tool helps analyze problems through a flexible thinking process that can adapt and evolve.

Each thought can build on, question, or revise previous insights as understanding deepens.

## When to Use This Tool

- Breaking down complex problems into steps
- Planning and design with room for revision
- Analysis that might need course correction
- Problems where the full scope might not be clear initially
- Problems that require a multi-step solution
- Tasks that need to maintain context over multiple steps
- Situations where irrelevant information needs to be filtered out

## Key Features

- You can adjust total_thoughts up or down as you progress
- You can question or revise previous thoughts
- You can add more thoughts even after reaching what seemed like the end
- You can express uncertainty and explore alternative approaches
- Not every thought needs to build linearly - you can branch or backtrack
- Generates a solution hypothesis
- Verifies the hypothesis based on the Chain of Thought steps
- Repeats the process until satisfied
- Provides a correct answer

## Parameters Explained

- **thought**: Your current thinking step, which can include:
  * Regular analytical steps
  * Revisions of previous thoughts
  * Questions about previous decisions
  * Realizations about needing more analysis
  * Changes in approach
  * Hypothesis generation
  * Hypothesis verification
  
  **CRITICAL - User-Friendly Thinking**: Write your thoughts in natural, user-friendly language. NEVER mention tool names (like "grep_chunks", "knowledge_search", "web_search", etc.) in your thinking process. Instead, describe your actions in plain language:
  - ❌ BAD: "I'll use grep_chunks to search for keywords, then knowledge_search for semantic understanding"
  - ✅ GOOD: "I'll start by searching for key terms in the knowledge base, then explore related concepts"
  - ❌ BAD: "After grep_chunks returns results, I'll use knowledge_search"
  - ✅ GOOD: "After finding relevant documents, I'll search for semantically related content"
  
  Write thinking as if explaining your reasoning to a user, not documenting technical steps. Focus on WHAT you're trying to find and WHY, not HOW (which tools you'll use).

- **next_thought_needed**: True if you need more thinking, even if at what seemed like the end
- **thought_number**: Current number in sequence (can go beyond initial total if needed)
- **total_thoughts**: Current estimate of thoughts needed (can be adjusted up/down)
- **is_revision**: A boolean indicating if this thought revises previous thinking
- **revises_thought**: If is_revision is true, which thought number is being reconsidered
- **branch_from_thought**: If branching, which thought number is the branching point
- **branch_id**: Identifier for the current branch (if any)
- **needs_more_thoughts**: If reaching end but realizing more thoughts needed

## Best Practices

1. Start with an initial estimate of needed thoughts, but be ready to adjust
2. Feel free to question or revise previous thoughts
3. Don't hesitate to add more thoughts if needed, even at the "end"
4. Express uncertainty when present
5. Mark thoughts that revise previous thinking or branch into new paths
6. Ignore information that is irrelevant to the current step
7. Generate a solution hypothesis when appropriate
8. Verify the hypothesis based on the Chain of Thought steps
9. Repeat the process until satisfied with the solution
10. Provide a single, ideally correct answer as the final output
11. Only set next_thought_needed to false when truly done and a satisfactory answer is reached`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "thought": {
      "type": "string",
      "description": "Your current thinking step. Write in natural, user-friendly language. NEVER mention tool names (like \"grep_chunks\", \"knowledge_search\", \"web_search\", etc.). Instead, describe actions in plain language (e.g., \"I'll search for key terms\" instead of \"I'll use grep_chunks\"). Focus on WHAT you're trying to find and WHY, not HOW (which tools you'll use)."
    },
    "next_thought_needed": {
      "type": "boolean",
      "description": "Whether another thought step is needed"
    },
    "thought_number": {
      "type": "integer",
      "description": "Current thought number (numeric value, e.g., 1, 2, 3)",
      "minimum": 1
    },
    "total_thoughts": {
      "type": "integer",
      "description": "Estimated total thoughts needed (numeric value, e.g., 5, 10)",
      "minimum": 5
    },
    "is_revision": {
      "type": "boolean",
      "description": "Whether this revises previous thinking"
    },
    "revises_thought": {
      "type": "integer",
      "description": "Which thought is being reconsidered",
      "minimum": 1
    },
    "branch_from_thought": {
      "type": "integer",
      "description": "Branching point thought number",
      "minimum": 1
    },
    "branch_id": {
      "type": "string",
      "description": "Branch identifier"
    },
    "needs_more_thoughts": {
      "type": "boolean",
      "description": "If more thoughts are needed"
    }
  },
  "required": ["thought", "next_thought_needed", "thought_number", "total_thoughts"]
}`),
}

// SequentialThinkingTool is a dynamic and reflective problem-solving tool
// This tool helps analyze problems through a flexible thinking process that can adapt and evolve
type SequentialThinkingTool struct {
	BaseTool
	thoughtHistory []SequentialThinkingInput
	branches       map[string][]SequentialThinkingInput
}

// SequentialThinkingInput defines the input parameters for sequential thinking tool
type SequentialThinkingInput struct {
	Thought           string `json:"thought"`
	NextThoughtNeeded bool   `json:"next_thought_needed"`
	ThoughtNumber     int    `json:"thought_number"`
	TotalThoughts     int    `json:"total_thoughts"`
	IsRevision        bool   `json:"is_revision,omitempty"`
	RevisesThought    *int   `json:"revises_thought,omitempty"`
	BranchFromThought *int   `json:"branch_from_thought,omitempty"`
	BranchID          string `json:"branch_id,omitempty"`
	NeedsMoreThoughts bool   `json:"needs_more_thoughts,omitempty"`
}

// NewSequentialThinkingTool creates a new sequential thinking tool instance
func NewSequentialThinkingTool() *SequentialThinkingTool {
	return &SequentialThinkingTool{
		BaseTool:       sequentialThinkingTool,
		thoughtHistory: make([]SequentialThinkingInput, 0),
		branches:       make(map[string][]SequentialThinkingInput),
	}
}

// Execute executes the sequential thinking tool
func (t *SequentialThinkingTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][SequentialThinking] Execute started")

	// Parse args from json.RawMessage
	var input SequentialThinkingInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][SequentialThinking] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Validate and parse input
	if err := t.validate(input); err != nil {
		logger.Errorf(ctx, "[Tool][SequentialThinking] Validation failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Validation failed: %v", err),
		}, err
	}

	// Adjust totalThoughts if thoughtNumber exceeds it
	if input.ThoughtNumber > input.TotalThoughts {
		input.TotalThoughts = input.ThoughtNumber
	}

	// Add to thought history
	t.thoughtHistory = append(t.thoughtHistory, input)

	// Handle branching
	if input.BranchFromThought != nil && input.BranchID != "" {
		if t.branches[input.BranchID] == nil {
			t.branches[input.BranchID] = make([]SequentialThinkingInput, 0)
		}
		t.branches[input.BranchID] = append(t.branches[input.BranchID], input)
	}

	logger.Debugf(ctx, "[Tool][SequentialThinking] %s", input.Thought)

	// Prepare response data
	branchKeys := make([]string, 0, len(t.branches))
	for k := range t.branches {
		branchKeys = append(branchKeys, k)
	}

	incomplete := input.NextThoughtNeeded || input.NeedsMoreThoughts || input.ThoughtNumber < input.TotalThoughts

	responseData := map[string]interface{}{
		"thought_number":         input.ThoughtNumber,
		"total_thoughts":         input.TotalThoughts,
		"next_thought_needed":    input.NextThoughtNeeded,
		"branches":               branchKeys,
		"thought_history_length": len(t.thoughtHistory),
		"display_type":           "thinking",
		"thought":                input.Thought,
		"incomplete_steps":       incomplete,
	}

	logger.Infof(
		ctx,
		"[Tool][SequentialThinking] Execute completed - Thought %d/%d",
		input.ThoughtNumber,
		input.TotalThoughts,
	)

	outputMsg := "Thought process recorded"
	if incomplete {
		outputMsg = "Thought process recorded - unfinished steps remain, continue exploring and calling tools"
	}

	return &types.ToolResult{
		Success: true,
		Output:  outputMsg,
		Data:    responseData,
	}, nil
}

// validate validates the input thought data
func (t *SequentialThinkingTool) validate(data SequentialThinkingInput) error {
	// Validate thought (required)
	if data.Thought == "" {
		return fmt.Errorf("invalid thought: must be a non-empty string")
	}

	// Validate thoughtNumber (required)
	if data.ThoughtNumber < 1 {
		return fmt.Errorf("invalid thoughtNumber: must be >= 1")
	}

	// Validate totalThoughts (required)
	if data.TotalThoughts < 1 {
		return fmt.Errorf("invalid totalThoughts: must be >= 1")
	}

	return nil
}
