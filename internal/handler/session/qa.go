package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// qaRequestContext holds all the common data needed for QA requests
type qaRequestContext struct {
	ctx               context.Context
	c                 *gin.Context
	sessionID         string
	requestID         string
	query             string
	session           *types.Session
	customAgent       *types.CustomAgent
	assistantMessage  *types.Message
	knowledgeBaseIDs  []string
	knowledgeIDs      []string
	summaryModelID    string
	webSearchEnabled  bool
	enableMemory      bool // Whether memory feature is enabled
	mentionedItems    types.MentionedItems
	effectiveTenantID uint64 // when using shared agent, tenant ID for model/KB/MCP resolution; 0 = use context tenant
}

// parseQARequest 是问答流程的入口点，负责解析HTTP请求、验证参数、加载会话、
// 解析Agent配置、合并@提及的资源，并构建完整的请求上下文。
//
//	1. 解析请求
//	  ├── 从 URL 获取 sessionID
//	  ├── 解析 JSON 请求体（Query、KnowledgeBaseIDs、MentionedItems 等）
//	  └── 校验 Query 非空
//	2. 获取会话
//	  └── 调用 sessionService.GetSession 确保会话存在
//	3. 解析 Agent 配置
//	  ├── 若使用共享 Agent → 通过 AgentShareService 解析
//	  └── 若使用自有 Agent → 通过 CustomAgentService 获取
//	  └── 失败则降级使用默认配置
//	4. 合并 @ 提及的资源
//	  ├── @ kb（知识库）→ 合并到 KnowledgeBaseIDs
//	  └── @ file（文件）→ 合并到 KnowledgeIDs
//	  └── 目的：即使侧边栏未勾选，@了的资源也会被检索
//	5. 构建请求上下文
//	  └── 封装 qaRequestContext，包含会话、Agent、检索范围等

// [重要]
//
// CloneContext 重新从 c.Request.Context() 开了一个新 context ，
// 如果用户网络抖动、浏览器刷新等原因，导致 SSE 连接断开，只会让 SSE 推流协程退出。

// parseQARequest parses and validates a QA request, returns the request context
func (h *Handler) parseQARequest(c *gin.Context, logPrefix string) (*qaRequestContext, *CreateKnowledgeQARequest, error) {
	ctx := logger.CloneContext(c.Request.Context())
	logger.Infof(ctx, "[%s] Start processing request", logPrefix)

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		return nil, nil, errors.NewBadRequestError(errors.ErrInvalidSessionID.Error())
	}

	// Parse request body
	var request CreateKnowledgeQARequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		return nil, nil, errors.NewBadRequestError(err.Error())
	}

	// Validate query content
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		return nil, nil, errors.NewBadRequestError("Query content cannot be empty")
	}

	// Log request details
	if requestJSON, err := json.Marshal(request); err == nil {
		logger.Infof(ctx, "[%s] Request: session_id=%s, request=%s",
			logPrefix, sessionID, secutils.SanitizeForLog(string(requestJSON)))
	}

	// Get session
	session, err := h.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session, session ID: %s, error: %v", sessionID, err)
		return nil, nil, errors.NewNotFoundError("Session not found")
	}

	// Get custom agent if agent_id is provided. Backend resolves shared agent from share relation (no client-provided tenant).
	var customAgent *types.CustomAgent
	var effectiveTenantID uint64
	if request.AgentID != "" {
		logger.Infof(ctx, "Resolving agent, agent ID: %s", secutils.SanitizeForLog(request.AgentID))
		userIDVal, _ := c.Get(types.UserIDContextKey.String())
		currentTenantID := c.GetUint64(types.TenantIDContextKey.String())
		if h.agentShareService != nil && userIDVal != nil && currentTenantID != 0 {
			userID, _ := userIDVal.(string)
			agent, err := h.agentShareService.GetSharedAgentForUser(ctx, userID, currentTenantID, request.AgentID)
			if err == nil && agent != nil {
				effectiveTenantID = agent.TenantID
				customAgent = agent
				logger.Infof(ctx, "Using shared agent: ID=%s, Name=%s, effectiveTenantID=%d (retrieval scope)",
					customAgent.ID, customAgent.Name, effectiveTenantID)
			}
		}
		if customAgent == nil {
			agent, err := h.customAgentService.GetAgentByID(ctx, request.AgentID)
			if err == nil {
				customAgent = agent
				logger.Infof(ctx, "Using own agent: ID=%s, Name=%s, AgentMode=%s",
					customAgent.ID, customAgent.Name, customAgent.Config.AgentMode)
			} else {
				logger.Warnf(ctx, "Failed to get custom agent, agent ID: %s, error: %v, using default config",
					secutils.SanitizeForLog(request.AgentID), err)
			}
		} else {
			logger.Infof(ctx, "Using custom agent: ID=%s, Name=%s, IsBuiltin=%v, AgentMode=%s, effectiveTenantID=%d",
				customAgent.ID, customAgent.Name, customAgent.IsBuiltin, customAgent.Config.AgentMode, effectiveTenantID)
		}
	}

	// Merge @mentioned items into knowledge_base_ids and knowledge_ids so that
	// retrieval (quick-answer and agent mode) uses the same targets the user @mentioned.
	// This fixes the case where user only @mentions a (shared) KB in the input but
	// does not select it in the sidebar — without this merge, retrieval would not search those KBs.
	kbIDs := make([]string, 0, len(request.KnowledgeBaseIDs)+len(request.MentionedItems))
	kbIDSet := make(map[string]bool)
	for _, id := range request.KnowledgeBaseIDs {
		if id != "" && !kbIDSet[id] {
			kbIDs = append(kbIDs, id)
			kbIDSet[id] = true
		}
	}
	knowledgeIDs := make([]string, 0, len(request.KnowledgeIds)+len(request.MentionedItems))
	knowledgeIDSet := make(map[string]bool)
	for _, id := range request.KnowledgeIds {
		if id != "" && !knowledgeIDSet[id] {
			knowledgeIDs = append(knowledgeIDs, id)
			knowledgeIDSet[id] = true
		}
	}
	for _, item := range request.MentionedItems {
		if item.ID == "" {
			continue
		}
		switch item.Type {
		case "kb":
			if !kbIDSet[item.ID] {
				kbIDs = append(kbIDs, item.ID)
				kbIDSet[item.ID] = true
			}
		case "file":
			if !knowledgeIDSet[item.ID] {
				knowledgeIDs = append(knowledgeIDs, item.ID)
				knowledgeIDSet[item.ID] = true
			}
		}
	}

	// Log merge results for debugging
	logger.Infof(ctx, "[%s] @mention merge: request.KnowledgeBaseIDs=%v, request.MentionedItems=%d, merged kbIDs=%v, merged knowledgeIDs=%v",
		logPrefix, request.KnowledgeBaseIDs, len(request.MentionedItems), kbIDs, knowledgeIDs)

	// Build request context
	reqCtx := &qaRequestContext{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		requestID:   secutils.SanitizeForLog(c.GetString(types.RequestIDContextKey.String())),
		query:       secutils.SanitizeForLog(request.Query),
		session:     session,
		customAgent: customAgent,
		assistantMessage: &types.Message{
			SessionID:   sessionID,
			Role:        "assistant",
			RequestID:   c.GetString(types.RequestIDContextKey.String()),
			IsCompleted: false,
		},
		knowledgeBaseIDs:  secutils.SanitizeForLogArray(kbIDs),
		knowledgeIDs:      secutils.SanitizeForLogArray(knowledgeIDs),
		summaryModelID:    secutils.SanitizeForLog(request.SummaryModelID),
		webSearchEnabled:  request.WebSearchEnabled,
		enableMemory:      request.EnableMemory,
		mentionedItems:    convertMentionedItems(request.MentionedItems),
		effectiveTenantID: effectiveTenantID,
	}

	return reqCtx, &request, nil
}

// [重要]
//
// 一个 Session 代表整个会话，包含很多轮，一次问答是一轮。
// 每轮都是一次独立的流式回答请求，通常来讲，每轮对话都会对应一条新的 SSE 连接。
// 如果断线，可以再建一条连接去续同一轮的流。
//
// 如果整个 Session 会话只用一条 SSE，连接管理会复杂很多，很容易相互污染。
// 因此，每轮对话都会新建一个 EventBus，它的生命周期跟单轮请求绑定，不跟整个 session 绑定。

// EventBus 是单轮问答请求内的事件总线
//
//【设计思想】
// 在一轮问答过程中，多个组件协同工作：检索知识库、调用 LLM 生成答案、工具执行等。
// 这些组件之间需要传递状态信息，但彼此不直接依赖，因此通过 EventBus 实现解耦的发布-订阅通信。
//
// 它只服务于当前这一次请求的内部协作，不直接对前端输出。
// 前端真正收到的 SSE 数据，是由订阅者处理事件后再写入 streamManager，
// 最后由 handleAgentEventsForSSE 从 streamManager 拉取并推送出去。
//
//【生命周期】
// 一个 EventBus 只在一轮问答内有效，问答结束后随 context 一起销毁。
// 每轮问答都会 new一个新的 EventBus，确保事件不串到其他会话。
//
//【事件类型】
// 主要分三类：
// 1. 状态类 - agent.query、agent.complete、error、stop
// 2. 流式输出类 - thought、final_answer（携带 done 标志区分是否结束）
// 3. 工具类 - tool_call、tool_result、references、reflection
// 4. 辅助类 - session_title
//
//【参与者】
// - 生产者：执行具体任务的组件，在执行过程中 Emit 事件
//   - KnowledgeQA / AgentEngine：发 thought、tool_call、tool_result、references、final_answer、complete
//   - GenerateTitleAsync：发 session_title
//   - handleAgentEventsForSSE：检测到 stop 时补发 EventStop
//
// - 消费者：需要感知状态的组件，On 监听事件
//   - AgentStreamHandler：收各种事件，写入 streamManager 推 SSE
//   - setupStopEventHandler：只收 EventStop，收后 cancel() 取消任务并收尾
//   - executeNormalModeQA 里的监听器：收 EventAgentFinalAnswer，累加 chunk
//
// - 不参与者：与 EventBus 完全无关
//   - handleAgentEventsForSSE：轮询 streamManager 拉事件，不监听 EventBus
//   - createUserMessage：直接落库，不参与事件流转

// sseStreamContext holds the context for SSE streaming
type sseStreamContext struct {
	eventBus         *event.EventBus
	asyncCtx         context.Context
	cancel           context.CancelFunc
	assistantMessage *types.Message
}

// setupSSEStream 初始化并配置 SSE 流式传输的上下文环境
//
// 	设置 SSE 响应头，让这个 HTTP 请求进入流式返回模式。
//	先写一个 agent_query 事件，通知前端“这次问答已经开始了”。
//	构造异步执行用的 context 和 cancel，后面真正跑 KnowledgeQA / AgentQA 的 goroutine 都用这个上下文。
//	创建一个 EventBus，让后台任务、停止逻辑、流处理器之间可以用事件通信。
//	注册“停止生成”处理器：
//		监听 EventStop，一旦用户点了停止，或者流里出现 stop 事件，就会做三件事：
//		 - 取消异步上下文 — 调用 cancel() 终止正在执行的后台任务
//		 - 设置停止消息 — 将助手消息内容设为"用户停止了本次对话"
//		 - 持久化终止状态 — 调用 completeAssistantMessage 将停止状态写入数据库
//	注册流处理器：
//		创建 streamHandler ，订阅问答过程中产生的各种事件，然后把它们写进 streamManager ，
//		然后 handleAgentEventsForSSE 再从 streamManager 里轮询这些事件，推给前端。
//		前端实时接收并渲染（如显示思考过程、工具调用进度、答案片段等）。
//	如果会话还没有标题，并且这次允许生成标题，就异步触发标题生成。

// asyncCtx 是一条脱离前端连接生命周期的后台执行上下文。
// 用户网络抖动、浏览器刷新、页面切后台、网络中断等，导致 SSE 连接断开，只会让 SSE 推流协程退出，
// 但不会自动让 KnowledgeQA/AgentQA/AgentEngine.Execute 这些后台任务停掉，
// 因为它们跑的是 asyncCtx，不是 c.Request.Context()。
//
// 这套设计是故意做成这样的：前端连接断开 ≠ 后台任务取消，
// 这样才能支持：ContinueStream、页面刷新后续流、网络抖动后补流；
// 当前实现中，让后台停下来的流程只有一个：
//	- 用户显式调用 StopSession
//	- stop 事件写入 streamManager
//	- handleAgentEventsForSSE 读到 stop
//	- eventBus.Emit(EventStop)
//	- setupStopEventHandler 里执行 cancel()
//
// 但是，当前的实现可能导致资源占用和浪费，后面版本可能支持超时回收机制。

// setupSSEStream sets up the SSE streaming context
func (h *Handler) setupSSEStream(reqCtx *qaRequestContext, generateTitle bool) *sseStreamContext {
	// Set SSE headers
	setSSEHeaders(reqCtx.c)

	// Write initial agent_query event
	h.writeAgentQueryEvent(reqCtx.ctx, reqCtx.sessionID, reqCtx.assistantMessage.ID)

	// Base context for async work: when using shared agent, use source tenant for model/KB/MCP resolution
	baseCtx := reqCtx.ctx
	if reqCtx.effectiveTenantID != 0 && h.tenantService != nil {
		if tenant, err := h.tenantService.GetTenantByID(reqCtx.ctx, reqCtx.effectiveTenantID); err == nil && tenant != nil {
			baseCtx = context.WithValue(context.WithValue(reqCtx.ctx, types.TenantIDContextKey, reqCtx.effectiveTenantID), types.TenantInfoContextKey, tenant)
			logger.Infof(reqCtx.ctx, "Using effective tenant %d for shared agent (model/KB/MCP)", reqCtx.effectiveTenantID)
		}
	}

	// Create EventBus and cancellable context
	eventBus := event.NewEventBus()
	asyncCtx, cancel := context.WithCancel(logger.CloneContext(baseCtx))

	streamCtx := &sseStreamContext{
		eventBus:         eventBus,
		asyncCtx:         asyncCtx,
		cancel:           cancel,
		assistantMessage: reqCtx.assistantMessage,
	}

	// Setup stop event handler
	h.setupStopEventHandler(eventBus, reqCtx.sessionID, reqCtx.session.TenantID, reqCtx.assistantMessage, cancel)

	// Setup stream handler
	h.setupStreamHandler(asyncCtx, reqCtx.sessionID, reqCtx.assistantMessage.ID, reqCtx.requestID, reqCtx.assistantMessage, eventBus)

	// Generate title if needed
	if generateTitle && reqCtx.session.Title == "" {
		// Use the same model as the conversation for title generation
		modelID := ""
		if reqCtx.customAgent != nil && reqCtx.customAgent.Config.ModelID != "" {
			modelID = reqCtx.customAgent.Config.ModelID
		}
		logger.Infof(reqCtx.ctx, "Session has no title, starting async title generation, session ID: %s, model: %s", reqCtx.sessionID, modelID)
		h.sessionService.GenerateTitleAsync(asyncCtx, reqCtx.session, reqCtx.query, modelID, eventBus)
	}

	return streamCtx
}

// SearchKnowledge
//
// 使用场景
//	- 前端需要自己展示检索结果、不想让 LLM 二次加工
//	- 只需要获取相关文档片段做其他用途
//	- 调试检索质量时单独测试检索阶段
//
// 执行流程
//	SearchKnowledge
//		│
//		├── 1. 解析请求（Query、KB IDs、Knowledge IDs）
//		├── 2. 校验至少提供一个检索范围
//		└── 3. 调用 sessionService.SearchKnowledge
//		   └── 执行向量检索 + 关键词检索，返回原始 chunks
//
// 返回示例
//	{
//	 "success": true,
//	 "data": [
//	   {
//		 "id": "chunk_xxx",
//		 "content": "这是相关文档片段...",
//		 "score": 0.85,
//		 "source": "文件名.pdf"
//	   }
//	 ]
//	}

// SearchKnowledge godoc
// @Summary      知识搜索
// @Description  在知识库中搜索（不使用LLM总结）
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        request  body      SearchKnowledgeRequest  true  "搜索请求"
// @Success      200      {object}  map[string]interface{}  "搜索结果"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/search [post]
func (h *Handler) SearchKnowledge(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	logger.Info(ctx, "Start processing knowledge search request")

	// Parse request body
	var request SearchKnowledgeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Validate request parameters
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		c.Error(errors.NewBadRequestError("Query content cannot be empty"))
		return
	}

	// Merge single knowledge_base_id into knowledge_base_ids for backward compatibility
	knowledgeBaseIDs := request.KnowledgeBaseIDs
	if request.KnowledgeBaseID != "" {
		// Check if it's already in the list to avoid duplicates
		found := false
		for _, id := range knowledgeBaseIDs {
			if id == request.KnowledgeBaseID {
				found = true
				break
			}
		}
		if !found {
			knowledgeBaseIDs = append(knowledgeBaseIDs, request.KnowledgeBaseID)
		}
	}

	if len(knowledgeBaseIDs) == 0 && len(request.KnowledgeIDs) == 0 {
		logger.Error(ctx, "No knowledge base IDs or knowledge IDs provided")
		c.Error(errors.NewBadRequestError("At least one knowledge_base_id, knowledge_base_ids or knowledge_ids must be provided"))
		return
	}

	logger.Infof(
		ctx,
		"Knowledge search request, knowledge base IDs: %v, knowledge IDs: %v, query: %s",
		secutils.SanitizeForLogArray(knowledgeBaseIDs),
		secutils.SanitizeForLogArray(request.KnowledgeIDs),
		secutils.SanitizeForLog(request.Query),
	)

	// Directly call knowledge retrieval service without LLM summarization
	searchResults, err := h.sessionService.SearchKnowledge(ctx, knowledgeBaseIDs, request.KnowledgeIDs, request.Query)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Knowledge search completed, found %d results", len(searchResults))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    searchResults,
	})
}

// KnowledgeQA godoc
// @Summary      知识问答
// @Description  基于知识库的问答（使用LLM总结），支持SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/knowledge-qa [post]
func (h *Handler) KnowledgeQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "KnowledgeQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Execute normal mode QA, generate title unless disabled
	h.executeNormalModeQA(reqCtx, !request.DisableTitle)
}

// AgentQA godoc
// @Summary      Agent问答
// @Description  基于Agent的智能问答，支持多轮对话和SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/agent-qa [post]
func (h *Handler) AgentQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "AgentQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Determine if agent mode should be enabled
	// Priority: customAgent.IsAgentMode() > request.AgentEnabled
	agentModeEnabled := request.AgentEnabled
	if reqCtx.customAgent != nil {
		agentModeEnabled = reqCtx.customAgent.IsAgentMode()
		logger.Infof(reqCtx.ctx, "Agent mode determined by custom agent: %v (config.agent_mode=%s)",
			agentModeEnabled, reqCtx.customAgent.Config.AgentMode)
	}

	// Route to appropriate handler based on agent mode
	if agentModeEnabled {
		h.executeAgentModeQA(reqCtx)
	} else {
		logger.Infof(reqCtx.ctx, "Agent mode disabled, delegating to normal mode for session: %s", reqCtx.sessionID)
		// Knowledge bases should be specified in request or from custom agent
		h.executeNormalModeQA(reqCtx, false)
	}
}

// 会话过程
//
// Session
//  ├── Round 1
//  │   ├── User Message
//  │   └── Assistant Message
//  │        ├── EventAgentFinalAnswer(chunk1, done=false)
//  │        ├── EventAgentFinalAnswer(chunk2, done=false)
//  │        └── EventAgentFinalAnswer(chunk3, done=true)
//  │
//  ├── Round 2
//  │   ├── User Message
//  │   └── Assistant Message
//  │        ├── EventAgentFinalAnswer(chunk1, done=false)
//  │        ├── EventAgentFinalAnswer(chunk2, done=false)
//  │        └── ...
//  │
//  └── ...
//
//
// 每轮 = 1 个 User Message + 1 个 Assistant Message
// 由于 Assistant 消息是流式生成 （SSE chunk by chunk），所以需要一个累积过程：
//	 ```streamCtx.assistantMessage.Content += data.Content  // chunk 不断追加 ```

// 除了 EventAgentFinalAnswer，同一轮里还可能穿插别的事件，比如：
//	thought
//	tool_call
//	tool_result
//	references
// 这些也会通过 SSE 发给前端，但它们不会累积成 assistantMessage.Content。
// 只有 EventAgentFinalAnswer 才是在拼“最终展示给用户的回复正文”。
//
// Session
//  │
//  ├── Round 1
//  │   │
//  │   ├── User Message
//  │   │     "什么是 RAG？"
//  │   │
//  │   └── Assistant Message  (role=assistant, IsCompleted=false)
//  │         │
//  │         ├── agent_query
//  │         │     前端知道：这一轮开始了
//  │         │
//  │         ├── thought
//  │         │     "我需要先解释 RAG 的定义..."
//  │         │
//  │         ├── tool_call
//  │         │     调知识库检索 / WebSearch / MCP / 其他工具
//  │         │
//  │         ├── tool_result
//  │         │     工具返回检索结果、网页内容、结构化数据等
//  │         │
//  │         ├── references
//  │         │     返回引用文档、知识片段、来源链接
//  │         │
//  │         ├── final_answer chunk #1
//  │         │     content="RAG 是"
//  │         │     done=false
//  │         │
//  │         ├── final_answer chunk #2
//  │         │     content="检索增强生成，"
//  │         │     done=false
//  │         │
//  │         ├── final_answer chunk #3
//  │         │     content="它会先检索再生成回答。"
//  │         │     done=true  ◄── done=true 时：持久化到 DB，触发 complete
//  │         │
//  │         ├── complete
//  │         │     这一轮 assistant 回复结束，IsCompleted=true
//  │         │
//  │         └── session_title
//  │               如果这是新会话且原本没标题，可能异步补发标题
//  │
//  ├── Round 2
//  │   │
//  │   ├── User Message
//  │   │     "它和微调有什么区别？"
//  │   │
//  │   └── Assistant Message  (role=assistant, IsCompleted=false)
//  │         │
//  │         ├── agent_query
//  │         ├── thought
//  │         ├── tool_call
//  │         ├── tool_result
//  │         ├── references
//  │         ├── final_answer chunk #1
//  │         ├── final_answer chunk #2
//  │         ├── ...
//  │         └── complete
//  │
//  └── Round 3
//        ...
//
// 关键说明
//  - done=true 的 final_answer 触发 收尾 ：持久化消息到 DB + 发送 complete 事件
//	- User Message / Assistant Message 是消息实体，会落库。
//	- agent_query / thought / tool_call / tool_result / references / final_answer / complete / session_title 是流式事件。
//	- 其中只有 final_answer 会累积进 assistantMessage.Content。
//	- complete 表示这一轮 assistant 消息正式结束。
//	- session_title 不是每轮都有，通常只在新会话第一次问答时异步生成。

// 如果用户中途点"停止生成"，会触发 stop 事件，跳过剩余流程直接 complete
//
//  User Message
//	Assistant Message (未完成)
//	 ├── agent_query
//	 ├── thought
//	 ├── tool_call
//	 ├── stop
//	 └── assistantMessage.Content = "用户停止了本次对话"

// 执行过程
//
//	executeNormalModeQA
//	├── 1. 创建用户消息
//	│   └── h.createUserMessage() → 把用户这次提问先落库
//	├── 2. 创建助手消息占位
//	│   └── h.createAssistantMessage() → 先创建一条未完成的助手消息，后面流式内容会不断往这条消息里填。
//	├── 3. 初始化 SSE 流
//	│   └── h.setupSSEStream() → 把事件总线、流处理器、停止处理器、标题生成这些配套环境准备好。
//	├── 4. 注册完成事件处理器
//	│   └── 监听 EventAgentFinalAnswer，累积答案内容，每来一段最终答案，就追加到 assistantMessage.Content
//	│   └── Done=true 时：
//	│       ├── h.completeAssistantMessage() → 持久化消息到数据库
//	│       └── 触发 EventAgentComplete → 发送 SSE 完成信号
//	├── 5. 异步执行真正的知识库问答
//	│   └── goroutine 中调用 h.sessionService.KnowledgeQA()
//	│       ├── 检索知识库（向量检索 + 关键词检索）
//	│       ├── 排序合并（Rerank）
//	│       ├── 调用 LLM 生成答案（流式）
//	│       └── 通过 EventBus 推送事件（SSE）
//	└── 6. 阻塞式处理 SSE 事件
//	   └── h.handleAgentEventsForSSE() → 不断从 EventBus 流里拿事件，转成 SSE 返回给前端。

// executeNormalModeQA executes the normal (KnowledgeQA) mode
func (h *Handler) executeNormalModeQA(reqCtx *qaRequestContext, generateTitle bool) {
	ctx := reqCtx.ctx
	sessionID := reqCtx.sessionID

	// Create user message
	if err := h.createUserMessage(ctx, sessionID, reqCtx.query, reqCtx.requestID, reqCtx.mentionedItems); err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Create assistant message
	if _, err := h.createAssistantMessage(ctx, reqCtx.assistantMessage); err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Using knowledge bases: %v", reqCtx.knowledgeBaseIDs)

	// Setup SSE stream
	streamCtx := h.setupSSEStream(reqCtx, generateTitle)

	// Setup completion handler for normal mode
	// Note: Thinking content is now embedded in answer stream with <think> tags
	// by chat_completion_stream.go, so we don't need separate thinking event handling
	var completionHandled bool // Prevent duplicate completion handling

	streamCtx.eventBus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		streamCtx.assistantMessage.Content += data.Content
		if data.Done {
			// Prevent duplicate completion handling
			if completionHandled {
				return nil
			}
			completionHandled = true

			logger.Infof(streamCtx.asyncCtx, "Knowledge QA service completed for session: %s", sessionID)
			// Content already contains <think>...</think> tags from chat_completion_stream.go
			// Use session's tenant for message update (asyncCtx may have effectiveTenantID when using shared agent)
			updateCtx := context.WithValue(streamCtx.asyncCtx, types.TenantIDContextKey, reqCtx.session.TenantID)
			h.completeAssistantMessage(updateCtx, streamCtx.assistantMessage)
			// Emit EventAgentComplete - this will trigger handleComplete which sends the SSE complete event
			// Note: Don't cancel context here, let the SSE handler close naturally after receiving the complete event
			streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
				Type:      event.EventAgentComplete,
				SessionID: sessionID,
				Data:      event.AgentCompleteData{FinalAnswer: streamCtx.assistantMessage.Content},
			})
		}
		return nil
	})

	// Execute KnowledgeQA asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 10240)
				runtime.Stack(buf, true)
				logger.ErrorWithFields(streamCtx.asyncCtx,
					errors.NewInternalServerError(fmt.Sprintf("Knowledge QA service panicked: %v\n%s", r, string(buf))), nil)
			}
		}()

		err := h.sessionService.KnowledgeQA(
			streamCtx.asyncCtx,
			reqCtx.session,
			reqCtx.query,
			reqCtx.knowledgeBaseIDs,
			reqCtx.knowledgeIDs,
			reqCtx.assistantMessage.ID,
			reqCtx.summaryModelID,
			reqCtx.webSearchEnabled,
			streamCtx.eventBus,
			reqCtx.customAgent,
			reqCtx.enableMemory,
		)
		if err != nil {
			logger.ErrorWithFields(streamCtx.asyncCtx, err, nil)
			streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
				Type:      event.EventError,
				SessionID: sessionID,
				Data: event.ErrorData{
					Error:     err.Error(),
					Stage:     "knowledge_qa_execution",
					SessionID: sessionID,
				},
			})
		}
	}()

	// Handle SSE events (blocking)
	shouldWaitForTitle := generateTitle && reqCtx.session.Title == ""
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID, reqCtx.requestID, streamCtx.eventBus, shouldWaitForTitle)
}

// executeAgentModeQA
//	│
//	├── 1. Emit agent_query 事件
//	│      → 标记这轮 Agent 问答开始
//	│      → 让前端尽早收到“正在处理”信号
//	│
//	├── 2. createUserMessage
//	│      → 将本轮用户输入落库
//	│      → 生成一条 role=user 的消息
//	│
//	├── 3. createAssistantMessage
//	│      → 先创建一条未完成的 assistant 消息
//	│      → 作为本轮流式回答的消息实体
//	│
//	├── 4. setupSSEStream
//	│      → 设置 SSE 响应头
//	│      → 创建本轮专属 EventBus 和 asyncCtx
//	│      → 注册 stop handler / stream handler
//	│      → 如有需要，异步生成 session title
//	│
//	├── 5. goroutine: h.sessionService.AgentQA
//	│      → 真正执行 Agent 模式问答
//	│      → 运行 Agent Engine / ReAct 循环
//	│      → 在过程中 emit thought / tool_call / tool_result / final_answer / complete 等事件
//	│      → defer 中负责 assistant 消息收尾和完成标记
//	│
//	├── 6. 若 AgentQA 返回 error
//	│      → 记录错误日志
//	│      → 向 EventBus emit error 事件
//	│      → 交给后续流处理链路发给前端
//	│
//	└── 7. handleAgentEventsForSSE（阻塞）
//		  → 从 StreamManager 轮询本轮事件
//		  → 将事件转换成 SSE 持续推送给前端
//		  → 遇到 complete / stop / 断连 后结束

// executeAgentModeQA executes the agent mode
func (h *Handler) executeAgentModeQA(reqCtx *qaRequestContext) {
	ctx := reqCtx.ctx
	sessionID := reqCtx.sessionID

	// Emit agent query event
	if err := event.Emit(ctx, event.Event{
		Type:      event.EventAgentQuery,
		SessionID: sessionID,
		RequestID: reqCtx.requestID,
		Data: event.AgentQueryData{
			SessionID: sessionID,
			Query:     reqCtx.query,
			RequestID: reqCtx.requestID,
		},
	}); err != nil {
		logger.Errorf(ctx, "Failed to emit agent query event: %v", err)
		return
	}

	// Create user message
	if err := h.createUserMessage(ctx, sessionID, reqCtx.query, reqCtx.requestID, reqCtx.mentionedItems); err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Create assistant message
	assistantMessagePtr, err := h.createAssistantMessage(ctx, reqCtx.assistantMessage)
	if err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	reqCtx.assistantMessage = assistantMessagePtr

	logger.Infof(ctx, "Calling agent QA service, session ID: %s", sessionID)

	// Setup SSE stream (agent mode always generates title)
	streamCtx := h.setupSSEStream(reqCtx, true)

	// Execute AgentQA asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 1024)
				runtime.Stack(buf, true)
				logger.ErrorWithFields(streamCtx.asyncCtx,
					errors.NewInternalServerError(fmt.Sprintf("Agent QA service panicked: %v\n%s", r, string(buf))),
					map[string]interface{}{"session_id": sessionID})
			}
			// Use session's tenant for message update (session belongs to session.TenantID; asyncCtx may have effectiveTenantID when using shared agent)
			updateCtx := context.WithValue(streamCtx.asyncCtx, types.TenantIDContextKey, reqCtx.session.TenantID)
			h.completeAssistantMessage(updateCtx, streamCtx.assistantMessage)
			logger.Infof(streamCtx.asyncCtx, "Agent QA service completed for session: %s", sessionID)
		}()

		err := h.sessionService.AgentQA(
			streamCtx.asyncCtx,
			reqCtx.session,
			reqCtx.query,
			reqCtx.assistantMessage.ID,
			reqCtx.summaryModelID,
			streamCtx.eventBus,
			reqCtx.customAgent,
			reqCtx.knowledgeBaseIDs,
			reqCtx.knowledgeIDs,
		)
		if err != nil {
			logger.ErrorWithFields(streamCtx.asyncCtx, err, nil)
			streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
				Type:      event.EventError,
				SessionID: sessionID,
				Data: event.ErrorData{
					Error:     err.Error(),
					Stage:     "agent_execution",
					SessionID: sessionID,
				},
			})
		}
	}()

	// Handle SSE events (blocking)
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, streamCtx.eventBus, reqCtx.session.Title == "")
}

// completeAssistantMessage marks an assistant message as complete and updates it
func (h *Handler) completeAssistantMessage(ctx context.Context, assistantMessage *types.Message) {
	assistantMessage.UpdatedAt = time.Now()
	assistantMessage.IsCompleted = true
	_ = h.messageService.UpdateMessage(ctx, assistantMessage)
}
