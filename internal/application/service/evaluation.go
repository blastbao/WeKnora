package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"golang.org/x/sync/errgroup"
)

/*
corpus: pid -> content
queries: qid -> content
answers: aid -> content
qrels: qid -> pid
arels: qid -> aid
*/

// EvaluationService handles evaluation tasks for knowledge base and chat models
type EvaluationService struct {
	config               *config.Config                  // Application configuration
	dataset              interfaces.DatasetService       // Service for dataset operations
	knowledgeBaseService interfaces.KnowledgeBaseService // Service for knowledge base operations
	knowledgeService     interfaces.KnowledgeService     // Service for knowledge operations
	sessionService       interfaces.SessionService       // Service for chat sessions
	modelService         interfaces.ModelService         // Service for model operations

	evaluationMemoryStorage *evaluationMemoryStorage // In-memory storage for evaluation tasks
}

func NewEvaluationService(
	config *config.Config,
	dataset interfaces.DatasetService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	sessionService interfaces.SessionService,
	modelService interfaces.ModelService,
) interfaces.EvaluationService {
	evaluationMemoryStorage := newEvaluationMemoryStorage()
	return &EvaluationService{
		config:                  config,
		dataset:                 dataset,
		knowledgeBaseService:    knowledgeBaseService,
		knowledgeService:        knowledgeService,
		sessionService:          sessionService,
		modelService:            modelService,
		evaluationMemoryStorage: evaluationMemoryStorage,
	}
}

// evaluationMemoryStorage stores evaluation tasks in memory with thread-safe access
type evaluationMemoryStorage struct {
	store map[string]*types.EvaluationDetail // Map of taskID to evaluation details
	mu    *sync.RWMutex                      // Read-write lock for concurrent access
}

func newEvaluationMemoryStorage() *evaluationMemoryStorage {
	res := &evaluationMemoryStorage{
		store: make(map[string]*types.EvaluationDetail),
		mu:    &sync.RWMutex{},
	}
	return res
}

func (e *evaluationMemoryStorage) register(params *types.EvaluationDetail) {
	e.mu.Lock()
	defer e.mu.Unlock()
	logger.Infof(context.Background(), "Registering evaluation task: %s", params.Task.ID)
	e.store[params.Task.ID] = params
}

func (e *evaluationMemoryStorage) get(taskID string) (*types.EvaluationDetail, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	logger.Infof(context.Background(), "Getting evaluation task: %s", taskID)
	res, ok := e.store[taskID]
	if !ok {
		return nil, errors.New("task not found")
	}
	return res, nil
}

func (e *evaluationMemoryStorage) update(taskID string, fn func(params *types.EvaluationDetail)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	params, ok := e.store[taskID]
	if !ok {
		return errors.New("task not found")
	}
	fn(params)
	return nil
}

func (e *EvaluationService) EvaluationResult(ctx context.Context, taskID string) (*types.EvaluationDetail, error) {
	logger.Info(ctx, "Start getting evaluation result")
	logger.Infof(ctx, "Task ID: %s", taskID)

	detail, err := e.evaluationMemoryStorage.get(taskID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get evaluation task: %v", err)
		return nil, err
	}

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	logger.Infof(
		ctx,
		"Checking tenant ID match, task tenant ID: %d, current tenant ID: %d",
		detail.Task.TenantID, tenantID,
	)

	if tenantID != detail.Task.TenantID {
		logger.Error(ctx, "Tenant ID mismatch")
		return nil, errors.New("tenant ID does not match")
	}

	logger.Info(ctx, "Evaluation result retrieved successfully")
	return detail, nil
}

// Evaluation starts a new evaluation task with given parameters
// datasetID: ID of the dataset to evaluate against
// knowledgeBaseID: ID of the knowledge base to use (empty to create new)
// chatModelID: ID of the chat model to evaluate
// rerankModelID: ID of the rerank model to evaluate
func (e *EvaluationService) Evaluation(ctx context.Context,
	datasetID string, knowledgeBaseID string, chatModelID string, rerankModelID string,
) (*types.EvaluationDetail, error) {
	logger.Info(ctx, "Start evaluation")
	logger.Infof(ctx, "Dataset ID: %s, Knowledge Base ID: %s, Chat Model ID: %s, Rerank Model ID: %s",
		datasetID, knowledgeBaseID, chatModelID, rerankModelID)

	// Get tenant ID from context for multi-tenancy support
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	logger.Infof(ctx, "Tenant ID: %d", tenantID)

	// Handle knowledge base creation if not provided
	if knowledgeBaseID == "" {
		logger.Info(ctx, "No knowledge base ID provided, creating new knowledge base")
		// Create new knowledge base with default evaluation settings
		// 获取默认的嵌入模型和LLM模型
		models, err := e.modelService.ListModels(ctx)
		if err != nil {
			logger.Errorf(ctx, "Failed to list models: %v", err)
			return nil, err
		}

		var embeddingModelID, llmModelID string
		for _, model := range models {
			if model == nil {
				continue
			}
			if model.Type == types.ModelTypeEmbedding {
				embeddingModelID = model.ID
			}
			if model.Type == types.ModelTypeKnowledgeQA {
				llmModelID = model.ID
			}
		}

		if embeddingModelID == "" || llmModelID == "" {
			return nil, fmt.Errorf("no default models found for evaluation")
		}

		kb, err := e.knowledgeBaseService.CreateKnowledgeBase(ctx, &types.KnowledgeBase{
			Name:             "evaluation",
			Description:      "evaluation",
			EmbeddingModelID: embeddingModelID,
			SummaryModelID:   llmModelID,
		})
		if err != nil {
			logger.Errorf(ctx, "Failed to create knowledge base: %v", err)
			return nil, err
		}
		knowledgeBaseID = kb.ID
		logger.Infof(ctx, "Created new knowledge base with ID: %s", knowledgeBaseID)
	} else {
		logger.Infof(ctx, "Using existing knowledge base ID: %s", knowledgeBaseID)
		// Create evaluation-specific knowledge base based on existing one
		kb, err := e.knowledgeBaseService.GetKnowledgeBaseByID(ctx, knowledgeBaseID)
		if err != nil {
			logger.Errorf(ctx, "Failed to get knowledge base: %v", err)
			return nil, err
		}

		kb, err = e.knowledgeBaseService.CreateKnowledgeBase(ctx, &types.KnowledgeBase{
			Name:             "evaluation",
			Description:      "evaluation",
			EmbeddingModelID: kb.EmbeddingModelID,
			SummaryModelID:   kb.SummaryModelID,
		})
		if err != nil {
			logger.Errorf(ctx, "Failed to create knowledge base: %v", err)
			return nil, err
		}
		knowledgeBaseID = kb.ID
		logger.Infof(ctx, "Created new knowledge base with ID: %s based on existing one", knowledgeBaseID)
	}

	// Set default values for optional parameters
	if datasetID == "" {
		datasetID = "default"
		logger.Info(ctx, "Using default dataset")
	}

	if rerankModelID == "" {
		// 获取默认的重排模型
		models, err := e.modelService.ListModels(ctx)
		if err == nil {
			for _, model := range models {
				if model == nil {
					continue
				}
				if model.Type == types.ModelTypeRerank {
					rerankModelID = model.ID
					break
				}
			}
		}
		if rerankModelID == "" {
			logger.Warnf(ctx, "No rerank model found, skipping rerank")
		} else {
			logger.Infof(ctx, "Using default rerank model: %s", rerankModelID)
		}
	}

	if chatModelID == "" {
		// 获取默认的LLM模型
		models, err := e.modelService.ListModels(ctx)
		if err == nil {
			for _, model := range models {
				if model == nil {
					continue
				}
				if model.Type == types.ModelTypeKnowledgeQA {
					chatModelID = model.ID
					break
				}
			}
		}
		if chatModelID == "" {
			return nil, fmt.Errorf("no default chat model found")
		}
		logger.Infof(ctx, "Using default chat model: %s", chatModelID)
	}

	// Create evaluation task with unique ID
	logger.Info(ctx, "Creating evaluation task")
	taskID := utils.GenerateTaskID("evaluation", tenantID, datasetID)
	logger.Infof(ctx, "Generated task ID: %s", taskID)

	// Prepare evaluation detail with all parameters
	detail := &types.EvaluationDetail{
		Task: &types.EvaluationTask{
			ID:        taskID,
			TenantID:  tenantID,
			DatasetID: datasetID,
			Status:    types.EvaluationStatuePending,
			StartTime: time.Now(),
		},
		Params: &types.ChatManage{
			VectorThreshold:  e.config.Conversation.VectorThreshold,
			KeywordThreshold: e.config.Conversation.KeywordThreshold,
			EmbeddingTopK:    e.config.Conversation.EmbeddingTopK,
			MaxRounds:        e.config.Conversation.MaxRounds,
			RerankModelID:    rerankModelID,
			RerankTopK:       e.config.Conversation.RerankTopK,
			RerankThreshold:  e.config.Conversation.RerankThreshold,
			ChatModelID:      chatModelID,
			SummaryConfig: types.SummaryConfig{
				MaxTokens:           e.config.Conversation.Summary.MaxTokens,
				RepeatPenalty:       e.config.Conversation.Summary.RepeatPenalty,
				TopK:                e.config.Conversation.Summary.TopK,
				TopP:                e.config.Conversation.Summary.TopP,
				Prompt:              e.config.Conversation.Summary.Prompt,
				ContextTemplate:     e.config.Conversation.Summary.ContextTemplate,
				FrequencyPenalty:    e.config.Conversation.Summary.FrequencyPenalty,
				PresencePenalty:     e.config.Conversation.Summary.PresencePenalty,
				NoMatchPrefix:       e.config.Conversation.Summary.NoMatchPrefix,
				Temperature:         e.config.Conversation.Summary.Temperature,
				Seed:                e.config.Conversation.Summary.Seed,
				MaxCompletionTokens: e.config.Conversation.Summary.MaxCompletionTokens,
			},
			FallbackResponse:    e.config.Conversation.FallbackResponse,
			RewritePromptSystem: e.config.Conversation.RewritePromptSystem,
			RewritePromptUser:   e.config.Conversation.RewritePromptUser,
		},
	}

	// Store evaluation task in memory storage
	logger.Info(ctx, "Registering evaluation task")
	e.evaluationMemoryStorage.register(detail)

	// Start evaluation in background goroutine
	logger.Info(ctx, "Starting evaluation in background")
	go func() {
		// Create new context with logger for background task
		newCtx := logger.CloneContext(ctx)
		logger.Infof(newCtx, "Background evaluation started for task ID: %s", taskID)

		// Update task status to running
		detail.Task.Status = types.EvaluationStatueRunning
		logger.Info(newCtx, "Evaluation task status set to running")

		// Execute actual evaluation
		if err := e.EvalDataset(newCtx, detail, knowledgeBaseID); err != nil {
			detail.Task.Status = types.EvaluationStatueFailed
			detail.Task.ErrMsg = err.Error()
			logger.Errorf(newCtx, "Evaluation task failed: %v, task ID: %s", err, taskID)
			return
		}

		// Mark task as completed successfully
		logger.Infof(newCtx, "Evaluation task completed successfully, task ID: %s", taskID)
		detail.Task.Status = types.EvaluationStatueSuccess
	}()

	logger.Infof(ctx, "Evaluation task created successfully, task ID: %s", taskID)
	return detail, nil
}

// EvalDataset 执行数据集的评估任务
//
// 功能说明:
//   - 加载指定数据集，获取所有问答对
//   - 将数据集中的段落创建为临时知识库
//   - 并行处理每个问答对，执行 RAG 问答流程
//   - 记录检索、重排序、生成等各阶段的评估指标
//   - 实时更新任务进度和评估指标
//   - 评估完成后自动清理临时资源
//
// 参数:
//   - ctx: 上下文，用于控制执行超时和传递日志追踪信息
//   - detail: 评估任务详情，包含任务配置、参数、指标等信息
//   - knowledgeBaseID: 临时知识库 ID，用于存储数据集段落
//
// 返回值:
//   - error: 评估过程中的错误，nil 表示评估成功完成
//
// 执行流程:
//   1. 加载数据集 → 2. 创建临时知识库 → 3. 设置资源清理（defer）
//   4. 初始化并发控制 → 5. 并行处理问答对（RAG 流程）
//   6. 记录评估指标 → 7. 更新任务进度 → 8. 等待完成 → 9. 清理资源
//
// 并发控制:
//   - 使用 errgroup.Group 管理并发任务
//   - 工作线程数设置为 CPU 核心数减 1（至少 1 个）
//   - 使用 sync.Mutex 保护共享的进度计数器和指标数据
//
// 评估指标记录:
//   - recordInit: 初始化当前问答对记录
//   - recordQaPair: 记录标准问答对信息
//   - recordSearchResult: 记录检索结果
//   - recordRerankResult: 记录重排序结果
//   - recordChatResponse: 记录模型生成回答
//   - recordFinish: 标记当前问答对评估完成
//
// 资源管理:
//   - 创建临时知识库存储数据集段落
//   - 使用 defer 确保评估结束后删除临时知识库和知识库
//   - 删除失败时记录错误日志但不中断流程
//
// 进度更新:
//   - 实时更新已完成数量（finished）和评估指标（Metric）
//   - 通过 evaluationMemoryStorage 持久化任务状态
//   - 记录每个问答对的处理日志
//
// 错误处理:
//   - 数据集加载失败: 返回错误，终止评估
//   - 知识库创建失败: 返回错误，终止评估
//   - 单个问答对处理失败: 返回错误，终止整个评估任务
//   - 资源清理失败: 记录错误日志，不影响评估结果
//
// 注意事项:
//   - 评估过程中会修改 detail 对象的状态（通过闭包更新）
//   - 临时资源清理在函数返回时执行，即使评估失败也会执行
//   - 评估指标通过 HookMetric 收集，支持多维度分析
//
// 调用示例:
//   detail := &types.EvaluationDetail{
//       Task: &types.EvaluationTask{ID: "eval_001", DatasetID: "dataset_001"},
//       Params: &types.ChatManageParams{...},
//   }
//   err := service.EvalDataset(ctx, detail, "kb_temp_001")
//
// 返回结果:
//   - 成功: 返回 nil，detail 对象中的 Task.Finished 等于 Task.Total，Metric 包含完整的评估指标（检索准确率、重排序质量、回答相关性等）
//   - 失败: 返回错误对象，可能的错误包括：
//       - 数据集不存在或加载失败
//       - 临时知识库创建失败
//       - 问答对处理过程中出错
//       - 并发执行异常

// EvalDataset performs the actual evaluation of a dataset
// Processes each QA pair in parallel and records metrics
func (e *EvaluationService) EvalDataset(ctx context.Context, detail *types.EvaluationDetail, knowledgeBaseID string) error {
	logger.Info(ctx, "Start evaluating dataset")
	logger.Infof(ctx, "Task ID: %s, Dataset ID: %s", detail.Task.ID, detail.Task.DatasetID)

	// Retrieve dataset from storage
	dataset, err := e.dataset.GetDatasetByID(ctx, detail.Task.DatasetID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get dataset: %v", err)
		return err
	}
	logger.Infof(ctx, "Dataset retrieved successfully with %d QA pairs", len(dataset))

	// Update total QA pairs count in task details
	e.evaluationMemoryStorage.update(detail.Task.ID, func(params *types.EvaluationDetail) {
		params.Task.Total = len(dataset)
		logger.Infof(ctx, "Updated task total to %d QA pairs", params.Task.Total)
	})

	// Extract and organize passages from dataset
	passages := getPassageList(dataset)
	logger.Infof(ctx, "Creating knowledge from %d passages", len(passages))

	// Create knowledge base from passages
	knowledge, err := e.knowledgeService.CreateKnowledgeFromPassage(ctx, knowledgeBaseID, passages)
	if err != nil {
		logger.Errorf(ctx, "Failed to create knowledge from passages: %v", err)
		return err
	}
	logger.Infof(ctx, "Knowledge created successfully, ID: %s", knowledge.ID)

	// Setup cleanup of temporary resources
	defer func() {
		logger.Infof(ctx, "Cleaning up resources - deleting knowledge: %s", knowledge.ID)
		if err := e.knowledgeService.DeleteKnowledge(ctx, knowledge.ID); err != nil {
			logger.Errorf(ctx, "Failed to delete knowledge: %v, knowledge ID: %s", err, knowledge.ID)
		}

		logger.Infof(ctx, "Cleaning up resources - deleting knowledge base: %s", knowledgeBaseID)
		if err := e.knowledgeBaseService.DeleteKnowledgeBase(ctx, knowledgeBaseID); err != nil {
			logger.Errorf(
				ctx,
				"Failed to delete knowledge base: %v, knowledge base ID: %s",
				err, knowledgeBaseID,
			)
		}
	}()

	// Initialize parallel evaluation metrics
	var finished int
	var mu sync.Mutex
	var g errgroup.Group
	metricHook := NewHookMetric(len(dataset))

	// Set worker limit based on available CPUs
	g.SetLimit(max(runtime.GOMAXPROCS(0)-1, 1))
	logger.Infof(ctx, "Starting evaluation with %d parallel workers", max(runtime.GOMAXPROCS(0)-1, 1))

	// Process each QA pair in parallel
	for i, qaPair := range dataset {
		qaPair := qaPair
		i := i
		g.Go(func() error {
			logger.Infof(ctx, "Processing QA pair %d, question: %s", i, qaPair.Question)

			// Prepare chat management parameters for this QA pair
			chatManage := detail.Params.Clone()
			chatManage.Query = qaPair.Question
			chatManage.RewriteQuery = qaPair.Question
			// Set knowledge base ID and search targets for this evaluation
			chatManage.KnowledgeBaseIDs = []string{knowledgeBaseID}
			chatManage.SearchTargets = types.SearchTargets{
				&types.SearchTarget{
					Type:            types.SearchTargetTypeKnowledgeBase,
					KnowledgeBaseID: knowledgeBaseID,
				},
			}

			// Execute knowledge QA pipeline
			logger.Infof(ctx, "Running knowledge QA for question: %s", qaPair.Question)
			err = e.sessionService.KnowledgeQAByEvent(ctx, chatManage, types.Pipline["rag"])
			if err != nil {
				logger.Errorf(ctx, "Failed to process question %d: %v", i, err)
				return err
			}

			// Record evaluation metrics
			logger.Infof(ctx, "Recording metrics for QA pair %d", i)
			metricHook.recordInit(i)
			metricHook.recordQaPair(i, qaPair)
			metricHook.recordSearchResult(i, chatManage.SearchResult)
			metricHook.recordRerankResult(i, chatManage.RerankResult)
			metricHook.recordChatResponse(i, chatManage.ChatResponse)
			metricHook.recordFinish(i)

			// Update progress metrics
			mu.Lock()
			finished += 1
			metricResult := metricHook.MetricResult()
			mu.Unlock()
			e.evaluationMemoryStorage.update(detail.Task.ID, func(params *types.EvaluationDetail) {
				params.Metric = metricResult
				params.Task.Finished = finished
				logger.Infof(ctx, "Updated task progress: %d/%d completed", finished, params.Task.Total)
			})
			return nil
		})
	}

	// Wait for all parallel evaluations to complete
	logger.Info(ctx, "Waiting for all evaluation tasks to complete")
	if err := g.Wait(); err != nil {
		logger.Errorf(ctx, "Evaluation error: %v", err)
		return err
	}

	// Final update of evaluation metrics
	e.evaluationMemoryStorage.update(detail.Task.ID, func(params *types.EvaluationDetail) {
		params.Metric = metricHook.MetricResult()
		params.Task.Finished = finished
	})

	logger.Infof(ctx, "Dataset evaluation completed successfully, task ID: %s", detail.Task.ID)
	return nil
}

// getPassageList extracts and organizes passages from QA pairs
// Returns a slice of passages indexed by their passage IDs
func getPassageList(dataset []*types.QAPair) []string {
	pIDMap := make(map[int]string)
	maxPID := 0
	for _, qaPair := range dataset {
		for i := 0; i < len(qaPair.PIDs); i++ {
			pIDMap[qaPair.PIDs[i]] = qaPair.Passages[i]
			maxPID = max(maxPID, qaPair.PIDs[i])
		}
	}
	passages := make([]string, maxPID)
	for i := 0; i < maxPID; i++ {
		if _, ok := pIDMap[i]; ok {
			passages[i] = pIDMap[i]
		}
	}
	return passages
}
