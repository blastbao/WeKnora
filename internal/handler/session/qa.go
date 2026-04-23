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

// parseQARequest 解析并验证知识库问答请求，构建请求上下文对象
//
// 该函数是问答流程的入口点，负责解析HTTP请求、验证参数、加载会话、解析Agent配置、合并@提及的资源，
// 并构建完整的请求上下文。
//
// 执行过程：
// 1. 基础校验：从 URL 获取 SessionID，解析 JSON 请求体，校验 Query 内容非空
// 2. 获取会话：调用 SessionService 获取会话详情，确保会话存在
// 3. 解析智能体（Agent）：
//    - 优先尝试通过 AgentShareService 解析共享智能体，若成功则记录有效租户ID（用于检索范围控制）
//    - 若非共享智能体，则通过 CustomAgentService 获取自有智能体配置
// 4. 处理 @提及逻辑（关键）：
//    - 遍历请求中的 MentionedItems
//    - 将 @提及的知识库（kb）ID 合并至 knowledgeBaseIDs 列表
//    - 将 @提及的文件（file）ID 合并至 knowledgeIDs 列表
//    - 目的：确保即使用户未在侧边栏勾选，只要 @ 了特定资源，检索时也会包含这些资源
// 5. 构建上下文：封装 qaRequestContext 对象，包含会话、智能体、合并后的检索ID列表及用户配置，供后续流程使用

// 执行流程 details ：
//  1. 克隆上下文并记录日志（便于链路追踪）
//  2. 从URL路径参数获取session_id并进行防注入清理
//     - 验证非空，无效则返回400错误
//  3. 解析JSON请求体到CreateKnowledgeQARequest结构体
//     - 绑定失败返回400错误
//  4. 验证查询内容（query）非空
//  5. 记录请求日志（序列化为JSON并清理敏感信息）
//  6. 调用sessionService获取会话信息
//     - 会话不存在返回404错误
//  7. 解析Agent配置（如果请求中指定了agent_id）：
//     a) 优先尝试解析共享Agent：
//        - 需要用户ID和租户ID都存在
//        - 调用agentShareService.GetSharedAgentForUser获取共享Agent
//        - 成功后记录effectiveTenantID（用于检索隔离）
//     b) 如果共享Agent不存在，尝试获取自有Agent：
//        - 调用customAgentService.GetAgentByID
//     c) 两者都失败时记录警告日志，使用默认配置（customAgent保持nil）
//  8. 合并@提及的资源到知识库ID和知识ID列表：
//     a) 初始化kbIDs和knowledgeIDs切片及去重set
//     b) 添加请求中原有的KnowledgeBaseIDs
//     c) 添加请求中原有的KnowledgeIds
//     d) 遍历MentionedItems，根据类型分别合并：
//        - Type="kb"：合并到知识库ID列表
//        - Type="file"：合并到知识ID列表
//     e) 使用set去重，避免重复ID
//  9. 记录合并结果日志（便于调试@提及功能）
//  10. 构建qaRequestContext对象，包含：
//      - 原始上下文和Gin上下文
//      - 会话ID、请求ID、查询内容
//      - 会话对象、自定义Agent配置
//      - 助手消息对象（预创建，IsCompleted=false）
//      - 合并后的知识库ID列表、知识ID列表
//      - 摘要模型ID、Web搜索开关、记忆开关
//      - @提及项、生效租户ID（用于共享Agent场景）
//  11. 返回请求上下文和原始请求对象
//
// 参数：
//   - c: Gin上下文，包含HTTP请求信息和参数
//   - logPrefix: 日志前缀，用于区分不同调用场景（如"KnowledgeQA"或"AgentQA"）
//
// 返回值：
//   - *qaRequestContext: 构建好的请求上下文对象，包含后续处理所需的所有数据
//   - *CreateKnowledgeQARequest: 原始解析的请求对象，供调用方进一步使用
//   - error: 校验失败时的错误信息（400/404等）
//
// 核心特性：
//   1. 防注入：所有用户输入的ID都经过secutils.SanitizeForLog清理
//   2. 共享Agent支持：自动识别并解析共享Agent，设置正确的检索租户隔离
//   3. @提及资源合并：自动将用户在消息中@的知识库/文件合并到检索目标中
//      - 解决用户只在输入框@提及但未在侧边栏选择KB的问题
//   4. 配置降级：Agent获取失败时使用默认配置，不影响主流程
//   5. 租户隔离：共享Agent场景下设置effectiveTenantID确保数据安全
//
// 支持的@提及类型：
//   - "kb" (Knowledge Base)：知识库，合并到KnowledgeBaseIDs
//   - "file"：文件/文档，合并到KnowledgeIDs
//
// 示例场景：
//   用户输入："@项目文档 介绍一下这个项目"，同时侧边栏未选择任何知识库
//   - request.MentionedItems = [{ID:"kb_123", Type:"kb", Name:"项目文档"}]
//   - 合并后kbIDs = ["kb_123"]，确保检索会搜索该知识库
//
// 注意事项：
//   - 助手消息对象仅创建占位，实际持久化由调用方执行
//   - customAgent为nil时表示使用系统默认Agent配置
//   - effectiveTenantID仅在共享Agent场景下非0，用于覆盖当前租户ID（检索数据隔离）

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

// sseStreamContext holds the context for SSE streaming
type sseStreamContext struct {
	eventBus         *event.EventBus
	asyncCtx         context.Context
	cancel           context.CancelFunc
	assistantMessage *types.Message
}

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
	h.setupStreamHandler(asyncCtx, reqCtx.sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, reqCtx.assistantMessage, eventBus)

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
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, streamCtx.eventBus, shouldWaitForTitle)
}

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
