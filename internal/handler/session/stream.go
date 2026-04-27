package session

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// ContinueStream
//	│
//	├── 1. 获取 ctx
//	│      → 从 c.Request.Context() 取请求上下文
//	│      → 用于日志和后续 streamManager 查询
//	│
//	├── 2. 读取 session_id
//	│      → 从 URL path 参数中取 session_id
//	│      → 如果为空，返回 BadRequest
//	│
//	├── 3. 读取 message_id
//	│      → 从 query 参数中取 message_id
//	│      → 这个 message_id 指向的是本轮 assistant 消息
//	│      → 如果为空，返回 BadRequest
//	│
//	├── 4. 校验 session 是否存在
//	│      → 调 h.sessionService.GetSession(ctx, sessionID)
//	│      → 同时也隐含完成租户归属校验
//	│      → 如果 session 不存在，返回 404 / 500
//	│
//	├── 5. 查询目标 message
//	│      → 调 h.messageService.GetMessage(ctx, sessionID, messageID)
//	│      → 获取当前要续流的 assistant message
//	│      → 如果 message 不存在，返回 404
//	│
//	├── 6. 从 streamManager 读取已有事件
//	│      → 调 h.streamManager.GetEvents(ctx, sessionID, messageID, 0)
//	│      → 从 offset=0 开始取这轮流里所有已存在的事件
//	│      → 返回：
//	│           - events
//	│           - currentOffset
//	│
//	├── 7. 如果没有任何事件
//	│      → 说明这条流根本还没建立，或者数据已经丢失
//	│      → 返回 404 "No stream events found"
//	│
//	├── 8. 设置 SSE 响应头
//	│      → 调 setSSEHeaders(c)
//	│      → 准备进入 SSE 流式返回模式
//	│
//	├── 9. 检查已有事件里是否已经 complete
//	│      → 遍历 events
//	│      → 只要发现 evt.Type == "complete"
//	│      → 就说明这轮流其实已经结束了，因为 complete 是这条流的结束信号。
//	│
//	├── 10. 回放已有事件
//	│       → 遍历 events
//	│       → 每个 evt 都转成 StreamResponse
//	│       → c.SSEvent("message", response)
//	│       → c.Writer.Flush()
//	│       → 这一步的作用是：
//	│           让断线重连的前端先补齐之前漏掉的所有流式内容
//	│
//	├── 11. 如果流已经完成
//	│       → replay 完已有事件后
//	│       → 再 sendCompletionEvent(c, message.RequestID)
//	│       → 然后直接 return
//	│       → 也就是说：
//	│           “只做补发，不再继续监听新事件”
//	│
//	├── 12. 如果流还没完成，开始轮询新事件
//	│       → 创建 100ms ticker
//	│       → 进入 for-select 循环
//	│
//	├── 13. 监听客户端断连
//	│       → case <-c.Request.Context().Done()
//	│       → 如果客户端又断开了，就直接退出
//	│
//	├── 14. 定时拉取新事件
//	│       → 每 100ms 调一次：
//	│           h.streamManager.GetEvents(ctx, sessionID, messageID, currentOffset)
//	│       → 只取 offset 之后的新事件
//	│
//	├── 15. 推送新事件
//	│       → 遍历 newEvents
//	│       → buildStreamResponse(evt, message.RequestID)
//	│       → c.SSEvent("message", response)
//	│       → c.Writer.Flush()
//	│
//	├── 16. 检查本次新事件里是否出现 complete
//	│       → 如果某个 evt.Type == "complete"
//	│       → 标记 streamCompletedNow = true
//	│
//	├── 17. 更新 currentOffset
//	│       → currentOffset = newOffset
//	│       → 确保下一轮只读更新增量
//	│
//	├── 18. 如果本轮检测到 complete
//	│       → sendCompletionEvent(c, message.RequestID)
//	│       → return
//	│       → 结束这次续流连接
//	│
//	└── 19. 整体作用
//		  → 先补发旧事件
//		  → 再继续监听新事件
//		  → 相当于“断线重连后接着看这轮回答”

// ContinueStream godoc
// @Summary      继续流式响应
// @Description  继续获取正在进行的流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string  true  "会话ID"
// @Param        message_id  query     string  true  "消息ID"
// @Success      200         {object}  map[string]interface{}  "流式响应"
// @Failure      404         {object}  errors.AppError         "会话或消息不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/continue [get]
func (h *Handler) ContinueStream(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start continuing stream response processing")

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Get message ID from query parameter
	messageID := secutils.SanitizeForLog(c.Query("message_id"))
	if messageID == "" {
		logger.Error(ctx, "Message ID is empty")
		c.Error(errors.NewBadRequestError("Missing message ID"))
		return
	}

	logger.Infof(ctx, "Continuing stream, session ID: %s, message ID: %s", sessionID, messageID)

	// Verify that the session exists and belongs to this tenant
	_, err := h.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		if err == errors.ErrSessionNotFound {
			logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
			c.Error(errors.NewNotFoundError(err.Error()))
		} else {
			logger.ErrorWithFields(ctx, err, nil)
			c.Error(errors.NewInternalServerError(err.Error()))
		}
		return
	}

	// Get the incomplete message
	message, err := h.messageService.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	if message == nil {
		logger.Warnf(ctx, "Incomplete message not found, session ID: %s, message ID: %s", sessionID, messageID)
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "Incomplete message not found",
		})
		return
	}

	// Get initial events from stream (offset 0)
	events, currentOffset, err := h.streamManager.GetEvents(ctx, sessionID, messageID, 0)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(fmt.Sprintf("Failed to get stream data: %s", err.Error())))
		return
	}

	if len(events) == 0 {
		logger.Warnf(ctx, "No events found in stream, session ID: %s, message ID: %s", sessionID, messageID)
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "No stream events found",
		})
		return
	}

	logger.Infof(
		ctx, "Preparing to replay %d events and continue streaming, session ID: %s, message ID: %s",
		len(events), sessionID, messageID,
	)

	// Set headers for SSE
	setSSEHeaders(c)

	// Check if stream is already completed
	streamCompleted := false
	for _, evt := range events {
		if evt.Type == "complete" {
			streamCompleted = true
			break
		}
	}

	// Replay existing events
	logger.Debugf(ctx, "Replaying %d existing events", len(events))
	for _, evt := range events {
		response := buildStreamResponse(evt, message.RequestID)
		c.SSEvent("message", response)
		c.Writer.Flush()
	}

	// If stream is already completed, send final event and return
	if streamCompleted {
		logger.Infof(ctx, "Stream already completed, session ID: %s, message ID: %s", sessionID, messageID)
		sendCompletionEvent(c, message.RequestID)
		return
	}

	// Continue polling for new events
	logger.Debug(ctx, "Starting event update monitoring")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			logger.Debug(ctx, "Client connection closed")
			return

		case <-ticker.C:
			// Get new events from current offset
			newEvents, newOffset, err := h.streamManager.GetEvents(ctx, sessionID, messageID, currentOffset)
			if err != nil {
				logger.Errorf(ctx, "Failed to get new events: %v", err)
				return
			}

			// Send new events
			streamCompletedNow := false
			for _, evt := range newEvents {
				// Check for completion event
				if evt.Type == "complete" {
					streamCompletedNow = true
				}

				response := buildStreamResponse(evt, message.RequestID)
				c.SSEvent("message", response)
				c.Writer.Flush()
			}

			// Update offset
			currentOffset = newOffset

			// If stream completed, send final event and exit
			if streamCompletedNow {
				logger.Infof(ctx, "Stream completed, session ID: %s, message ID: %s", sessionID, messageID)
				sendCompletionEvent(c, message.RequestID)
				return
			}
		}
	}
}

// StopSession
//	│
//	├── 1. 获取 ctx 和 sessionID
//	│      → 从请求上下文克隆 logger context
//	│      → 从 URL path 里取 session_id
//	│      → 如果 session_id 为空，直接返回 400
//	│
//	├── 2. 解析请求体
//	│      → 解析 StopSessionRequest
//	│      → 目标是拿到 message_id（也就是当前正在生成的 assistantMessageID）
//	│      → 如果没传或格式不对，返回 400
//	│
//	├── 3. 获取并校验 tenantID
//	│      → 从 gin context 里取当前租户 ID
//	│      → 如果取不到，说明未授权，返回 401
//	│
//	├── 4. 查询目标 message
//	│      → 调 h.messageService.GetMessage(ctx, sessionID, assistantMessageID)
//	│      → 确认这条 assistant 消息存在
//	│      → 如果不存在，返回 404
//	│
//	├── 5. 校验 message 是否真的属于这个 session
//	│      → message.SessionID 必须等于 sessionID
//	│      → 防止传错 message_id 或越权操作
//	│      → 不匹配则返回 403
//	│
//	├── 6. 查询 session
//	│      → 调 h.sessionService.GetSession(ctx, sessionID)
//	│      → 确认该 session 存在
//	│      → 如果 session 不存在，返回 404
//	│
//	├── 7. 校验 session 是否属于当前 tenant
//	│      → session.TenantID 必须等于 tenantIDUint
//	│      → 防止跨租户停止别人的对话
//	│      → 不匹配则返回 403
//	│
//	├── 8. 检查 message 是否已经完成
//	│      → 如果 message.IsCompleted == true
//	│      → 说明这轮回答已经结束，没必要再 stop
//	│      → 直接返回 success=true, "Message already completed"
//	│
//	├── 9. 构造 stopEvent
//	│      → 创建一个 StreamEvent：
//	│           - ID = stop-时间戳
//	│           - Type = stop
//	│           - Done = true
//	│           - Data = {session_id, message_id, reason}
//	│      → 这个事件代表“用户请求停止当前这轮生成”
//	│
//	├── 10. 把 stopEvent 写入 streamManager
//	│       → 调 h.streamManager.AppendEvent(ctx, sessionID, assistantMessageID, stopEvent)
//	│       → 这是 stop 入口最关键的一步
//	│       → 把“停止信号”写进当前这轮流事件里
//	│
//	├── 11. 处理写入失败
//	│       → 如果 AppendEvent 失败
//	│       → 记录错误日志
//	│       → 返回 500
//	│
//	├── 12. 写入成功
//	│       → 记录 stop event 写入成功日志
//	│
//	└── 13. 返回 HTTP 200
//		  → 返回：
//		      {
//		          "success": true,
//		          "message": "Generation stopped"
//		      }
//		  → 这里只是“成功发起停止请求”
//		  → 真正的取消动作会在后续 SSE/事件链路里发生

// StopSession godoc
// @Summary      停止生成
// @Description  停止当前正在进行的生成任务
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        session_id  path      string              true  "会话ID"
// @Param        request     body      StopSessionRequest  true  "停止请求"
// @Success      200         {object}  map[string]interface{}  "停止成功"
// @Failure      404         {object}  errors.AppError         "会话或消息不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/stop [post]
func (h *Handler) StopSession(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))

	if sessionID == "" {
		c.JSON(400, gin.H{"error": "Session ID is required"})
		return
	}

	// Parse request body to get message_id
	var req StopSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
		})
		c.JSON(400, gin.H{"error": "message_id is required"})
		return
	}

	assistantMessageID := secutils.SanitizeForLog(req.MessageID)
	logger.Infof(ctx, "Stop generation request for session: %s, message: %s", sessionID, assistantMessageID)

	// Get tenant ID from context
	tenantID, exists := c.Get(types.TenantIDContextKey.String())
	if !exists {
		logger.Error(ctx, "Failed to get tenant ID")
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}
	tenantIDUint := tenantID.(uint64)

	// Verify message ownership and status
	message, err := h.messageService.GetMessage(ctx, sessionID, assistantMessageID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
		})
		c.JSON(404, gin.H{"error": "Message not found"})
		return
	}

	// Verify message belongs to this session (double check)
	if message.SessionID != sessionID {
		logger.Warnf(ctx, "Message %s does not belong to session %s", assistantMessageID, sessionID)
		c.JSON(403, gin.H{"error": "Message does not belong to this session"})
		return
	}

	// Verify message belongs to the current tenant
	session, err := h.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
		})
		c.JSON(404, gin.H{"error": "Session not found"})
		return
	}

	if session.TenantID != tenantIDUint {
		logger.Warnf(ctx, "Session %s does not belong to tenant %d", sessionID, tenantIDUint)
		c.JSON(403, gin.H{"error": "Access denied"})
		return
	}

	// Check if message is already completed (stopped)
	if message.IsCompleted {
		logger.Infof(ctx, "Message %s is already completed, no need to stop", assistantMessageID)
		c.JSON(200, gin.H{
			"success": true,
			"message": "Message already completed",
		})
		return
	}

	// Write stop event to StreamManager for distributed support
	stopEvent := interfaces.StreamEvent{
		ID:        fmt.Sprintf("stop-%d", time.Now().UnixNano()),
		Type:      types.ResponseType(event.EventStop),
		Content:   "",
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
			"reason":     "user_requested",
		},
	}

	if err := h.streamManager.AppendEvent(ctx, sessionID, assistantMessageID, stopEvent); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
		})
		c.JSON(500, gin.H{"error": "Failed to write stop event"})
		return
	}

	logger.Infof(ctx, "Stop event written successfully for session: %s, message: %s", sessionID, assistantMessageID)
	c.JSON(200, gin.H{
		"success": true,
		"message": "Generation stopped",
	})
}

// 在 AI 对话系统中，事件的典型产生顺序：
//
//	开始生成
//    ↓
//	内容事件 (content) ──→ 逐词推送
//	  ↓
//	内容事件 (content) ──→ 逐词推送
//	  ↓
//	内容事件 (content) ──→ 逐词推送
//	  ↓
//	 ...
//	  ↓
//	完成事件 (complete) ←── 所有内容生成完毕（此时才确定完整对话内容）
//	  ↓
//	[后台生成标题] ←── 基于完整对话内容生成标题
//	  ↓
//	标题事件 (title) ←── 标题生成完毕，推送给前端
//
//
// 示例 -- 时间线：
//	T0: 用户发送消息
//	T1: 开始生成回答
//	T2: "Go 语言的并发模型基于 goroutine..."		← 第一个内容块
//	T3: "...这是一种轻量级线程。"  				← 最后一个内容块
//	T4: 触发完成事件 (complete)      				← 此时内容已完整
//	T5: 后台开始生成标题（基于完整对话）：			← "分析对话内容 → 提取主题 → 生成标题"
//	T6: 标题生成完毕："Go语言并发模型详解"
//	T7: 触发标题事件 (title)         				← 标题已生成，可以保存到数据库

// handleAgentEventsForSSE 采用轮询模式从 StreamManager 中拉取事件并推送给客户端。
//
// 作用：消费 EventBus 上的事件，转发给 streamManager，最终推送到前端 SSE 连接               │
//                                                                                        │
// 执行流程：                                                                                │
//   1. 等待 EventBus 上的事件                                                                 │
//   2. 收到 complete 事件 → 发送 SSE 完成信号 → 退出循环                                    │
//   3. 收到 stop 事件   → 退出循环                                                           │
//   4. 收到 error 事件  → 记录日志 → 退出循环
//
// 核心逻辑：
//   - 使用 100ms 间隔轮询 StreamManager，通过 offset 机制确保事件不遗漏
//   - 检测到停止事件时，通过 EventBus 发射停止信号，并向前端发送停止通知
//   - 流完成后，若 waitForTitle 为 true，最多等待 3 秒接收会话标题事件
//   - 客户端断连时（Context.Done），立即退出，避免资源泄漏和 panic
//
// 参数:
//   - eventBus: 事件总线，用于发射停止事件以触发上下文取消
//   - waitForTitle: 是否在流完成后等待标题事件（适用于无标题的新会话）

// handleAgentEventsForSSE handles agent events for SSE streaming using an existing handler
// The handler is already subscribed to events and AgentQA is already running
// This function polls StreamManager and pushes events to SSE, allowing graceful handling of disconnections
// waitForTitle: if true, wait for title event after completion (for new sessions without title)
func (h *Handler) handleAgentEventsForSSE(
	ctx context.Context,
	c *gin.Context,
	sessionID, assistantMessageID, requestID string,
	eventBus *event.EventBus,
	waitForTitle bool,
) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	lastOffset := 0
	log := logger.GetLogger(ctx)

	log.Infof("Starting pull-based SSE streaming for session=%s, message=%s", sessionID, assistantMessageID)

	for {
		select {
		case <-c.Request.Context().Done():
			// Connection closed, exit gracefully without panic
			log.Infof(
				"Client disconnected, stopping SSE streaming for session=%s, message=%s",
				sessionID,
				assistantMessageID,
			)
			return

		case <-ticker.C:
			// Get new events from StreamManager using offset
			events, newOffset, err := h.streamManager.GetEvents(ctx, sessionID, assistantMessageID, lastOffset)
			if err != nil {
				log.Warnf("Failed to get events from stream: %v", err)
				continue
			}

			// Send any new events
			streamCompleted := false
			titleReceived := false
			for _, evt := range events {
				// Check for stop event
				if evt.Type == types.ResponseType(event.EventStop) {
					log.Infof("Detected stop event, triggering stop via EventBus for session=%s", sessionID)

					// 通过 EventBus 广播停止信号
					// Emit stop event to the EventBus to trigger context cancellation
					if eventBus != nil {
						eventBus.Emit(ctx, event.Event{
							Type:      event.EventStop,
							SessionID: sessionID,
							Data: event.StopData{
								SessionID: sessionID,
								MessageID: assistantMessageID,
								Reason:    "user_requested",
							},
						})
					}

					// 向客户端推送停止通知
					// Send stop notification to frontend
					c.SSEvent("message", &types.StreamResponse{
						ID:           requestID,
						ResponseType: "stop",
						Content:      "Generation stopped by user",
						Done:         true,
					})
					c.Writer.Flush()
					return
				}

				// Build StreamResponse from StreamEvent
				response := buildStreamResponse(evt, requestID)

				// Check for completion event
				// 遇到完成事件，标记 streamCompleted = true ，触发后续的标题等待逻辑
				if evt.Type == "complete" {
					streamCompleted = true
				}

				// Check for title event
				if evt.Type == types.ResponseTypeSessionTitle {
					titleReceived = true
				}

				// Check if connection is still alive before writing
				if c.Request.Context().Err() != nil {
					log.Info("Connection closed during event sending, stopping")
					return
				}

				c.SSEvent("message", response)
				c.Writer.Flush()
			}

			// Update offset
			lastOffset = newOffset

			// Check if stream is completed - wait for title event only if needed and not already received
			if streamCompleted {

				// 流已完成，但还没收到标题事件，等待最多 3 秒
				if waitForTitle && !titleReceived {
					log.Infof("Stream completed for session=%s, message=%s, waiting for title event", sessionID, assistantMessageID)
					// Wait up to 3 seconds for title event after completion
					titleTimeout := time.After(3 * time.Second)
				titleWaitLoop:
					for { // 继续轮询等待标题
						select {
						case <-titleTimeout:
							log.Info("Title wait timeout, closing stream")
							break titleWaitLoop
						case <-c.Request.Context().Done():
							log.Info("Connection closed while waiting for title")
							return
						default:
							// Check for new events (title event)
							events, newOff, err := h.streamManager.GetEvents(c.Request.Context(), sessionID, assistantMessageID, lastOffset)
							if err != nil {
								log.Warnf("Error getting events while waiting for title: %v", err)
								break titleWaitLoop
							}
							if len(events) > 0 {
								for _, evt := range events {
									response := buildStreamResponse(evt, requestID)
									c.SSEvent("message", response)
									c.Writer.Flush()
									// If we got the title, we can exit
									// 检查是否收到标题事件，收到标题则立即退出等待
									if evt.Type == types.ResponseTypeSessionTitle {
										log.Infof("Title event received: %s", evt.Content)
										break titleWaitLoop
									}
								}
								lastOffset = newOff
							} else {
								// No events, wait a bit before checking again
								time.Sleep(100 * time.Millisecond)
							}
						}
					}
				} else {
					log.Infof("Stream completed for session=%s, message=%s", sessionID, assistantMessageID)
				}
				sendCompletionEvent(c, requestID)
				return
			}
		}
	}
}
