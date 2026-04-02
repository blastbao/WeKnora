package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	chatpipline "github.com/Tencent/WeKnora/internal/application/service/chat_pipline"
	llmcontext "github.com/Tencent/WeKnora/internal/application/service/llmcontext"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/tracing"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// generateEventID generates a unique event ID with type suffix for better traceability
func generateEventID(suffix string) string {
	return fmt.Sprintf("%s-%s", uuid.New().String()[:8], suffix)
}

// sessionService implements the SessionService interface for managing conversation sessions
type sessionService struct {
	cfg                  *config.Config                   // Application configuration
	sessionRepo          interfaces.SessionRepository     // Repository for session data
	messageRepo          interfaces.MessageRepository     // Repository for message data
	knowledgeBaseService interfaces.KnowledgeBaseService  // Service for knowledge base operations
	modelService         interfaces.ModelService          // Service for model operations
	tenantService        interfaces.TenantService         // Service for tenant operations
	eventManager         *chatpipline.EventManager        // Event manager for chat pipeline
	agentService         interfaces.AgentService          // Service for agent operations
	sessionStorage       llmcontext.ContextStorage        // Session storage
	knowledgeService     interfaces.KnowledgeService      // Service for knowledge operations
	chunkService         interfaces.ChunkService          // Service for chunk operations
	webSearchStateRepo   interfaces.WebSearchStateService // Service for web search state
	kbShareService       interfaces.KBShareService        // Service for KB sharing operations
	memoryService        interfaces.MemoryService         // Service for memory operations
}

// NewSessionService creates a new session service instance with all required dependencies
func NewSessionService(cfg *config.Config,
	sessionRepo interfaces.SessionRepository,
	messageRepo interfaces.MessageRepository,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	modelService interfaces.ModelService,
	tenantService interfaces.TenantService,
	eventManager *chatpipline.EventManager,
	agentService interfaces.AgentService,
	sessionStorage llmcontext.ContextStorage,
	webSearchStateRepo interfaces.WebSearchStateService,
	kbShareService interfaces.KBShareService,
	memoryService interfaces.MemoryService,
) interfaces.SessionService {
	return &sessionService{
		cfg:                  cfg,
		sessionRepo:          sessionRepo,
		messageRepo:          messageRepo,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		chunkService:         chunkService,
		modelService:         modelService,
		tenantService:        tenantService,
		eventManager:         eventManager,
		agentService:         agentService,
		sessionStorage:       sessionStorage,
		webSearchStateRepo:   webSearchStateRepo,
		kbShareService:       kbShareService,
		memoryService:        memoryService,
	}
}

// CreateSession creates a new conversation session
func (s *sessionService) CreateSession(ctx context.Context, session *types.Session) (*types.Session, error) {
	logger.Info(ctx, "Start creating session")

	// Validate tenant ID
	if session.TenantID == 0 {
		logger.Error(ctx, "Failed to create session: tenant ID cannot be empty")
		return nil, errors.New("tenant ID is required")
	}

	logger.Infof(ctx, "Creating session, tenant ID: %d", session.TenantID)

	// Create session in repository
	createdSession, err := s.sessionRepo.Create(ctx, session)
	if err != nil {
		return nil, err
	}

	logger.Infof(ctx, "Session created successfully, ID: %s, tenant ID: %d", createdSession.ID, createdSession.TenantID)
	return createdSession, nil
}

// GetSession retrieves a session by its ID
func (s *sessionService) GetSession(ctx context.Context, id string) (*types.Session, error) {
	logger.Info(ctx, "Start retrieving session")

	// Validate session ID
	if id == "" {
		logger.Error(ctx, "Failed to get session: session ID cannot be empty")
		return nil, errors.New("session id is required")
	}

	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	logger.Infof(ctx, "Retrieving session, ID: %s, tenant ID: %d", id, tenantID)

	// Get session from repository
	session, err := s.sessionRepo.Get(ctx, tenantID, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": id,
			"tenant_id":  tenantID,
		})
		return nil, err
	}

	logger.Infof(ctx, "Session retrieved successfully, ID: %s, tenant ID: %d", session.ID, session.TenantID)
	return session, nil
}

// GetSessionsByTenant retrieves all sessions for the current tenant
func (s *sessionService) GetSessionsByTenant(ctx context.Context) ([]*types.Session, error) {
	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	logger.Infof(ctx, "Retrieving all sessions for tenant, tenant ID: %d", tenantID)

	// Get sessions from repository
	sessions, err := s.sessionRepo.GetByTenantID(ctx, tenantID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
		})
		return nil, err
	}

	logger.Infof(
		ctx, "Tenant sessions retrieved successfully, tenant ID: %d, session count: %d", tenantID, len(sessions),
	)
	return sessions, nil
}

// GetPagedSessionsByTenant retrieves sessions for the current tenant with pagination
func (s *sessionService) GetPagedSessionsByTenant(ctx context.Context,
	pagination *types.Pagination,
) (*types.PageResult, error) {
	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	// Get paged sessions from repository
	sessions, total, err := s.sessionRepo.GetPagedByTenantID(ctx, tenantID, pagination)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
			"page":      pagination.Page,
			"page_size": pagination.PageSize,
		})
		return nil, err
	}

	return types.NewPageResult(total, pagination, sessions), nil
}

// UpdateSession updates an existing session's properties
func (s *sessionService) UpdateSession(ctx context.Context, session *types.Session) error {
	// Validate session ID
	if session.ID == "" {
		logger.Error(ctx, "Failed to update session: session ID cannot be empty")
		return errors.New("session id is required")
	}

	// Update session in repository
	err := s.sessionRepo.Update(ctx, session)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": session.ID,
			"tenant_id":  session.TenantID,
		})
		return err
	}

	logger.Infof(ctx, "Session updated successfully, ID: %s", session.ID)
	return nil
}

// DeleteSession removes a session by its ID
func (s *sessionService) DeleteSession(ctx context.Context, id string) error {
	// Validate session ID
	if id == "" {
		logger.Error(ctx, "Failed to delete session: session ID cannot be empty")
		return errors.New("session id is required")
	}

	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// Cleanup temporary KB stored in Redis for this session
	if err := s.webSearchStateRepo.DeleteWebSearchTempKBState(ctx, id); err != nil {
		logger.Warnf(ctx, "Failed to cleanup temporary KB for session %s: %v", id, err)
	}

	// Cleanup conversation context stored in Redis for this session
	if err := s.sessionStorage.Delete(ctx, id); err != nil {
		logger.Warnf(ctx, "Failed to cleanup conversation context for session %s: %v", id, err)
	}

	// Delete session from repository
	err := s.sessionRepo.Delete(ctx, tenantID, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": id,
			"tenant_id":  tenantID,
		})
		return err
	}

	return nil
}

// BatchDeleteSessions deletes multiple sessions by IDs
func (s *sessionService) BatchDeleteSessions(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		logger.Error(ctx, "Failed to batch delete sessions: IDs list is empty")
		return errors.New("session ids are required")
	}

	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// Cleanup associated resources for each session
	for _, id := range ids {
		if err := s.webSearchStateRepo.DeleteWebSearchTempKBState(ctx, id); err != nil {
			logger.Warnf(ctx, "Failed to cleanup temporary KB for session %s: %v", id, err)
		}
		if err := s.sessionStorage.Delete(ctx, id); err != nil {
			logger.Warnf(ctx, "Failed to cleanup conversation context for session %s: %v", id, err)
		}
	}

	// Batch delete sessions from repository
	if err := s.sessionRepo.BatchDelete(ctx, tenantID, ids); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_ids": ids,
			"tenant_id":   tenantID,
		})
		return err
	}

	return nil
}

// GenerateTitle generates a title for the current conversation content
// modelID: optional model ID to use for title generation (if empty, uses first available KnowledgeQA model)
func (s *sessionService) GenerateTitle(ctx context.Context,
	session *types.Session, messages []types.Message, modelID string,
) (string, error) {
	if session == nil {
		logger.Error(ctx, "Failed to generate title: session cannot be empty")
		return "", errors.New("session cannot be empty")
	}

	// Skip if title already exists
	if session.Title != "" {
		return session.Title, nil
	}
	var err error
	// Get the first user message, either from provided messages or repository
	var message *types.Message
	if len(messages) == 0 {
		message, err = s.messageRepo.GetFirstMessageOfUser(ctx, session.ID)
		if err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"session_id": session.ID,
			})
			return "", err
		}
	} else {
		for _, m := range messages {
			if m.Role == "user" {
				message = &m
				break
			}
		}
	}

	// Ensure a user message was found
	if message == nil {
		logger.Error(ctx, "No user message found, cannot generate title")
		return "", errors.New("no user message found")
	}

	// Use provided modelID, or fallback to first available KnowledgeQA model
	if modelID == "" {
		models, err := s.modelService.ListModels(ctx)
		if err != nil {
			logger.ErrorWithFields(ctx, err, nil)
			return "", fmt.Errorf("failed to list models: %w", err)
		}
		for _, model := range models {
			if model == nil {
				continue
			}
			if model.Type == types.ModelTypeKnowledgeQA {
				modelID = model.ID
				logger.Infof(ctx, "Using first available KnowledgeQA model for title: %s", modelID)
				break
			}
		}
		if modelID == "" {
			logger.Error(ctx, "No KnowledgeQA model found")
			return "", errors.New("no KnowledgeQA model available for title generation")
		}
	} else {
		logger.Infof(ctx, "Using specified model for title generation: %s", modelID)
	}

	chatModel, err := s.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"model_id": modelID,
		})
		return "", err
	}

	// Prepare messages for title generation
	var chatMessages []chat.Message
	chatMessages = append(chatMessages,
		chat.Message{Role: "system", Content: s.cfg.Conversation.GenerateSessionTitlePrompt},
	)
	chatMessages = append(chatMessages,
		chat.Message{Role: "user", Content: message.Content},
	)

	// Call model to generate title
	thinking := false
	response, err := chatModel.Chat(ctx, chatMessages, &chat.ChatOptions{
		Temperature: 0.3,
		Thinking:    &thinking,
	})
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		return "", err
	}

	// Process and store the generated title
	session.Title = strings.TrimPrefix(response.Content, "<think>\n\n</think>")

	// Update session with new title
	err = s.sessionRepo.Update(ctx, session)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		return "", err
	}

	return session.Title, nil
}

// GenerateTitleAsync generates a title for the session asynchronously
// This method clones the session and generates the title in a goroutine
// It emits an event when the title is generated
// modelID: optional model ID to use for title generation (if empty, uses first available KnowledgeQA model)
func (s *sessionService) GenerateTitleAsync(
	ctx context.Context,
	session *types.Session,
	userQuery string,
	modelID string,
	eventBus *event.EventBus,
) {
	// Use context tenant (effective tenant when using shared agent) so ListModels/GetChatModel find the agent's model.
	// sessionRepo.Update uses session.TenantID in WHERE, so the session row is updated correctly regardless of ctx.
	tenantID := ctx.Value(types.TenantIDContextKey)
	requestID := ctx.Value(types.RequestIDContextKey)
	go func() {
		bgCtx := context.Background()
		if tenantID != nil {
			bgCtx = context.WithValue(bgCtx, types.TenantIDContextKey, tenantID)
		}
		if requestID != nil {
			bgCtx = context.WithValue(bgCtx, types.RequestIDContextKey, requestID)
		}

		// Skip if title already exists
		if session.Title != "" {
			return
		}

		// Generate title using the first user message
		messages := []types.Message{
			{
				Role:    "user",
				Content: userQuery,
			},
		}

		title, err := s.GenerateTitle(bgCtx, session, messages, modelID)
		if err != nil {
			logger.ErrorWithFields(bgCtx, err, map[string]interface{}{
				"session_id": session.ID,
			})
			return
		}

		// Emit title update event - BUG FIX: use bgCtx instead of ctx
		// The original ctx is from the HTTP request and may be cancelled by the time we get here
		if eventBus != nil {
			if err := eventBus.Emit(bgCtx, event.Event{
				Type:      event.EventSessionTitle,
				SessionID: session.ID,
				Data: event.SessionTitleData{
					SessionID: session.ID,
					Title:     title,
				},
			}); err != nil {
				logger.ErrorWithFields(bgCtx, err, map[string]interface{}{
					"session_id": session.ID,
				})
			} else {
				logger.Infof(bgCtx, "Title update event emitted successfully, session ID: %s, title: %s", session.ID, title)
			}
		}
	}()
}

// KnowledgeQA 执行基于知识库的问答，通过 LLM 对检索结果进行总结生成答案。
//
//【核心功能】
// 该函数负责协调用户查询、知识库检索策略、模型配置及上下文管理，最终启动异步事件流水线来生成流式回答。
// 它充当“调度员”角色，根据请求参数和自定义 Agent 配置动态决定：
//  1. 检索范围：是仅搜索用户明确指定的知识库，还是使用 Agent 默认绑定的库，亦或是禁用检索（纯聊天）。
//  2. 运行配置：合并全局默认值、Agent 特化配置（如温度、Prompt、TopK）及请求级覆盖参数。
//  3. 执行路径：根据是否命中知识库或开启联网搜索，智能路由到 "rag_stream" (检索增强) 或 "chat_stream" (纯对话) 流水线。
//
//【配置优先级逻辑】
// 参数生效顺序严格遵循：请求显式参数 > CustomAgent 配置 > 系统全局默认值 (config.yaml)。
// 特别是知识库选择：若用户通过 @mention 显式指定了 KB/文档，将忽略 Agent 的默认配置；
// 若未指定且 Agent 开启了 "RetrieveKBOnlyWhenMentioned"，则强制降级为纯聊天模式。
//
// 【事件驱动与副作用】
// 本函数本身不直接返回生成的文本，而是通过 eventBus 异步发射以下事件供前端消费：
// - event.EventAgentReferences: 当检索到相关文档时，发送引用来源列表。
// - event.EventAgentAnswerChunk: (由下游 pipeline 触发) 流式发送回答片段。
// - event.EventAgentCompletion: (由下游 pipeline 触发) 标记回答结束。
//
//【参数说明】
//   - ctx: 上下文，包含租户ID、用户ID等信息。
//   - session: 当前会话对象，包含会话ID、租户ID等信息。
//   - query: 用户输入的查询文本。
//   - knowledgeBaseIDs: [可选] 用户显式指定的知识库ID列表 (@KB)。优先级最高。
//   - knowledgeIDs: [可选] 用户显式指定的具体文档ID列表 (@Doc)。优先级最高。
//   - assistantMessageID: 助手消息ID，用于在 pipeline 中关联消息事件。
//   - summaryModelID: [可选] 请求级指定使用的摘要模型ID，为空时使用默认或 CustomAgent 配置。
//   - webSearchEnabled: 是否启用联网搜索（将搜索引擎结果作为知识来源）。若为 true 且无本地知识库，将触发纯联网检索流程。
//   - eventBus: 事件总线，用于向客户端异步的推送答案块、参考资料等事件。
//   - customAgent: [可选] 自定义 Agent 配置。若提供，将覆盖全局默认参数，包括模型、提示词、温度、检索参数、多轮对话设置等。
//   - enableMemory: 是否启用长期记忆（将对话内容存入记忆库供后续参考）。若为 false，将忽略历史上下文。
//
// 【返回值】
//   - error: 若在参数解析、配置合并或流水线启动阶段发生致命错误则返回 err；
//            若成功启动流水线（即使后续生成失败），通常返回 nil（具体错误通过事件流或日志上报）。
//
// 执行流程：
//  1. 确定检索范围（知识库/文档）：
//    - 检查请求中是否包含显式的 knowledgeBaseIDs 或 knowledgeIDs。
//    - 若有显式指定：直接使用，忽略 Agent 配置。
//    - 若无显式指定：检查 Agent 配置中的 RetrieveKBOnlyWhenMentioned 标志。
//      - 若为 true：清空知识库列表（强制纯聊天）。
//      - 若为 false：加载 Agent 默认绑定的知识库列表。
//  2. 选择对话模型：
//    - 确定 Chat Model ID：优先级 summaryModelID > CustomAgent.ModelID > 会话默认模型。
//    - 构建 SummaryConfig：将 Agent 中的 SystemPrompt, Temperature, ContextTemplate 等参数覆盖到全局默认配置中。
//    - 更新检索参数：应用 Agent 特定的 EmbeddingTopK, RerankThreshold, VectorThreshold 等微调参数。
//    - 处理多轮记忆：根据 Agent 的 MultiTurnEnabled 和 HistoryTurns 设置 maxRounds，决定是否携带历史上下文。
//  3. 上下文环境构建 (Context Building):
//    - 确定检索租户范围 (retrievalTenantID)：优先使用 Agent 所属租户，否则回退到 Session 租户或 Context 中的租户。
//    - 构建统一的搜索目标列表 (searchTargets)，用于后续 pipeline 的快速查找。
//  4. 构建 ChatManage 对象：
//     - 实例化 types.ChatManage 结构体，封装所有问答所需的参数（查询文本、检索配置、模型配置、FAQ策略等）
//     - 注入 EventBus 接口，确保下游 pipeline 能直接发射事件。
//  5. 流水线路由决策：
//    - 判断条件：(无本地知识库) AND (无联网搜索)？
//      - 是 (纯聊天)：
//        - 若 maxRounds > 0：选择 "chat_history_stream" (带记忆的纯聊)。
//        - 若 maxRounds = 0：选择 "chat_stream" (单次纯聊)。
//      - 否 (RAG/联网)：
//        - 统一选择 "rag_stream" 流水线（该流水线内部会自动处理 KB 检索、重排序、联网搜索及最终生成）。
//  6. 通过 KnowledgeQAByEvent 执行实际的事件处理流程
//  7. 若存在检索结果，发送 EventAgentReferences 事件供前端展示参考资料
//
// 支持的特性：
//   - 多知识库检索：支持同时从多个知识库中检索
//   - 精准文档定位：支持指定具体文档ID进行检索
//   - 自定义助手覆盖：CustomAgent 可覆盖系统默认的所有配置项
//   - FAQ 优先级策略：FAQ 匹配度高于阈值时可直接返回答案
//   - 联网搜索：可与知识库检索并行或单独使用
//   - 多轮对话：根据配置保留对话历史上下文
//   - 查询改写：可启用 query rewrite 优化检索效果
//   - 查询扩展：可启用 query expansion 增加召回率
//   - 长期记忆：可启用 memory 功能记录用户偏好和历史
//
// 使用示例：
//   // 创建事件总线
//   eventBus := event.NewEventBus()
//
//   // 执行问答
//   err := sessionService.KnowledgeQA(
//       ctx,
//       session,
//       "向量数据库是什么？",
//       []string{"kb-123"},            // 知识库ID
//       nil,                           // 无指定文档
//       msgID,
//       "",                            // 使用默认模型
//       false,                         // 不启用联网搜索
//       eventBus,
//       customAgent,                   // 可选的自定义助手
//       true,                          // 启用长期记忆
//   )
//
//   // 通过 eventBus 监听答案事件
//   eventBus.On(event.EventAgentMessageChunk, func(e event.Event) {
//       data := e.Data.(event.AgentMessageChunkData)
//       fmt.Print(data.Chunk) // 逐块输出答案
//   })
//
// 注意事项：
//   - 本方法为异步设计，答案通过 EventBus 返回，不阻塞等待
//   - CustomAgent 的 MultiTurnEnabled 为 false 时会清空历史上下文
//   - 当 RetrieveKBOnlyWhenMentioned 为 true 且用户无 @提及 时，不会检索任何知识库
//   - FAQ 优先级策略需要知识库中包含 FAQ 类型的知识条目
//   - 联网搜索需要在系统配置中启用并配置搜索引擎

// KnowledgeQA performs knowledge base question answering with LLM summarization
// Events are emitted through eventBus (references, answer chunks, completion)
// customAgent is optional - if provided, uses custom agent configuration for multiTurnEnabled and historyTurns
func (s *sessionService) KnowledgeQA(
	ctx context.Context,
	session *types.Session,
	query string,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
	assistantMessageID string,
	summaryModelID string,
	webSearchEnabled bool,
	eventBus *event.EventBus,
	customAgent *types.CustomAgent,
	enableMemory bool,
) error {
	logger.Infof(
		ctx,
		"Knowledge base question answering parameters, session ID: %s, query: %s, webSearchEnabled: %v, enableMemory: %v",
		session.ID,
		query,
		webSearchEnabled,
		enableMemory,
	)

	// Use custom agent's knowledge bases only if request didn't specify any
	// When user explicitly @mentions a knowledge base or document, only search those
	// If RetrieveKBOnlyWhenMentioned is enabled and no @ mentions, don't use KB at all
	hasExplicitMention := len(knowledgeBaseIDs) > 0 || len(knowledgeIDs) > 0
	if customAgent != nil {
		logger.Infof(ctx, "KB resolution (quick-answer): hasExplicitMention=%v, RetrieveKBOnlyWhenMentioned=%v, KBSelectionMode=%s",
			hasExplicitMention, customAgent.Config.RetrieveKBOnlyWhenMentioned, customAgent.Config.KBSelectionMode)
	}
	if hasExplicitMention {
		logger.Infof(ctx, "Using request-specified targets (ignoring agent config): kbs=%v, docs=%v", knowledgeBaseIDs, knowledgeIDs)
	} else if customAgent != nil && customAgent.Config.RetrieveKBOnlyWhenMentioned {
		// User didn't mention any KB/file, and the setting requires explicit mention
		knowledgeBaseIDs = nil
		knowledgeIDs = nil
		logger.Infof(ctx, "RetrieveKBOnlyWhenMentioned is enabled and no @ mention found, KB retrieval disabled for this request")
	} else {
		knowledgeBaseIDs = s.resolveKnowledgeBasesFromAgent(ctx, customAgent)
	}

	// Determine chat model ID: prioritize request's summaryModelID, then Remote models
	chatModelID, err := s.selectChatModelIDWithOverride(ctx, session, knowledgeBaseIDs, knowledgeIDs, summaryModelID)
	if err != nil {
		return err
	}

	// Initialize default values from config.yaml
	rewritePromptSystem := s.cfg.Conversation.RewritePromptSystem
	rewritePromptUser := s.cfg.Conversation.RewritePromptUser
	vectorThreshold := s.cfg.Conversation.VectorThreshold
	keywordThreshold := s.cfg.Conversation.KeywordThreshold
	embeddingTopK := s.cfg.Conversation.EmbeddingTopK
	rerankTopK := s.cfg.Conversation.RerankTopK
	rerankThreshold := s.cfg.Conversation.RerankThreshold
	maxRounds := s.cfg.Conversation.MaxRounds
	fallbackStrategy := types.FallbackStrategy(s.cfg.Conversation.FallbackStrategy)
	fallbackResponse := s.cfg.Conversation.FallbackResponse
	fallbackPrompt := s.cfg.Conversation.FallbackPrompt
	enableRewrite := s.cfg.Conversation.EnableRewrite
	enableQueryExpansion := s.cfg.Conversation.EnableQueryExpansion
	rerankModelID := ""

	summaryConfig := types.SummaryConfig{
		Prompt:              s.cfg.Conversation.Summary.Prompt,
		ContextTemplate:     s.cfg.Conversation.Summary.ContextTemplate,
		Temperature:         s.cfg.Conversation.Summary.Temperature,
		NoMatchPrefix:       s.cfg.Conversation.Summary.NoMatchPrefix,
		MaxCompletionTokens: s.cfg.Conversation.Summary.MaxCompletionTokens,
		Thinking:            s.cfg.Conversation.Summary.Thinking,
	}

	// Set default fallback strategy if not set
	if fallbackStrategy == "" {
		fallbackStrategy = types.FallbackStrategyFixed
		logger.Infof(ctx, "Fallback strategy not set, using default: %v", fallbackStrategy)
	}

	// Apply custom agent configuration if provided
	if customAgent != nil {
		// Ensure defaults are set
		customAgent.EnsureDefaults()

		// Override model ID only if request didn't specify summaryModelID
		// Request's summaryModelID has highest priority
		if summaryModelID == "" && customAgent.Config.ModelID != "" {
			chatModelID = customAgent.Config.ModelID
			logger.Infof(ctx, "Using custom agent's model_id: %s", chatModelID)
		}
		// Override system prompt
		if customAgent.Config.SystemPrompt != "" {
			summaryConfig.Prompt = customAgent.Config.SystemPrompt
			logger.Infof(ctx, "Using custom agent's system_prompt")
		}
		// Override context template
		if customAgent.Config.ContextTemplate != "" {
			summaryConfig.ContextTemplate = customAgent.Config.ContextTemplate
			logger.Infof(ctx, "Using custom agent's context_template")
		}
		// Override temperature
		if customAgent.Config.Temperature > 0 {
			summaryConfig.Temperature = customAgent.Config.Temperature
			logger.Infof(ctx, "Using custom agent's temperature: %f", customAgent.Config.Temperature)
		}
		// Override max completion tokens
		if customAgent.Config.MaxCompletionTokens > 0 {
			summaryConfig.MaxCompletionTokens = customAgent.Config.MaxCompletionTokens
			logger.Infof(ctx, "Using custom agent's max_completion_tokens: %d", customAgent.Config.MaxCompletionTokens)
		}
		// Override thinking mode from agent config
		// Agent-level thinking setting takes full control (no global fallback)
		summaryConfig.Thinking = customAgent.Config.Thinking
		if customAgent.Config.Thinking != nil {
			logger.Infof(ctx, "Using custom agent's thinking: %v", *customAgent.Config.Thinking)
		}
		// Override retrieval strategy settings
		if customAgent.Config.EmbeddingTopK > 0 {
			embeddingTopK = customAgent.Config.EmbeddingTopK
		}
		if customAgent.Config.KeywordThreshold > 0 {
			keywordThreshold = customAgent.Config.KeywordThreshold
		}
		if customAgent.Config.VectorThreshold > 0 {
			vectorThreshold = customAgent.Config.VectorThreshold
		}
		if customAgent.Config.RerankTopK > 0 {
			rerankTopK = customAgent.Config.RerankTopK
		}
		if customAgent.Config.RerankThreshold > 0 {
			rerankThreshold = customAgent.Config.RerankThreshold
		}
		if customAgent.Config.RerankModelID != "" {
			rerankModelID = customAgent.Config.RerankModelID
		}
		// Override rewrite settings
		enableRewrite = customAgent.Config.EnableRewrite
		enableQueryExpansion = customAgent.Config.EnableQueryExpansion
		if customAgent.Config.RewritePromptSystem != "" {
			rewritePromptSystem = customAgent.Config.RewritePromptSystem
		}
		if customAgent.Config.RewritePromptUser != "" {
			rewritePromptUser = customAgent.Config.RewritePromptUser
		}
		// Override fallback settings
		if customAgent.Config.FallbackStrategy != "" {
			fallbackStrategy = types.FallbackStrategy(customAgent.Config.FallbackStrategy)
		}
		if customAgent.Config.FallbackResponse != "" {
			fallbackResponse = customAgent.Config.FallbackResponse
		}
		if customAgent.Config.FallbackPrompt != "" {
			fallbackPrompt = customAgent.Config.FallbackPrompt
		}
		// Override history turns
		if customAgent.Config.HistoryTurns > 0 {
			maxRounds = customAgent.Config.HistoryTurns
			logger.Infof(ctx, "Using custom agent's history_turns: %d", maxRounds)
		}
		// Check if multi-turn is disabled
		if !customAgent.Config.MultiTurnEnabled {
			maxRounds = 0 // Disable history
			logger.Infof(ctx, "Multi-turn disabled by custom agent, clearing history")
		}
	}

	// Extract FAQ strategy settings from custom agent
	var faqPriorityEnabled bool
	var faqDirectAnswerThreshold float64
	var faqScoreBoost float64
	if customAgent != nil {
		faqPriorityEnabled = customAgent.Config.FAQPriorityEnabled
		faqDirectAnswerThreshold = customAgent.Config.FAQDirectAnswerThreshold
		faqScoreBoost = customAgent.Config.FAQScoreBoost
		if faqPriorityEnabled {
			logger.Infof(ctx, "FAQ priority enabled: threshold=%.2f, boost=%.2f",
				faqDirectAnswerThreshold, faqScoreBoost)
		}
	}

	// Retrieval scope: when agent is set, use agent's tenant (own or shared); otherwise session tenant or context
	retrievalTenantID := session.TenantID
	if customAgent != nil && customAgent.TenantID != 0 {
		retrievalTenantID = customAgent.TenantID
		logger.Infof(ctx, "Using agent tenant %d for retrieval scope", retrievalTenantID)
	} else if v := ctx.Value(types.TenantIDContextKey); v != nil {
		if tid, ok := v.(uint64); ok && tid != 0 {
			retrievalTenantID = tid
			logger.Infof(ctx, "Using effective tenant %d for retrieval from context", retrievalTenantID)
		}
	}

	// Build unified search targets (computed once, used throughout pipeline)
	searchTargets, err := s.buildSearchTargets(ctx, retrievalTenantID, knowledgeBaseIDs, knowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to build search targets: %v", err)
	}

	// Create chat management object with session settings
	logger.Infof(
		ctx,
		"Creating chat manage object, knowledge base IDs: %v, knowledge IDs: %v, chat model ID: %s, search targets: %d",
		knowledgeBaseIDs,
		knowledgeIDs,
		chatModelID,
		len(searchTargets),
	)

	// Get UserID from context
	userID, _ := ctx.Value(types.UserIDContextKey).(string)

	chatManage := &types.ChatManage{
		Query:                query,
		RewriteQuery:         query,
		SessionID:            session.ID,
		UserID:               userID,
		MessageID:            assistantMessageID, // NEW: For event emission in pipeline
		KnowledgeBaseIDs:     knowledgeBaseIDs,   // Multi-KB support
		KnowledgeIDs:         knowledgeIDs,       // Specific knowledge (file) IDs
		SearchTargets:        searchTargets,      // Pre-computed search targets
		VectorThreshold:      vectorThreshold,
		KeywordThreshold:     keywordThreshold,
		EmbeddingTopK:        embeddingTopK,
		RerankModelID:        rerankModelID,
		RerankTopK:           rerankTopK,
		RerankThreshold:      rerankThreshold,
		MaxRounds:            maxRounds,
		ChatModelID:          chatModelID,
		SummaryConfig:        summaryConfig,
		FallbackStrategy:     fallbackStrategy,
		FallbackResponse:     fallbackResponse,
		FallbackPrompt:       fallbackPrompt,
		EventBus:             eventBus.AsEventBusInterface(), // NEW: For pipeline to emit events directly
		WebSearchEnabled:     webSearchEnabled,
		EnableMemory:         enableMemory,      // Enable memory feature
		TenantID:             retrievalTenantID, // Effective tenant for retrieval (shared agent = agent's tenant)
		RewritePromptSystem:  rewritePromptSystem,
		RewritePromptUser:    rewritePromptUser,
		EnableRewrite:        enableRewrite,
		EnableQueryExpansion: enableQueryExpansion,
		// FAQ Strategy Settings
		FAQPriorityEnabled:       faqPriorityEnabled,
		FAQDirectAnswerThreshold: faqDirectAnswerThreshold,
		FAQScoreBoost:            faqScoreBoost,
	}

	// Determine pipeline based on knowledge bases availability and web search setting
	// If no knowledge bases are selected AND web search is disabled, use pure chat pipeline
	// Otherwise use rag_stream pipeline (which handles both KB search and web search)
	var pipeline []types.EventType
	if len(knowledgeBaseIDs) == 0 && len(knowledgeIDs) == 0 && !webSearchEnabled {
		logger.Info(ctx, "No knowledge bases selected and web search disabled, using chat pipeline")
		// For pure chat, UserContent is the Query (since INTO_CHAT_MESSAGE is skipped)
		chatManage.UserContent = query

		// Use chat_history_stream if multi-turn is enabled, otherwise use chat_stream
		if maxRounds > 0 {
			logger.Infof(ctx, "Multi-turn enabled with maxRounds=%d, using chat_history_stream pipeline", maxRounds)
			pipeline = types.Pipline["chat_history_stream"]
		} else {
			logger.Info(ctx, "Multi-turn disabled, using chat_stream pipeline")
			pipeline = types.Pipline["chat_stream"]
		}
	} else {
		if webSearchEnabled && len(knowledgeBaseIDs) == 0 && len(knowledgeIDs) == 0 {
			logger.Info(ctx, "Web search enabled without knowledge bases, using rag_stream pipeline for web search only")
		} else {
			logger.Info(ctx, "Knowledge bases selected, using rag_stream pipeline")
		}
		pipeline = types.Pipline["rag_stream"]
	}

	// Start knowledge QA event processing (set session tenant so pipeline session/message lookups use session owner)
	ctx = context.WithValue(ctx, types.SessionTenantIDContextKey, session.TenantID)
	logger.Info(ctx, "Triggering question answering event")
	err = s.KnowledgeQAByEvent(ctx, chatManage, pipeline)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": session.ID,
		})
		return err
	}

	// Emit references event if we have search results
	if len(chatManage.MergeResult) > 0 {
		logger.Infof(ctx, "Emitting references event with %d results", len(chatManage.MergeResult))
		if err := eventBus.Emit(ctx, event.Event{
			ID:        generateEventID("references"),
			Type:      event.EventAgentReferences,
			SessionID: session.ID,
			Data: event.AgentReferencesData{
				References: chatManage.MergeResult,
			},
		}); err != nil {
			logger.Errorf(ctx, "Failed to emit references event: %v", err)
		}
	}

	// Note: Answer events are now emitted directly by chat_completion_stream plugin
	// Completion event will be emitted when the last answer event has Done=true
	// We can optionally add a completion watcher here if needed, but for now
	// the frontend can detect completion from the Done flag

	logger.Info(ctx, "Knowledge base question answering initiated")
	return nil
}

// selectChatModelIDWithOverride selects the appropriate chat model ID with priority for request override
// Priority order:
// 1. Request's summaryModelID (if provided and valid)
// 2. Session's SummaryModelID if it's a Remote model
// 3. First knowledge base with a Remote model
// 4. Session's SummaryModelID (if not Remote)
// 5. First knowledge base's SummaryModelID
func (s *sessionService) selectChatModelIDWithOverride(
	ctx context.Context,
	session *types.Session,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
	summaryModelID string,
) (string, error) {
	// First, check if request has summaryModelID override
	if summaryModelID != "" {
		// Validate that the model exists
		model, err := s.modelService.GetModelByID(ctx, summaryModelID)
		if err != nil {
			logger.Warnf(
				ctx,
				"Request provided invalid summary model ID %s: %v, falling back to default selection",
				summaryModelID,
				err,
			)
		} else if model != nil {
			logger.Infof(ctx, "Using request's summary model override: %s", summaryModelID)
			return summaryModelID, nil
		}
	}

	// If no valid override, use default selection logic
	return s.selectChatModelID(ctx, session, knowledgeBaseIDs, knowledgeIDs)
}

// selectChatModelID selects the appropriate chat model ID with priority for Remote models
// Priority order:
// 1. Session's SummaryModelID if it's a Remote model
// 2. First knowledge base with a Remote model (from knowledgeBaseIDs or derived from knowledgeIDs)
// 3. Session's SummaryModelID (if not Remote)
// 4. First knowledge base's SummaryModelID
func (s *sessionService) selectChatModelID(
	ctx context.Context,
	session *types.Session,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
) (string, error) {
	// If no knowledge base IDs but have knowledge IDs, derive KB IDs from knowledge IDs (include shared KB files)
	if len(knowledgeBaseIDs) == 0 && len(knowledgeIDs) > 0 {
		tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
		knowledgeList, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge batch for model selection: %v", err)
		} else {
			// Collect unique KB IDs from knowledge items
			kbIDSet := make(map[string]bool)
			for _, k := range knowledgeList {
				if k != nil && k.KnowledgeBaseID != "" {
					kbIDSet[k.KnowledgeBaseID] = true
				}
			}
			for kbID := range kbIDSet {
				knowledgeBaseIDs = append(knowledgeBaseIDs, kbID)
			}
			logger.Infof(ctx, "Derived %d knowledge base IDs from %d knowledge IDs for model selection",
				len(knowledgeBaseIDs), len(knowledgeIDs))
		}
	}
	// Check knowledge bases for models
	if len(knowledgeBaseIDs) > 0 {
		// Try to find a knowledge base with Remote model
		for _, kbID := range knowledgeBaseIDs {
			kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get knowledge base: %v", err)
				continue
			}
			if kb != nil && kb.SummaryModelID != "" {
				model, err := s.modelService.GetModelByID(ctx, kb.SummaryModelID)
				if err == nil && model != nil && model.Source == types.ModelSourceRemote {
					logger.Info(ctx, "Using Remote summary model from knowledge base")
					return kb.SummaryModelID, nil
				}
			}
		}

		// If no Remote model found, use first knowledge base's model
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, knowledgeBaseIDs[0])
		if err != nil {
			logger.Errorf(ctx, "Failed to get knowledge base for model ID: %v", err)
			return "", fmt.Errorf("failed to get knowledge base %s: %w", knowledgeBaseIDs[0], err)
		}
		if kb != nil && kb.SummaryModelID != "" {
			logger.Infof(
				ctx,
				"Using summary model from first knowledge base %s: %s",
				knowledgeBaseIDs[0],
				kb.SummaryModelID,
			)
			return kb.SummaryModelID, nil
		}
	}

	// No knowledge bases - try to find any available chat model
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		logger.Errorf(ctx, "Failed to list models: %v", err)
		return "", fmt.Errorf("failed to list models: %w", err)
	}
	for _, model := range models {
		if model != nil && model.Type == types.ModelTypeKnowledgeQA {
			logger.Infof(ctx, "Using first available KnowledgeQA model: %s", model.ID)
			return model.ID, nil
		}
	}

	logger.Error(ctx, "No chat model ID available")
	return "", errors.New("no chat model ID available: no knowledge bases configured and no available models")
}

// resolveKnowledgeBasesFromAgent 根据智能体（Agent）的配置策略，解析并返回当前会话应加载的知识库 ID 列表。
//
// 该函数依据 customAgent.Config.KBSelectionMode 字段决定加载策略：
//   - "all" (全量模式): 自动获取当前租户下的所有自有知识库，并合并当前用户有权限访问的共享知识库。内部会自动进行去重处理，确保 ID 唯一。
//   - "selected" (指定模式): 仅返回配置中显式指定的知识库 ID 列表。
//   - "none" (禁用模式): 返回 nil，表示不加载任何知识库。
//   - 默认/未设置: 为了向后兼容，行为降级为 "selected" 模式，使用配置中指定的列表（若有）。
//
// 容错机制:
//   - 若 customAgent 为 nil，直接返回 nil。
//   - 在 "all" 模式下，若获取自有或共享知识库列表失败，仅记录警告日志 (Warn)，并返回已成功获取的部分数据，避免因依赖服务异常导致整个请求中断（尽力而为策略）。
//   - 自动处理上下文中的 TenantID 和 UserID，若缺失或类型断言失败可能导致 panic (建议调用前确保上下文完整)。
//
// 参数:
//   - ctx: 上下文对象，用于传递链路追踪信息、租户 ID (TenantID) 及用户 ID (UserID)。
//   - customAgent: 自定义智能体配置对象，包含知识库选择模式及预配置的知识库列表。
//
// 返回:
//   - []string: 解析后的知识库 ID 切片。若模式为 "none" 或发生严重前置错误，可能返回 nil。

// resolveKnowledgeBasesFromAgent resolves knowledge base IDs based on agent's KBSelectionMode
// Returns the resolved knowledge base IDs based on the selection mode:
//   - "all": fetches all knowledge bases for the tenant
//   - "selected": uses the explicitly configured knowledge bases
//   - "none": returns empty slice
//   - default: falls back to configured knowledge bases for backward compatibility
func (s *sessionService) resolveKnowledgeBasesFromAgent(
	ctx context.Context,
	customAgent *types.CustomAgent,
) []string {
	if customAgent == nil {
		return nil
	}

	switch customAgent.Config.KBSelectionMode {
	case "all":
		// Get own knowledge bases
		allKBs, err := s.knowledgeBaseService.ListKnowledgeBases(ctx)
		if err != nil {
			logger.Warnf(ctx, "Failed to list all knowledge bases: %v", err)
		}
		kbIDSet := make(map[string]bool)
		kbIDs := make([]string, 0, len(allKBs))
		for _, kb := range allKBs {
			kbIDs = append(kbIDs, kb.ID)
			kbIDSet[kb.ID] = true
		}

		// Also include shared knowledge bases the user has access to
		tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
		userIDVal := ctx.Value(types.UserIDContextKey)
		if userIDVal != nil {
			if userID, ok := userIDVal.(string); ok && userID != "" && s.kbShareService != nil {
				sharedList, err := s.kbShareService.ListSharedKnowledgeBases(ctx, userID, tenantID)
				if err != nil {
					logger.Warnf(ctx, "Failed to list shared knowledge bases: %v", err)
				} else {
					for _, info := range sharedList {
						if info != nil && info.KnowledgeBase != nil && !kbIDSet[info.KnowledgeBase.ID] {
							kbIDs = append(kbIDs, info.KnowledgeBase.ID)
							kbIDSet[info.KnowledgeBase.ID] = true
						}
					}
				}
			}
		}

		logger.Infof(ctx, "KBSelectionMode=all: loaded %d knowledge bases (own + shared)", len(kbIDs))
		return kbIDs
	case "selected":
		logger.Infof(ctx, "KBSelectionMode=selected: using %d configured knowledge bases", len(customAgent.Config.KnowledgeBases))
		return customAgent.Config.KnowledgeBases
	case "none":
		logger.Infof(ctx, "KBSelectionMode=none: no knowledge bases configured")
		return nil
	default:
		// Default to "selected" behavior for backward compatibility
		if len(customAgent.Config.KnowledgeBases) > 0 {
			logger.Infof(ctx, "KBSelectionMode not set: using %d configured knowledge bases", len(customAgent.Config.KnowledgeBases))
		}
		return customAgent.Config.KnowledgeBases
	}
}

// configureSkillsFromAgent 根据 CustomAgent 的配置，初始化 AgentConfig 中的技能（Skills）相关参数。
//
// 支持的 SkillsSelectionMode：
//   - "all": 启用所有预加载技能（SkillDirs 指向默认目录）
//   - "selected": 仅启用 CustomAgent 中显式选中的技能
//   - "none" 或 "": 禁用技能
//   - 其他值: 未知模式，默认禁用技能并记录 warning
//
// 特殊规则：
//   - 当沙箱模式（WEKNORA_SANDBOX_MODE）被禁用或为 "disabled" 时，无论选择模式如何，技能都会被强制禁用
//
// 参数：
//   - ctx: 上下文，用于日志记录和上下文传递
//   - agentConfig: 要填充的 Agent 配置（会被直接修改）
//   - customAgent: 自定义 Agent 配置来源
//
// 说明：
//   - 该函数无返回值，所有配置结果直接写入 agentConfig
//   - 若 customAgent 为 nil，函数直接返回
//   - AllowedSkills 为空时表示允许所有技能
//   - 每个分支都会记录对应的 info / warn 日志，便于排查

// configureSkillsFromAgent configures skills settings in AgentConfig based on CustomAgentConfig
// Returns the skill directories and allowed skills based on the selection mode:
//   - "all": uses all preloaded skills
//   - "selected": uses the explicitly selected skills
//   - "none" or "": skills are disabled
func (s *sessionService) configureSkillsFromAgent(
	ctx context.Context,
	agentConfig *types.AgentConfig,
	customAgent *types.CustomAgent,
) {
	if customAgent == nil {
		return
	}
	// When sandbox is disabled, skills cannot be enabled (no script execution environment)
	sandboxMode := os.Getenv("WEKNORA_SANDBOX_MODE")
	if sandboxMode == "" || sandboxMode == "disabled" {
		agentConfig.SkillsEnabled = false
		agentConfig.SkillDirs = nil
		agentConfig.AllowedSkills = nil
		logger.Infof(ctx, "Sandbox is disabled: skills are not available")
		return
	}

	switch customAgent.Config.SkillsSelectionMode {
	case "all":
		// Enable all preloaded skills
		agentConfig.SkillsEnabled = true
		agentConfig.SkillDirs = []string{DefaultPreloadedSkillsDir}
		agentConfig.AllowedSkills = nil // Empty means all skills allowed
		logger.Infof(ctx, "SkillsSelectionMode=all: enabled all preloaded skills")
	case "selected":
		// Enable only selected skills
		if len(customAgent.Config.SelectedSkills) > 0 {
			agentConfig.SkillsEnabled = true
			agentConfig.SkillDirs = []string{DefaultPreloadedSkillsDir}
			agentConfig.AllowedSkills = customAgent.Config.SelectedSkills
			logger.Infof(ctx, "SkillsSelectionMode=selected: enabled %d selected skills: %v",
				len(customAgent.Config.SelectedSkills), customAgent.Config.SelectedSkills)
		} else {
			agentConfig.SkillsEnabled = false
			logger.Infof(ctx, "SkillsSelectionMode=selected but no skills selected: skills disabled")
		}
	case "none", "":
		// Skills disabled
		agentConfig.SkillsEnabled = false
		logger.Infof(ctx, "SkillsSelectionMode=%s: skills disabled", customAgent.Config.SkillsSelectionMode)
	default:
		// Unknown mode, disable skills
		agentConfig.SkillsEnabled = false
		logger.Warnf(ctx, "Unknown SkillsSelectionMode=%s: skills disabled", customAgent.Config.SkillsSelectionMode)
	}

}

// buildSearchTargets 构建统一的搜索目标列表（SearchTargets），将知识库 ID 和知识 ID 转换为结构化的搜索目标。
// 它负责解析每个目标的实际租户归属（TenantID），处理自有、组织共享及跨租户共享（通过 Agent）的权限逻辑。
//
// 该函数在请求入口处调用一次，将知识库列表和知识条目列表统一转换为 SearchTargets 结构，
// 避免在后续处理流程中重复查询和解析，提升性能并统一权限控制逻辑。

// 构建逻辑：
//   - 处理知识库，对于每个 knowledgeBaseID：
//     * 解析其真实 TenantID（自身 KB / 组织共享 KB / 共享 Agent 场景下的检索租户）
//     * 若用户对该 KB 有访问权限，则使用 KB 原始 TenantID
//     * 否则退回到传入的 tenantID
//     * 生成类型为 `SearchTargetTypeKnowledgeBase` 的的搜索目标，标识着搜索整个知识库
//   - 处理知识，对于每个 knowledgeID：
//     * 找到其所属 KnowledgeBaseID
//     * 若该 KB 已被整体加入搜索目标，则跳过
//     * 否则生成一个 SearchTargetTypeKnowledge 类型的搜索目标，标识着搜索指定知识
//
// 参数:
//   - ctx: 上下文，包含租户 ID、用户 ID 等权限验证信息
//   - tenantID: 租户 ID，通常为当前会话的租户 ID 或共享 Agent 的有效租户
//   - knowledgeBaseIDs: 需要搜索的知识库 ID 列表（完整搜索）
//   - knowledgeIDs: 需要搜索的知识条目 ID 列表（限定在特定文件/知识内搜索）
//
// 返回:
//   - targets: 构建好的搜索目标列表，包含知识库级别和知识条目级别的搜索目标
//   - error: 执行错误（当前实现中即使出错也倾向于返回部分结果，error 通常为 nil）
//
// 注意事项:
//   - 租户 ID 由调用方（handler）根据会话或共享 Agent 设置，函数内部不修改租户范围
//   - 知识库访问权限通过 knowledgeBaseService 和 kbShareService 双重验证
//   - 知识条目获取使用 GetKnowledgeBatchWithSharedAccess，自动处理共享权限
//   - 已标记为完整搜索的知识库，其下的知识条目会被跳过，避免重复搜索
//   - 知识条目获取失败时只记录警告，返回已构建的目标（部分成功）

// buildSearchTargets computes the unified search targets from knowledgeBaseIDs and knowledgeIDs.
// tenantID is the retrieval scope: session.TenantID or effective tenant from shared agent (set by handler).
// This is called once at the request entry point to avoid repeated queries later in the pipeline.
// Logic:
//   - For each knowledgeBaseID: resolve actual TenantID (own, org-shared, or in retrieval-tenant scope for shared agent)
//   - For each knowledgeID: find its knowledgeBaseID; if the KB is already in the list, skip; otherwise add SearchTargetTypeKnowledge
func (s *sessionService) buildSearchTargets(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
) (types.SearchTargets, error) {
	var targets types.SearchTargets

	// Build a map from KB ID to TenantID for all KBs we need to process
	kbTenantMap := make(map[string]uint64)

	// Track which KBs are fully searched
	fullKBSet := make(map[string]bool)

	// First pass: batch-fetch KBs, then resolve tenant per ID (tenant scope already set by caller)
	if len(knowledgeBaseIDs) > 0 {
		kbs, _ := s.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, knowledgeBaseIDs)
		kbByID := make(map[string]*types.KnowledgeBase, len(kbs))
		for _, kb := range kbs {
			if kb != nil {
				kbByID[kb.ID] = kb
			}
		}
		userID, _ := ctx.Value(types.UserIDContextKey).(string)
		for _, kbID := range knowledgeBaseIDs {
			fullKBSet[kbID] = true
			kb := kbByID[kbID]
			if kb == nil {
				kbTenantMap[kbID] = tenantID
			} else if kb.TenantID == tenantID {
				kbTenantMap[kbID] = tenantID
			} else if s.kbShareService != nil && userID != "" {
				hasAccess, _ := s.kbShareService.HasKBPermission(ctx, kbID, userID, types.OrgRoleViewer)
				if hasAccess {
					kbTenantMap[kbID] = kb.TenantID
				} else {
					kbTenantMap[kbID] = tenantID
				}
			} else {
				kbTenantMap[kbID] = tenantID
			}
			targets = append(targets, &types.SearchTarget{
				Type:            types.SearchTargetTypeKnowledgeBase,
				KnowledgeBaseID: kbID,
				TenantID:        kbTenantMap[kbID],
			})
		}
	}

	// Process individual knowledge IDs (include shared KB files the user has access to)
	if len(knowledgeIDs) > 0 {
		knowledgeList, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge batch for search targets: %v", err)
			return targets, nil // Return what we have, don't fail
		}

		// Group knowledge IDs by their KB, excluding those already covered by full KB search
		// Also track KB tenant IDs from knowledge items
		kbToKnowledgeIDs := make(map[string][]string)
		for _, k := range knowledgeList {
			if k == nil || k.KnowledgeBaseID == "" {
				continue
			}
			// Track KB -> TenantID mapping from knowledge items
			if kbTenantMap[k.KnowledgeBaseID] == 0 {
				kbTenantMap[k.KnowledgeBaseID] = k.TenantID
			}
			// Skip if this KB is already fully searched
			if fullKBSet[k.KnowledgeBaseID] {
				continue
			}
			kbToKnowledgeIDs[k.KnowledgeBaseID] = append(kbToKnowledgeIDs[k.KnowledgeBaseID], k.ID)
		}

		// Create SearchTargetTypeKnowledge targets for each KB with specific files
		for kbID, kidList := range kbToKnowledgeIDs {
			kbTenant := kbTenantMap[kbID]
			if kbTenant == 0 {
				kbTenant = tenantID // fallback
			}
			targets = append(targets, &types.SearchTarget{
				Type:            types.SearchTargetTypeKnowledge,
				KnowledgeBaseID: kbID,
				TenantID:        kbTenant,
				KnowledgeIDs:    kidList,
			})
		}
	}

	logger.Infof(ctx, "Built %d search targets: %d full KB, %d partial KB, kbTenantMap=%v",
		len(targets), len(knowledgeBaseIDs), len(targets)-len(knowledgeBaseIDs), kbTenantMap)

	return targets, nil
}

// KnowledgeQAByEvent 通过事件管道处理知识库问答流程
//
// 该函数是会话服务的核心入口之一，负责将用户的查询请求转化为一系列有序的事件处理步骤。
// 它通过调用 `eventManager` 依次触发配置好的事件链（如：意图识别、检索增强、答案生成等），
// 并根据执行结果进行相应的错误处理或降级响应。
//
// 这种事件驱动的流水线模式，将复杂的问答流程分解为多个可独立执行的步骤，实现高内聚低耦合的处理架构。
//
//
// 核心流程：
// 1. 链路追踪与日志初始化：
//    - 创建新的 Span 以记录整个 QA 过程的性能指标。
//    - 记录关键参数（SessionID, Query）及即将触发的事件列表。
//
// 2. 顺序事件执行：
//    - 遍历 `eventList`，依次调用 `s.eventManager.Trigger` 处理每个事件。
//    - 每个事件都会接收并可能修改 `chatManage` 对象（作为上下文载体）。
//
// 3. 异常与降级处理：
//    - 如果某个事件返回"搜索无结果"错误(ErrSearchNothing): 返回此错误，视为“未找到相关知识”。
//      系统将立即触发 `handleFallbackResponse`（兜底回复策略，如通用回答或引导语），并正常结束流程（返回 nil）。
//    - 其他错误: 记录详细错误日志（类型、描述、原始错误），更新 Span 状态为 Error，并直接返回原始错误对象，中断后续事件执行。
//
// 4. 成功完成：
//    - 若所有事件均成功执行，记录成功日志并返回 nil。
//
// 参数:
//   - ctx: 上下文对象，包含链路追踪信息、RequestID 及用户身份等。
//   - chatManage: 聊天管理对象，携带会话状态、用户查询 (Query)、会话 ID 及兜底策略配置。该对象在事件链中会被各处理器修改（如填入检索结果、生成的回答等）。
//   - eventList: 需要触发的事件类型列表，定义了 QA 的处理流水线顺序，按序执行（如: [REWRITE_QUERY, SEARCH, RERANK, GENERATE]）
//
// 返回:
//   - error: 错误信息，可能的情况包括:
//     - 搜索无结果时返回 nil（已通过兜底响应处理）
//     - 事件处理失败时返回具体的错误（如检索服务异常、生成服务超时等）
//
// 说明：
//   - 该函数通常用于知识库问答的主流程入口
//   - 事件列表由上层根据 Agent / Session 配置决定
//   - 所有关键节点均有日志和 tracing 记录，便于问题排查
//
// 事件类型示例:
//   - REWRITE_QUERY: 查询重写，优化用户输入
//   - SEARCH: 向量检索，从知识库中检索相关内容
//   - RERANK: 重排序，优化检索结果排序
//   - GENERATE: 生成回答，基于检索结果生成最终答案
//   - FILTER: 过滤，过滤不符合条件的检索结果
//
// 注意事项:
//   - 事件按顺序执行，前一个事件的结果会影响后续事件的处理
//   - 搜索无结果时不会中断流程，而是触发兜底响应（如返回默认回复或建议）
//   - 所有事件通过 eventManager 统一管理，支持事件插拔和动态配置
//   - 使用 OpenTelemetry 进行链路追踪，便于监控和问题排查

// 用户查询: "什么是向量数据库？"
//					   ↓
//	┌─────────────────────────────────────────────────────────────┐
//	│              事件管道顺序执行                                  │
//	├─────────────────────────────────────────────────────────────┤
//	│                                                             │
//	│  Event 1: REWRITE_QUERY                                     │
//	│  ├─ 输入: "什么是向量数据库？"                                  │
//	│  └─ 输出: 优化后的查询语句（可选项）                             │
//	│                    ↓                                        │
//	│  Event 2: SEARCH                                            │
//	│  ├─ 输入: 优化后的查询                                         │
//	│  └─ 输出: 检索到的相关文档列表（3-5 条）                         │
//	│                    ↓                                        │
//	│  Event 3: RERANK                                            │
//	│  ├─ 输入: 原始检索结果                                         │
//	│  └─ 输出: 重排序后的文档列表（提高相关性）                        │
//	│                    ↓                                        │
//	│  Event 4: GENERATE                                          │
//	│  ├─ 输入: 重排序后的文档 + 原始查询                              │
//	│  └─ 输出: 最终生成的回答 + 引用来源                             │
//	│                                                             │
//	└─────────────────────────────────────────────────────────────┘
//					   ↓
//	最终回答: "向量数据库是一种专门用于存储和检索向量嵌入的数据库..."

// KnowledgeQAByEvent processes knowledge QA through a series of events in the pipeline
func (s *sessionService) KnowledgeQAByEvent(
	ctx context.Context,
	chatManage *types.ChatManage,
	eventList []types.EventType,
) error {
	ctx, span := tracing.ContextWithSpan(ctx, "SessionService.KnowledgeQAByEvent")
	defer span.End()

	logger.Info(ctx, "Start processing knowledge base question answering through events")
	logger.Infof(ctx, "Knowledge base question answering parameters, session ID: %s,  query: %s",
		chatManage.SessionID, chatManage.Query)

	// Prepare method list for logging and tracing
	methods := []string{}
	for _, event := range eventList {
		methods = append(methods, string(event))
	}

	// Set up tracing attributes
	logger.Infof(ctx, "Trigger event list: %v", methods)
	span.SetAttributes(
		attribute.String("request_id", ctx.Value(types.RequestIDContextKey).(string)),
		attribute.String("query", chatManage.Query),
		attribute.String("method", strings.Join(methods, ",")),
	)

	// Process each event in sequence
	for _, eventType := range eventList {
		logger.Infof(ctx, "Starting to trigger event: %v", eventType)
		err := s.eventManager.Trigger(ctx, eventType, chatManage)

		// Handle case where search returns no results
		if err == chatpipline.ErrSearchNothing {
			logger.Warnf(
				ctx,
				"Event %v triggered, search result is empty, using fallback response, strategy: %v",
				eventType,
				chatManage.FallbackStrategy,
			)
			s.handleFallbackResponse(ctx, chatManage)
			return nil
		}

		// Handle other errors
		if err != nil {
			logger.Errorf(ctx, "Event triggering failed, event: %v, error type: %s, description: %s, error: %v",
				eventType, err.ErrorType, err.Description, err.Err)
			span.RecordError(err.Err)
			span.SetStatus(codes.Error, err.Description)
			span.SetAttributes(attribute.String("error_type", err.ErrorType))
			return err.Err
		}
		logger.Infof(ctx, "Event %v triggered successfully", eventType)
	}

	logger.Info(ctx, "All events triggered successfully")
	return nil
}

// SearchKnowledge 执行纯知识库检索（不包含大模型总结生成）
// 该函数用于在指定的知识库或文件中查找与用户查询最相关的文本片段，
// 并直接返回经过重排序和合并的原始检索结果，不经过大模型处理。
//
// 主要执行步骤:
//  1. 参数校验与上下文获取: 从上下文中提取租户ID，确保多租户数据隔离。
//  2. 构建检索目标: 调用 buildSearchTargets 解析知识库ID和文件ID，确定检索目标。
//  3. 初始化上下文管理器: 创建 ChatManage 对象，填充查询语句、用户信息及系统默认的检索参数（如阈值、TopK、轮数）。
//  4. 配置重排序模型: 自动查找并加载系统默认的重排序模型，用于优化检索精度。
//  5. 定义检索流水线: 设定需要触发的事件列表，包括向量检索、重排序、结果合并和过滤。
//  6. 执行流水线: 依次触发上述事件，通过事件管理器驱动各个插件执行具体的检索逻辑。
//  7. 结果返回: 若检索成功，返回合并后的结果列表；若无结果或出错，返回相应状态或错误。
//
// 参数:
//   - ctx: 请求上下文，包含租户ID、用户ID及链路追踪信息。
//   - knowledgeBaseIDs: 待检索的知识库ID列表，支持多库联合检索。
//   - knowledgeIDs: 待检索的具体知识（文件）ID列表，用于指定特定文件。
//   - query: 用户的查询语句。
//
// 返回值:
//   - []*types.SearchResult: 检索到的文本片段列表，已按相关性排序。
//   - error: 检索过程中发生的错误，如参数错误、系统内部错误等。

// SearchKnowledge performs knowledge base search without LLM summarization
// knowledgeBaseIDs: list of knowledge base IDs to search (supports multi-KB)
// knowledgeIDs: list of specific knowledge (file) IDs to search
func (s *sessionService) SearchKnowledge(ctx context.Context,
	knowledgeBaseIDs []string, knowledgeIDs []string, query string,
) ([]*types.SearchResult, error) {
	logger.Info(ctx, "Start knowledge base search without LLM summary")
	logger.Infof(ctx, "Knowledge base search parameters, knowledge base IDs: %v, knowledge IDs: %v, query: %s",
		knowledgeBaseIDs, knowledgeIDs, query)

	// Get tenant ID from context
	tenantID, ok := ctx.Value(types.TenantIDContextKey).(uint64)
	if !ok {
		logger.Error(ctx, "Failed to get tenant ID from context")
		return nil, fmt.Errorf("tenant ID not found in context")
	}

	// Build unified search targets (computed once, used throughout pipeline)
	searchTargets, err := s.buildSearchTargets(ctx, tenantID, knowledgeBaseIDs, knowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to build search targets: %v", err)
	}

	if len(searchTargets) == 0 {
		logger.Warn(ctx, "No search targets available, returning empty results")
		return []*types.SearchResult{}, nil
	}

	// Create default retrieval parameters
	userID, _ := ctx.Value(types.UserIDContextKey).(string)
	chatManage := &types.ChatManage{
		Query:            query,
		RewriteQuery:     query,
		UserID:           userID,
		KnowledgeBaseIDs: knowledgeBaseIDs,
		KnowledgeIDs:     knowledgeIDs,
		SearchTargets:    searchTargets,
		VectorThreshold:  s.cfg.Conversation.VectorThreshold,  // Use default configuration
		KeywordThreshold: s.cfg.Conversation.KeywordThreshold, // Use default configuration
		EmbeddingTopK:    s.cfg.Conversation.EmbeddingTopK,    // Use default configuration
		RerankTopK:       s.cfg.Conversation.RerankTopK,       // Use default configuration
		RerankThreshold:  s.cfg.Conversation.RerankThreshold,  // Use default configuration
		MaxRounds:        s.cfg.Conversation.MaxRounds,
	}

	// Get default models
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		logger.Errorf(ctx, "Failed to get models: %v", err)
		return nil, err
	}

	// Find the first available rerank model
	for _, model := range models {
		if model == nil {
			continue
		}
		if model.Type == types.ModelTypeRerank {
			chatManage.RerankModelID = model.ID
			break
		}
	}

	// Use specific event list, only including retrieval-related events, not LLM summarization
	searchEvents := []types.EventType{
		types.CHUNK_SEARCH, // Vector search
		types.CHUNK_RERANK, // Rerank search results
		types.CHUNK_MERGE,  // Merge search results
		types.FILTER_TOP_K, // Filter top K results
	}

	ctx, span := tracing.ContextWithSpan(ctx, "SessionService.SearchKnowledge")
	defer span.End()

	// Prepare method list for logging and tracing
	methods := []string{}
	for _, event := range searchEvents {
		methods = append(methods, string(event))
	}

	// Set up tracing attributes
	logger.Infof(ctx, "Trigger search event list: %v", methods)
	span.SetAttributes(
		attribute.String("query", query),
		attribute.StringSlice("knowledge_base_ids", knowledgeBaseIDs),
		attribute.StringSlice("knowledge_ids", knowledgeIDs),
		attribute.String("method", strings.Join(methods, ",")),
	)

	// Process each search event in sequence
	for _, event := range searchEvents {
		logger.Infof(ctx, "Starting to trigger search event: %v", event)
		err := s.eventManager.Trigger(ctx, event, chatManage)

		// Handle case where search returns no results
		if err == chatpipline.ErrSearchNothing {
			logger.Warnf(ctx, "Event %v triggered, search result is empty", event)
			return []*types.SearchResult{}, nil
		}

		// Handle other errors
		if err != nil {
			logger.Errorf(ctx, "Event triggering failed, event: %v, error type: %s, description: %s, error: %v",
				event, err.ErrorType, err.Description, err.Err)
			span.RecordError(err.Err)
			span.SetStatus(codes.Error, err.Description)
			span.SetAttributes(attribute.String("error_type", err.ErrorType))
			return nil, err.Err
		}
		logger.Infof(ctx, "Event %v triggered successfully", event)
	}

	logger.Infof(ctx, "Knowledge base search completed, found %d results", len(chatManage.MergeResult))
	return chatManage.MergeResult, nil
}

// AgentQA 执行基于Agent的问答（支持对话历史和流式输出）
//
// 该函数是Agent模式问答的核心入口，支持自定义Agent配置、多轮对话、 知识库检索、MCP工具调用、网络搜索等高级功能。
// 通过EventBus实现流式事件输出，适用于需要复杂推理和工具调用的场景。

// AgentQA performs agent-based question answering with conversation history and streaming support
// customAgent is optional - if provided, uses custom agent configuration instead of tenant defaults
// summaryModelID is optional - if provided, overrides the model from customAgent config
func (s *sessionService) AgentQA(
	ctx context.Context, // 请求上下文，包含租户ID、用户信息等
	session *types.Session, // 会话对象，包含ID、租户ID等基础信息
	query string, // 用户当前查询内容
	assistantMessageID string, // 助手消息ID，用于标识本次回复
	summaryModelID string, // 摘要模型ID（可选），覆盖Agent默认配置
	eventBus *event.EventBus, // 事件总线，用于流式输出事件
	customAgent *types.CustomAgent, // 自定义Agent配置（必须）
	knowledgeBaseIDs []string, // 知识库ID列表（可选，通过@提及）
	knowledgeIDs []string, // 知识文件ID列表（可选，通过@提及）
) error {
	sessionID := session.ID
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		logger.Errorf(ctx, "Failed to marshal session, session ID: %s, error: %v", sessionID, err)
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	// customAgent is required for AgentQA (handler has already done permission check for shared agent)
	if customAgent == nil {
		logger.Warnf(ctx, "Custom agent not provided for session: %s", sessionID)
		return errors.New("custom agent configuration is required for agent QA")
	}

	// Use agent's tenant for retrieval and tenant-scoped config (handler has validated access)
	agentTenantID := customAgent.TenantID
	if agentTenantID == 0 {
		agentTenantID = session.TenantID
	}
	logger.Infof(ctx, "Start agent-based question answering, session ID: %s, agent tenant ID: %d, query: %s, session: %s",
		sessionID, agentTenantID, query, string(sessionJSON))

	// 尝试获取租户详细信息，用于后续的配额或配置检查
	var tenantInfo *types.Tenant
	if v := ctx.Value(types.TenantInfoContextKey); v != nil {
		tenantInfo, _ = v.(*types.Tenant)
	}

	// 如果上下文中的租户信息与 Agent 的不一致，则去数据库加载 Agent 所属租户的信息
	//
	// 如果你使用了一个别人分享给你的智能体，系统会自动切换到那个智能体的租户去查找它的模型和知识库，而不是用你家的。

	// When agent belongs to another tenant (shared agent), use agent's tenant for KB/model scope; load tenantInfo if needed
	if tenantInfo == nil || tenantInfo.ID != agentTenantID {
		if s.tenantService != nil {
			if agentTenant, err := s.tenantService.GetTenantByID(ctx, agentTenantID); err == nil && agentTenant != nil {
				tenantInfo = agentTenant
				logger.Infof(ctx, "Using agent tenant info for retrieval scope, tenant ID: %d", agentTenantID)
			}
		}
	}
	if tenantInfo == nil {
		logger.Warnf(ctx, "Tenant info not available for agent tenant %d, proceeding with defaults", agentTenantID)
		tenantInfo = &types.Tenant{ID: agentTenantID}
	}

	// 调用 EnsureDefaults() 填充 CustomAgent 缺失配置。
	// 创建 Agent 运行时配置，将 customAgent 里的配置参数映射到运行时配置 AgentConfig 中。
	// 同时，根据 CustomAgent 的配置，初始化 AgentConfig 中的技能（Skills）相关参数。

	// Ensure defaults are set
	customAgent.EnsureDefaults()

	// Create runtime AgentConfig from customAgent
	// Note: tenantInfo.AgentConfig is deprecated, all config comes from customAgent now
	agentConfig := &types.AgentConfig{
		MaxIterations:               customAgent.Config.MaxIterations,
		ReflectionEnabled:           customAgent.Config.ReflectionEnabled,
		Temperature:                 customAgent.Config.Temperature,
		WebSearchEnabled:            customAgent.Config.WebSearchEnabled,
		WebSearchMaxResults:         customAgent.Config.WebSearchMaxResults,
		MultiTurnEnabled:            customAgent.Config.MultiTurnEnabled,
		HistoryTurns:                customAgent.Config.HistoryTurns,
		MCPSelectionMode:            customAgent.Config.MCPSelectionMode,
		MCPServices:                 customAgent.Config.MCPServices,
		Thinking:                    customAgent.Config.Thinking,
		RetrieveKBOnlyWhenMentioned: customAgent.Config.RetrieveKBOnlyWhenMentioned,
	}

	// Configure skills based on CustomAgentConfig
	s.configureSkillsFromAgent(ctx, agentConfig, customAgent)

	// 如果用户在消息里用 @ 提到了具体的库或文件，则覆盖 Agent 原有的默认知识库。
	// 如果开启了“仅在 @ 文件中检索”且用户本次没 @ ，则直接禁用知识库检索。
	// 以上都不满足，则使用 Agent 配置里预设的知识库。

	// Resolve knowledge bases: request-level @ mentions take priority over agent config
	// If RetrieveKBOnlyWhenMentioned is enabled and no @ mentions, don't use KB at all
	hasExplicitMention := len(knowledgeBaseIDs) > 0 || len(knowledgeIDs) > 0
	logger.Infof(ctx, "KB resolution: hasExplicitMention=%v, RetrieveKBOnlyWhenMentioned=%v, KBSelectionMode=%s",
		hasExplicitMention, agentConfig.RetrieveKBOnlyWhenMentioned, customAgent.Config.KBSelectionMode)
	if hasExplicitMention {
		// 情况 A：用户强制指定了知识库（例如在输入框里 @ 了某个文档）
		// User explicitly specified via @ mention
		if len(knowledgeBaseIDs) > 0 {
			agentConfig.KnowledgeBases = knowledgeBaseIDs
			logger.Infof(ctx, "Using request-specified knowledge bases: %v", knowledgeBaseIDs)
		}
		if len(knowledgeIDs) > 0 {
			agentConfig.KnowledgeIDs = knowledgeIDs
			logger.Infof(ctx, "Using request-specified knowledge IDs: %v", knowledgeIDs)
		}
	} else if agentConfig.RetrieveKBOnlyWhenMentioned {
		// 情况 B：配置了“仅提及才查”，但用户没提 -> 进入“纯模式”，禁用知识库检索，Agent 只靠 LLM 回答，不能查文档
		// User didn't mention any KB/file, and the setting requires explicit mention
		agentConfig.KnowledgeBases = nil
		agentConfig.KnowledgeIDs = nil
		logger.Infof(ctx, "RetrieveKBOnlyWhenMentioned is enabled and no @ mention found, KB retrieval disabled for this request")
	} else {
		// 情况 C：使用 Agent 默认配置的知识库
		// Use agent's configured knowledge bases based on KBSelectionMode
		agentConfig.KnowledgeBases = s.resolveKnowledgeBasesFromAgent(ctx, customAgent)
	}

	// 加载工具。
	// 如果 Agent 配置了 AllowedTools 则使用它，否则加载系统默认的一套工具（如天气、搜索、计算器等）。

	// Use custom agent's allowed tools if specified, otherwise use defaults
	if len(customAgent.Config.AllowedTools) > 0 {
		agentConfig.AllowedTools = customAgent.Config.AllowedTools
	} else {
		agentConfig.AllowedTools = tools.DefaultAllowedTools()
	}

	// 如果配置了自定义系统提示词，则注入到配置中，这是定义 Agent 角色的关键。
	// Use custom agent's system prompt if specified
	if customAgent.Config.SystemPrompt != "" {
		agentConfig.UseCustomSystemPrompt = true
		agentConfig.SystemPrompt = customAgent.Config.SystemPrompt
	}

	logger.Infof(ctx, "Custom agent config applied: MaxIterations=%d, Temperature=%.2f, AllowedTools=%v, WebSearchEnabled=%v",
		agentConfig.MaxIterations, agentConfig.Temperature, agentConfig.AllowedTools, agentConfig.WebSearchEnabled)

	// 设置 Web Search（网络搜索）的最大结果数，若租户有特殊配置则覆盖。
	// Set web search max results from tenant config if not set (default: 5)
	if agentConfig.WebSearchMaxResults == 0 {
		agentConfig.WebSearchMaxResults = 5
		if tenantInfo.WebSearchConfig != nil && tenantInfo.WebSearchConfig.MaxResults > 0 {
			agentConfig.WebSearchMaxResults = tenantInfo.WebSearchConfig.MaxResults
		}
	}

	logger.Infof(ctx, "Merged agent config from tenant %d and session %s", tenantInfo.ID, sessionID)

	// Log knowledge bases if present
	if len(agentConfig.KnowledgeBases) > 0 {
		logger.Infof(ctx, "Agent configured with %d knowledge base(s): %v",
			len(agentConfig.KnowledgeBases), agentConfig.KnowledgeBases)
	} else {
		// Allow running without knowledge bases (Pure Agent mode)
		logger.Infof(ctx, "No knowledge bases specified for agent, running in pure agent mode")
	}

	// 根据租户和知识库/知识 ID 列表，构建底层的检索目标对象。
	// Build search targets using agent's tenant (handler has validated access for shared agent)
	searchTargets, err := s.buildSearchTargets(ctx, agentTenantID, agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to build search targets for agent: %v", err)
		// Continue without search targets, the tool will handle empty targets
	}
	agentConfig.SearchTargets = searchTargets
	logger.Infof(ctx, "Agent search targets built: %d targets", len(searchTargets))

	// 确定使用哪个模型，如果请求参数未指定，使用 Agent 默认配置。
	//
	// 虽然模型名叫 summary，但在 AgentQA 代码中，它的职责已经远远超出了“总结”：
	//	- 任务拆解：分析用户的复杂问题。
	//	- 工具调度：决定什么时候该去搜索，什么时候该用计算器。
	//	- 推理决策：在 engine.Execute 过程中，它负责每一轮的 Thought（思考）。
	//	- 内容生成：最后把所有的工具执行结果和知识库内容整合，给出最终答案。

	// Get summary model: prioritize request's summaryModelID, then custom agent config
	// Note: tenantInfo.ConversationConfig is deprecated, all config comes from customAgent now
	effectiveModelID := summaryModelID
	if effectiveModelID == "" {
		effectiveModelID = customAgent.Config.ModelID
	}
	if effectiveModelID == "" {
		logger.Warnf(ctx, "No summary model configured for custom agent %s", customAgent.ID)
		return errors.New("summary model (model_id) is not configured in custom agent settings")
	}
	if summaryModelID != "" {
		logger.Infof(ctx, "Using request's summary model override: %s", effectiveModelID)
	} else {
		logger.Infof(ctx, "Using custom agent's model_id: %s", effectiveModelID)
	}

	// 获取 summary 模型
	summaryModel, err := s.modelService.GetChatModel(ctx, effectiveModelID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get chat model: %v", err)
		return fmt.Errorf("failed to get chat model: %w", err)
	}

	// 获取 Rerank 模型
	//
	// 重排模型通常工作在 “检索” 和 “生成” 之间。
	// 如果没有指定知识库（即处于纯 LLM 模式），系统里就没有“一堆文档片段”需要筛选，自然也就不需要重排模型。

	// Get rerank model from custom agent config (only required when knowledge bases are configured)
	var rerankModel rerank.Reranker
	hasKnowledge := len(agentConfig.KnowledgeBases) > 0 || len(agentConfig.KnowledgeIDs) > 0
	if hasKnowledge {
		rerankModelID := customAgent.Config.RerankModelID
		if rerankModelID == "" {
			logger.Warnf(ctx, "No rerank model configured for custom agent %s, but knowledge bases are specified", customAgent.ID)
			return errors.New("rerank model (rerank_model_id) is not configured in custom agent settings")
		}

		rerankModel, err = s.modelService.GetRerankModel(ctx, rerankModelID)
		if err != nil {
			logger.Warnf(ctx, "Failed to get rerank model: %v", err)
			return fmt.Errorf("failed to get rerank model: %w", err)
		}
	} else {
		logger.Infof(ctx, "No knowledge bases configured, skipping rerank model initialization")
	}

	// 获取当前会话的上下文管理器。
	//
	// contextManager 是管理对话历史的组件，每个会话有独立的上下文管理器。
	//	- 对话历史持久化存储
	//	- 处理历史过长时的压缩、总结
	//  - 维护当前的系统提示词，用户可以在同一个会话里切换不同的 Agent（角色）
	//
	// 如果该会话是第一次使用，会自动创建新的管理器；如果已存在，则返回已有的实例。
	// 传入 summaryModel 是为了让管理器知道使用哪个模型进行上下文压缩（当历史太长时）。

	// Get or create contextManager for this session
	contextManager := s.getContextManagerForSession(ctx, session, summaryModel)

	// 获取系统提示词
	//
	// 如果开启了联网搜索，生成的 Prompt 会自动包含：“你是一个智能助手。当你不知道答案时，必须调用搜索工具。”。
	// 否则，可能包含 “你是一个智能助手。请仅根据你已有的知识回答，不要尝试联网。”
	//
	// 取到系统提示词之后，把它设置到上下文管理器中。
	// contextManager 会把旧的提示词换掉，相当于 Agent 切换身份。

	// Set system prompt for the current agent in context manager
	// This ensures the context uses the correct system prompt when switching agents
	systemPrompt := agentConfig.ResolveSystemPrompt(agentConfig.WebSearchEnabled)
	if systemPrompt != "" {
		if err := contextManager.SetSystemPrompt(ctx, sessionID, systemPrompt); err != nil {
			logger.Warnf(ctx, "Failed to set system prompt in context manager: %v", err)
		} else {
			logger.Infof(ctx, "System prompt updated in context manager for agent")
		}
	}

	// 利用 contextManager 查找并加载 sessionID 的历史消息列表。
	// 内部可能会根据模型 Token 限制对过长的历史进行自动截断或总结（取决于实现）。
	// 这些消息可能是存储在 redis 或数据库中的。

	// Get LLM context from context manager
	llmContext, err := s.getContextForSession(ctx, contextManager, sessionID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get LLM context: %v, continuing without history", err)
		llmContext = []chat.Message{}
	}
	logger.Infof(ctx, "Loaded %d messages from LLM context manager", len(llmContext))

	// 检查配置项 MultiTurnEnabled（多轮对话开关）。
	//
	// 有些 Agent 只需要处理当前任务（如：格式化一段文本、翻译一个句子），不需要被之前的对话干扰。
	// 此时，需要将刚才从数据库/缓存里取出来的 llmContext 彻底清空，使 LLM 不参考任何历史。

	// Apply multi-turn configuration for Agent mode
	// Note: In Agent mode, context is managed by contextManager with compression strategies,
	// so we don't apply HistoryTurns limit here. HistoryTurns is used in normal (KnowledgeQA) mode.
	if !agentConfig.MultiTurnEnabled {
		// Multi-turn disabled, clear history
		logger.Infof(ctx, "Multi-turn disabled for this agent, clearing history context")
		llmContext = []chat.Message{}
	}

	// 实例化一个 Agent 执行对象

	// Create agent engine with EventBus and ContextManager
	logger.Info(ctx, "Creating agent engine")
	engine, err := s.agentService.CreateAgentEngine(
		ctx,
		agentConfig,    // 1. 规则：智能体配置（允许用什么工具、查什么库）
		summaryModel,   // 2. 大脑：LLM 模型实例
		rerankModel,    // 3. 过滤器：重排序模型（用于知识库筛选）
		eventBus,       // 4. 通信线：事件总线（关键！用于流式输出）
		contextManager, // 5. 记忆管家：负责管理上下文
		session.ID,     // 6. 身份证：当前会话 ID
	)
	if err != nil {
		logger.Errorf(ctx, "Failed to create agent engine: %v", err)
		return err
	}

	// 把 llmContext（历史记录）和 query（当前问题）交给引擎开始执行。

	// Execute agent with streaming (asynchronously)
	// Events will be emitted to EventBus and handled by the Handler layer
	logger.Info(ctx, "Executing agent with streaming")
	if _, err := engine.Execute(ctx, sessionID, assistantMessageID, query, llmContext); err != nil {
		logger.Errorf(ctx, "Agent execution failed: %v", err)
		// Emit error event to the EventBus used by this agent
		eventBus.Emit(ctx, event.Event{
			Type:      event.EventError,
			SessionID: sessionID,
			Data: event.ErrorData{
				Error:     err.Error(),
				Stage:     "agent_execution",
				SessionID: sessionID,
			},
		})
	}
	// Return empty - events will be handled by Handler via EventBus subscription
	return nil
}

// getContextManagerForSession creates a context manager for the session based on configuration
// Returns the configured context manager (tenant-level or session-level) or default
func (s *sessionService) getContextManagerForSession(
	ctx context.Context,
	session *types.Session,
	chatModel chat.Chat,
) interfaces.ContextManager {
	// Get tenant to access global context configuration
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	// Determine which context config to use: tenant-level or default
	var contextConfig *types.ContextConfig
	if tenant != nil && tenant.ContextConfig != nil {
		// Use tenant-level configuration
		contextConfig = tenant.ContextConfig
		logger.Infof(ctx, "Using tenant-level context config for session %s", session.ID)
	} else {
		// Use service's default context manager
		logger.Debugf(ctx, "Using default context manager for session %s", session.ID)
		contextConfig = &types.ContextConfig{
			MaxTokens:           llmcontext.DefaultMaxTokens,
			CompressionStrategy: llmcontext.DefaultCompressionStrategy,
			RecentMessageCount:  llmcontext.DefaultRecentMessageCount,
			SummarizeThreshold:  llmcontext.DefaultSummarizeThreshold,
		}
	}
	return llmcontext.NewContextManagerFromConfig(contextConfig, s.sessionStorage, chatModel)
}

// getContextForSession retrieves LLM context for a session
func (s *sessionService) getContextForSession(
	ctx context.Context,
	contextManager interfaces.ContextManager,
	sessionID string,
) ([]chat.Message, error) {
	history, err := contextManager.GetContext(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get context: %w", err)
	}

	// Log context statistics
	stats, _ := contextManager.GetContextStats(ctx, sessionID)
	if stats != nil {
		logger.Infof(ctx, "LLM context stats for session %s: messages=%d, tokens=~%d, compressed=%v",
			sessionID, stats.MessageCount, stats.TokenCount, stats.IsCompressed)
	}

	return history, nil
}

// ClearContext clears the LLM context for a session
// This is useful when switching knowledge bases or agent modes to prevent context contamination
func (s *sessionService) ClearContext(ctx context.Context, sessionID string) error {
	logger.Infof(ctx, "Clearing context for session: %s", sessionID)
	return s.sessionStorage.Delete(ctx, sessionID)
}

// handleFallbackResponse handles fallback response based on strategy
func (s *sessionService) handleFallbackResponse(ctx context.Context, chatManage *types.ChatManage) {
	if chatManage.FallbackStrategy == types.FallbackStrategyModel {
		s.handleModelFallback(ctx, chatManage)
	} else {
		s.handleFixedFallback(ctx, chatManage)
	}
}

// handleFixedFallback handles fixed fallback response
func (s *sessionService) handleFixedFallback(ctx context.Context, chatManage *types.ChatManage) {
	fallbackContent := chatManage.FallbackResponse
	chatManage.ChatResponse = &types.ChatResponse{Content: fallbackContent}
	s.emitFallbackAnswer(ctx, chatManage, fallbackContent)
}

// handleModelFallback handles model-based fallback response using streaming
func (s *sessionService) handleModelFallback(ctx context.Context, chatManage *types.ChatManage) {
	// Check if FallbackPrompt is available
	if chatManage.FallbackPrompt == "" {
		logger.Warnf(ctx, "Fallback strategy is 'model' but FallbackPrompt is empty, falling back to fixed response")
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	// Render template with Query variable
	promptContent, err := s.renderFallbackPrompt(ctx, chatManage)
	if err != nil {
		logger.Errorf(ctx, "Failed to render fallback prompt: %v, falling back to fixed response", err)
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	// Check if EventBus is available for streaming
	if chatManage.EventBus == nil {
		logger.Warnf(ctx, "EventBus not available for streaming fallback, falling back to fixed response")
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	// Get chat model
	chatModel, err := s.modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chat model for fallback: %v, falling back to fixed response", err)
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	// Prepare chat options
	thinking := false
	opt := &chat.ChatOptions{
		Temperature:         chatManage.SummaryConfig.Temperature,
		MaxCompletionTokens: chatManage.SummaryConfig.MaxCompletionTokens,
		Thinking:            &thinking,
	}

	// Start streaming response
	responseChan, err := chatModel.ChatStream(ctx, []chat.Message{
		{Role: "user", Content: promptContent},
	}, opt)
	if err != nil {
		logger.Errorf(ctx, "Failed to start streaming fallback response: %v, falling back to fixed response", err)
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	if responseChan == nil {
		logger.Errorf(ctx, "Chat stream returned nil channel, falling back to fixed response")
		s.handleFixedFallback(ctx, chatManage)
		return
	}

	// Start goroutine to consume stream and emit events
	go s.consumeFallbackStream(ctx, chatManage, responseChan)
}

// renderFallbackPrompt renders the fallback prompt template with Query variable
func (s *sessionService) renderFallbackPrompt(ctx context.Context, chatManage *types.ChatManage) (string, error) {
	// Use simple string replacement instead of Go template
	result := strings.ReplaceAll(chatManage.FallbackPrompt, "{{query}}", chatManage.Query)
	return result, nil
}

// consumeFallbackStream consumes the streaming response and emits events
func (s *sessionService) consumeFallbackStream(
	ctx context.Context,
	chatManage *types.ChatManage,
	responseChan <-chan types.StreamResponse,
) {
	fallbackID := generateEventID("fallback")
	eventBus := chatManage.EventBus
	var finalContent string
	streamCompleted := false

	for response := range responseChan {
		// Emit event for each answer chunk
		if response.ResponseType == types.ResponseTypeAnswer {
			finalContent += response.Content
			if err := eventBus.Emit(ctx, types.Event{
				ID:        fallbackID,
				Type:      types.EventType(event.EventAgentFinalAnswer),
				SessionID: chatManage.SessionID,
				Data: event.AgentFinalAnswerData{
					Content: response.Content,
					Done:    response.Done,
				},
			}); err != nil {
				logger.Errorf(ctx, "Failed to emit fallback answer chunk event: %v", err)
			}

			// Update ChatResponse with final content when done
			if response.Done {
				chatManage.ChatResponse = &types.ChatResponse{Content: finalContent}
				streamCompleted = true
				logger.Infof(ctx, "Fallback streaming response completed")
				break
			}
		}
	}

	// If channel closed without Done=true, emit final event with fixed response
	if !streamCompleted {
		logger.Warnf(ctx, "Fallback stream closed without completion, emitting final event with fixed response")
		s.emitFallbackAnswer(ctx, chatManage, chatManage.FallbackResponse)
	}
}

// emitFallbackAnswer emits fallback answer event
func (s *sessionService) emitFallbackAnswer(ctx context.Context, chatManage *types.ChatManage, content string) {
	if chatManage.EventBus == nil {
		return
	}

	fallbackID := generateEventID("fallback")
	if err := chatManage.EventBus.Emit(ctx, types.Event{
		ID:        fallbackID,
		Type:      types.EventType(event.EventAgentFinalAnswer),
		SessionID: chatManage.SessionID,
		Data: event.AgentFinalAnswerData{
			Content: content,
			Done:    true,
		},
	}); err != nil {
		logger.Errorf(ctx, "Failed to emit fallback answer event: %v", err)
	} else {
		logger.Infof(ctx, "Fallback answer event emitted successfully")
	}
}
