package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
)

// Memory 操作在 chat_pipline 中以 Plugin 插件 形式存在。
//
// 对话流程 (Pipeline)
//    │
//    ├── Plugin: MemoryPlugin
//    │      │
//    │      ├── MEMORY_RETRIEVAL (LLM 生成前)
//    │      │       │
//    │      │       └── handleRetrieval() → 召回记忆，注入 Prompt
//    │      │
//    │      └── MEMORY_STORAGE (LLM 生成后)
//    │              │
//    │              └── handleStorage() → 存储对话为 Episode
//    │
//    ├── 其他 Plugin...
//    │
//    └── LLM 生成回答

// 示例
//
// (1) 对话结束后的处理：
//	User: "RAG 是什么？"
//	Bot:  "RAG 是检索增强生成..."
//			│
//			▼
//	AddEpisode 被调用
//			│
//			▼
//	LLM 提取：
//	 Summary: "用户了解 RAG 的定义"
//	 Entities: [{"RAG", "概念"}, {"检索增强生成", "技术"}]
//	 Relationships: [{"RAG", "is-a", "检索增强生成"}]
//			│
//			▼
//	存入 Neo4j
//
// (2) 下次对话：
//	User: "RAG 和微调有什么区别？"
//			│
//			▼
//	RetrieveMemory
//			│
//			▼
//	LLM 提取关键词: ["RAG", "微调"]
//			│
//			▼
//	Neo4j 搜索 → 找到相关 Episode
//			│
//			▼
//	把历史经验注入 LLM Context
//			│
//			▼
//	Bot 回答: "RAG 是...，和微调的区别是..."

// 架构分析对比
//
// 现有 WeKnora memory
//  │
//  ├── 输入
//  │     对话内容（user + assistant）
//  │
//  ├── 写入方式
//  │     LLM 抽取：
//  │     - summary
//  │     - entities
//  │     - relationships
//  │
//  ├── 存储结构
//  │     Episode / Entity / Relation
//  │     （Neo4j 图谱式长期记忆）
//  │
//  ├── 读取方式
//  │     当前 query
//  │       -> LLM 提关键词
//  │       -> FindRelatedEpisodes
//  │       -> 返回相关 episode summary
//  │
//  ├── 注入方式
//  │     Relevant Memory:
//  │     - 2026-03-30 (Summary: ...)
//  │
//  ├── 主要价值
//  │     - 记住用户说过什么
//  │     - 记住用户经历/偏好/历史事件
//  │     - 做跨轮指代消解
//  │     - 做个性化回答增强
//  │
//  └── 本质
//        “对话记忆 / 用户记忆 / 情景记忆”

// 理想的经验型 agent memory
//  │
//  ├── 输入
//  │     不只是对话文本
//  │     还包括：
//  │     - thought
//  │     - tool_call
//  │     - tool_result
//  │     - reflection
//  │     - final outcome
//  │
//  ├── 写入方式
//  │     LLM / rule 总结：
//  │     - 什么策略成功了
//  │     - 什么工具组合有效
//  │     - 哪些参数容易失败
//  │     - 哪种任务该先做什么
//  │     - 失败后的经验教训
//  │
//  ├── 存储结构
//  │     不只是 Episode / Entity / Relation
//  │     还应有：
//  │     - Preference Memory
//  │     - Task Memory
//  │     - Procedural Memory
//  │     - Tool Usage Memory
//  │     - Failure / Recovery Memory
//  │
//  ├── 读取方式
//  │     当前 query / 当前任务类型 / 当前工具计划
//  │       -> 检索相关经验
//  │       -> 返回“可执行建议”
//  │
//  ├── 注入方式
//  │     不只是 Relevant Memory
//  │     还应有：
//  │     - Relevant Experience
//  │     - Recommended Strategy
//  │     - Known Failure Patterns
//  │     - Preferred Tool Sequence
//  │
//  ├── 主要价值
//  │     - 提高工具使用成功率
//  │     - 降低重复犯错
//  │     - 形成 agent 自己的工作方法
//  │     - 逐步优化复杂任务执行表现
//  │
//  └── 本质
//        “经验记忆 / 策略记忆 / 可执行记忆”

// WeKnora 当前的 memory 更偏“摘要型 / 情景型记忆”。
// 它主要记录用户说过什么、经历过什么，以及当前问题和哪些历史事件相关，
// 适合做用户历史召回、偏好记忆和跨轮上下文增强。
//
// 但理想的 agent memory 不应只记住“发生过什么”，
// 还应记住“这类任务以后怎么做更好”，例如：
//   - 哪种方法最有效
//   - 哪种工具组合更靠谱
//   - 上次失败原因是什么
//   - 下次应该避开什么
//
// 因此，后续可考虑将 memory 从“摘要型”升级为“经验型”，重点增强：
//   1. 结构化经验记录：保存 task type、tools used、success/failure、key insight、recommendation
//   2. 工具执行经验：记录每次 tool call 的效果、耗时、是否有用
//   3. 失败模式沉淀：归纳常见失败原因及更优替代方案
//   4. 检索改进：不仅召回 Relevant Memory，也召回 Best Methods / Pitfalls
//   5. 反思增强：让 reflection 产出结构化经验，而不只是自然语言文本
//   6. Agent 注入经验：在 Execute 前先检索类似任务经验，作为策略提示注入 Prompt
//
// 一句话：
// 当前 memory 解决“记住用户历史”，未来经验型 memory 要进一步解决“帮助 Agent 持续优化执行策略”。

// MemoryService implements the MemoryService interface
type MemoryService struct {
	repo         interfaces.MemoryRepository
	modelService interfaces.ModelService
}

// NewMemoryService creates a new memory service
func NewMemoryService(repo interfaces.MemoryRepository, modelService interfaces.ModelService) interfaces.MemoryService {
	return &MemoryService{
		repo:         repo,
		modelService: modelService,
	}
}

const extractGraphPrompt = `
You are an AI assistant that extracts knowledge graphs from conversations.
Given the following conversation, extract entities and relationships.
Output the result in JSON format with the following structure:
{
  "summary": "A brief summary of the conversation",
  "entities": [
    {
      "title": "Entity Name",
      "type": "Entity Type (e.g., Person, Location, Concept)",
      "description": "Description of the entity"
    }
  ],
  "relationships": [
    {
      "source": "Source Entity Name",
      "target": "Target Entity Name",
      "description": "Description of the relationship",
      "weight": 1.0
    }
  ]
}

Conversation:
%s
`

const extractKeywordsPrompt = `
You are an AI assistant that extracts search keywords from a user query.
Given the following query, extract relevant keywords for searching a knowledge graph.
Output the result in JSON format:
{
  "keywords": ["keyword1", "keyword2"]
}

Query:
%s
`

type extractionResult struct {
	Summary       string                `json:"summary" jsonschema:"a brief summary of the conversation"`
	Entities      []*types.Entity       `json:"entities"`
	Relationships []*types.Relationship `json:"relationships"`
}

type keywordsResult struct {
	Keywords []string `json:"keywords" jsonschema:"relevant keywords for searching a knowledge graph"`
}

func (s *MemoryService) getChatModel(ctx context.Context) (chat.Chat, error) {
	// Find the first available KnowledgeQA model
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %v", err)
	}

	var modelID string
	for _, model := range models {
		if model.Type == types.ModelTypeKnowledgeQA {
			modelID = model.ID
			break
		}
	}

	if modelID == "" {
		return nil, fmt.Errorf("no KnowledgeQA model found")
	}

	return s.modelService.GetChatModel(ctx, modelID)
}

// AddEpisode adds a new episode to the memory graph
func (s *MemoryService) AddEpisode(ctx context.Context, userID string, sessionID string, messages []types.Message) error {
	if !s.repo.IsAvailable(ctx) {
		return fmt.Errorf("memory repository is not available")
	}
	chatModel, err := s.getChatModel(ctx)
	if err != nil {
		return err
	}

	// 1. Construct conversation string
	var conversation string
	for _, msg := range messages {
		conversation += fmt.Sprintf("%s: %s\n", msg.Role, msg.Content)
	}

	// 2. Call LLM to extract graph
	prompt := fmt.Sprintf(extractGraphPrompt, conversation)
	resp, err := chatModel.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}}, &chat.ChatOptions{
		Format: utils.GenerateSchema[extractionResult](),
	})
	if err != nil {
		return fmt.Errorf("failed to call LLM: %v", err)
	}

	var result extractionResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return fmt.Errorf("failed to parse LLM response: %v", err)
	}

	// 3. Create Episode object
	episode := &types.Episode{
		ID:        uuid.New().String(),
		UserID:    userID,
		SessionID: sessionID,
		Summary:   result.Summary,
		CreatedAt: time.Now(),
	}

	// 4. Save to repository
	if err := s.repo.SaveEpisode(ctx, episode, result.Entities, result.Relationships); err != nil {
		return fmt.Errorf("failed to save episode: %v", err)
	}

	return nil
}

// RetrieveMemory retrieves relevant memory context based on the current query and user
func (s *MemoryService) RetrieveMemory(ctx context.Context, userID string, query string) (*types.MemoryContext, error) {
	if !s.repo.IsAvailable(ctx) {
		return nil, fmt.Errorf("memory repository is not available")
	}
	chatModel, err := s.getChatModel(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Extract keywords
	prompt := fmt.Sprintf(extractKeywordsPrompt, query)
	resp, err := chatModel.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}}, &chat.ChatOptions{
		Format: utils.GenerateSchema[keywordsResult](),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to call LLM: %v", err)
	}

	var result keywordsResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %v", err)
	}

	// 2. Retrieve related episodes
	episodes, err := s.repo.FindRelatedEpisodes(ctx, userID, result.Keywords, 5)
	if err != nil {
		return nil, fmt.Errorf("failed to find related episodes: %v", err)
	}

	// 3. Construct MemoryContext
	memoryContext := &types.MemoryContext{
		RelatedEpisodes: make([]types.Episode, len(episodes)),
	}
	for i, ep := range episodes {
		memoryContext.RelatedEpisodes[i] = *ep
	}

	return memoryContext, nil
}

// 经验型记忆系统改进说明
//
// 一、现状分析：现有 Memory 的能力与局限
// 当前 WeKnora 的 MemoryPlugin 实现的是「用户历史记忆」，它擅长回答：
//   - 用户以前说过什么？（UserQuery → History）
//   - 用户经历过什么事件？（Event → Episode）
//   - 这次问题和哪些历史有关？（Query → RelatedEpisodes）
// 存储结构：
//   Episode { ID, UserID, SessionID, Summary, CreatedAt }
//   Entity  { Title, Type, Description }
//   Relationship { Source, Target, Weight }
//
// 提取方式：对话结束后，用 LLM 从整个对话中提取 Summary
//
// 局限：
//   1. Summary 是「对话摘要」，不是「任务经验」
//      - 存的是"用户问了什么、AI 答了什么"
//      - 丢的是"用什么方法解决的、效果如何"
//   2. 检索粒度粗：按关键词匹配 Episode，无法回答策略性问题
//   3. 没有记录工具执行过程，无法学习工具组合效果
//   4. 没有失败模式记录，同样的错误可能重复发生
//
// 二、目标：经验型 Agent Memory
// 理想的经验型记忆更像一个「方法论库」，它能回答：
//   - 以前做这类任务时，什么方法最好？（Best Practice）
//   - 哪种工具组合最有效？（Tool Combination Effectiveness）
//   - 上次失败是因为什么，这次要避开什么？（Failure Pattern）
//   - 这类问题通常需要几步？先查什么再做什么？（Process Pattern）
//
// 三、改进方案
// 1. 结构化经验记录（Episode → Experience）
//    现有 Episode 存的是"一段文字摘要"，改为存结构化经验：
//
//    type Experience struct {
//        ID              string    // 经验唯一ID
//        TaskType        string    // 任务类型：code_review, data_analysis, research, qa...
//        QueryPatterns   []string  // 触发这类经验的问题模式
//        ToolsUsed       []string  // 使用的工具列表
//        ToolOrder       []int     // 工具调用顺序
//        Success         bool      // 任务是否成功完成
//        FailureReason   string    // 失败原因（如果失败）
//        KeyInsight      string    // 关键洞察/方法总结
//        BestPractice    string    // 推荐的最佳实践
//        SessionID       string    // 来源会话
//        CreatedAt       time.Time // 创建时间
//    }
//
//    提取时机：反思阶段（Reflection）结束后，从反思内容中提取结构化经验
//
// 2. 工具执行记录（ToolCall Experience）
//    每次工具执行后，自动记录执行日志：
//
//    type ToolExecutionLog struct {
//        ToolName      string            // 工具名
//        Arguments     map[string]any    // 调用参数
//        ResultQuality string            // 结果质量：good, partial, failed
//        ResultSummary string            // 结果摘要（用于后续检索）
//        LatencyMs     int               // 执行耗时
//        WasUseful     bool              // LLM 反思后标记是否有用
//        SessionID     string
//        Round         int               // 属于哪一轮 ReAct
//    }
//
//    提取时机：工具执行完毕后，e.toolRegistry.ExecuteTool 返回结果时记录
//
// 3. 失败模式记录（Failure Memory）
//    记录历史上的失败教训，避免重复踩坑：
//
//    type FailurePattern struct {
//        ID              string   // 模式ID
//        QueryPattern    string   // 触发失败的问题模式
//        TaskType        string   // 任务类型
//        ToolsAttempted  []string // 尝试过的工具组合
//        WhyFailed       string   // 失败根因
//        BetterApproach  string   // 更好的方法（反思产出）
//        Frequency       int      // 出现次数（用于排序）
//        LastSeenAt      time.Time
//    }
//
//    提取时机：任务失败时（LLM 输出错误或工具全部失败）或反思阶段明确提到失败
//
// 4. 检索能力升级
//    从「关键词匹配」升级到「策略性检索」：
//
//    func (s *MemoryService) RetrieveExperience(
//        ctx context.Context,
//        taskType string,    // 任务类型
//        query string,       // 当前问题
//    ) (*ExperienceContext, error) {
//        // 1. 找同类任务的最佳方法
//        bestPractices, _ := s.repo.FindBestPractices(ctx, taskType)
//
//        // 2. 找成功案例的工具组合
//        successfulCombos, _ := s.repo.FindToolCombinations(ctx, taskType, successOnly=true)
//
//        // 3. 找同类任务的失败教训
//        pitfalls, _ := s.repo.FindFailurePatterns(ctx, taskType)
//
//        // 4. 找相似问题的解决经验
//        similarCases, _ := s.repo.FindSimilarExperiences(ctx, query, limit=3)
//
//        return &ExperienceContext{
//            TaskType:        taskType,
//            BestPractices:   bestPractices,
//            EffectiveTools:  successfulCombos,
//            PitfallsToAvoid: pitfalls,
//            SimilarCases:    similarCases,
//        }
//    }
//
// 5. 反思阶段增强
//    现有反思只输出文本，改为同时产出结构化经验：
//
//    const experienceExtractionPrompt = `
//    分析这次任务执行，输出结构化经验：
//
//    {
//      "task_type": "任务类型（code_review/data_analysis/research/qa/other）",
//      "query_pattern": "这个问题可以归类的模式",
//      "tools_used": ["使用的工具名"],
//      "tool_order": [1, 2, 3],  // 调用顺序
//      "success": true/false,
//      "failure_reason": "如果失败了，原因是什么",
//      "key_insight": "关键洞察/方法总结（最多50字）",
//      "best_practice": "推荐的最佳实践（最多100字）",
//      "improvement": "下次可以改进的地方（最多50字）"
//    }
//    `
//
//    提取时机：streamReflectionToEventBus 执行完毕后，调用 LLM 提取经验
//
// 四、Agent 引擎集成
// 在 AgentEngine.Execute() 中，注入经验到 System Prompt：
//
// func (e *AgentEngine) Execute(...) (*AgentState, error) {
//     // 1. 判断任务类型（可让 LLM 分类，或根据工具使用历史推断）
//     taskType := e.classifyTaskType(query)
//
//     // 2. 检索相关经验
//     experienceCtx, _ := e.memoryService.RetrieveExperience(ctx, taskType, query)
//
//     // 3. 构建增强型 System Prompt
//     systemPrompt := e.buildSystemPrompt(...)
//     if experienceCtx != nil {
//         systemPrompt += e.buildExperienceSection(experienceCtx)
//     }
//
//     // 4. 执行 ReAct 循环...
// }
//
// func (e *AgentEngine) buildExperienceSection(ctx *ExperienceContext) string {
//     section := "\n\n## 经验参考（来自历史任务）\n"
//
//     if len(ctx.BestPractices) > 0 {
//         section += "### 最佳实践\n"
//         for _, bp := range ctx.BestPractices {
//             section += fmt.Sprintf("- %s\n", bp)
//         }
//     }
//
//     if len(ctx.EffectiveTools) > 0 {
//         section += "### 有效工具组合\n"
//         for _, combo := range ctx.EffectiveTools {
//             section += fmt.Sprintf("- %s\n", combo)
//         }
//     }
//
//     if len(ctx.PitfallsToAvoid) > 0 {
//         section += "### 需要避开的坑\n"
//         for _, pitfall := range ctx.PitfallsToAvoid {
//             section += fmt.Sprintf("- %s\n", pitfall)
//         }
//     }
//
//     return section
// }
//
// 五、整体数据流
//
// 对话结束 → 反思阶段 → 经验提取 → 存储到 Neo4j
//                       ↓
//                   结构化经验：
//                    - Experience（任务经验）
//                    - ToolExecutionLog（工具日志）
//                    - FailurePattern（失败模式）
//
// 下次对话 → 任务类型判断 → 经验召回 → 注入到 System Prompt
//                          ↓
//                       召回内容：
//                        - 这类任务最佳实践
//                        - 有效工具组合
//                        - 需要避开的坑
//                        - 相似问题案例
//
// 六、与现有系统的兼容
// 1. MemoryPlugin 保持不变，新增 ExperiencePlugin 处理经验型记忆
// 2. 经验存储使用独立的 Label/Relation，与现有 Episode 并存
// 3. 通过 task_type 建立经验与任务的关联，支持多维度检索
// 4. 现有 Summary 字段保留，作为 Experience 的补充描述
//
// 七、预期收益
// 1. Agent 能主动学习"什么方法最好"
// 2. 避免重复踩坑（Failure Pattern）
// 3. 工具组合效果可量化、可复用
// 4. 从"对话记忆"升级为"方法论库"

// 新架构知识图谱：
//	- Episode 负责记“发生过什么”
//	- Experience 负责记“以后怎么做更好”
//	- ToolExecution / FailurePattern / Method 负责把经验拆成可分析、可检索、可注入 Agent 的结构化部件
//
// 数据结构：
//	(User {id: "u1"})
//	  -[:HAS_EPISODE]->
//	(Episode {id: "ep1", session_id: "s1", summary: "用户询问代码评审方法", created_at: "2026-04-27"})
//
//	(Episode {id: "ep1"})
//	  -[:MENTIONS]->
//	(Entity {name: "code review", type: "Task", description: "代码评审相关任务"})
//
//	(Entity {name: "code review"})
//	  -[:RELATED_TO]->
//	(Entity {name: "bug detection", type: "Goal", description: "发现潜在问题"})
//
//
//	(User {id: "u1"})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {
//	   id: "exp1",
//	   session_id: "s1",
//	   message_id: "m1",
//	   task_type: "code_review",
//	   query: "帮我 review 这个改动",
//	   success: true,
//	   key_insight: "先看 diff，再看调用链更有效",
//	   recommendation: "优先定位高风险改动点",
//	   created_at: "2026-04-27"
//	})
//
//	(Experience {id: "exp1"})
//	  -[:GENERATED_FROM]->
//	(Episode {id: "ep1"})
//
//	(Experience {id: "exp1"})
//	  -[:USES_TOOL {order: 1}]->
//	(ToolExecution {
//	   tool_call_id: "tc1",
//	   tool_name: "grep_chunks",
//	   result_quality: "good",
//	   latency_ms: 120,
//	   was_useful: true
//	})
//
//	(Experience {id: "exp1"})
//	  -[:USES_TOOL {order: 2}]->
//	(ToolExecution {
//	   tool_call_id: "tc2",
//	   tool_name: "web_search",
//	   result_quality: "partial",
//	   latency_ms: 860,
//	   was_useful: false
//	})
//
//	(ToolExecution {tool_call_id: "tc1"})
//	  -[:NEXT_TOOL]->
//	(ToolExecution {tool_call_id: "tc2"})
//
//	(Experience {id: "exp1"})
//	  -[:HAS_PATTERN]->
//	(QueryPattern {
//	   pattern: "code review request",
//	   description: "用户请求分析代码改动、找风险点"
//	})
//
//	(Experience {id: "exp1"})
//	  -[:RECOMMENDS]->
//	(Method {
//	   name: "diff-first review",
//	   description: "先读 diff，再沿关键函数调用链下钻",
//	   applicable_task_type: "code_review"
//	})
//
//	(Experience {id: "exp1"})
//	  -[:HAS_FAILURE]->
//	(FailurePattern {
//	   query_pattern: "broad review request",
//	   tool_attempted: "web_search",
//	   why_failed: "查询范围太宽，结果噪音高",
//	   better_approach: "优先本地代码定位，再补充外部资料",
//	   frequency: 3
//	})
//
// 查询方式：
//
//	1. 查类似任务的成功经验
//
//	(User {id: $user_id})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {task_type: $task_type, success: true})
//
//	(Experience)
//	  -[:HAS_PATTERN]->
//	(QueryPattern)
//
//	用途：
//	- 找这个用户过去在同类任务上的成功案例
//	- 给 Agent 注入“类似任务以前怎么做比较好”
//
//	2. 查推荐的方法
//
//	(User {id: $user_id})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {task_type: $task_type, success: true})
//	  -[:RECOMMENDS]->
//	(Method)
//
//	用途：
//	- 找某类任务最常出现的推荐方法
//	- 汇总成 Best Methods 注入 Prompt
//
//	3. 查推荐工具顺序
//
//	(User {id: $user_id})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {task_type: $task_type, success: true})
//	  -[:USES_TOOL {order: 1}]->
//	(ToolExecution)
//
//	(ToolExecution)
//	  -[:NEXT_TOOL]->
//	(ToolExecution)
//	  -[:NEXT_TOOL]->
//	(ToolExecution)
//
//	用途：
//	- 找某类任务里常见的成功工具链，如：web_search -> data_analysis -> calculator
//	- 给 Agent 注入“Recommended Tool Sequence”
//
//	4. 查某个工具在这类任务里是否常有用
//
//	(User {id: $user_id})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {task_type: $task_type})
//	  -[:USES_TOOL]->
//	(ToolExecution {tool_name: $tool_name, was_useful: true})
//
//	用途：
//	- 判断某工具在某类任务里是否值得优先尝试
//	- 也可统计 success / usefulness / latency
//
//	5. 查常见失败模式
//
//	(User {id: $user_id})
//	  -[:HAS_EXPERIENCE]->
//	(Experience {task_type: $task_type})
//	  -[:HAS_FAILURE]->
//	(FailurePattern)
//
//	用途：
//	- 找这个用户/这类任务里历史上容易踩过的坑
//	- 给 Agent 注入 Pitfalls To Avoid
//
//	6. 查某个 query pattern 对应的失败原因
//
//	(QueryPattern {pattern: $pattern})
//	  <-[:HAS_PATTERN]-
//	(Experience)
//	  -[:HAS_FAILURE]->
//	(FailurePattern)
//
//	用途：
//	- 面对类似提问模式时，提前知道：
//	 - 哪个工具容易失败
//	 - 失败原因是什么
//	 - 更好的替代方案是什么
//
//	7. 从当前 query 反查可参考经验
//
//	(QueryPattern {pattern: 当前问题模式})
//	  <-[:HAS_PATTERN]-
//	(Experience {success: true})
//
//	(Experience)
//	  -[:RECOMMENDS]->
//	(Method)
//
//	(Experience)
//	  -[:USES_TOOL]->
//	(ToolExecution)
//
//	用途：
//	- 根据当前 query 命中的 pattern
//	- 直接找相关成功经验、方法、工具链
//
//	8. 从历史对话追溯经验来源
//
//	(Experience)
//	  -[:GENERATED_FROM]->
//	(Episode)
//
//	(Episode)
//	  -[:MENTIONS]->
//	(Entity)
//
//	用途：
//	- 不只是知道“推荐这样做”
//	- 还可以追溯：这条经验是从哪次对话里提炼出来的
//	- 方便解释性和调试

// 最终可以给 Agent 的注入内容
//
//	Relevant Experience
//
//	Successful Patterns:
//	- 对于 code_review 类任务，先看 diff，再沿调用链下钻效果更好
//
//	Recommended Tool Sequence:
//	- grep_chunks -> web_search -> data_analysis
//
//	Pitfalls To Avoid:
//	- broad review request 下直接 web_search 噪音较大
//	- query 太长时 web_search 容易失败
//
//	Recommended Methods:
//	- diff-first review
//	- risk-first inspection
