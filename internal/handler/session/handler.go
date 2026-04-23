package session

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// Handler 处理所有与对话会话相关的 HTTP 请求，包括会话的增删改查
//  - 消息服务：管理会话中的消息
//  - 会话服务：管理会话本身
//  - 流管理器：处理SSE流式响应
//  - 应用配置
//  - 知识库服务：管理知识库
//  - 自定义Agent服务：管理Agent配置
//  - 租户服务：加载租户共享Agent上下文
//  - Agent共享服务：解析共享Agent（检索时

// Handler handles all HTTP requests related to conversation sessions
type Handler struct {
	messageService       interfaces.MessageService       // Service for managing messages
	sessionService       interfaces.SessionService       // Service for managing sessions
	streamManager        interfaces.StreamManager        // Manager for handling streaming responses
	config               *config.Config                  // Application configuration
	knowledgebaseService interfaces.KnowledgeBaseService // Service for managing knowledge bases
	customAgentService   interfaces.CustomAgentService   // Service for managing custom agents
	tenantService        interfaces.TenantService        // Service for loading tenant (shared agent context)
	agentShareService    interfaces.AgentShareService    // Service for resolving shared agents (KB scope in retrieval)
}

// NewHandler creates a new instance of Handler with all necessary dependencies
func NewHandler(
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	streamManager interfaces.StreamManager,
	config *config.Config,
	knowledgebaseService interfaces.KnowledgeBaseService,
	customAgentService interfaces.CustomAgentService,
	tenantService interfaces.TenantService,
	agentShareService interfaces.AgentShareService,
) *Handler {
	return &Handler{
		sessionService:       sessionService,
		messageService:       messageService,
		streamManager:        streamManager,
		config:               config,
		knowledgebaseService: knowledgebaseService,
		customAgentService:   customAgentService,
		tenantService:        tenantService,
		agentShareService:    agentShareService,
	}
}

// CreateSession 创建新会话。
//
// 执行过程：
//  1. 解析并验证请求体：将 JSON 请求绑定到 CreateSessionRequest 结构体，验证失败返回 400 错误。
//  2. 从 gin 上下文中提取 tenant ID，不存在则返回 401 未授权错误。
//  3. 构建 Session 对象：仅包含基础信息（tenant ID、标题、描述），会话本身与知识库解耦，所有配置在查询时由自定义 Agent 提供。
//  4. 调用 sessionService.CreateSession 创建会话的持久化信息。
//  5. 返回 201 状态码及创建的会话数据。

// CreateSession godoc
// @Summary      创建会话
// @Description  创建新的对话会话
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        request  body      CreateSessionRequest  true  "会话创建请求"
// @Success      201      {object}  map[string]interface{}  "创建的会话"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions [post]
func (h *Handler) CreateSession(c *gin.Context) {
	ctx := c.Request.Context()
	// Parse and validate the request body
	var request CreateSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to validate session creation parameters", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Get tenant ID from context
	tenantID, exists := c.Get(types.TenantIDContextKey.String())
	if !exists {
		logger.Error(ctx, "Failed to get tenant ID")
		c.Error(errors.NewUnauthorizedError("Unauthorized"))
		return
	}

	// Sessions are now knowledge-base-independent:
	// - All configuration comes from custom agent at query time
	// - Session only stores basic info (tenant ID, title, description)
	logger.Infof(
		ctx,
		"Processing session creation request, tenant ID: %d",
		tenantID,
	)

	// Create session object with base properties
	createdSession := &types.Session{
		TenantID:    tenantID.(uint64),
		Title:       request.Title,
		Description: request.Description,
	}

	// Call service to create session
	logger.Infof(ctx, "Calling session service to create session")
	createdSession, err := h.sessionService.CreateSession(ctx, createdSession)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return created session
	logger.Infof(ctx, "Session created successfully, ID: %s", createdSession.ID)
	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    createdSession,
	})
}

// GetSession 根据 ID 获取会话详情。
//
// 执行过程：
//  1. 从 URL 路径参数中提取 session ID，并对 ID 进行日志安全过滤（SanitizeForLog）。
//  2. 若 ID 为空，返回 400 参数错误。
//  3. 调用 sessionService.GetSession 查询会话详情。
//  4. 若返回 ErrSessionNotFound，返回 404 错误；其他错误返回 500。
//  5. 返回 200 状态码及会话详情。

// GetSession godoc
// @Summary      获取会话详情
// @Description  根据ID获取会话详情
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "会话ID"
// @Success      200  {object}  map[string]interface{}  "会话详情"
// @Failure      404  {object}  errors.AppError         "会话不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{id} [get]
func (h *Handler) GetSession(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start retrieving session")

	// Get session ID from URL parameter
	id := secutils.SanitizeForLog(c.Param("id"))
	if id == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Call service to get session details
	logger.Infof(ctx, "Retrieving session, ID: %s", id)
	session, err := h.sessionService.GetSession(ctx, id)
	if err != nil {
		if err == errors.ErrSessionNotFound {
			logger.Warnf(ctx, "Session not found, ID: %s", id)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return session data
	logger.Infof(ctx, "Session retrieved successfully, ID: %s", id)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    session,
	})
}

// GetSessionsByTenant 获取当前租户下的会话列表，支持分页查询。
//
// 执行过程：
//  1. 从 URL 查询参数中解析分页信息（page、page_size），绑定到 types.Pagination。
//  2. 解析失败返回 400 参数错误。
//  3. 调用 sessionService.GetPagedSessionsByTenant 执行分页查询。
//  4. 查询出错返回 500 内部错误。
//  5. 返回 200 状态码，附带会话数据及分页元数据（total、page、page_size）。

// GetSessionsByTenant godoc
// @Summary      获取会话列表
// @Description  获取当前租户的会话列表，支持分页
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        page       query     int  false  "页码"
// @Param        page_size  query     int  false  "每页数量"
// @Success      200        {object}  map[string]interface{}  "会话列表"
// @Failure      400        {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions [get]
func (h *Handler) GetSessionsByTenant(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse pagination parameters from query
	var pagination types.Pagination
	if err := c.ShouldBindQuery(&pagination); err != nil {
		logger.Error(ctx, "Failed to parse pagination parameters", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Use paginated query to get sessions
	result, err := h.sessionService.GetPagedSessionsByTenant(ctx, &pagination)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return sessions with pagination data
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"data":      result.Data,
		"total":     result.Total,
		"page":      result.Page,
		"page_size": result.PageSize,
	})
}

// UpdateSession 更新指定会话的属性。
//
// 执行过程：
//  1. 从 URL 路径参数中提取 session ID，并做日志安全过滤；为空则返回 400。
//  2. 从 gin 上下文中提取 tenant ID 用于鉴权，缺失则返回 401。
//  3. 解析请求体 JSON 到 types.Session，失败返回 400。
//  4. 将 URL 中的 session ID 与上下文中的 TenantID 注入到 session 对象，确保只能更新当前租户的数据。
//  5. 调用 sessionService.UpdateSession 执行更新。
//  6. 若会话不存在返回 404；其他错误返回 500。
//  7. 返回 200 状态码及更新后的会话数据。

// UpdateSession godoc
// @Summary      更新会话
// @Description  更新会话属性
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        id       path      string         true  "会话ID"
// @Param        request  body      types.Session  true  "会话信息"
// @Success      200      {object}  map[string]interface{}  "更新后的会话"
// @Failure      404      {object}  errors.AppError         "会话不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{id} [put]
func (h *Handler) UpdateSession(c *gin.Context) {
	ctx := c.Request.Context()

	// Get session ID from URL parameter
	id := secutils.SanitizeForLog(c.Param("id"))
	if id == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Verify tenant ID from context for authorization
	tenantID, exists := c.Get(types.TenantIDContextKey.String())
	if !exists {
		logger.Error(ctx, "Failed to get tenant ID")
		c.Error(errors.NewUnauthorizedError("Unauthorized"))
		return
	}

	// Parse request body to session object
	var session types.Session
	if err := c.ShouldBindJSON(&session); err != nil {
		logger.Error(ctx, "Failed to parse session data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	session.ID = id
	session.TenantID = tenantID.(uint64)

	// Call service to update session
	if err := h.sessionService.UpdateSession(ctx, &session); err != nil {
		if err == errors.ErrSessionNotFound {
			logger.Warnf(ctx, "Session not found, ID: %s", id)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return updated session
	logger.Infof(ctx, "Session updated successfully, ID: %s", id)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    session,
	})
}

// DeleteSession godoc
// @Summary      删除会话
// @Description  删除指定的会话
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "会话ID"
// @Success      200  {object}  map[string]interface{}  "删除成功"
// @Failure      404  {object}  errors.AppError         "会话不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{id} [delete]
func (h *Handler) DeleteSession(c *gin.Context) {
	ctx := c.Request.Context()

	// Get session ID from URL parameter
	id := secutils.SanitizeForLog(c.Param("id"))
	if id == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Call service to delete session
	if err := h.sessionService.DeleteSession(ctx, id); err != nil {
		if err == errors.ErrSessionNotFound {
			logger.Warnf(ctx, "Session not found, ID: %s", id)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Return success message
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Session deleted successfully",
	})
}

// batchDeleteRequest represents the request body for batch deleting sessions
type batchDeleteRequest struct {
	IDs []string `json:"ids" binding:"required,min=1"`
}

// BatchDeleteSessions godoc
// @Summary      批量删除会话
// @Description  根据ID列表批量删除对话会话
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        request  body      batchDeleteRequest  true  "批量删除请求"
// @Success      200      {object}  map[string]interface{}  "删除结果"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/batch [delete]
func (h *Handler) BatchDeleteSessions(c *gin.Context) {
	ctx := c.Request.Context()

	var req batchDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Errorf(ctx, "Invalid batch delete request: %v", err)
		c.Error(errors.NewBadRequestError("invalid request: ids are required"))
		return
	}

	// Sanitize all IDs
	sanitizedIDs := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		sanitized := secutils.SanitizeForLog(id)
		if sanitized != "" {
			sanitizedIDs = append(sanitizedIDs, sanitized)
		}
	}

	if len(sanitizedIDs) == 0 {
		c.Error(errors.NewBadRequestError("no valid session IDs provided"))
		return
	}

	if err := h.sessionService.BatchDeleteSessions(ctx, sanitizedIDs); err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Sessions deleted successfully",
	})
}
