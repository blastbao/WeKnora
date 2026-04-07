package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

const MAX_ITERATIONS = 100 // Max iterations for agent execution

// agentService implements agent-related business logic
type agentService struct {
	cfg                   *config.Config
	modelService          interfaces.ModelService
	mcpServiceService     interfaces.MCPServiceService
	mcpManager            *mcp.MCPManager
	eventBus              *event.EventBus
	db                    *gorm.DB
	webSearchService      interfaces.WebSearchService
	knowledgeBaseService  interfaces.KnowledgeBaseService
	knowledgeService      interfaces.KnowledgeService
	fileService           interfaces.FileService
	chunkService          interfaces.ChunkService
	duckdb                *sql.DB
	webSearchStateService interfaces.WebSearchStateService
}

// NewAgentService creates a new agent service
func NewAgentService(
	cfg *config.Config,
	modelService interfaces.ModelService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
	chunkService interfaces.ChunkService,
	mcpServiceService interfaces.MCPServiceService,
	mcpManager *mcp.MCPManager,
	eventBus *event.EventBus,
	db *gorm.DB,
	webSearchService interfaces.WebSearchService,
	duckdb *sql.DB,
	webSearchStateService interfaces.WebSearchStateService,
) interfaces.AgentService {
	return &agentService{
		cfg:                   cfg,
		modelService:          modelService,
		knowledgeBaseService:  knowledgeBaseService,
		knowledgeService:      knowledgeService,
		fileService:           fileService,
		chunkService:          chunkService,
		mcpServiceService:     mcpServiceService,
		mcpManager:            mcpManager,
		eventBus:              eventBus,
		db:                    db,
		webSearchService:      webSearchService,
		duckdb:                duckdb,
		webSearchStateService: webSearchStateService,
	}
}

// CreateAgentEngine 创建并初始化一个 Agent 执行引擎（AgentEngine）
//
// 该函数负责将 Agent 的运行时配置（AgentConfig）、模型（LLM / Rerank）、工具体系（内置工具 + MCP 工具）、知识库信息、上下文管理器等组件，
// 组装成一个完整可执行的 Agent 引擎实例。
//
// 核心流程包括：
// 1. 校验 AgentConfig 和基础依赖（如 chatModel）。
// 2. 创建 ToolRegistry 并注册基础工具（检索、WebSearch 等）。
// 3. 根据租户配置和 Agent 策略注册 MCP 工具（支持 all / selected / none 模式）。
// 4. 加载知识库信息（KnowledgeBases）和用户指定文档（KnowledgeIDs），用于 Prompt 增强。
// 5. 构建系统提示词（System Prompt），支持自定义 Agent Prompt。
// 6. 初始化 AgentEngine，并注入：模型、工具、EventBus、上下文管理器等。
// 7. （可选）如果配置了 Skills，则初始化沙箱环境，将技能系统注入以赋予 Agent 执行脚本的能力。
//
// 参数说明：
// - ctx: 上下文对象，用于控制请求生命周期及日志追踪。
// - config: Agent 运行时配置，包含工具、知识库、推理策略等。
// - chatModel: 聊天模型（LLM），用于生成回复。
// - rerankModel: 重排序模型（可为 nil），仅在启用知识库检索时使用。
// - eventBus: 事件总线，用于 Agent 执行过程中的流式事件分发（如 thought、tool_call、final_answer）。
// - contextManager: 上下文管理器，用于管理多轮对话历史和系统提示。
// - sessionID: 当前会话 ID，用于隔离上下文和事件流。
//
// 返回值：
// - interfaces.AgentEngine: 构建完成的 Agent 引擎实例，可用于执行推理。
// - error: 若配置校验、工具注册或初始化过程中发生错误，则返回对应错误。

// CreateAgentEngineWithEventBus creates an agent engine with the given configuration and EventBus
func (s *agentService) CreateAgentEngine(
	ctx context.Context,
	config *types.AgentConfig,
	chatModel chat.Chat,
	rerankModel rerank.Reranker,
	eventBus *event.EventBus,
	contextManager interfaces.ContextManager,
	sessionID string,
) (interfaces.AgentEngine, error) {
	logger.Infof(ctx, "Creating agent engine with custom EventBus")

	// Validate config
	if err := s.ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid agent config: %w", err)
	}

	if chatModel == nil {
		return nil, fmt.Errorf("chat model is nil after initialization")
	}

	// Note: rerankModel can be nil when no knowledge bases are configured
	// The registerTools function will filter out knowledge-related tools in this case

	// Create tool registry
	toolRegistry := tools.NewToolRegistry()

	// Register tools
	if err := s.registerTools(ctx, toolRegistry, config, rerankModel, chatModel, sessionID); err != nil {
		return nil, fmt.Errorf("failed to register tools: %w", err)
	}

	// Register MCP tools from enabled services for this tenant
	tenantID := uint64(0)
	if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		tenantID = tid
	}
	if tenantID > 0 && s.mcpServiceService != nil && s.mcpManager != nil {
		// Check MCP selection mode from agent config
		mcpMode := config.MCPSelectionMode
		if mcpMode == "" {
			mcpMode = "all" // Default to all enabled MCP services
		}

		// Skip MCP registration if mode is "none"
		if mcpMode == "none" {
			logger.Infof(ctx, "MCP services disabled by agent config (mode: none)")
		} else {
			var mcpServices []*types.MCPService
			var err error

			if mcpMode == "selected" && len(config.MCPServices) > 0 {
				// Get only selected MCP services
				mcpServices, err = s.mcpServiceService.ListMCPServicesByIDs(ctx, tenantID, config.MCPServices)
				if err != nil {
					logger.Warnf(ctx, "Failed to list selected MCP services: %v", err)
				} else {
					logger.Infof(ctx, "Using %d selected MCP services from agent config", len(mcpServices))
				}
			} else {
				// Get all MCP services for this tenant
				mcpServices, err = s.mcpServiceService.ListMCPServices(ctx, tenantID)
				if err != nil {
					logger.Warnf(ctx, "Failed to list MCP services: %v", err)
				}
			}

			if err == nil && len(mcpServices) > 0 {
				// Filter enabled services
				enabledServices := make([]*types.MCPService, 0)
				for _, svc := range mcpServices {
					if svc != nil && svc.Enabled {
						enabledServices = append(enabledServices, svc)
					}
				}

				// Register MCP tools
				if len(enabledServices) > 0 {
					if err := tools.RegisterMCPTools(ctx, toolRegistry, enabledServices, s.mcpManager); err != nil {
						logger.Warnf(ctx, "Failed to register MCP tools: %v", err)
					} else {
						logger.Infof(ctx, "Registered MCP tools from %d enabled services", len(enabledServices))
					}
				}
			}
		}
	}

	// Get knowledge base detailed information for prompt
	kbInfos, err := s.getKnowledgeBaseInfos(ctx, config.KnowledgeBases)
	if err != nil {
		logger.Warnf(ctx, "Failed to get knowledge base details, using IDs only: %v", err)
		// Create fallback info with IDs only
		kbInfos = make([]*agent.KnowledgeBaseInfo, 0, len(config.KnowledgeBases))
		for _, kbID := range config.KnowledgeBases {
			kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
				ID:          kbID,
				Name:        kbID, // Use ID as name when details unavailable
				Description: "",
				DocCount:    0,
			})
		}
	}

	// Get selected documents information (user @ mentioned documents)
	selectedDocs, err := s.getSelectedDocumentInfos(ctx, config.KnowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to get selected document details: %v", err)
		selectedDocs = []*agent.SelectedDocumentInfo{}
	}

	systemPromptTemplate := ""
	if config.UseCustomSystemPrompt {
		systemPromptTemplate = config.ResolveSystemPrompt(config.WebSearchEnabled)
	}

	// Create engine with provided EventBus and contextManager
	engine := agent.NewAgentEngine(
		config,
		chatModel,
		toolRegistry,
		eventBus,
		kbInfos,
		selectedDocs,
		contextManager,
		sessionID,
		systemPromptTemplate,
	)

	// Initialize skills manager if skills are enabled
	if config.SkillsEnabled && len(config.SkillDirs) > 0 {
		skillsManager, err := s.initializeSkillsManager(ctx, config, toolRegistry)
		if err != nil {
			logger.Warnf(ctx, "Failed to initialize skills manager: %v", err)
		} else if skillsManager != nil {
			engine.SetSkillsManager(skillsManager)
			logger.Infof(ctx, "Skills manager initialized with %d skills", len(skillsManager.GetAllMetadata()))
		}
	}

	return engine, nil
}

// WEKNORA_SANDBOX_MODE: 决定技能脚本在哪里运行。
//	- docker: 最安全，在隔离的容器中运行。如果 Docker 初始化失败，代码包含一个自动降级机制（Fallback），会切换到禁用状态。
//	- local: 在本地进程运行（风险较高，通常用于开发）。
//	- disabled: 默认值，不提供执行环境。

// initializeSkillsManager creates and initializes the skills manager
func (s *agentService) initializeSkillsManager(
	ctx context.Context,
	config *types.AgentConfig,
	toolRegistry *tools.ToolRegistry,
) (*skills.Manager, error) {
	// Initialize sandbox manager based on environment variables
	// WEKNORA_SANDBOX_MODE: "docker", "local", "disabled" (default: "disabled")
	// WEKNORA_SANDBOX_TIMEOUT: timeout in seconds (default: 60)
	// WEKNORA_SANDBOX_DOCKER_IMAGE: custom Docker image (default: wechatopenai/weknora-sandbox:latest)
	var sandboxMgr sandbox.Manager
	var err error

	sandboxMode := os.Getenv("WEKNORA_SANDBOX_MODE")
	if sandboxMode == "" {
		sandboxMode = "disabled"
	}
	dockerImage := os.Getenv("WEKNORA_SANDBOX_DOCKER_IMAGE")
	if dockerImage == "" {
		dockerImage = sandbox.DefaultDockerImage
	}
	sandboxTimeoutStr := os.Getenv("WEKNORA_SANDBOX_TIMEOUT")
	sandboxTimeout := 60
	if sandboxTimeoutStr != "" {
		if v, err := strconv.Atoi(sandboxTimeoutStr); err == nil && v > 0 {
			sandboxTimeout = v
		}
	}

	switch sandboxMode {
	case "docker":
		sandboxMgr, err = sandbox.NewManagerFromType("docker", true, dockerImage) // Enable fallback to local
		if err != nil {
			logger.Warnf(ctx, "Failed to initialize Docker sandbox, falling back to disabled: %v", err)
			sandboxMgr = sandbox.NewDisabledManager()
		}
	case "local":
		sandboxMgr, err = sandbox.NewManagerFromType("local", false, "")
		if err != nil {
			logger.Warnf(ctx, "Failed to initialize local sandbox: %v", err)
			sandboxMgr = sandbox.NewDisabledManager()
		}
	default:
		sandboxMgr = sandbox.NewDisabledManager()
	}
	logger.Infof(ctx, "Sandbox configured: mode=%s, timeout=%ds, image=%s", sandboxMode, sandboxTimeout, dockerImage)

	// Create skills manager
	skillsConfig := &skills.ManagerConfig{
		SkillDirs:     config.SkillDirs,
		AllowedSkills: config.AllowedSkills,
		Enabled:       config.SkillsEnabled,
	}

	skillsManager := skills.NewManager(skillsConfig, sandboxMgr)

	// Initialize (discover skills)
	if err := skillsManager.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize skills: %w", err)
	}

	// Register skills tools
	readSkillTool := tools.NewReadSkillTool(skillsManager)
	toolRegistry.RegisterTool(readSkillTool)
	logger.Infof(ctx, "Registered read_skill tool")

	if sandboxMode != "disabled" {
		executeSkillTool := tools.NewExecuteSkillScriptTool(skillsManager)
		toolRegistry.RegisterTool(executeSkillTool)
		logger.Infof(ctx, "Registered execute_skill_script tool")
	}

	return skillsManager, nil
}

// registerTools 注册工具到注册表
//
// 功能说明:
//   - 确定允许注册的工具列表（使用配置指定或默认列表）
//   - 根据知识库配置过滤相关工具（无知识库时禁用知识类工具）
//   - 根据 Web 搜索配置启用网络搜索工具
//   - 实例化每个工具并注册到工具注册表
//
// 参数:
//   - ctx: 上下文，用于日志记录和追踪
//   - registry: 工具注册表，用于存储和管理工具实例
//   - config: 代理配置，包含允许的工具列表、知识库配置、Web 搜索开关等
//   - rerankModel: 重排序模型，用于知识检索结果重排序
//   - chatModel: 聊天模型，用于 Web 内容获取等场景
//   - sessionID: 当前会话 ID，用于工具实例的会话隔离
//
// 返回值:
//   - error: 注册过程中的错误，当前实现始终返回 nil
//
// 工具过滤逻辑:
//   - 自定义工具列表: 优先使用 config.AllowedTools 指定的工具
//   - 默认工具列表: 未指定时使用 tools.DefaultAllowedTools()
//   - 知识库工具过滤: 无知识库配置时，移除以下工具:
//     - knowledge_search, grep_chunks, list_knowledge_chunks
//     - query_knowledge_graph, get_document_info
//     - database_query, data_analysis, data_schema
//   - 纯聊天模式: 无知识库且无 Web 搜索时，额外禁用 todo_write
//   - Web 搜索工具: 启用时添加 web_search 和 web_fetch
//
// 工具实例化:
//   - ToolThinking: 顺序思考工具，支持结构化推理
//   - ToolTodoWrite: 待办事项管理工具
//   - ToolKnowledgeSearch: 知识检索工具（需重排序模型和聊天模型）
//   - ToolGrepChunks: 文本分块搜索工具
//   - ToolListKnowledgeChunks: 知识分块列表工具
//   - ToolQueryKnowledgeGraph: 知识图谱查询工具
//   - ToolGetDocumentInfo: 文档信息获取工具
//   - ToolDatabaseQuery: 数据库查询工具
//   - ToolWebSearch: Web 搜索工具（需会话 ID 和最大结果数配置）
//   - ToolWebFetch: Web 内容获取工具（需聊天模型）
//   - ToolDataAnalysis: 数据分析工具（需 DuckDB 和会话 ID）
//   - ToolDataSchema: 数据 Schema 工具
//
// 日志记录:
//   - Info: 使用的工具列表、纯代理模式提示、各工具注册信息、注册总数
//   - Warn: 工具名称不匹配、未知工具类型
//
// 注意事项:
//   - 工具名称需与注册名一致，不匹配时记录警告但不中断
//   - 知识库工具依赖多个服务（knowledgeBaseService, knowledgeService, chunkService 等）
//   - Web 搜索工具需要额外的状态服务（webSearchStateService）
//   - 数据库查询工具直接使用数据库连接（s.db）

// registerTools registers tools based on the agent configuration
func (s *agentService) registerTools(
	ctx context.Context,
	registry *tools.ToolRegistry,
	config *types.AgentConfig,
	rerankModel rerank.Reranker,
	chatModel chat.Chat,
	sessionID string,
) error {
	// Use config's allowed tools if specified, otherwise use defaults
	var allowedTools []string
	if len(config.AllowedTools) > 0 {
		allowedTools = make([]string, len(config.AllowedTools))
		copy(allowedTools, config.AllowedTools)
		logger.Infof(ctx, "Using custom allowed tools from config: %v", allowedTools)
	} else {
		allowedTools = tools.DefaultAllowedTools()
		logger.Infof(ctx, "Using default allowed tools: %v", allowedTools)
	}

	// Filter out knowledge base tools if no knowledge bases or knowledge IDs are configured
	hasKnowledge := len(config.KnowledgeBases) > 0 || len(config.KnowledgeIDs) > 0
	if !hasKnowledge {
		filteredTools := make([]string, 0)
		kbTools := map[string]bool{
			tools.ToolKnowledgeSearch:     true,
			tools.ToolGrepChunks:          true,
			tools.ToolListKnowledgeChunks: true,
			tools.ToolQueryKnowledgeGraph: true,
			tools.ToolGetDocumentInfo:     true,
			tools.ToolDatabaseQuery:       true,
			tools.ToolDataAnalysis:        true,
			tools.ToolDataSchema:          true,
		}

		// If no knowledge and no web search, also disable todo_write (not useful for simple chat)
		if !config.WebSearchEnabled {
			kbTools[tools.ToolTodoWrite] = true
		}

		for _, toolName := range allowedTools {
			if !kbTools[toolName] {
				filteredTools = append(filteredTools, toolName)
			}
		}
		allowedTools = filteredTools
		logger.Infof(ctx, "Pure Agent Mode: Knowledge base tools filtered out, remaining: %v", allowedTools)
	}

	// If web search is enabled, add web_search to allowedTools
	if config.WebSearchEnabled {
		allowedTools = append(allowedTools, tools.ToolWebSearch)
		allowedTools = append(allowedTools, tools.ToolWebFetch)
	}
	logger.Infof(ctx, "Registering tools: %v, webSearchEnabled: %v", allowedTools, config.WebSearchEnabled)

	// Register each allowed tool
	for _, toolName := range allowedTools {
		var toolToRegister types.Tool

		switch toolName {
		case tools.ToolThinking:
			toolToRegister = tools.NewSequentialThinkingTool()
		case tools.ToolTodoWrite:
			toolToRegister = tools.NewTodoWriteTool()
		case tools.ToolKnowledgeSearch:
			toolToRegister = tools.NewKnowledgeSearchTool(
				s.knowledgeBaseService,
				s.knowledgeService,
				s.chunkService,
				config.SearchTargets,
				rerankModel,
				chatModel,
				s.cfg,
			)
		case tools.ToolGrepChunks:
			toolToRegister = tools.NewGrepChunksTool(s.db, config.SearchTargets)
			logger.Infof(ctx, "Registered grep_chunks tool with searchTargets: %d targets", len(config.SearchTargets))
		case tools.ToolListKnowledgeChunks:
			toolToRegister = tools.NewListKnowledgeChunksTool(s.knowledgeService, s.chunkService, config.SearchTargets)
		case tools.ToolQueryKnowledgeGraph:
			toolToRegister = tools.NewQueryKnowledgeGraphTool(s.knowledgeBaseService)
		case tools.ToolGetDocumentInfo:
			toolToRegister = tools.NewGetDocumentInfoTool(s.knowledgeService, s.chunkService, config.SearchTargets)
		case tools.ToolDatabaseQuery:
			toolToRegister = tools.NewDatabaseQueryTool(s.db)
		case tools.ToolWebSearch:
			toolToRegister = tools.NewWebSearchTool(
				s.webSearchService,
				s.knowledgeBaseService,
				s.knowledgeService,
				s.webSearchStateService,
				sessionID,
				config.WebSearchMaxResults,
			)
			logger.Infof(ctx, "Registered web_search tool for session: %s, maxResults: %d", sessionID, config.WebSearchMaxResults)

		case tools.ToolWebFetch:
			toolToRegister = tools.NewWebFetchTool(chatModel)
			logger.Infof(ctx, "Registered web_fetch tool for session: %s", sessionID)

		case tools.ToolDataAnalysis:
			toolToRegister = tools.NewDataAnalysisTool(s.knowledgeService, s.fileService, s.duckdb, sessionID)
			logger.Infof(ctx, "Registered data_analysis tool for session: %s", sessionID)

		case tools.ToolDataSchema:
			toolToRegister = tools.NewDataSchemaTool(s.knowledgeService, s.chunkService.GetRepository())
			logger.Infof(ctx, "Registered data_schema tool")

		default:
			logger.Warnf(ctx, "Unknown tool: %s", toolName)
		}

		if toolToRegister != nil {
			if toolToRegister.Name() != toolName {
				logger.Warnf(ctx, "Tool name mismatch: expected %s, got %s", toolName, toolToRegister.Name())
			}
			registry.RegisterTool(toolToRegister)
		}
	}

	logger.Infof(ctx, "Registered %d tools", len(registry.ListTools()))
	return nil
}

// ValidateConfig validates the agent configuration
func (s *agentService) ValidateConfig(config *types.AgentConfig) error {
	if config == nil {
		return fmt.Errorf("config cannot be nil")
	}

	if config.MaxIterations <= 0 {
		config.MaxIterations = 5 // Default
	}

	if config.MaxIterations > MAX_ITERATIONS {
		return fmt.Errorf("max iterations too high: %d (max %d)", config.MaxIterations, MAX_ITERATIONS)
	}

	return nil
}

// getKnowledgeBaseInfos retrieves detailed information for knowledge bases
func (s *agentService) getKnowledgeBaseInfos(ctx context.Context, kbIDs []string) ([]*agent.KnowledgeBaseInfo, error) {
	if len(kbIDs) == 0 {
		return []*agent.KnowledgeBaseInfo{}, nil
	}

	kbInfos := make([]*agent.KnowledgeBaseInfo, 0, len(kbIDs))

	for _, kbID := range kbIDs {
		// Get knowledge base details
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge base %s: %v", secutils.SanitizeForLog(kbID), err)
			// Add fallback info
			kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
				ID:          kbID,
				Name:        kbID,
				Type:        "document", // Default type
				Description: "",
				DocCount:    0,
				RecentDocs:  []agent.RecentDocInfo{},
			})
			continue
		}

		// Get document count and recent documents
		docCount := 0
		recentDocs := []agent.RecentDocInfo{}

		if kb.Type == types.KnowledgeBaseTypeFAQ {
			pageResult, err := s.knowledgeService.ListFAQEntries(ctx, kbID, &types.Pagination{
				Page:     1,
				PageSize: 10,
			}, 0, "", "", "")
			if err == nil && pageResult != nil {
				docCount = int(pageResult.Total)
				if entries, ok := pageResult.Data.([]*types.FAQEntry); ok {
					for _, entry := range entries {
						if len(recentDocs) >= 10 {
							break
						}
						recentDocs = append(recentDocs, agent.RecentDocInfo{
							ChunkID:             entry.ChunkID,
							KnowledgeID:         entry.KnowledgeID,
							KnowledgeBaseID:     entry.KnowledgeBaseID,
							Title:               entry.StandardQuestion,
							Type:                string(types.ChunkTypeFAQ),
							CreatedAt:           entry.CreatedAt.Format("2006-01-02"),
							FAQStandardQuestion: entry.StandardQuestion,
							FAQSimilarQuestions: entry.SimilarQuestions,
							FAQAnswers:          entry.Answers,
						})
					}
				}
			} else if err != nil {
				logger.Warnf(ctx, "Failed to list FAQ entries for %s: %v", kbID, err)
			}
		}

		// Fallback to generic knowledge listing when not FAQ or FAQ retrieval failed
		if kb.Type != types.KnowledgeBaseTypeFAQ || len(recentDocs) == 0 {
			pageResult, err := s.knowledgeService.ListPagedKnowledgeByKnowledgeBaseID(ctx, kbID, &types.Pagination{
				Page:     1,
				PageSize: 10,
			}, "", "", "")

			if err == nil && pageResult != nil {
				docCount = int(pageResult.Total)

				// Convert to Knowledge slice
				if knowledges, ok := pageResult.Data.([]*types.Knowledge); ok {
					for _, k := range knowledges {
						if len(recentDocs) >= 10 {
							break
						}
						recentDocs = append(recentDocs, agent.RecentDocInfo{
							KnowledgeID: k.ID,
							Title:       k.Title,
							Description: k.Description,
							FileName:    k.FileName,
							Type:        k.FileType,
							CreatedAt:   k.CreatedAt.Format("2006-01-02"),
							FileSize:    k.FileSize,
						})
					}
				}
			}
		}

		kbType := kb.Type
		if kbType == "" {
			kbType = "document" // Default type
		}
		kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
			ID:          kb.ID,
			Name:        kb.Name,
			Type:        kbType,
			Description: kb.Description,
			DocCount:    docCount,
			RecentDocs:  recentDocs,
		})
	}

	return kbInfos, nil
}

// getSelectedDocumentInfos retrieves detailed information for user-selected documents (via @ mention)
// This loads the actual content of the documents to include in the system prompt
func (s *agentService) getSelectedDocumentInfos(ctx context.Context, knowledgeIDs []string) ([]*agent.SelectedDocumentInfo, error) {
	if len(knowledgeIDs) == 0 {
		return []*agent.SelectedDocumentInfo{}, nil
	}

	// Get tenant ID from context
	tenantID := uint64(0)
	if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		tenantID = tid
	}

	// Fetch knowledge metadata (include docs from shared KBs the user has access to)
	knowledges, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get knowledge batch: %w", err)
	}

	// Build map for quick lookup
	knowledgeMap := make(map[string]*types.Knowledge)
	for _, k := range knowledges {
		if k != nil {
			knowledgeMap[k.ID] = k
		}
	}

	selectedDocs := make([]*agent.SelectedDocumentInfo, 0, len(knowledgeIDs))

	for _, kid := range knowledgeIDs {
		k, ok := knowledgeMap[kid]
		if !ok {
			logger.Warnf(ctx, "Selected knowledge %s not found", kid)
			continue
		}

		docInfo := &agent.SelectedDocumentInfo{
			KnowledgeID:     k.ID,
			KnowledgeBaseID: k.KnowledgeBaseID,
			Title:           k.Title,
			FileName:        k.FileName,
			FileType:        k.FileType,
		}

		selectedDocs = append(selectedDocs, docInfo)
	}

	logger.Infof(ctx, "Loaded %d selected documents metadata for prompt", len(selectedDocs))
	return selectedDocs, nil
}
