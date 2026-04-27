package chatpipline

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginLoadHistory 是负责“短期记忆”，MemoryPlugin 是负责“长期记忆”（记住用户的偏好、历史经历等关键信息）。
//
// 监听事件：
//	 - MEMORY_RETRIEVAL (记忆检索)：在 LLM 生成回复前，寻找相关的历史背景。
//	 - MEMORY_STORAGE (记忆存储)：在对话结束后，将当前的问答存入记忆库。
//
// 核心职责：
// 	1. 检索：根据用户当前 Query 从记忆库中召回历史相关片段，并动态注入到 Prompt 上下文中。
// 	2. 存储：监控对话生命周期，在回答生成结束后（支持流式与非流式），异步将问答对存入长期记忆库。
//
// 具体说明：
// 	A. 记忆提取 (MEMORY_RETRIEVAL - “读”)
//		在 AI 生成回答前触发。
//		前置条件：需满足 EnableMemory 为开启状态。
//		检索逻辑：优先使用“重写后的查询（RewriteQuery）”，如果没有则使用原始查询。这能确保检索记忆时语义更精准。
//		注入方式：将检索到的相关记忆（RelatedEpisodes）格式化为字符串，直接追加到 chatManage.UserContent（即发送给大模型的 Prompt）末尾。
//		容错设计：记忆提取失败不会中断整个对话流水线，保证了系统的鲁棒性。
//
// 	B. 记忆存储 (MEMORY_STORAGE - “写”)
//		在 AI 完成回答后触发。
//		同步存储（非流式）：如果对话是一次性返回的（ChatResponse != nil），直接开启一个 goroutine（协程） 异步将问答对存入记忆库。
//		异步监听（流式）：如果是流式输出，插件会订阅 EventBus 中的 EventAgentFinalAnswer 事件。它会累加流式块中的内容，直到监听到 Done 信号，再将完整的对话内容存入记忆。

// 示例一：记忆检索
//
//	时间：2026年3月31日 10:00（第二天）
//	用户提问：“去那边的高铁票好买吗？”
//
// 	代码在后台做了什么？（handleRetrieval）
//		触发检索：handleRetrieval 被触发。
//		关键词分析：它拿到用户的 query “去那边的高铁票好买吗”。
//		搜索记忆：它调用 memoryService.RetrieveMemory，拿着这句话去数据库里搜，发现这句话和昨天的记忆（“用户计划下周去上海出差”）语义非常相关。
//		注入上下文：
//			代码找到了那条旧记忆。
//			它构造了一段字符串：
//				Relevant Memory:
//				- 2026-03-30 (Summary: 用户计划下周去上海出差)
//			它把这段话追加到了发送给大模型的最终 Prompt 后面。
//
//	最终发送给大模型的 Prompt 实际上长这样：
//		System: 你是一个助手。
//		User: 去那边的高铁票好买吗？
//		Relevant Memory:
//		2026-03-30 (Summary: 用户计划下周去上海出差)
//		(当前时间：2026-03-31 ...)
//
//	结果：
//		大模型看到了“Relevant Memory”，瞬间明白了“那边”指的是“上海”。
//		于是它回答：“去上海的高铁票目前比较紧张，建议你尽快通过12306购买。”

// 场景二：记忆存储
//
//	时间：2026年3月30日 18:30
//	用户提问：“我下周要去上海出差，帮我查查那边的天气。”
//	AI 回答：“好的，上海下周主要是雨天，气温 15-20度，记得带伞。”
//
//	代码在后台做了什么？（handleStorage）
//		监听结束：AI 回答完毕，handleStorage 被触发。
//		提取内容：它抓取了用户的“提问”和 AI 的“回答”。
//		异步存储：代码启动了一个后台协程（go func()），把这段对话发给 memoryService。
//		生成记忆：后台服务可能通过另一个大模型总结出一条记忆：
//			摘要：用户计划下周去上海出差。
//			关键词：上海、出差、旅行。
//			时间：2026-03-30。
//		    这条记忆被存入了向量数据库。

type MemoryPlugin struct {
	memoryService interfaces.MemoryService
}

func NewMemoryPlugin(eventManager *EventManager, memoryService interfaces.MemoryService) *MemoryPlugin {
	res := &MemoryPlugin{
		memoryService: memoryService,
	}
	eventManager.Register(res)
	return res
}

func (p *MemoryPlugin) ActivationEvents() []types.EventType {
	return []types.EventType{
		types.MEMORY_RETRIEVAL,
		types.MEMORY_STORAGE,
	}
}

func (p *MemoryPlugin) OnEvent(
	ctx context.Context,
	eventType types.EventType,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	switch eventType {
	case types.MEMORY_RETRIEVAL:
		return p.handleRetrieval(ctx, chatManage, next)
	case types.MEMORY_STORAGE:
		return p.handleStorage(ctx, chatManage, next)
	default:
		return next()
	}
}

// 1. 如果当前会话没开启“长期记忆”功能，直接跳过，不浪费资源去查数据库。
// 2. 优先使用改写后的查询，如果没有改写，就用原始查询。
// 3. 去数据库（通常是向量数据库）里检索记忆。
// 4. 把搜到的历史片段拼成一段文本，通常包含了时间点和摘要。
// 5. 把文本追加到 chatManage.UserContent 的末尾，混在用户的提问里一起发给大模型。
//
// 举例
//
// 	假设用户提问：“我上次提到的那本书叫什么？”
// 	经过本插件处理后，发给大模型的 UserContent 可能会变成：
//		我上次提到的那本书叫什么？
//
//		Relevant Memory:
//			2026-02-10 (Summary: 用户提到他正在读《三体》，并觉得内容非常震撼)
//	AI 的反应：
//		“根据之前的记录，你上次（2026年2月10日）提到的书是《三体》。”

// handleRetrieval
//	│
//	├── 1. 检查是否启用 memory
//	│      → 如果 chatManage.EnableMemory == false
//	│      → 直接跳过，不做长期记忆检索
//	│      → return next()
//	│
//	├── 2. 记录“开始检索记忆”日志
//	│      → 标记进入长期记忆读取流程
//	│
//	├── 3. 选择检索 query
//	│      → 优先使用 chatManage.RewriteQuery
//	│      → 如果没有改写后的 query
//	│      → 则回退到 chatManage.Query
//	│      → 目的是让记忆检索更贴近语义后的问题表达
//	│
//	├── 4. 调用 memoryService.RetrieveMemory()
//	│      → 输入：userID + query
//	│      → 内部流程通常是：
//	│           - 先让模型提关键词
//	│           - 再去 MemoryRepository 查相关 episodes
//	│      → 返回 MemoryContext
//	│
//	├── 5. 检索失败时容错
//	│      → 如果 RetrieveMemory 出错
//	│      → 只记日志，不中断主问答链路
//	│      → return next()
//	│      → memory 只是增强项，不应阻塞回答
//	│
//	├── 6. 如果检索到了相关记忆
//	│      → 遍历 memoryContext.RelatedEpisodes
//	│      → 构造一段 Prompt 文本：
//	│           Relevant Memory:
//	│           - 日期 (Summary: ...)
//	│
//	├── 7. 注入到 chatManage.UserContent
//	│      → 将 Relevant Memory 追加到用户输入末尾
//	│      → 这样后续发给模型的 prompt 就带上了长期记忆背景
//	│
//	├── 8. 记录“记忆检索完成”日志
//	│      → 标记 retrieval 流程结束
//	│
//	└── 9. 继续主流水线
//		  → return next()
//		  → 后续插件 / 问答流程继续执行

func (p *MemoryPlugin) handleRetrieval(
	ctx context.Context,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	// Check if memory is enabled
	if !chatManage.EnableMemory {
		return next()
	}
	logger.Info(ctx, "Start to retrieve memory")

	// Retrieve memory context
	query := chatManage.RewriteQuery
	if query == "" {
		query = chatManage.Query
	}

	memoryContext, err := p.memoryService.RetrieveMemory(ctx, chatManage.UserID, query)
	if err != nil {
		logger.Errorf(ctx, "failed to retrieve memory: %v", err)
		// Don't block the pipeline if memory retrieval fails
		return next()
	}

	// Add memory context to chatManage
	if len(memoryContext.RelatedEpisodes) > 0 {
		memoryStr := "\n\nRelevant Memory:\n"
		for _, ep := range memoryContext.RelatedEpisodes {
			memoryStr += fmt.Sprintf("- %s (Summary: %s)\n", ep.CreatedAt.Format("2006-01-02"), ep.Summary)
		}
		chatManage.UserContent += memoryStr
		logger.Info(ctx, "Retrieved memory: %s", memoryStr)
	}
	logger.Info(ctx, "End to retrieve memory")

	return next()
}

// 1. 插件会先让下游的 AI 生成回答。只有当 AI 成功把话吐出来之后，后面的存储代码才会运行。
// 2. 如果当前会话没开启长期记忆功能，直接返回，不存储。
// 3. AI 回答通常有两种方式：一次性给全（非流式）和蹦单词（流式）。代码分别处理了这两种情况：
//	情况 A：非流式（ChatResponse 直接可用）
//		如果 chatManage.ChatResponse 不是空的，说明 AI 已经一次性把完整答案给出来了。
//		动作：直接把用户的问题和 AI 的回答打包。
//		优化：启动一个 go func()（协程）。
//		用意：存储到数据库或向量库可能耗时 500ms 甚至更久。通过协程，服务器可以立刻给用户返回结果，让存储在后台慢慢跑，用户感知不到延迟。
//	情况 B：流式（EventBus 监听）
//		这是最复杂也最优雅的部分。流式输出时，AI 是一点点往外蹦词的，此时拿不到完整回答。
//		动作：插件变身为“监听者”，订阅 EventAgentFinalAnswer 事件。
//		过程：每蹦出一个词，就把词累加到 fullResponse 变量里，过程中盯着那个 data.Done 标志位。
//		收网：一旦 Done 为 true，说明话讲完了。这时候才启动协程，把攒好的 fullResponse 存进记忆库。

// handleStorage
//	│
//	├── 1. 先执行下游主流程
//	│      → 调 next()
//	│      → 让真正的回答先跑完
//	│      → 如果下游报错，直接返回错误，不做记忆存储
//	│
//	├── 2. 检查是否启用 memory
//	│      → 如果 chatManage.EnableMemory == false
//	│      → 直接返回，不做长期记忆写入
//	│
//	├── 3. 记录“开始存储记忆”日志
//	│      → 标记进入长期记忆写入流程
//	│
//	├── 4. 判断回答是非流式还是流式
//	│      → 分成两条路径：
//	│           A. ChatResponse 已经完整可用（非流式）
//	│           B. 需要监听 EventAgentFinalAnswer（流式）
//	│
//	├── 5A. 非流式路径：如果 chatManage.ChatResponse != nil
//	│      → 直接组装 messages：
//	│           - user: chatManage.Query
//	│           - assistant: chatManage.ChatResponse.Content
//	│      → 然后异步 go func() 调 memoryService.AddEpisode()
//	│      → 目的是后台存储，不阻塞用户响应
//	│      → return nil
//	│
//	├── 5B. 流式路径：如果 chatManage.EventBus != nil
//	│      → 注册 EventAgentFinalAnswer 监听器
//	│      → 准备一个 fullResponse 字符串用于累加
//	│
//	├── 6B. 监听流式 final_answer chunk
//	│      → 每次收到 EventAgentFinalAnswer
//	│      → fullResponse += data.Content
//	│      → 不断把 assistant 正文 chunk 累积起来
//	│
//	├── 7B. 检测 data.Done
//	│      → 当 data.Done == true
//	│      → 说明这轮 assistant 的最终回答已经完整生成
//	│      → 此时组装 messages：
//	│           - user: chatManage.Query
//	│           - assistant: fullResponse
//	│
//	├── 8B. 异步写入 memory
//	│      → go func() 调 memoryService.AddEpisode()
//	│      → 把本轮问答沉淀成长期记忆
//	│      → 不阻塞流式收尾
//	│
//	├── 9. 记录“结束存储记忆”日志
//	│      → 标记 storage 流程主体结束
//	│
//	└── 10. 返回 nil
//		  → memory 写入是增强功能，走异步后台执行

func (p *MemoryPlugin) handleStorage(
	ctx context.Context,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	if err := next(); err != nil {
		return err
	}

	// Check if memory is enabled
	if !chatManage.EnableMemory {
		return nil
	}

	logger.Info(ctx, "Start to store memory")
	// If ChatResponse is already available (non-streaming), store it directly
	if chatManage.ChatResponse != nil {
		messages := []types.Message{
			{Role: "user", Content: chatManage.Query},
			{Role: "assistant", Content: chatManage.ChatResponse.Content},
		}
		// Capture UserID and SessionID for goroutine
		userID := chatManage.UserID
		sessionID := chatManage.SessionID
		go func() {
			if err := p.memoryService.AddEpisode(ctx, userID, sessionID, messages); err != nil {
				logger.Errorf(ctx, "failed to add episode: %v", err)
			}
		}()
		return nil
	}

	// If streaming, subscribe to events
	if chatManage.EventBus != nil {
		var fullResponse string
		// Capture UserID and SessionID for closure
		userID := chatManage.UserID
		sessionID := chatManage.SessionID

		chatManage.EventBus.On(types.EventType(event.EventAgentFinalAnswer), func(ctx context.Context, evt types.Event) error {
			data, ok := evt.Data.(event.AgentFinalAnswerData)
			if !ok {
				return nil
			}
			fullResponse += data.Content
			if data.Done {
				messages := []types.Message{
					{Role: "user", Content: chatManage.Query},
					{Role: "assistant", Content: fullResponse},
				}
				go func() {
					if err := p.memoryService.AddEpisode(ctx, userID, sessionID, messages); err != nil {
						logger.Errorf(ctx, "failed to add episode: %v", err)
					}
				}()
			}
			return nil
		})
	}
	logger.Info(ctx, "End to store memory")

	return nil
}
