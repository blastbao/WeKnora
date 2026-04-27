package session

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

// GenerateTitle
//	│
//	├── 1. 获取请求上下文 ctx
//	│      → 从 c.Request.Context() 取出上下文
//	│      → 用于日志、租户信息、后续 service 调用
//	│
//	├── 2. 记录“开始生成标题”日志
//	│      → 标记本次接口调用开始
//	│
//	├── 3. 读取 session_id
//	│      → 从 URL path 参数中取 session_id
//	│      → 如果为空，直接返回 BadRequest
//	│
//	├── 4. 解析请求体
//	│      → 反序列化 GenerateTitleRequest
//	│      → 其中通常包含 messages
//	│      → 如果 JSON 非法，直接返回 BadRequest
//	│
//	├── 5. 查询 session
//	│      → 调 h.sessionService.GetSession(ctx, sessionID)
//	│      → 确认这个会话存在，并拿到完整 session 对象
//	│      → 如果查询失败，返回 InternalServerError
//	│
//	├── 6. 调用 sessionService.GenerateTitle()
//	│      → 输入：
//	│           - ctx
//	│           - session
//	│           - request.Messages
//	│           - modelID=""（表示不手动指定模型，由 service 自己决定）
//	│      → 根据会话消息内容生成标题
//	│
//	├── 7. 处理生成失败
//	│      → 如果 GenerateTitle 返回 error
//	│      → 记录错误日志
//	│      → 返回 InternalServerError
//	│
//	├── 8. 处理生成成功
//	│      → 得到 title
//	│      → 记录“标题生成成功”日志
//	│
//	└── 9. 返回 HTTP JSON 响应
//		  → c.JSON(200, {
//		        "success": true,
//		        "data": title,
//		    })
//		  → 把生成好的标题直接返回给前端

// GenerateTitle godoc
// @Summary      生成会话标题
// @Description  根据消息内容自动生成会话标题
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        session_id  path      string                true  "会话ID"
// @Param        request     body      GenerateTitleRequest  true  "生成请求"
// @Success      200         {object}  map[string]interface{}  "生成的标题"
// @Failure      400         {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/title [post]
func (h *Handler) GenerateTitle(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start generating session title")

	// Get session ID from URL parameter
	sessionID := c.Param("session_id")
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Parse request body
	var request GenerateTitleRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Get session from database
	session, err := h.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Call service to generate title
	logger.Infof(ctx, "Generating session title, session ID: %s, message count: %d", sessionID, len(request.Messages))
	title, err := h.sessionService.GenerateTitle(ctx, session, request.Messages, "")
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return generated title
	logger.Infof(ctx, "Session title generated successfully, session ID: %s, title: %s", sessionID, title)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    title,
	})
}
