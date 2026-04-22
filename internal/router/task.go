package router

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
)

// WeKnora 的异步任务处理机制是典型的生产者-消费者模式，利用 Asynq 作为任务队列系统。
//	触发时机：当用户上传文档、批量导入 FAQ 或让 Agent 执行复杂任务时，系统会触发异步任务。
//	处理过程：这些任务进入 Redis 队列，由后台 Worker 协程按优先级（critical, default, low）处理。
//	结果反馈：
//	 - 对于进度敏感型任务（如文件上传），通过轮询 API 查询状态和进度。
//	 - 对于Agent 对话等流式任务，通过 SSE/WebSocket 进行实时流式推送。
//	 - 对于最终状态，通过更新 UI 中的文档/任务状态来告知用户。
//
// Asynq 会根据权重分配 Worker。
// 当 critical 有任务时，Worker 会以 6/10 的概率优先处理它。
// 这保证了核心业务（如立即执行的索引删除）不会被海量的背景任务（如大规模 FAQ 导入）阻塞。
//
// Async 任务处理流程
//	用户请求（API）
//	   ↓
//	Handler 层
//	   ↓
//	Service 层（提交任务）
//	   ├── client.Enqueue(task, asynq.Queue("critical"))
//	   └── 立即返回"任务已提交"
//	   ↓
//	Asynq Server（后台处理）
//	   ├── 从 Redis 拉取任务
//	   ├── 根据队列权重分配资源
//	   ├── 调用对应的 Handler
//	   └── 更新任务状态
//	   ↓
//	完成（用户可查询结果）

// 什么时候使用异步任务？
//
//	WeKnora 遵循一个原则：凡是耗时超过 500ms 或涉及外部 AI 模型调用的操作，全部转入异步。
//	具体场景：
//		文档解析与切片 (Document Processing)
//			用户上传一个 50MB 的 PDF，需要经过：解析内容 -> 清洗文本 -> 语义切片 -> 向量化 (Embedding) -> 存入向量库。整个过程可能持续数分钟。
//		大规模 FAQ 导入 (FAQ Import)
//			批量导入成千上万条问答对时，如果同步处理会造成 API 接口超时，甚至导致数据库锁死。
//		大模型批量生成 (AI Generation)
//			问题生成：根据文档自动生成问答对。
//			摘要生成：对长文本进行总结。
//			这些任务依赖 LLM 响应，网络延迟和计算耗时极高且不稳定。
//		资源清理与克隆 (Heavy Cleanup)
//			删除一个拥有 10 万个分片的知识库，或者克隆一个庞大的知识库到另一个租户。这种涉及大量磁盘和索引 I/O 的操作必须异步。
//
// 步任务结果如何实时推送给用户？
//	由于异步任务是在独立的 Worker 进程中运行的，它无法直接通过 HTTP 响应把结果给用户。
//	WeKnora 通常采用以下三种方案组合：
//
//	1. WebSocket 实时推送（最主流）
//	 流程：
//		前端在页面加载时，与后端建立一个 WebSocket 连接（通常带上 user_id 或 request_id）。
//		Asynq Worker 完成任务后，发布一条消息到 Redis Pub/Sub。
//		WebSocket 服务订阅该 Redis 频道，一旦收到消息，立即推送到对应的客户端。
//	 优点：
//	 	真正意义上的“实时”，用户体验最好。
//
//	2. 状态轮询 (Polling) + 状态机
//	 流程：
//		异步任务启动时，后端在数据库（MySQL）中创建一个任务记录，状态设为 Processing，并返回给前端一个 task_id。
//		前端启动定时器（如每 2 秒一次），拿着 task_id 调用查询接口。
//		Worker 处理完后，将数据库中的状态更新为 Success 或 Failed。
//		前端查到状态变更后，停止轮询并展示结果。
//	 优点：
//		实现最简单，不占用持久连接，兼容性最强。
//
//	3. 消息中心/通知系统
//	 流程：
//		任务结束时，Worker 向“系统消息”表插入一条记录。
//		前端导航栏的小铃铛图标会显示红点。

type AsynqTaskParams struct {
	dig.In

	Server               *asynq.Server
	KnowledgeService     interfaces.KnowledgeService
	KnowledgeBaseService interfaces.KnowledgeBaseService
	TagService           interfaces.KnowledgeTagService
	ChunkExtractor       interfaces.TaskHandler `name:"chunkExtractor"`
	DataTableSummary     interfaces.TaskHandler `name:"dataTableSummary"`
}

func getAsynqRedisClientOpt() *asynq.RedisClientOpt {
	db := 0
	if dbStr := os.Getenv("REDIS_DB"); dbStr != "" {
		if parsed, err := strconv.Atoi(dbStr); err == nil {
			db = parsed
		}
	}
	opt := &asynq.RedisClientOpt{
		Addr:         os.Getenv("REDIS_ADDR"),
		Username:     os.Getenv("REDIS_USERNAME"),
		Password:     os.Getenv("REDIS_PASSWORD"),
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		DB:           db,
	}
	return opt
}

func NewAsyncqClient() (*asynq.Client, error) {
	opt := getAsynqRedisClientOpt()
	client := asynq.NewClient(opt)
	err := client.Ping()
	if err != nil {
		return nil, err
	}
	return client, nil
}

func NewAsynqServer() *asynq.Server {
	opt := getAsynqRedisClientOpt()
	srv := asynq.NewServer(
		opt,
		asynq.Config{
			Queues: map[string]int{
				"critical": 6, // Highest priority queue
				"default":  3, // Default priority queue
				"low":      1, // Lowest priority queue
			},
		},
	)
	return srv
}

func RunAsynqServer(params AsynqTaskParams) *asynq.ServeMux {
	// Create a new mux and register all handlers
	mux := asynq.NewServeMux()

	// Register extract handlers - router will dispatch to appropriate handler
	mux.HandleFunc(types.TypeChunkExtract, params.ChunkExtractor.Handle)
	mux.HandleFunc(types.TypeDataTableSummary, params.DataTableSummary.Handle)

	// Register document processing handler
	mux.HandleFunc(types.TypeDocumentProcess, params.KnowledgeService.ProcessDocument)

	// Register FAQ import handler (includes dry run mode)
	mux.HandleFunc(types.TypeFAQImport, params.KnowledgeService.ProcessFAQImport)

	// Register question generation handler
	mux.HandleFunc(types.TypeQuestionGeneration, params.KnowledgeService.ProcessQuestionGeneration)

	// Register summary generation handler
	mux.HandleFunc(types.TypeSummaryGeneration, params.KnowledgeService.ProcessSummaryGeneration)

	// Register KB clone handler
	mux.HandleFunc(types.TypeKBClone, params.KnowledgeService.ProcessKBClone)

	// Register knowledge list delete handler
	mux.HandleFunc(types.TypeKnowledgeListDelete, params.KnowledgeService.ProcessKnowledgeListDelete)

	// Register index delete handler
	mux.HandleFunc(types.TypeIndexDelete, params.TagService.ProcessIndexDelete)

	// Register KB delete handler
	mux.HandleFunc(types.TypeKBDelete, params.KnowledgeBaseService.ProcessKBDelete)

	go func() {
		// Start the server
		if err := params.Server.Run(mux); err != nil {
			log.Fatalf("could not run server: %v", err)
		}
	}()
	return mux
}
