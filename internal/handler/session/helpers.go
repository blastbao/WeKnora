package session

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// convertMentionedItems 将 MentionedItemRequest 切片转换为 types.MentionedItems。
//
// 执行过程：
//  1. 若输入切片为空，直接返回 nil。
//  2. 创建与输入等长的 types.MentionedItems 切片。
//  3. 遍历每个 MentionedItemRequest，将其 ID、Name、Type、KBType 映射到 types.MentionedItem。
//  4. 返回转换后的结果切片。

// convertMentionedItems converts MentionedItemRequest slice to types.MentionedItems
func convertMentionedItems(items []MentionedItemRequest) types.MentionedItems {
	if len(items) == 0 {
		return nil
	}
	result := make(types.MentionedItems, len(items))
	for i, item := range items {
		result[i] = types.MentionedItem{
			ID:     item.ID,
			Name:   item.Name,
			Type:   item.Type,
			KBType: item.KBType,
		}
	}
	return result
}

// setSSEHeaders 设置标准的 SSE 响应头。
//
// 执行过程：
//  1. 设置 Content-Type 为 text/event-stream，标识 SSE 流格式。
//  2. 设置 Cache-Control 为 no-cache，禁止浏览器缓存。
//  3. 设置 Connection 为 keep-alive，保持长连接。
//  4. 设置 X-Accel-Buffering 为 no，禁用 Nginx 等代理的响应缓冲。
//
// 场景：在建立 SSE 连接前调用

// setSSEHeaders sets the standard Server-Sent Events headers
func setSSEHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

// buildStreamResponse 将内部 StreamEvent 构建为 API 响应格式 StreamResponse 。
//
// 执行过程：
//  1. 初始化 StreamResponse，填入基础字段：ID（使用 requestID）、ResponseType、Content、Done、Data。
//  2. 若事件类型为 ResponseTypeAgentQuery，从 evt.Data 中提取 session_id 和 assistant_message_id，写入 response 的对应字段，供前端关联会话与消息。
//  3. 若事件类型为 ResponseTypeReferences，对 KnowledgeReferences 做多层类型降级解析：
//     - 优先尝试直接转换为 types.References；
//     - 尝试转换为 []*types.SearchResult 并转换；
//     - 最后尝试 []interface{}（如 Redis 序列化后的场景），逐元素解析为 map[string]interface{}，提取各字段构建 SearchResult。
//  4. 返回构建完成的响应对象。

// buildStreamResponse constructs a StreamResponse from a StreamEvent
func buildStreamResponse(evt interfaces.StreamEvent, requestID string) *types.StreamResponse {
	response := &types.StreamResponse{
		ID:           requestID,
		ResponseType: evt.Type,
		Content:      evt.Content,
		Done:         evt.Done,
		Data:         evt.Data,
	}

	// Extract session_id and assistant_message_id for agent_query events
	if evt.Type == types.ResponseTypeAgentQuery {
		if sid, ok := evt.Data["session_id"].(string); ok {
			response.SessionID = sid
		}
		if amid, ok := evt.Data["assistant_message_id"].(string); ok {
			response.AssistantMessageID = amid
		}
	}

	// Special handling for references event
	if evt.Type == types.ResponseTypeReferences {
		refsData := evt.Data["references"]
		if refs, ok := refsData.(types.References); ok {
			response.KnowledgeReferences = refs
		} else if refs, ok := refsData.([]*types.SearchResult); ok {
			response.KnowledgeReferences = types.References(refs)
		} else if refs, ok := refsData.([]interface{}); ok {
			// Handle case where data was serialized/deserialized (e.g., from Redis)
			searchResults := make([]*types.SearchResult, 0, len(refs))
			for _, ref := range refs {
				if refMap, ok := ref.(map[string]interface{}); ok {
					sr := &types.SearchResult{
						ID:                getString(refMap, "id"),
						Content:           getString(refMap, "content"),
						KnowledgeID:       getString(refMap, "knowledge_id"),
						ChunkIndex:        int(getFloat64(refMap, "chunk_index")),
						KnowledgeTitle:    getString(refMap, "knowledge_title"),
						StartAt:           int(getFloat64(refMap, "start_at")),
						EndAt:             int(getFloat64(refMap, "end_at")),
						Seq:               int(getFloat64(refMap, "seq")),
						Score:             getFloat64(refMap, "score"),
						ChunkType:         getString(refMap, "chunk_type"),
						ParentChunkID:     getString(refMap, "parent_chunk_id"),
						ImageInfo:         getString(refMap, "image_info"),
						KnowledgeFilename: getString(refMap, "knowledge_filename"),
						KnowledgeSource:   getString(refMap, "knowledge_source"),
					}
					searchResults = append(searchResults, sr)
				}
			}
			response.KnowledgeReferences = types.References(searchResults)
		}
	}

	return response
}

// sendCompletionEvent 发送最终完成事件（已废弃）
// 注意：此函数现在为空实现，因为：
//   1. handleComplete 发送的 'complete' 事件已标识流结束
//   2. 额外发送空 answer 事件带 done:true 会导致前端状态混乱
//   3. 前端应使用 response_type='complete' 检测流结束

// sendCompletionEvent sends a final completion event to the client
// NOTE: This is now a no-op because:
//  1. The 'complete' event from handleComplete already signals stream completion
//  2. Sending an extra empty 'answer' event with done:true causes frontend issues
//     (multiple done events can confuse state management)
//
// The frontend should use 'complete' response_type to detect stream completion
func sendCompletionEvent(c *gin.Context, requestID string) {
	// Intentionally empty - completion is signaled by the 'complete' event
	// which is already sent before this function is called
}

// createAgentQueryEvent 创建标准的 Agent 查询开始事件
//
// 执行过程：
//  1. 生成唯一事件 ID，格式为 "query-{纳秒时间戳}"。
//  2. 设置事件类型为 ResponseTypeAgentQuery，Done 标记为 true（查询事件为瞬时态）。
//  3. 在 Data 中携带 session_id 和 assistant_message_id，供前端建立关联。

// createAgentQueryEvent creates a standard agent query event
func createAgentQueryEvent(sessionID, assistantMessageID string) interfaces.StreamEvent {
	return interfaces.StreamEvent{
		ID:        fmt.Sprintf("query-%d", time.Now().UnixNano()),
		Type:      types.ResponseTypeAgentQuery,
		Content:   "",
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id":           sessionID,
			"assistant_message_id": assistantMessageID,
		},
	}
}

// createUserMessage 将用户发送的消息持久化到数据库。
//
// 执行过程：
//  1. 构造 types.Message 对象，角色为 "user"，标记 IsCompleted 为 true。
//  2. 调用 messageService.CreateMessage 写入数据库。
//
// 参数：
//   - ctx: 上下文
//   - sessionID: 所属会话 ID
//   - query: 用户问题内容
//   - requestID: 请求唯一标识
//   - mentionedItems: @提及的资源列表

// createUserMessage creates a user message
func (h *Handler) createUserMessage(ctx context.Context, sessionID, query, requestID string, mentionedItems types.MentionedItems) error {
	_, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:      sessionID,
		Role:           "user",
		Content:        query,
		RequestID:      requestID,
		CreatedAt:      time.Now(),
		IsCompleted:    true,
		MentionedItems: mentionedItems,
	})
	return err
}

// createAssistantMessage 创建助手消息并存储到数据库
//
// 执行过程：
//  1. 为 assistantMessage 设置当前创建时间。
//  2. 调用 messageService.CreateMessage 写入数据库。

// createAssistantMessage creates an assistant message
func (h *Handler) createAssistantMessage(ctx context.Context, assistantMessage *types.Message) (*types.Message, error) {
	assistantMessage.CreatedAt = time.Now()
	return h.messageService.CreateMessage(ctx, assistantMessage)
}

// setupStreamHandler 创建并订阅 Agent 流式事件处理器。
//
// 执行过程：
//  1. 创建 AgentStreamHandler 实例
//  2. 调用 Subscribe 订阅所有事件
//
// 参数:
//   - ctx: 请求上下文。
//   - sessionID: 会话 ID。
//   - assistantMessageID: 助手消息 ID。
//   - requestID: 请求 ID。
//   - assistantMessage: 助手消息对象（用于状态更新）。
//   - eventBus: 事件总线。

// setupStreamHandler creates and subscribes a stream handler
func (h *Handler) setupStreamHandler(
	ctx context.Context,
	sessionID, assistantMessageID, requestID string,
	assistantMessage *types.Message,
	eventBus *event.EventBus,
) *AgentStreamHandler {
	streamHandler := NewAgentStreamHandler(
		ctx,
		sessionID,
		assistantMessageID,
		requestID,
		assistantMessage,
		h.streamManager,
		eventBus,
	)
	streamHandler.Subscribe()
	return streamHandler
}

// setupStopEventHandler 注册停止事件处理器
//
//
//
// 执行过程：
//  1. 在 eventBus 上订阅 EventStop 事件。
//  2. 收到停止事件时：
//     - 调用 cancel() 取消异步执行的 context 。
//     - 设置助手消息内容为"用户停止了本次对话"。
//     - 调用 completeAssistantMessage 将终止状态持久化。
//
// 参数：
//   - eventBus: 事件总线
//   - sessionID: 会话 ID
//   - sessionTenantID: 会话所属租户 ID（用于更新消息时的上下文）
//   - assistantMessage: 助手消息对象
//   - cancel: context 取消函数

// setupStopEventHandler registers a stop event handler
func (h *Handler) setupStopEventHandler(
	eventBus *event.EventBus,
	sessionID string,
	sessionTenantID uint64,
	assistantMessage *types.Message,
	cancel context.CancelFunc,
) {
	eventBus.On(event.EventStop, func(ctx context.Context, evt event.Event) error {
		logger.Infof(ctx, "Received stop event, cancelling async operations for session: %s", sessionID)
		cancel()
		assistantMessage.Content = "用户停止了本次对话"
		// Use session's tenant for message update (ctx may have effectiveTenantID when using shared agent)
		updateCtx := context.WithValue(ctx, types.TenantIDContextKey, sessionTenantID)
		h.completeAssistantMessage(updateCtx, assistantMessage)
		return nil
	})
}

// writeAgentQueryEvent 向流管理器写入 Agent 查询事件，通知前端 Agent 已开始处理用户请求。
//
// 执行过程：
//  1. 调用 createAgentQueryEvent 构建查询事件。
//  2. 调用 streamManager.AppendEvent 将事件追加到 SSE 流。
//  3. 若追加失败，记录错误日志（非致命错误，不中断流程）。

// writeAgentQueryEvent writes an agent query event to the stream manager
func (h *Handler) writeAgentQueryEvent(ctx context.Context, sessionID, assistantMessageID string) {
	agentQueryEvent := createAgentQueryEvent(sessionID, assistantMessageID)
	if err := h.streamManager.AppendEvent(ctx, sessionID, assistantMessageID, agentQueryEvent); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
		})
		// Non-fatal error, continue
	}
}

// getRequestID 从 gin 上下文中提取请求 ID。

// getRequestID gets the request ID from gin context
func getRequestID(c *gin.Context) string {
	return c.GetString(types.RequestIDContextKey.String())
}

// Helper function for type assertion with default value
func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

func getFloat64(m map[string]interface{}, key string) float64 {
	if val, ok := m[key].(float64); ok {
		return val
	}
	if val, ok := m[key].(int); ok {
		return float64(val)
	}
	return 0.0
}

// createDefaultSummaryConfig 创建默认的摘要配置。
//
// 执行过程：
//   1. 从 config.yaml 加载默认值
//   2. 如果租户有自定义配置，覆盖对应字段

// createDefaultSummaryConfig creates a default summary configuration from config
// It prioritizes tenant-level ConversationConfig, then falls back to config.yaml defaults
func (h *Handler) createDefaultSummaryConfig(ctx context.Context) *types.SummaryConfig {
	// Try to get tenant from context
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)

	// Initialize with config.yaml defaults
	cfg := &types.SummaryConfig{
		MaxTokens:           h.config.Conversation.Summary.MaxTokens,
		TopP:                h.config.Conversation.Summary.TopP,
		TopK:                h.config.Conversation.Summary.TopK,
		FrequencyPenalty:    h.config.Conversation.Summary.FrequencyPenalty,
		PresencePenalty:     h.config.Conversation.Summary.PresencePenalty,
		RepeatPenalty:       h.config.Conversation.Summary.RepeatPenalty,
		Prompt:              h.config.Conversation.Summary.Prompt,
		ContextTemplate:     h.config.Conversation.Summary.ContextTemplate,
		NoMatchPrefix:       h.config.Conversation.Summary.NoMatchPrefix,
		Temperature:         h.config.Conversation.Summary.Temperature,
		Seed:                h.config.Conversation.Summary.Seed,
		MaxCompletionTokens: h.config.Conversation.Summary.MaxCompletionTokens,
	}

	// Override with tenant-level conversation config if available
	if tenant != nil && tenant.ConversationConfig != nil {
		// Use custom prompt if provided
		if tenant.ConversationConfig.Prompt != "" {
			cfg.Prompt = tenant.ConversationConfig.Prompt
		}

		// Use custom context template if provided
		if tenant.ConversationConfig.ContextTemplate != "" {
			cfg.ContextTemplate = tenant.ConversationConfig.ContextTemplate
		}
		if tenant.ConversationConfig.Temperature > 0 {
			cfg.Temperature = tenant.ConversationConfig.Temperature
		}
		if tenant.ConversationConfig.MaxCompletionTokens > 0 {
			cfg.MaxCompletionTokens = tenant.ConversationConfig.MaxCompletionTokens
		}
	}

	return cfg
}

// fillSummaryConfigDefaults 补全配置中缺失的字段

// fillSummaryConfigDefaults fills missing fields in summary config with defaults
// It prioritizes tenant-level ConversationConfig, then falls back to config.yaml defaults
func (h *Handler) fillSummaryConfigDefaults(ctx context.Context, config *types.SummaryConfig) {
	// Try to get tenant from context
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)

	// Determine default values: tenant config first, then config.yaml
	var defaultPrompt, defaultContextTemplate, defaultNoMatchPrefix string
	var defaultTemperature float64
	var defaultMaxCompletionTokens int

	if tenant != nil && tenant.ConversationConfig != nil {
		// Use custom prompt if provided
		if tenant.ConversationConfig.Prompt != "" {
			defaultPrompt = tenant.ConversationConfig.Prompt
		}

		// Use custom context template if provided
		if tenant.ConversationConfig.ContextTemplate != "" {
			defaultContextTemplate = tenant.ConversationConfig.ContextTemplate
		}
		defaultTemperature = tenant.ConversationConfig.Temperature
		defaultMaxCompletionTokens = tenant.ConversationConfig.MaxCompletionTokens
	}

	// Fall back to config.yaml if tenant config is empty
	if defaultPrompt == "" {
		defaultPrompt = h.config.Conversation.Summary.Prompt
	}
	if defaultContextTemplate == "" {
		defaultContextTemplate = h.config.Conversation.Summary.ContextTemplate
	}
	if defaultTemperature == 0 {
		defaultTemperature = h.config.Conversation.Summary.Temperature
	}
	if defaultMaxCompletionTokens == 0 {
		defaultMaxCompletionTokens = h.config.Conversation.Summary.MaxCompletionTokens
	}
	defaultNoMatchPrefix = h.config.Conversation.Summary.NoMatchPrefix

	// Fill missing fields
	if config.Prompt == "" {
		config.Prompt = defaultPrompt
	}
	if config.ContextTemplate == "" {
		config.ContextTemplate = defaultContextTemplate
	}
	if config.Temperature == 0 {
		config.Temperature = defaultTemperature
	}
	if config.MaxCompletionTokens == 0 {
		config.MaxCompletionTokens = defaultMaxCompletionTokens
	}
	if config.NoMatchPrefix == "" {
		config.NoMatchPrefix = defaultNoMatchPrefix
	}
}
