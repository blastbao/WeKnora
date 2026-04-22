package router

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types/interfaces"

	_ "github.com/Tencent/WeKnora/docs" // swagger docs
)

// RouterParams 路由参数
type RouterParams struct {
	dig.In

	Config                *config.Config
	UserService           interfaces.UserService
	KBService             interfaces.KnowledgeBaseService
	KnowledgeService      interfaces.KnowledgeService
	ChunkService          interfaces.ChunkService
	SessionService        interfaces.SessionService
	MessageService        interfaces.MessageService
	ModelService          interfaces.ModelService
	EvaluationService     interfaces.EvaluationService
	KBHandler             *handler.KnowledgeBaseHandler
	KnowledgeHandler      *handler.KnowledgeHandler
	TenantHandler         *handler.TenantHandler
	TenantService         interfaces.TenantService
	ChunkHandler          *handler.ChunkHandler
	SessionHandler        *session.Handler
	MessageHandler        *handler.MessageHandler
	ModelHandler          *handler.ModelHandler
	EvaluationHandler     *handler.EvaluationHandler
	AuthHandler           *handler.AuthHandler
	InitializationHandler *handler.InitializationHandler
	SystemHandler         *handler.SystemHandler
	MCPServiceHandler     *handler.MCPServiceHandler
	WebSearchHandler      *handler.WebSearchHandler
	FAQHandler            *handler.FAQHandler
	TagHandler            *handler.TagHandler
	CustomAgentHandler    *handler.CustomAgentHandler
	SkillHandler          *handler.SkillHandler
	OrganizationHandler   *handler.OrganizationHandler
}

// 中间件执行顺序
//
// 	请求进入
//	   ↓
//	1. CORS（跨域处理）
//	   ↓
//	2. RequestID（生成请求ID）
//	   ↓
//	3. Logger（记录日志）
//	   ↓
//	4. Recovery（异常恢复）
//	   ↓
//	5. ErrorHandler（错误处理）
//	   ↓
//	6. Auth（JWT认证）
//	   ↓
//	7. Tracing（链路追踪）
//	   ↓
//	业务处理

// NewRouter 创建新的路由
func NewRouter(params RouterParams) *gin.Engine {
	r := gin.New()

	// CORS 中间件应放在最前面
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-API-Key", "X-Request-ID"},
		ExposeHeaders:    []string{"Content-Length", "Access-Control-Allow-Origin"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// 基础中间件（不需要认证）
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.ErrorHandler())

	// 健康检查（不需要认证）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Swagger API 文档（仅在非生产环境下启用）
	// 通过 GIN_MODE 环境变量判断：release 模式下禁用 Swagger
	if gin.Mode() != gin.ReleaseMode {
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler,
			ginSwagger.DefaultModelsExpandDepth(-1), // 默认折叠 Models
			ginSwagger.DocExpansion("list"),         // 展开模式: "list"(展开标签), "full"(全部展开), "none"(全部折叠)
			ginSwagger.DeepLinking(true),            // 启用深度链接
			ginSwagger.PersistAuthorization(true),   // 持久化认证信息
		))
	}

	// 认证中间件
	r.Use(middleware.Auth(params.TenantService, params.UserService, params.Config))

	// 添加OpenTelemetry追踪中间件
	r.Use(middleware.TracingMiddleware())

	// 需要认证的API路由
	v1 := r.Group("/api/v1")
	{
		RegisterAuthRoutes(v1, params.AuthHandler)
		RegisterTenantRoutes(v1, params.TenantHandler)
		RegisterKnowledgeBaseRoutes(v1, params.KBHandler)
		RegisterKnowledgeTagRoutes(v1, params.TagHandler)
		RegisterKnowledgeRoutes(v1, params.KnowledgeHandler)
		RegisterFAQRoutes(v1, params.FAQHandler)
		RegisterChunkRoutes(v1, params.ChunkHandler)
		RegisterSessionRoutes(v1, params.SessionHandler)
		RegisterChatRoutes(v1, params.SessionHandler)
		RegisterMessageRoutes(v1, params.MessageHandler)
		RegisterModelRoutes(v1, params.ModelHandler)
		RegisterEvaluationRoutes(v1, params.EvaluationHandler)
		RegisterInitializationRoutes(v1, params.InitializationHandler)
		RegisterSystemRoutes(v1, params.SystemHandler)
		RegisterMCPServiceRoutes(v1, params.MCPServiceHandler)
		RegisterWebSearchRoutes(v1, params.WebSearchHandler)
		RegisterCustomAgentRoutes(v1, params.CustomAgentHandler)
		RegisterSkillRoutes(v1, params.SkillHandler)
		RegisterOrganizationRoutes(v1, params.OrganizationHandler)
	}

	return r
}

// RegisterChunkRoutes 注册分块相关的路由
//
// 路由列表：
//   GET    /chunks/:knowledge_id                    # 获取分块列表
//   GET    /chunks/by-id/:id                        # 通过ID获取单个分块
//   DELETE /chunks/:knowledge_id/:id                # 删除分块
//   DELETE /chunks/:knowledge_id                    # 删除知识下所有分块
//   PUT    /chunks/:knowledge_id/:id                # 更新分块信息
//   DELETE /chunks/by-id/:id/questions              # 删除生成的问题

// RegisterChunkRoutes 注册分块相关的路由
func RegisterChunkRoutes(r *gin.RouterGroup, handler *handler.ChunkHandler) {
	// 分块路由组
	chunks := r.Group("/chunks")
	{
		// 获取分块列表
		chunks.GET("/:knowledge_id", handler.ListKnowledgeChunks)
		// 通过chunk_id获取单个chunk（不需要knowledge_id）
		chunks.GET("/by-id/:id", handler.GetChunkByIDOnly)
		// 删除分块
		chunks.DELETE("/:knowledge_id/:id", handler.DeleteChunk)
		// 删除知识下的所有分块
		chunks.DELETE("/:knowledge_id", handler.DeleteChunksByKnowledgeID)
		// 更新分块信息
		chunks.PUT("/:knowledge_id/:id", handler.UpdateChunk)
		// 删除单个生成的问题（通过问题ID）
		chunks.DELETE("/by-id/:id/questions", handler.DeleteGeneratedQuestion)
	}
}

// RegisterKnowledgeRoutes 注册知识相关的路由
//
// 路由列表：
//
//   # 知识库下的知识（嵌套资源）
//   POST   /knowledge-bases/:id/knowledge/file    # 从文件创建知识
//   POST   /knowledge-bases/:id/knowledge/url     # 从URL创建知识
//   POST   /knowledge-bases/:id/knowledge/manual  # 手工录入知识
//   GET    /knowledge-bases/:id/knowledge         # 获取知识列表
//
//   # 知识直接操作
//   GET    /knowledge/batch                       # 批量获取知识
//   GET    /knowledge/:id                         # 获取知识详情
//   PUT    /knowledge/:id                         # 更新知识
//   PUT    /knowledge/manual/:id                  # 更新手工知识
//   DELETE /knowledge/:id                         # 删除知识
//   POST   /knowledge/:id/reparse                 # 重新解析知识
//   GET    /knowledge/:id/download                # 下载知识文件
//   PUT    /knowledge/tags                        # 批量更新知识标签
//   GET    /knowledge/search                      # 搜索知识
//   PUT    /knowledge/image/:id/:chunk_id         # 更新图像分块信息
//

// RegisterKnowledgeRoutes 注册知识相关的路由
func RegisterKnowledgeRoutes(r *gin.RouterGroup, handler *handler.KnowledgeHandler) {
	// 知识库下的知识路由组
	kb := r.Group("/knowledge-bases/:id/knowledge")
	{
		// 从文件创建知识
		kb.POST("/file", handler.CreateKnowledgeFromFile)
		// 从URL创建知识（支持网页URL和文件URL，传 file_name/file_type 或 URL 含已知扩展名时自动切换为文件下载模式）
		kb.POST("/url", handler.CreateKnowledgeFromURL)
		// 手工 Markdown 录入
		kb.POST("/manual", handler.CreateManualKnowledge)
		// 获取知识库下的知识列表
		kb.GET("", handler.ListKnowledge)
	}

	// 知识路由组
	k := r.Group("/knowledge")
	{
		// 批量获取知识
		k.GET("/batch", handler.GetKnowledgeBatch)
		// 获取知识详情
		k.GET("/:id", handler.GetKnowledge)
		// 删除知识
		k.DELETE("/:id", handler.DeleteKnowledge)
		// 更新知识
		k.PUT("/:id", handler.UpdateKnowledge)
		// 更新手工 Markdown 知识
		k.PUT("/manual/:id", handler.UpdateManualKnowledge)
		// 重新解析知识
		k.POST("/:id/reparse", handler.ReparseKnowledge)
		// 获取知识文件
		k.GET("/:id/download", handler.DownloadKnowledgeFile)
		// 更新图像分块信息
		k.PUT("/image/:id/:chunk_id", handler.UpdateImageInfo)
		// 批量更新知识标签
		k.PUT("/tags", handler.UpdateKnowledgeTagBatch)
		// 搜索知识
		k.GET("/search", handler.SearchKnowledge)
	}
}

// RegisterFAQRoutes 注册 FAQ 相关路由
//
// 路由列表：
//   # 条目管理
//   GET    /knowledge-bases/:id/faq/entries                    			# 列表
//   GET    /knowledge-bases/:id/faq/entries/export             			# 导出
//   GET    /knowledge-bases/:id/faq/entries/:entry_id          			# 详情
//   POST   /knowledge-bases/:id/faq/entries                    			# 批量创建
//   POST   /knowledge-bases/:id/faq/entry                      			# 单个创建
//   PUT    /knowledge-bases/:id/faq/entries/:entry_id          			# 更新
//   POST   /knowledge-bases/:id/faq/entries/:entry_id/similar-questions  	# 添加相似问题
//   PUT    /knowledge-bases/:id/faq/entries/fields             			# 批量更新字段
//   PUT    /knowledge-bases/:id/faq/entries/tags               			# 批量更新标签
//   DELETE /knowledge-bases/:id/faq/entries                    			# 批量删除
//
//   # 搜索和导入
//   POST   /knowledge-bases/:id/faq/search                     # 搜索FAQ
//   PUT    /knowledge-bases/:id/faq/import/last-result/display # 更新导入结果显示状态
//   GET    /faq/import/progress/:task_id                       # 获取导入进度

// RegisterFAQRoutes 注册 FAQ 相关路由
func RegisterFAQRoutes(r *gin.RouterGroup, handler *handler.FAQHandler) {
	if handler == nil {
		return
	}
	faq := r.Group("/knowledge-bases/:id/faq")
	{
		faq.GET("/entries", handler.ListEntries)
		faq.GET("/entries/export", handler.ExportEntries)
		faq.GET("/entries/:entry_id", handler.GetEntry)
		faq.POST("/entries", handler.UpsertEntries)
		faq.POST("/entry", handler.CreateEntry)
		faq.PUT("/entries/:entry_id", handler.UpdateEntry)
		faq.POST("/entries/:entry_id/similar-questions", handler.AddSimilarQuestions)
		// Unified batch update API - supports is_enabled, is_recommended, tag_id
		faq.PUT("/entries/fields", handler.UpdateEntryFieldsBatch)
		faq.PUT("/entries/tags", handler.UpdateEntryTagBatch)
		faq.DELETE("/entries", handler.DeleteEntries)
		faq.POST("/search", handler.SearchFAQ)
		// FAQ import result display status
		faq.PUT("/import/last-result/display", handler.UpdateLastImportResultDisplayStatus)
	}
	// FAQ import progress route (outside of knowledge-base scope)
	faqImport := r.Group("/faq/import")
	{
		faqImport.GET("/progress/:task_id", handler.GetImportProgress)
	}
}

// RegisterKnowledgeBaseRoutes 注册知识库相关的路由
//
// 路由列表：
//   POST   /knowledge-bases                    		# 创建知识库
//   GET    /knowledge-bases                    		# 获取知识库列表
//   GET    /knowledge-bases/:id                		# 获取知识库详情
//   PUT    /knowledge-bases/:id                		# 更新知识库
//   DELETE /knowledge-bases/:id                		# 删除知识库
//   GET    /knowledge-bases/:id/hybrid-search  		# 混合搜索
//   POST   /knowledge-bases/copy               		# 复制知识库
//   GET    /knowledge-bases/copy/progress/:task_id  	# 获取复制进度

// RegisterKnowledgeBaseRoutes 注册知识库相关的路由
func RegisterKnowledgeBaseRoutes(r *gin.RouterGroup, handler *handler.KnowledgeBaseHandler) {
	// 知识库路由组
	kb := r.Group("/knowledge-bases")
	{
		// 创建知识库
		kb.POST("", handler.CreateKnowledgeBase)
		// 获取知识库列表
		kb.GET("", handler.ListKnowledgeBases)
		// 获取知识库详情
		kb.GET("/:id", handler.GetKnowledgeBase)
		// 更新知识库
		kb.PUT("/:id", handler.UpdateKnowledgeBase)
		// 删除知识库
		kb.DELETE("/:id", handler.DeleteKnowledgeBase)
		// 混合搜索
		kb.GET("/:id/hybrid-search", handler.HybridSearch)
		// 拷贝知识库
		kb.POST("/copy", handler.CopyKnowledgeBase)
		// 获取知识库复制进度
		kb.GET("/copy/progress/:task_id", handler.GetKBCloneProgress)
	}
}

// RegisterKnowledgeTagRoutes 注册知识库标签相关的路由
//
// 路由列表：
//   GET    /knowledge-bases/:id/tags        	# 获取标签列表
//   POST   /knowledge-bases/:id/tags        	# 创建标签
//   PUT    /knowledge-bases/:id/tags/:tag_id  	# 更新标签
//   DELETE /knowledge-bases/:id/tags/:tag_id  	# 删除标签

// RegisterKnowledgeTagRoutes 注册知识库标签相关路由
func RegisterKnowledgeTagRoutes(r *gin.RouterGroup, tagHandler *handler.TagHandler) {
	if tagHandler == nil {
		return
	}
	kbTags := r.Group("/knowledge-bases/:id/tags")
	{
		kbTags.GET("", tagHandler.ListTags)
		kbTags.POST("", tagHandler.CreateTag)
		kbTags.PUT("/:tag_id", tagHandler.UpdateTag)
		kbTags.DELETE("/:tag_id", tagHandler.DeleteTag)
	}
}

// RegisterMessageRoutes 注册消息相关的路由
//
// 路由列表：
//   GET    /messages/:session_id/load    # 加载更早的消息（向上滚动）
//   DELETE /messages/:session_id/:id     # 删除消息

// RegisterMessageRoutes 注册消息相关的路由
func RegisterMessageRoutes(r *gin.RouterGroup, handler *handler.MessageHandler) {
	// 消息路由组
	messages := r.Group("/messages")
	{
		// 加载更早的消息，用于向上滚动加载
		messages.GET("/:session_id/load", handler.LoadMessages)
		// 删除消息
		messages.DELETE("/:session_id/:id", handler.DeleteMessage)
	}
}

// RegisterSessionRoutes 注册会话相关的路由
//
// 路由列表：
//   POST   /sessions                                 # 创建会话
//   GET    /sessions                                 # 获取租户会话列表
//   GET    /sessions/:id                             # 获取会话详情
//   PUT    /sessions/:id                             # 更新会话
//   DELETE /sessions/:id                             # 删除会话
//   DELETE /sessions/batch                           # 批量删除会话
//   POST   /sessions/:session_id/generate_title      # 生成会话标题
//   POST   /sessions/:session_id/stop                # 停止会话
//   GET    /sessions/continue-stream/:session_id     # 继续接收流式响应

// RegisterSessionRoutes 注册路由
func RegisterSessionRoutes(r *gin.RouterGroup, handler *session.Handler) {
	sessions := r.Group("/sessions")
	{
		sessions.POST("", handler.CreateSession)
		sessions.DELETE("/batch", handler.BatchDeleteSessions)
		sessions.GET("/:id", handler.GetSession)
		sessions.GET("", handler.GetSessionsByTenant)
		sessions.PUT("/:id", handler.UpdateSession)
		sessions.DELETE("/:id", handler.DeleteSession)
		sessions.POST("/:session_id/generate_title", handler.GenerateTitle)
		sessions.POST("/:session_id/stop", handler.StopSession)
		// 继续接收活跃流
		sessions.GET("/continue-stream/:session_id", handler.ContinueStream)
	}
}

// RegisterChatRoutes 注册聊天相关的路由
//
// 路由列表：
//   # 知识问答
//   POST /knowledge-chat/:session_id    # 基于知识库的问答
//
//   # Agent问答
//   POST /agent-chat/:session_id        # 基于Agent的问答
//
//   # 知识检索（无需session）
//   POST /knowledge-search              # 知识检索接口

// RegisterChatRoutes 注册路由
func RegisterChatRoutes(r *gin.RouterGroup, handler *session.Handler) {
	knowledgeChat := r.Group("/knowledge-chat")
	{
		knowledgeChat.POST("/:session_id", handler.KnowledgeQA)
	}

	// Agent-based chat
	agentChat := r.Group("/agent-chat")
	{
		agentChat.POST("/:session_id", handler.AgentQA)
	}

	// 新增知识检索接口，不需要session_id
	knowledgeSearch := r.Group("/knowledge-search")
	{
		knowledgeSearch.POST("", handler.SearchKnowledge)
	}
}

// RegisterTenantRoutes 注册租户相关的路由
//
// 路由列表：
//   # 租户管理
//   POST   /tenants           # 创建租户
//   GET    /tenants           # 获取租户列表
//   GET    /tenants/:id       # 获取租户详情
//   PUT    /tenants/:id       # 更新租户
//   DELETE /tenants/:id       # 删除租户
//
//   # 跨租户操作（需要特殊权限）
//   GET    /tenants/all       # 获取所有租户
//   GET    /tenants/search    # 搜索租户
//
//   # KV配置管理（租户级）
//   GET    /tenants/kv/:key   # 获取KV配置
//   PUT    /tenants/kv/:key   # 更新KV配置

// RegisterTenantRoutes 注册租户相关的路由
func RegisterTenantRoutes(r *gin.RouterGroup, handler *handler.TenantHandler) {
	// 添加获取所有租户的路由（需要跨租户权限）
	r.GET("/tenants/all", handler.ListAllTenants)
	// 添加搜索租户的路由（需要跨租户权限，支持分页和搜索）
	r.GET("/tenants/search", handler.SearchTenants)
	// 租户路由组
	tenantRoutes := r.Group("/tenants")
	{
		tenantRoutes.POST("", handler.CreateTenant)
		tenantRoutes.GET("/:id", handler.GetTenant)
		tenantRoutes.PUT("/:id", handler.UpdateTenant)
		tenantRoutes.DELETE("/:id", handler.DeleteTenant)
		tenantRoutes.GET("", handler.ListTenants)

		// Generic KV configuration management (tenant-level)
		// Tenant ID is obtained from authentication context
		tenantRoutes.GET("/kv/:key", handler.GetTenantKV)
		tenantRoutes.PUT("/kv/:key", handler.UpdateTenantKV)
	}
}

// RegisterModelRoutes 注册模型相关的路由
//
// 路由列表：
//   GET    /models/providers   # 获取模型厂商列表
//   POST   /models             # 创建模型
//   GET    /models             # 获取模型列表
//   GET    /models/:id         # 获取单个模型
//   PUT    /models/:id         # 更新模型
//   DELETE /models/:id         # 删除模型

// RegisterModelRoutes 注册模型相关的路由
func RegisterModelRoutes(r *gin.RouterGroup, handler *handler.ModelHandler) {
	// 模型路由组
	models := r.Group("/models")
	{
		// 获取模型厂商列表
		models.GET("/providers", handler.ListModelProviders)
		// 创建模型
		models.POST("", handler.CreateModel)
		// 获取模型列表
		models.GET("", handler.ListModels)
		// 获取单个模型
		models.GET("/:id", handler.GetModel)
		// 更新模型
		models.PUT("/:id", handler.UpdateModel)
		// 删除模型
		models.DELETE("/:id", handler.DeleteModel)
	}
}

// RegisterEvaluationRoutes 注册评估相关的路由
//
// 路由列表：
//
//	POST /evaluation   # 执行评估任务
//	GET  /evaluation   # 获取评估结果

func RegisterEvaluationRoutes(r *gin.RouterGroup, handler *handler.EvaluationHandler) {
	evaluationRoutes := r.Group("/evaluation")
	{
		evaluationRoutes.POST("/", handler.Evaluation)
		evaluationRoutes.GET("/", handler.GetEvaluationResult)
	}
}

// RegisterAuthRoutes 注册认证相关的路由
//
// 路由列表：
//   POST /auth/register       	# 用户注册
//   POST /auth/login          	# 用户登录
//   POST /auth/refresh        	# 刷新访问令牌
//   GET  /auth/validate       	# 验证令牌有效性
//   POST /auth/logout         	# 用户登出
//   GET  /auth/me             	# 获取当前用户信息
//   POST /auth/change-password # 修改密码

// RegisterAuthRoutes registers authentication routes
func RegisterAuthRoutes(r *gin.RouterGroup, handler *handler.AuthHandler) {
	r.POST("/auth/register", handler.Register)
	r.POST("/auth/login", handler.Login)
	r.POST("/auth/refresh", handler.RefreshToken)
	r.GET("/auth/validate", handler.ValidateToken)
	r.POST("/auth/logout", handler.Logout)
	r.GET("/auth/me", handler.GetCurrentUser)
	r.POST("/auth/change-password", handler.ChangePassword)
}

// RegisterInitializationRoutes 注册初始化相关的路由
//
// 路由列表：
//   # 知识库配置
//   GET    /initialization/config/:kbId        # 获取配置
//   POST   /initialization/initialize/:kbId    # 初始化知识库
//   PUT    /initialization/config/:kbId        # 更新配置
//
//   # Ollama 管理
//   GET    /initialization/ollama/status                    # 检查状态
//   GET    /initialization/ollama/models                    # 模型列表
//   POST   /initialization/ollama/models/check              # 检查模型
//   POST   /initialization/ollama/models/download           # 下载模型
//   GET    /initialization/ollama/download/progress/:taskId # 下载进度
//   GET    /initialization/ollama/download/tasks            # 任务列表
//
//   # 远程模型
//   POST   /initialization/remote/check       # 检查远程模型
//   POST   /initialization/embedding/test     # 测试Embedding模型
//   POST   /initialization/rerank/check       # 检查Rerank模型
//   POST   /initialization/multimodal/test    # 测试多模态功能
//
//   # 文本处理
//   POST   /initialization/extract/text-relation  # 提取文本关系
//   POST   /initialization/extract/fabri-tag      # 生成标签
//   POST   /initialization/extract/fabri-text     # 生成文本
//

func RegisterInitializationRoutes(r *gin.RouterGroup, handler *handler.InitializationHandler) {
	// 初始化接口
	r.GET("/initialization/config/:kbId", handler.GetCurrentConfigByKB)
	r.POST("/initialization/initialize/:kbId", handler.InitializeByKB)
	r.PUT("/initialization/config/:kbId", handler.UpdateKBConfig) // 新的简化版接口，只传模型ID

	// Ollama相关接口
	r.GET("/initialization/ollama/status", handler.CheckOllamaStatus)
	r.GET("/initialization/ollama/models", handler.ListOllamaModels)
	r.POST("/initialization/ollama/models/check", handler.CheckOllamaModels)
	r.POST("/initialization/ollama/models/download", handler.DownloadOllamaModel)
	r.GET("/initialization/ollama/download/progress/:taskId", handler.GetDownloadProgress)
	r.GET("/initialization/ollama/download/tasks", handler.ListDownloadTasks)

	// 远程API相关接口
	r.POST("/initialization/remote/check", handler.CheckRemoteModel)
	r.POST("/initialization/embedding/test", handler.TestEmbeddingModel)
	r.POST("/initialization/rerank/check", handler.CheckRerankModel)
	r.POST("/initialization/multimodal/test", handler.TestMultimodalFunction)

	r.POST("/initialization/extract/text-relation", handler.ExtractTextRelations)
	r.POST("/initialization/extract/fabri-tag", handler.FabriTag)
	r.POST("/initialization/extract/fabri-text", handler.FabriText)
}

// RegisterSystemRoutes 注册系统信息相关的路由
//
// 路由列表：
//   GET /system/info              # 获取系统信息
//   GET /system/minio/buckets     # 获取MinIO存储桶列表

// RegisterSystemRoutes registers system information routes
func RegisterSystemRoutes(r *gin.RouterGroup, handler *handler.SystemHandler) {
	systemRoutes := r.Group("/system")
	{
		systemRoutes.GET("/info", handler.GetSystemInfo)
		systemRoutes.GET("/minio/buckets", handler.ListMinioBuckets)
	}
}

// RegisterMCPServiceRoutes 注册 MCP 服务相关的路由
//
// MCP (Model Context Protocol) 服务管理
//
// 路由列表：
//   POST   /mcp-services             	# 创建MCP服务
//   GET    /mcp-services              	# 获取服务列表
//   GET    /mcp-services/:id          	# 获取服务详情
//   PUT    /mcp-services/:id          	# 更新服务
//   DELETE /mcp-services/:id          	# 删除服务
//   POST   /mcp-services/:id/test     	# 测试服务连接
//   GET    /mcp-services/:id/tools    	# 获取工具列表
//   GET    /mcp-services/:id/resources # 获取资源列表

// RegisterMCPServiceRoutes registers MCP service routes
func RegisterMCPServiceRoutes(r *gin.RouterGroup, handler *handler.MCPServiceHandler) {
	mcpServices := r.Group("/mcp-services")
	{
		// Create MCP service
		mcpServices.POST("", handler.CreateMCPService)
		// List MCP services
		mcpServices.GET("", handler.ListMCPServices)
		// Get MCP service by ID
		mcpServices.GET("/:id", handler.GetMCPService)
		// Update MCP service
		mcpServices.PUT("/:id", handler.UpdateMCPService)
		// Delete MCP service
		mcpServices.DELETE("/:id", handler.DeleteMCPService)
		// Test MCP service connection
		mcpServices.POST("/:id/test", handler.TestMCPService)
		// Get MCP service tools
		mcpServices.GET("/:id/tools", handler.GetMCPServiceTools)
		// Get MCP service resources
		mcpServices.GET("/:id/resources", handler.GetMCPServiceResources)
	}
}

// RegisterWebSearchRoutes 注册网页搜索相关的路由
//
// 路由列表：
//   GET /web-search/providers   # 获取可用的搜索引擎提供商

// RegisterWebSearchRoutes registers web search routes
func RegisterWebSearchRoutes(r *gin.RouterGroup, webSearchHandler *handler.WebSearchHandler) {
	// Web search providers
	webSearch := r.Group("/web-search")
	{
		// Get available providers
		webSearch.GET("/providers", webSearchHandler.GetProviders)
	}
}

// RegisterCustomAgentRoutes 注册自定义 Agent 相关的路由
//
// 路由列表：
//   # 占位符
//   GET    /agents/placeholders        # 获取占位符定义
//
//   # Agent CRUD
//   POST   /agents                     # 创建Agent
//   GET    /agents                     # 获取Agent列表（含内置）
//   GET    /agents/:id                 # 获取Agent详情
//   PUT    /agents/:id                 # 更新Agent
//   DELETE /agents/:id                 # 删除Agent
//   POST   /agents/:id/copy            # 复制Agent

// RegisterCustomAgentRoutes registers custom agent routes
func RegisterCustomAgentRoutes(r *gin.RouterGroup, agentHandler *handler.CustomAgentHandler) {
	agents := r.Group("/agents")
	{
		// Get placeholder definitions (must be before /:id to avoid conflict)
		agents.GET("/placeholders", agentHandler.GetPlaceholders)
		// Create custom agent
		agents.POST("", agentHandler.CreateAgent)
		// List all agents (including built-in)
		agents.GET("", agentHandler.ListAgents)
		// Get agent by ID
		agents.GET("/:id", agentHandler.GetAgent)
		// Update agent
		agents.PUT("/:id", agentHandler.UpdateAgent)
		// Delete agent
		agents.DELETE("/:id", agentHandler.DeleteAgent)
		// Copy agent
		agents.POST("/:id/copy", agentHandler.CopyAgent)
	}
}

// RegisterSkillRoutes 注册技能相关的路由
//
// 路由列表：
//   GET /skills   # 获取预加载的技能列表

// RegisterSkillRoutes registers skill routes
func RegisterSkillRoutes(r *gin.RouterGroup, skillHandler *handler.SkillHandler) {
	skills := r.Group("/skills")
	{
		// List all preloaded skills
		skills.GET("", skillHandler.ListSkills)
	}
}

// RegisterOrganizationRoutes 注册组织和资源共享相关的路由
//
// 功能模块：
//   1. 组织管理（CRUD、成员管理、邀请机制）
//   2. 资源共享（知识库共享、Agent共享）
//   3. 公开资源访问
//
// 路由列表：
//   # 组织管理
//   POST   /organizations                                 # 创建组织
//   GET    /organizations                                 # 我的组织列表
//   GET    /organizations/preview/:code                   # 预览邀请码
//   POST   /organizations/join                            # 通过邀请码加入
//   POST   /organizations/join-request                    # 提交加入申请
//   GET    /organizations/search                          # 搜索可加入的组织
//   POST   /organizations/join-by-id                      # 通过ID加入
//   GET    /organizations/:id                             # 获取组织详情
//   PUT    /organizations/:id                             # 更新组织
//   DELETE /organizations/:id                             # 删除组织
//   POST   /organizations/:id/leave                       # 离开组织
//   POST   /organizations/:id/request-upgrade             # 请求角色升级
//
//   # 邀请管理
//   POST   /organizations/:id/invite-code                 # 生成邀请码
//   GET    /organizations/:id/search-users                # 搜索用户（邀请用）
//   POST   /organizations/:id/invite                      # 直接邀请成员
//
//   # 成员管理
//   GET    /organizations/:id/members                     # 成员列表
//   PUT    /organizations/:id/members/:user_id            # 更新成员角色
//   DELETE /organizations/:id/members/:user_id            # 移除成员
//
//   # 申请审核
//   GET    /organizations/:id/join-requests               # 申请列表
//   PUT    /organizations/:id/join-requests/:request_id/review # 审核申请
//
//   # 知识库共享
//   POST   /knowledge-bases/:id/shares                    # 共享知识库
//   GET    /knowledge-bases/:id/shares                    # 共享列表
//   PUT    /knowledge-bases/:id/shares/:share_id          # 更新权限
//   DELETE /knowledge-bases/:id/shares/:share_id          # 取消共享
//
//   # Agent共享
//   POST   /agents/:id/shares                             # 共享Agent
//   GET    /agents/:id/shares                             # 共享列表
//   DELETE /agents/:id/shares/:share_id                   # 取消共享
//
//   # 组织内资源视图
//   GET    /organizations/:id/shares                      # 组织共享的知识库
//   GET    /organizations/:id/agent-shares                # 组织共享的Agent
//   GET    /organizations/:id/shared-knowledge-bases      # 组织内知识库（含个人）
//   GET    /organizations/:id/shared-agents               # 组织内Agent（含个人）
//
//   # 公开资源
//   GET    /shared-knowledge-bases                        # 公开知识库列表
//   GET    /shared-agents                                 # 公开Agent列表
//   POST   /shared-agents/disabled                        # 禁用公开Agent

// RegisterOrganizationRoutes registers organization and sharing routes
func RegisterOrganizationRoutes(r *gin.RouterGroup, orgHandler *handler.OrganizationHandler) {
	// Organization routes
	orgs := r.Group("/organizations")
	{
		// Create organization
		orgs.POST("", orgHandler.CreateOrganization)
		// List my organizations
		orgs.GET("", orgHandler.ListMyOrganizations)
		// Preview organization by invite code (without joining)
		orgs.GET("/preview/:code", orgHandler.PreviewByInviteCode)
		// Join organization by invite code
		orgs.POST("/join", orgHandler.JoinByInviteCode)
		// Submit join request (for organizations that require approval)
		orgs.POST("/join-request", orgHandler.SubmitJoinRequest)
		// Search searchable (discoverable) organizations
		orgs.GET("/search", orgHandler.SearchOrganizations)
		// Join searchable organization by ID (no invite code)
		orgs.POST("/join-by-id", orgHandler.JoinByOrganizationID)
		// Get organization by ID
		orgs.GET("/:id", orgHandler.GetOrganization)
		// Update organization
		orgs.PUT("/:id", orgHandler.UpdateOrganization)
		// Delete organization
		orgs.DELETE("/:id", orgHandler.DeleteOrganization)
		// Leave organization
		orgs.POST("/:id/leave", orgHandler.LeaveOrganization)
		// Request role upgrade (for existing members)
		orgs.POST("/:id/request-upgrade", orgHandler.RequestRoleUpgrade)
		// Generate invite code
		orgs.POST("/:id/invite-code", orgHandler.GenerateInviteCode)
		// Search users for invite (admin only)
		orgs.GET("/:id/search-users", orgHandler.SearchUsersForInvite)
		// Invite member directly (admin only)
		orgs.POST("/:id/invite", orgHandler.InviteMember)
		// List members
		orgs.GET("/:id/members", orgHandler.ListMembers)
		// Update member role
		orgs.PUT("/:id/members/:user_id", orgHandler.UpdateMemberRole)
		// Remove member
		orgs.DELETE("/:id/members/:user_id", orgHandler.RemoveMember)
		// List join requests (admin only)
		orgs.GET("/:id/join-requests", orgHandler.ListJoinRequests)
		// Review join request (admin only)
		orgs.PUT("/:id/join-requests/:request_id/review", orgHandler.ReviewJoinRequest)
		// List knowledge bases shared to this organization
		orgs.GET("/:id/shares", orgHandler.ListOrgShares)
		// List agents shared to this organization
		orgs.GET("/:id/agent-shares", orgHandler.ListOrgAgentShares)
		// List all knowledge bases in this organization (including mine) for list-page space view
		orgs.GET("/:id/shared-knowledge-bases", orgHandler.ListOrganizationSharedKnowledgeBases)
		// List all agents in this organization (including mine) for list-page space view
		orgs.GET("/:id/shared-agents", orgHandler.ListOrganizationSharedAgents)
	}

	// Knowledge base sharing routes (add to existing kb routes)
	kbShares := r.Group("/knowledge-bases/:id/shares")
	{
		// Share knowledge base
		kbShares.POST("", orgHandler.ShareKnowledgeBase)
		// List shares
		kbShares.GET("", orgHandler.ListKBShares)
		// Update share permission
		kbShares.PUT("/:share_id", orgHandler.UpdateSharePermission)
		// Remove share
		kbShares.DELETE("/:share_id", orgHandler.RemoveShare)
	}

	// Agent sharing routes
	agentShares := r.Group("/agents/:id/shares")
	{
		agentShares.POST("", orgHandler.ShareAgent)
		agentShares.GET("", orgHandler.ListAgentShares)
		agentShares.DELETE("/:share_id", orgHandler.RemoveAgentShare)
	}

	// Shared knowledge bases route
	r.GET("/shared-knowledge-bases", orgHandler.ListSharedKnowledgeBases)
	// Shared agents route
	r.GET("/shared-agents", orgHandler.ListSharedAgents)
	r.POST("/shared-agents/disabled", orgHandler.SetSharedAgentDisabledByMe)
}
