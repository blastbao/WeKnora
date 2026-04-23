package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// AgentStreamHandler 负责将 Agent 执行过程中产生的事件转换为 SSE 流式响应。
// 它为每个请求使用专用的 EventBus 以避免 SessionID 过滤。
// 事件会被追加到 StreamManager 中，不在服务端进行内容累积（由前端负责累积）。

// AgentStreamHandler handles agent events for SSE streaming
// It uses a dedicated EventBus per request to avoid SessionID filtering
// Events are appended to StreamManager without accumulation
type AgentStreamHandler struct {
	ctx                context.Context
	sessionID          string
	assistantMessageID string
	requestID          string
	assistantMessage   *types.Message
	streamManager      interfaces.StreamManager

	eventBus *event.EventBus

	// State tracking
	knowledgeRefs   []*types.SearchResult
	finalAnswer     string
	eventStartTimes map[string]time.Time // Track start time for duration calculation
	mu              sync.Mutex
}

// NewAgentStreamHandler 创建一个用于 Agent SSE 流式传输的处理器

// NewAgentStreamHandler creates a new handler for agent SSE streaming
func NewAgentStreamHandler(
	ctx context.Context,
	sessionID, assistantMessageID, requestID string,
	assistantMessage *types.Message,
	streamManager interfaces.StreamManager,
	eventBus *event.EventBus,
) *AgentStreamHandler {
	return &AgentStreamHandler{
		ctx:                ctx,
		sessionID:          sessionID,
		assistantMessageID: assistantMessageID,
		requestID:          requestID,
		assistantMessage:   assistantMessage,
		streamManager:      streamManager,
		eventBus:           eventBus,
		knowledgeRefs:      make([]*types.SearchResult, 0),
		eventStartTimes:    make(map[string]time.Time),
	}
}

// Subscribe subscribes to all agent streaming events on the dedicated EventBus
// No SessionID filtering needed since we have a dedicated EventBus per request
func (h *AgentStreamHandler) Subscribe() {
	// Subscribe to all agent streaming events on the dedicated EventBus
	h.eventBus.On(event.EventAgentThought, h.handleThought)
	h.eventBus.On(event.EventAgentToolCall, h.handleToolCall)
	h.eventBus.On(event.EventAgentToolResult, h.handleToolResult)
	h.eventBus.On(event.EventAgentReferences, h.handleReferences)
	h.eventBus.On(event.EventAgentFinalAnswer, h.handleFinalAnswer)
	h.eventBus.On(event.EventAgentReflection, h.handleReflection)
	h.eventBus.On(event.EventError, h.handleError)
	h.eventBus.On(event.EventSessionTitle, h.handleSessionTitle)
	h.eventBus.On(event.EventAgentComplete, h.handleComplete)
}

// handleThought 处理Agent思考事件
// 将LLM流式返回的思考内容转换为SSE事件，实时推送给前端
//
// 执行过程：
//  1. 类型断言：将事件数据转换为AgentThoughtData，失败则静默返回
//  2. 加锁保护：操作eventStartTimes map前获取互斥锁
//  3. 时间追踪：
//     - 检查当前事件ID是否已有开始时间
//     - 若无记录且为首次chunk，记录开始时间戳
//     - 后续chunk复用已有开始时间
//  4. 构建元数据：
//     - 若为最后一个chunk（Done=true）：计算总耗时（当前时间-开始时间），
//       生成包含event_id、duration_ms、completed_at的元数据，并从map中删除开始时间记录
//     - 若为中间chunk（Done=false）：仅记录event_id
//  5. 解锁：释放互斥锁，允许其他事件处理
//  6. 追加事件：调用 streamManager.AppendEvent 将当前 chunk 追加到 SSE 流中
//     - 事件类型：ResponseTypeThinking
//     - 事件内容：当前 chunk 的思考文本，不做后端累积，由前端按 evt.ID 聚合。
//     - 完成标志：透传data.Done
//     - 元数据：包含耗时信息（完成时）
//  7. 错误处理：追加失败时记录错误日志，但不中断 Agent 执行（避免影响其他事件），最终返回 nil。

// handleThought handles agent thought events
func (h *AgentStreamHandler) handleThought(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentThoughtData)
	if !ok {
		return nil
	}

	h.mu.Lock()

	// Track start time on first chunk
	if _, exists := h.eventStartTimes[evt.ID]; !exists {
		h.eventStartTimes[evt.ID] = time.Now()
	}

	// Calculate duration if done
	var metadata map[string]interface{}
	if data.Done {
		startTime := h.eventStartTimes[evt.ID]
		duration := time.Since(startTime)
		metadata = map[string]interface{}{
			"event_id":     evt.ID,
			"duration_ms":  duration.Milliseconds(),
			"completed_at": time.Now().Unix(),
		}
		delete(h.eventStartTimes, evt.ID)
	} else {
		metadata = map[string]interface{}{
			"event_id": evt.ID,
		}
	}

	h.mu.Unlock()

	// Append this chunk to stream (no accumulation - frontend will accumulate)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeThinking,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append thought event to stream failed", "error", err)
	}

	return nil
}

// handleToolCall handles tool call events
func (h *AgentStreamHandler) handleToolCall(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentToolCallData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	// Track start time for this tool call (use tool_call_id as key)
	h.eventStartTimes[data.ToolCallID] = time.Now()
	h.mu.Unlock()

	metadata := map[string]interface{}{
		"tool_name":    data.ToolName,
		"arguments":    data.Arguments,
		"tool_call_id": data.ToolCallID,
	}

	// Append event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeToolCall,
		Content:   fmt.Sprintf("Calling tool: %s", data.ToolName),
		Done:      false,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool call event to stream failed", "error", err)
	}

	return nil
}

// handleToolResult 处理工具调用结果（Tool Result）事件
//
// 执行过程：
// 1. 数据解析：尝试将事件数据断言为 AgentToolResultData 类型，若失败则忽略
// 2. 耗时计算（加锁）：
//    - 优先根据 ToolCallID 查找记录的开始时间，计算执行耗时（毫秒）并清理记录
//    - 若未找到开始时间记录，则回退使用数据中提供的 Duration 字段作为耗时
// 3. 响应类型判定：
//    - 若工具执行成功（Success=true），设置类型为 ResponseTypeToolResult，内容为输出结果
//    - 若执行失败（Success=false），设置类型为 ResponseTypeError，内容优先使用错误信息
// 4. 元数据构建：
//    - 构建包含工具名称、执行状态、输出/错误信息、耗时及调用ID的基础元数据
//    - 将数据中的扩展字段（如 display_type, formatted results 等）合并至元数据，支持前端富文本渲染
// 5. 流式推送：调用 streamManager.AppendEvent 将结果内容及完整元数据推送到 SSE 流
//     - 事件ID：透传evt.ID
//     - 类型：根据成功/失败动态设置（ResponseTypeToolResult或ResponseTypeError）
//     - 内容：工具执行结果或错误信息
//     - 完成标志：固定为false（工具结果不是流的结束标志）
//     - 元数据：包含耗时、工具信息、结构化数据等
// 6. 错误处理：若推送失败，记录错误日志

// handleToolResult handles tool result events
func (h *AgentStreamHandler) handleToolResult(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentToolResultData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	// Calculate duration from start time if available, otherwise use provided duration
	var durationMs int64
	if startTime, exists := h.eventStartTimes[data.ToolCallID]; exists {
		durationMs = time.Since(startTime).Milliseconds()
		delete(h.eventStartTimes, data.ToolCallID)
	} else if data.Duration > 0 {
		// Fallback to provided duration if start time not tracked
		durationMs = data.Duration
	}
	h.mu.Unlock()

	// Send SSE response (both success and failure)
	responseType := types.ResponseTypeToolResult
	content := data.Output
	if !data.Success {
		responseType = types.ResponseTypeError
		if data.Error != "" {
			content = data.Error
		}
	}

	// Build metadata including tool result data for rich frontend rendering
	metadata := map[string]interface{}{
		"tool_name":    data.ToolName,
		"success":      data.Success,
		"output":       data.Output,
		"error":        data.Error,
		"duration_ms":  durationMs,
		"tool_call_id": data.ToolCallID,
	}

	// Merge tool result data (contains display_type, formatted results, etc.)
	if data.Data != nil {
		for k, v := range data.Data {
			metadata[k] = v
		}
	}

	// Append event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      responseType,
		Content:   content,
		Done:      false,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool result event to stream failed", "error", err)
	}

	return nil
}

// handleReferences 处理知识引用事件，解析并收集检索结果，更新助手消息并推送引用事件到 SSE 流。
//
// 执行过程：
//  1. 类型断言：将事件数据断言为 AgentReferencesData，类型不匹配则静默返回。
//  2. 加锁进入临界区（全程持有锁，因涉及共享切片读写）：
//     - 提取知识引用：优先尝试将 data.References 直接断言为 []*types.SearchResult，成功则全部追加到 h.knowledgeRefs。
//     - 若直接断言失败，降级尝试断言为 []interface{}，逐元素二次解析：
//       * 若元素本身为 *types.SearchResult，直接追加。
//       * 若元素为 map[string]interface{}，手动提取各字段构建 SearchResult；
//         若其中存在 metadata 子字典，遍历并过滤 string 类型值写入 SearchResult.Metadata。
//       * 其他类型元素忽略。
//     - 更新助手消息：将解析后的 h.knowledgeRefs 赋值给 assistantMessage.KnowledgeReferences，
//       确保后续入库包含完整引用。
//  3. 解锁（defer 自动释放）。
//  4. 向 SSE 流追加引用事件：
//     - Type 为 ResponseTypeReferences，Content 为空（数据在 Data 字段）。
//     - Data 包含 "references" 键，值为 types.References 转换后的 h.knowledgeRefs。
//  5. 若推送失败，记录错误日志，但不中断 Agent 执行，最终返回 nil。

// handleReferences handles knowledge references events
func (h *AgentStreamHandler) handleReferences(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentReferencesData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Extract knowledge references
	// Try to cast directly to []*types.SearchResult first
	if searchResults, ok := data.References.([]*types.SearchResult); ok {
		h.knowledgeRefs = append(h.knowledgeRefs, searchResults...)
	} else if refs, ok := data.References.([]interface{}); ok {
		// Fallback: convert from []interface{}
		for _, ref := range refs {
			if sr, ok := ref.(*types.SearchResult); ok {
				h.knowledgeRefs = append(h.knowledgeRefs, sr)
			} else if refMap, ok := ref.(map[string]interface{}); ok {
				// Parse from map if needed
				searchResult := &types.SearchResult{
					ID:             getString(refMap, "id"),
					Content:        getString(refMap, "content"),
					Score:          getFloat64(refMap, "score"),
					KnowledgeID:    getString(refMap, "knowledge_id"),
					KnowledgeTitle: getString(refMap, "knowledge_title"),
					ChunkIndex:     int(getFloat64(refMap, "chunk_index")),
				}

				if meta, ok := refMap["metadata"].(map[string]interface{}); ok {
					metadata := make(map[string]string)
					for k, v := range meta {
						if strVal, ok := v.(string); ok {
							metadata[k] = strVal
						}
					}
					searchResult.Metadata = metadata
				}

				h.knowledgeRefs = append(h.knowledgeRefs, searchResult)
			}
		}
	}

	// Update assistant message references
	h.assistantMessage.KnowledgeReferences = h.knowledgeRefs

	// Append references event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeReferences,
		Content:   "",
		Done:      false,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"references": types.References(h.knowledgeRefs),
		},
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append references event to stream failed", "error", err)
	}

	return nil
}

// handleFinalAnswer 处理 Agent 最终答案事件，将答案内容以 SSE 流式推送给前端，并在本地累积完整内容用于入库。
//
// 执行过程：
//  1. 类型断言：将事件数据断言为 AgentFinalAnswerData，类型不匹配则静默返回。
//  2. 加锁进入临界区：
//     - 若该事件首次收到 chunk（evt.ID 不在 eventStartTimes 中），记录当前时间为开始时间。
//     - 本地累积内容：将当前 chunk（data.Content）追加到 h.finalAnswer，用于后续写入数据库。
//     - 若该事件已结束（data.Done == true），计算从首块到末块的总耗时，生成包含
//       duration_ms 和 completed_at 的 metadata，并从 eventStartTimes 中删除该记录以防内存泄漏。
//     - 若未结束，仅生成包含 event_id 的基础 metadata。
//  3. 解锁退出临界区。
//  4. 调用 streamManager.AppendEvent 将当前 chunk 追加到 SSE 流中：
//     - Content 仅包含当前 chunk，不做后端拼接，由前端按 evt.ID 聚合展示。
//     - Type 固定为 ResponseTypeAnswer。
//     - 附带 metadata（耗时信息）。
//  5. 若推送失败，记录错误日志，但不中断 Agent 执行，最终返回 nil。

// handleFinalAnswer handles final answer events
func (h *AgentStreamHandler) handleFinalAnswer(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentFinalAnswerData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	// Track start time on first chunk
	if _, exists := h.eventStartTimes[evt.ID]; !exists {
		h.eventStartTimes[evt.ID] = time.Now()
	}

	// Accumulate final answer locally for assistant message (database)
	h.finalAnswer += data.Content

	// Calculate duration if done
	var metadata map[string]interface{}
	if data.Done {
		startTime := h.eventStartTimes[evt.ID]
		duration := time.Since(startTime)
		metadata = map[string]interface{}{
			"event_id":     evt.ID,
			"duration_ms":  duration.Milliseconds(),
			"completed_at": time.Now().Unix(),
		}
		delete(h.eventStartTimes, evt.ID)
	} else {
		metadata = map[string]interface{}{
			"event_id": evt.ID,
		}
	}
	h.mu.Unlock()

	// Append this chunk to stream (frontend will accumulate by event ID)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeAnswer,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append answer event to stream failed", "error", err)
	}

	return nil
}

// handleReflection 处理 Agent 反思事件，将反思内容以 SSE 流式推送给前端。
//
// 执行过程：
//  1. 类型断言：将事件数据断言为 AgentReflectionData，类型不匹配则静默返回。
//  2. 调用 streamManager.AppendEvent 将当前 chunk 追加到 SSE 流中：
//     - Content 仅包含当前 chunk，不做后端累积，由前端按 evt.ID 聚合展示。
//     - Type 固定为 ResponseTypeReflection。
//     - Done 标记该反思事件是否结束。
//  3. 若推送失败，记录错误日志，但不中断 Agent 执行，最终返回 nil。

// handleReflection handles agent reflection events
func (h *AgentStreamHandler) handleReflection(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentReflectionData)
	if !ok {
		return nil
	}

	// Append this chunk to stream (frontend will accumulate by event ID)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeReflection,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append reflection event to stream failed", "error", err)
	}

	return nil
}

// handleError 处理 Agent 执行过程中的错误事件，将错误信息以 SSE 流式推送给前端。
//
// 执行过程：
//  1. 类型断言：将事件数据断言为 ErrorData，类型不匹配则静默返回。
//  2. 构建错误元数据 metadata：包含错误阶段 stage 和错误详情 error。
//  3. 调用 streamManager.AppendEvent 将错误事件追加到 SSE 流中：
//     - Type 固定为 ResponseTypeError。
//     - Content 为错误描述文本，供前端直接展示。
//     - Done 标记为 true，表示该错误事件为终结态，无需后续 chunk。
//     - Data 附带 stage 和 error 元数据，便于前端分类处理与定位问题。
//  4. 若推送失败，记录错误日志，但不中断 Agent 执行，最终返回 nil。

// handleError handles error events
func (h *AgentStreamHandler) handleError(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.ErrorData)
	if !ok {
		return nil
	}

	// Build error metadata
	metadata := map[string]interface{}{
		"stage": data.Stage,
		"error": data.Error,
	}

	// Append error event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeError,
		Content:   data.Error,
		Done:      true,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append error event to stream failed", "error", err)
	}

	return nil
}

// handleSessionTitle handles session title update events
func (h *AgentStreamHandler) handleSessionTitle(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.SessionTitleData)
	if !ok {
		return nil
	}

	// Use background context for title event since it may arrive after stream completion
	bgCtx := context.Background()

	// Append title event to stream
	if err := h.streamManager.AppendEvent(bgCtx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeSessionTitle,
		Content:   data.Title,
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id": data.SessionID,
			"title":      data.Title,
		},
	}); err != nil {
		logger.GetLogger(h.ctx).Warn("Append session title event to stream failed (stream may have ended)", "error", err)
	}

	return nil
}

// handleComplete 处理Agent执行完成事件
// 当Agent完成所有任务后，更新助手消息的最终状态并通过SSE通知前端流结束
//
// 执行过程：
//  1. 类型断言：将事件数据转换为AgentCompleteData，类型不匹配时忽略并返回。
//  2. 加锁：获取互斥锁保护 assistantMessage 的并发修改
//  3. 延迟解锁：使用 defer 确保函数返回时释放锁
//  4. 消息 ID 校验：仅当 data.MessageID 与当前 handler 的 assistantMessageID 相等时才更新，目的是防止并发场景下错误更新其他消息
//  5. 更新助手消息状态：
//     a) 设置完成标志：assistantMessage.IsCompleted = true（注意：assistantMessage.Content 已在 handleFinalAnswer 中累积，此处不需要赋值）
//     b) 更新知识引用：如果有新的KnowledgeRefs，先转换为[]*types.SearchResult类型，再赋值给 assistantMessage.KnowledgeReferences
//     c) 更新Agent执行步骤：如果有AgentSteps，先转换为[]types.AgentStep类型，再赋值给 assistantMessage.AgentSteps
//  6. 发送完成事件到StreamManager：
//     - 事件类型：ResponseTypeComplete（前端识别为流结束标志）
//     - Done: true（告知前端数据流已结束）
//     - Data 中携带执行统计信息：总步骤数(total_steps)和总耗时毫秒数(total_duration_ms)
//  7. 错误处理：AppendEvent 失败时记录错误日志但不返回错误，避免中断其他处理

// handleComplete handles agent complete events
func (h *AgentStreamHandler) handleComplete(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentCompleteData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Update assistant message with final data
	if data.MessageID == h.assistantMessageID {
		// h.assistantMessage.Content = data.FinalAnswer
		h.assistantMessage.IsCompleted = true

		// Update knowledge references if provided
		if len(data.KnowledgeRefs) > 0 {
			knowledgeRefs := make([]*types.SearchResult, 0, len(data.KnowledgeRefs))
			for _, ref := range data.KnowledgeRefs {
				if sr, ok := ref.(*types.SearchResult); ok {
					knowledgeRefs = append(knowledgeRefs, sr)
				}
			}
			h.assistantMessage.KnowledgeReferences = knowledgeRefs
		}

		// Update agent steps if provided
		if data.AgentSteps != nil {
			if steps, ok := data.AgentSteps.([]types.AgentStep); ok {
				h.assistantMessage.AgentSteps = steps
			}
		}
	}

	// Send completion event to stream manager so SSE can detect completion
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeComplete,
		Content:   "",
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"total_steps":       data.TotalSteps,
			"total_duration_ms": data.TotalDurationMs,
		},
	}); err != nil {
		logger.GetLogger(h.ctx).Errorf("Append complete event to stream failed: %v", err)
	}

	return nil
}
