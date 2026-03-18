package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// ErrInvalidTenantID represents an error for invalid tenant ID
var ErrInvalidTenantID = errors.New("invalid tenant ID")

// knowledgeBaseService implements the knowledge base service interface
type knowledgeBaseService struct {
	repo           interfaces.KnowledgeBaseRepository
	kgRepo         interfaces.KnowledgeRepository
	chunkRepo      interfaces.ChunkRepository
	shareRepo      interfaces.KBShareRepository
	kbShareService interfaces.KBShareService
	modelService   interfaces.ModelService
	retrieveEngine interfaces.RetrieveEngineRegistry
	tenantRepo     interfaces.TenantRepository
	fileSvc        interfaces.FileService
	graphEngine    interfaces.RetrieveGraphRepository
	asynqClient    *asynq.Client
}

// NewKnowledgeBaseService creates a new knowledge base service
func NewKnowledgeBaseService(repo interfaces.KnowledgeBaseRepository,
	kgRepo interfaces.KnowledgeRepository,
	chunkRepo interfaces.ChunkRepository,
	shareRepo interfaces.KBShareRepository,
	kbShareService interfaces.KBShareService,
	modelService interfaces.ModelService,
	retrieveEngine interfaces.RetrieveEngineRegistry,
	tenantRepo interfaces.TenantRepository,
	fileSvc interfaces.FileService,
	graphEngine interfaces.RetrieveGraphRepository,
	asynqClient *asynq.Client,
) interfaces.KnowledgeBaseService {
	return &knowledgeBaseService{
		repo:           repo,
		kgRepo:         kgRepo,
		chunkRepo:      chunkRepo,
		shareRepo:      shareRepo,
		kbShareService: kbShareService,
		modelService:   modelService,
		retrieveEngine: retrieveEngine,
		tenantRepo:     tenantRepo,
		fileSvc:        fileSvc,
		graphEngine:    graphEngine,
		asynqClient:    asynqClient,
	}
}

// GetRepository gets the knowledge base repository
// Parameters:
//   - ctx: Context with authentication and request information
//
// Returns:
//   - interfaces.KnowledgeBaseRepository: Knowledge base repository
func (s *knowledgeBaseService) GetRepository() interfaces.KnowledgeBaseRepository {
	return s.repo
}

// CreateKnowledgeBase creates a new knowledge base
func (s *knowledgeBaseService) CreateKnowledgeBase(ctx context.Context,
	kb *types.KnowledgeBase,
) (*types.KnowledgeBase, error) {
	// Generate UUID and set creation timestamps
	if kb.ID == "" {
		kb.ID = uuid.New().String()
	}
	kb.CreatedAt = time.Now()
	kb.TenantID = ctx.Value(types.TenantIDContextKey).(uint64)
	kb.UpdatedAt = time.Now()
	kb.EnsureDefaults()

	logger.Infof(ctx, "Creating knowledge base, ID: %s, tenant ID: %d, name: %s", kb.ID, kb.TenantID, kb.Name)

	if err := s.repo.CreateKnowledgeBase(ctx, kb); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kb.ID,
			"tenant_id":         kb.TenantID,
		})
		return nil, err
	}

	logger.Infof(ctx, "Knowledge base created successfully, ID: %s, name: %s", kb.ID, kb.Name)
	return kb, nil
}

// GetKnowledgeBaseByID retrieves a knowledge base by its ID
func (s *knowledgeBaseService) GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	kb.EnsureDefaults()
	return kb, nil
}

// GetKnowledgeBaseByIDOnly retrieves knowledge base by ID without tenant filter
// Used for cross-tenant shared KB access where permission is checked elsewhere
func (s *knowledgeBaseService) GetKnowledgeBaseByIDOnly(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	kb.EnsureDefaults()
	return kb, nil
}

// GetKnowledgeBasesByIDsOnly retrieves knowledge bases by IDs without tenant filter (batch).
func (s *knowledgeBaseService) GetKnowledgeBasesByIDsOnly(ctx context.Context, ids []string) ([]*types.KnowledgeBase, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	kbs, err := s.repo.GetKnowledgeBaseByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, kb := range kbs {
		if kb != nil {
			kb.EnsureDefaults()
		}
	}
	return kbs, nil
}

// ListKnowledgeBases returns all knowledge bases for a tenant
func (s *knowledgeBaseService) ListKnowledgeBases(ctx context.Context) ([]*types.KnowledgeBase, error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	kbs, err := s.repo.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		for _, kb := range kbs {
			kb.EnsureDefaults()
		}

		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
		})
		return nil, err
	}

	// Query knowledge count and chunk count for each knowledge base
	for _, kb := range kbs {
		kb.EnsureDefaults()

		// Get knowledge count
		switch kb.Type {
		case types.KnowledgeBaseTypeDocument:
			knowledgeCount, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get knowledge count for knowledge base %s: %v", kb.ID, err)
			} else {
				kb.KnowledgeCount = knowledgeCount
			}
		case types.KnowledgeBaseTypeFAQ:
			// Get chunk count
			chunkCount, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get chunk count for knowledge base %s: %v", kb.ID, err)
			} else {
				kb.ChunkCount = chunkCount
			}
		}

		// Check if there is a processing import task
		processingCount, err := s.kgRepo.CountKnowledgeByStatus(
			ctx,
			tenantID,
			kb.ID,
			[]string{"pending", "processing"},
		)
		if err != nil {
			logger.Warnf(ctx, "Failed to check processing status for knowledge base %s: %v", kb.ID, err)
		} else {
			kb.IsProcessing = processingCount > 0
			kb.ProcessingCount = processingCount
		}
	}
	return kbs, nil
}

// ListKnowledgeBasesByTenantID returns all knowledge bases for the given tenant (e.g. for shared agent context).
func (s *knowledgeBaseService) ListKnowledgeBasesByTenantID(ctx context.Context, tenantID uint64) ([]*types.KnowledgeBase, error) {
	kbs, err := s.repo.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
		})
		return nil, err
	}
	for _, kb := range kbs {
		kb.EnsureDefaults()
		switch kb.Type {
		case types.KnowledgeBaseTypeDocument:
			if cnt, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
				kb.KnowledgeCount = cnt
			}
		case types.KnowledgeBaseTypeFAQ:
			if cnt, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
				kb.ChunkCount = cnt
			}
		}
		if processingCount, err := s.kgRepo.CountKnowledgeByStatus(ctx, tenantID, kb.ID, []string{"pending", "processing"}); err == nil {
			kb.IsProcessing = processingCount > 0
			kb.ProcessingCount = processingCount
		}
	}
	return kbs, nil
}

// FillKnowledgeBaseCounts fills KnowledgeCount, ChunkCount, IsProcessing, ProcessingCount for the given KB using kb.TenantID.
func (s *knowledgeBaseService) FillKnowledgeBaseCounts(ctx context.Context, kb *types.KnowledgeBase) error {
	if kb == nil {
		return nil
	}
	tenantID := kb.TenantID
	kb.EnsureDefaults()
	switch kb.Type {
	case types.KnowledgeBaseTypeDocument:
		if cnt, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
			kb.KnowledgeCount = cnt
		}
	case types.KnowledgeBaseTypeFAQ:
		if cnt, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
			kb.ChunkCount = cnt
		}
	}
	if processingCount, err := s.kgRepo.CountKnowledgeByStatus(ctx, tenantID, kb.ID, []string{"pending", "processing"}); err == nil {
		kb.IsProcessing = processingCount > 0
		kb.ProcessingCount = processingCount
	}
	return nil
}

// UpdateKnowledgeBase updates a knowledge base's properties
func (s *knowledgeBaseService) UpdateKnowledgeBase(ctx context.Context,
	id string,
	name string,
	description string,
	config *types.KnowledgeBaseConfig,
) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	logger.Infof(ctx, "Updating knowledge base, ID: %s, name: %s", id, name)

	// Get existing knowledge base
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	// Update the knowledge base properties
	kb.Name = name
	kb.Description = description
	kb.ChunkingConfig = config.ChunkingConfig
	kb.ImageProcessingConfig = config.ImageProcessingConfig
	// Update FAQ config if provided
	if config.FAQConfig != nil {
		kb.FAQConfig = config.FAQConfig
	}
	kb.UpdatedAt = time.Now()
	kb.EnsureDefaults()

	logger.Info(ctx, "Saving knowledge base update")
	if err := s.repo.UpdateKnowledgeBase(ctx, kb); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	logger.Infof(ctx, "Knowledge base updated successfully, ID: %s, name: %s", kb.ID, kb.Name)
	return kb, nil
}

// DeleteKnowledgeBase deletes a knowledge base by its ID
// This method marks the knowledge base as deleted and enqueues an async task
// to handle the heavy cleanup operations (embeddings, chunks, files, graph data)
func (s *knowledgeBaseService) DeleteKnowledgeBase(ctx context.Context, id string) error {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return errors.New("knowledge base ID cannot be empty")
	}

	logger.Infof(ctx, "Deleting knowledge base, ID: %s", id)

	// Get tenant ID from context
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)

	// Step 1: Delete the knowledge base record first (mark as deleted)
	logger.Infof(ctx, "Deleting knowledge base from database")
	err := s.repo.DeleteKnowledgeBase(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return err
	}

	// Step 1b: Remove all organization shares for this KB so org settings no longer show them
	if delErr := s.shareRepo.DeleteByKnowledgeBaseID(ctx, id); delErr != nil {
		logger.Warnf(ctx, "Failed to delete KB shares for knowledge base %s: %v", id, delErr)
	}

	// Step 2: Enqueue async task for heavy cleanup operations
	payload := types.KBDeletePayload{
		TenantID:         tenantID,
		KnowledgeBaseID:  id,
		EffectiveEngines: tenantInfo.GetEffectiveEngines(),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf(ctx, "Failed to marshal KB delete payload: %v", err)
		// Don't fail the request, the KB record is already deleted
		return nil
	}

	task := asynq.NewTask(types.TypeKBDelete, payloadBytes, asynq.Queue("low"), asynq.MaxRetry(3))
	info, err := s.asynqClient.Enqueue(task)
	if err != nil {
		logger.Warnf(ctx, "Failed to enqueue KB delete task: %v", err)
		// Don't fail the request, the KB record is already deleted
		return nil
	}

	logger.Infof(ctx, "KB delete task enqueued: %s, knowledge base ID: %s", info.ID, id)
	logger.Infof(ctx, "Knowledge base deleted successfully, ID: %s", id)
	return nil
}

// ProcessKBDelete handles async knowledge base deletion task
// This method performs heavy cleanup operations: deleting embeddings, chunks, files, and graph data
func (s *knowledgeBaseService) ProcessKBDelete(ctx context.Context, t *asynq.Task) error {
	var payload types.KBDeletePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal KB delete payload: %v", err)
		return err
	}

	tenantID := payload.TenantID
	kbID := payload.KnowledgeBaseID

	// Set tenant context for downstream services
	ctx = context.WithValue(ctx, types.TenantIDContextKey, tenantID)

	logger.Infof(ctx, "Processing KB delete task for knowledge base: %s", kbID)

	// Step 1: Get all knowledge entries in this knowledge base
	logger.Infof(ctx, "Fetching all knowledge entries in knowledge base, ID: %s", kbID)
	knowledgeList, err := s.kgRepo.ListKnowledgeByKnowledgeBaseID(ctx, tenantID, kbID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kbID,
		})
		return err
	}
	logger.Infof(ctx, "Found %d knowledge entries to delete", len(knowledgeList))

	// Step 2: Delete all knowledge entries and their resources
	if len(knowledgeList) > 0 {
		knowledgeIDs := make([]string, 0, len(knowledgeList))
		for _, knowledge := range knowledgeList {
			knowledgeIDs = append(knowledgeIDs, knowledge.ID)
		}

		logger.Infof(ctx, "Deleting all knowledge entries and their resources")

		// Delete embeddings from vector store
		logger.Infof(ctx, "Deleting embeddings from vector store")
		retrieveEngine, err := retriever.NewCompositeRetrieveEngine(
			s.retrieveEngine,
			payload.EffectiveEngines,
		)
		if err != nil {
			logger.Warnf(ctx, "Failed to create retrieve engine: %v", err)
		} else {
			// Group knowledge by embedding model and type
			type groupKey struct {
				EmbeddingModelID string
				Type             string
			}
			embeddingGroups := make(map[groupKey][]string)
			for _, knowledge := range knowledgeList {
				key := groupKey{EmbeddingModelID: knowledge.EmbeddingModelID, Type: knowledge.Type}
				embeddingGroups[key] = append(embeddingGroups[key], knowledge.ID)
			}

			for key, knowledgeGroup := range embeddingGroups {
				embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, key.EmbeddingModelID)
				if err != nil {
					logger.Warnf(ctx, "Failed to get embedding model %s: %v", key.EmbeddingModelID, err)
					continue
				}
				if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, knowledgeGroup, embeddingModel.GetDimensions(), key.Type); err != nil {
					logger.Warnf(ctx, "Failed to delete embeddings for model %s: %v", key.EmbeddingModelID, err)
				}
			}
		}

		// Delete all chunks
		logger.Infof(ctx, "Deleting all chunks in knowledge base")
		for _, knowledgeID := range knowledgeIDs {
			if err := s.chunkRepo.DeleteChunksByKnowledgeID(ctx, tenantID, knowledgeID); err != nil {
				logger.Warnf(ctx, "Failed to delete chunks for knowledge %s: %v", knowledgeID, err)
			}
		}

		// Delete physical files and adjust storage
		logger.Infof(ctx, "Deleting physical files")
		storageAdjust := int64(0)
		for _, knowledge := range knowledgeList {
			if knowledge.FilePath != "" {
				if err := s.fileSvc.DeleteFile(ctx, knowledge.FilePath); err != nil {
					logger.Warnf(ctx, "Failed to delete file %s: %v", knowledge.FilePath, err)
				}
			}
			storageAdjust -= knowledge.StorageSize
		}
		if storageAdjust != 0 {
			if err := s.tenantRepo.AdjustStorageUsed(ctx, tenantID, storageAdjust); err != nil {
				logger.Warnf(ctx, "Failed to adjust tenant storage: %v", err)
			}
		}

		// Delete knowledge graph data
		logger.Infof(ctx, "Deleting knowledge graph data")
		namespaces := make([]types.NameSpace, 0, len(knowledgeList))
		for _, knowledge := range knowledgeList {
			namespaces = append(namespaces, types.NameSpace{
				KnowledgeBase: knowledge.KnowledgeBaseID,
				Knowledge:     knowledge.ID,
			})
		}
		if s.graphEngine != nil && len(namespaces) > 0 {
			if err := s.graphEngine.DelGraph(ctx, namespaces); err != nil {
				logger.Warnf(ctx, "Failed to delete knowledge graph: %v", err)
			}
		}

		// Delete all knowledge entries from database
		logger.Infof(ctx, "Deleting knowledge entries from database")
		if err := s.kgRepo.DeleteKnowledgeList(ctx, tenantID, knowledgeIDs); err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"knowledge_base_id": kbID,
			})
			return err
		}
	}

	logger.Infof(ctx, "KB delete task completed successfully, knowledge base ID: %s", kbID)
	return nil
}

// SetEmbeddingModel sets the embedding model for a knowledge base
func (s *knowledgeBaseService) SetEmbeddingModel(ctx context.Context, id string, modelID string) error {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return errors.New("knowledge base ID cannot be empty")
	}

	if modelID == "" {
		logger.Error(ctx, "Model ID is empty")
		return errors.New("model ID cannot be empty")
	}

	logger.Infof(ctx, "Setting embedding model for knowledge base, knowledge base ID: %s, model ID: %s", id, modelID)

	// Get the knowledge base
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return err
	}

	// Update the knowledge base's embedding model
	kb.EmbeddingModelID = modelID
	kb.UpdatedAt = time.Now()

	logger.Info(ctx, "Saving knowledge base embedding model update")
	err = s.repo.UpdateKnowledgeBase(ctx, kb)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id":  id,
			"embedding_model_id": modelID,
		})
		return err
	}

	logger.Infof(
		ctx,
		"Knowledge base embedding model set successfully, knowledge base ID: %s, model ID: %s",
		id,
		modelID,
	)
	return nil
}

// CopyKnowledgeBase copies a knowledge base to a new knowledge base (shallow copy).
// Source and target must belong to the tenant in context; cross-tenant access is rejected.
func (s *knowledgeBaseService) CopyKnowledgeBase(ctx context.Context,
	srcKB string, dstKB string,
) (*types.KnowledgeBase, *types.KnowledgeBase, error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	// Load source KB with tenant scope to prevent cross-tenant cloning
	sourceKB, err := s.repo.GetKnowledgeBaseByIDAndTenant(ctx, srcKB, tenantID)
	if err != nil {
		logger.Errorf(ctx, "Get source knowledge base failed: %v", err)
		return nil, nil, err
	}
	sourceKB.EnsureDefaults()
	var targetKB *types.KnowledgeBase
	if dstKB != "" {
		// Load target KB with tenant scope so we only clone into the caller's tenant
		targetKB, err = s.repo.GetKnowledgeBaseByIDAndTenant(ctx, dstKB, tenantID)
		if err != nil {
			return nil, nil, err
		}
	} else {
		var faqConfig *types.FAQConfig
		if sourceKB.FAQConfig != nil {
			cfg := *sourceKB.FAQConfig
			faqConfig = &cfg
		}
		targetKB = &types.KnowledgeBase{
			ID:                    uuid.New().String(),
			Name:                  sourceKB.Name,
			Type:                  sourceKB.Type,
			Description:           sourceKB.Description,
			TenantID:              tenantID,
			ChunkingConfig:        sourceKB.ChunkingConfig,
			ImageProcessingConfig: sourceKB.ImageProcessingConfig,
			EmbeddingModelID:      sourceKB.EmbeddingModelID,
			SummaryModelID:        sourceKB.SummaryModelID,
			VLMConfig:             sourceKB.VLMConfig,
			StorageConfig:         sourceKB.StorageConfig,
			FAQConfig:             faqConfig,
		}
		targetKB.EnsureDefaults()
		if err := s.repo.CreateKnowledgeBase(ctx, targetKB); err != nil {
			return nil, nil, err
		}
	}
	return sourceKB, targetKB, nil
}

// HybridSearch：如何从海量数据中精准地捞出与用户问题最相关的知识。
//
// HybridSearch 执行混合搜索，包括向量检索和关键词检索。
//
// 1. 初始化与上下文准备
//   - 日志记录: 记录搜索参数（KB ID, 查询文本）。
//   - 租户信息获取: 从 Context 中提取当前租户信息，用于后续权限控制和模型选择。
//   - 引擎构建: 根据租户配置动态创建复合检索引擎 (CompositeRetrieveEngine)，决定使用哪些检索器（向量、关键词等）。
//   - 知识库元数据加载: 获取知识库对象 kb，用于判断类型（如是否为 FAQ）和获取嵌入模型 ID。
//   - 召回数量调整: matchCount := params.MatchCount * 3。初始召回数量设置为最终需要数量的 3 倍，以便在融合和去重后仍有足够的候选结果。
//
// 2. 构建检索参数 (多路召回策略)
//   代码根据配置和知识库类型，动态构建检索参数 retrieveParams：
//   A. 向量检索 (Vector Retrieval)
//     - 触发条件: 引擎支持且未禁用向量匹配。
//     - 跨租户模型适配 (关键亮点): 如果知识库属于其他租户（共享场景），系统会自动获取源租户的嵌入模型 (GetEmbeddingModelForTenant)。
//       原因: 向量空间依赖于特定的模型。如果用当前租户的模型去查询源租户的向量库，会导致语义不匹配。此逻辑确保了跨租户搜索的准确性。
//     - 向量化: 调用选定的模型将用户查询文本 (QueryText) 转换为向量 (queryEmbedding)。
//     - FAQ 特殊处理: 如果是 FAQ 类型，标记 KnowledgeType 为 FAQ，以便底层引擎使用专门的 FAQ 索引。
//   B. 关键词检索 (Keyword Retrieval)
//     - 触发条件: 引擎支持、未禁用关键词匹配，且不是 FAQ 类型。
//     - 注意: FAQ 类型通常只依赖向量语义匹配，因此跳过了关键词检索。
//     - 参数构建: 设置查询文本、TopK、阈值等。
//
// 3. 执行检索与结果分类
//   - 并行/批量执行: 调用 retrieveEngine.Retrieve 一次性执行所有构建好的检索任务。
//   - 结果分流: 将返回的结果按类型分离为 vectorResults 和 keywordResults。
//
// 4. 结果融合与排序 (核心算法)
//   采用了两种不同的策略：
//   策略 A: 纯向量结果 (仅向量或 FAQ 场景)
//     - 逻辑: 如果没有关键词结果，直接保留向量的原始相似度得分。
//     - 去重: 使用 ChunkID 作为键，保留每个 Chunk 的最高分（防止 FAQ 中相似问题导致重复）。
//     - 排序: 按原始分数降序排列。
//   策略 B: 混合结果 (RRF - 倒数排名融合)
//     - 场景: 同时存在向量和关键词结果时。
//     - 算法: Reciprocal Rank Fusion (RRF)。
//     - 公式概念: Score = sum( 1 / (k + rank) )，代码中 k = 60。
//     - 原理: 不直接比较不同引擎的原始分数（因为量纲不同，无法直接相加），而是比较排名。如果一个 Chunk 在两个列表中都排名靠前，它的 RRF 分数会非常高。
//     - 执行步骤: 建立排名映射 (vectorRanks, keywordRanks) -> 收集所有唯一的 Chunk -> 计算每个 Chunk 的 RRF 总分 -> 将 RRF 分数写入 info.Score 字段 -> 按 RRF 分数重新排序。
//     - 调试日志: 记录了前 15 个结果的详细排名来源，便于排查问题。
//
// 5. FAQ 特有优化逻辑
//   针对 FAQ 类型，增加了额外的后处理步骤：
//   - 迭代检索 (Iterative Retrieval):
//     - 触发条件: 去重后的结果数量不足 (< params.MatchCount) 且首次向量召回已满。
//     - 目的: FAQ 数据往往存在大量语义相似的问法，导致初次检索去重后数量不够。系统会自动扩大召回范围再次检索，直到凑够数量。
//     - 实现: 调用 s.iterativeRetrieveWithDeduplication。
//   - 负向问题过滤 (Negative Question Filtering):
//     - 触发条件: 非迭代检索路径。
//     - 目的: 排除用户明确不想要的问答对（例如用户问“怎么退款”，系统应排除“如何充值”的相关块，即使它们语义相近）。
//     - 实现: 调用 s.filterByNegativeQuestions。
//
// 6. 最终处理与返回
//   - 截断: 确保最终返回的数量不超过用户请求的 MatchCount。
//   - 上下文增强: 调用 s.processSearchResults。通常会加载父块、相邻块（前后文）或关联元数据，将匹配片段扩展为完整的上下文窗口，以便大模型生成更好的回答。
//
// 设计亮点总结:
//   - 智能的多租户支持: 自动处理跨租户搜索时的模型不一致问题。
//   - 科学的融合算法: 使用 RRF 解决了不同检索引擎分数不可比的问题。
//   - 针对 FAQ 的深度优化: 跳过无效关键词检索、引入迭代机制和负向过滤。
//   - 鲁棒性与性能: 详细的日志记录，初始放大召回倍数 (*3) 以应对去重损耗。

// HybridSearch performs hybrid search, including vector retrieval and keyword retrieval
func (s *knowledgeBaseService) HybridSearch(ctx context.Context,
	id string,
	params types.SearchParams,
) ([]*types.SearchResult, error) {
	logger.Infof(ctx, "Hybrid search parameters, knowledge base ID: %s, query text: %s", id, params.QueryText)

	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	currentTenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// Create a composite retrieval engine with tenant's configured retrievers
	retrieveEngine, err := retriever.NewCompositeRetrieveEngine(s.retrieveEngine, tenantInfo.GetEffectiveEngines())
	if err != nil {
		logger.Errorf(ctx, "Failed to create retrieval engine: %v", err)
		return nil, err
	}

	var retrieveParams []types.RetrieveParams
	var embeddingModel embedding.Embedder
	var kb *types.KnowledgeBase

	kb, err = s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	matchCount := params.MatchCount * 3

	// Add vector retrieval params if supported
	if retrieveEngine.SupportRetriever(types.VectorRetrieverType) && !params.DisableVectorMatch {
		logger.Info(ctx, "Vector retrieval supported, preparing vector retrieval parameters")

		logger.Infof(ctx, "Getting embedding model, model ID: %s", kb.EmbeddingModelID)

		// Check if this is a cross-tenant shared knowledge base
		// For shared KB, we must use the source tenant's embedding model to ensure vector compatibility
		if kb.TenantID != currentTenantID {
			logger.Infof(ctx, "Cross-tenant knowledge base detected, using source tenant's embedding model. KB tenant: %d, current tenant: %d", kb.TenantID, currentTenantID)
			embeddingModel, err = s.modelService.GetEmbeddingModelForTenant(ctx, kb.EmbeddingModelID, kb.TenantID)
		} else {
			embeddingModel, err = s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
		}

		if err != nil {
			logger.Errorf(ctx, "Failed to get embedding model, model ID: %s, error: %v", kb.EmbeddingModelID, err)
			return nil, err
		}
		logger.Infof(ctx, "Embedding model retrieved: %v", embeddingModel)

		// Generate embedding vector for the query text
		logger.Info(ctx, "Starting to generate query embedding")
		queryEmbedding, err := embeddingModel.Embed(ctx, params.QueryText)
		if err != nil {
			logger.Errorf(ctx, "Failed to embed query text, query text: %s, error: %v", params.QueryText, err)
			return nil, err
		}
		logger.Infof(ctx, "Query embedding generated successfully, embedding vector length: %d", len(queryEmbedding))

		vectorParams := types.RetrieveParams{
			Query:            params.QueryText,
			Embedding:        queryEmbedding,
			KnowledgeBaseIDs: []string{id},
			TopK:             matchCount,
			Threshold:        params.VectorThreshold,
			RetrieverType:    types.VectorRetrieverType,
			KnowledgeIDs:     params.KnowledgeIDs,
			TagIDs:           params.TagIDs,
		}

		// For FAQ knowledge base, use FAQ index
		if kb.Type == types.KnowledgeBaseTypeFAQ {
			vectorParams.KnowledgeType = types.KnowledgeTypeFAQ
		}

		retrieveParams = append(retrieveParams, vectorParams)
		logger.Info(ctx, "Vector retrieval parameters setup completed")
	}

	// Add keyword retrieval params if supported and not FAQ
	if retrieveEngine.SupportRetriever(types.KeywordsRetrieverType) && !params.DisableKeywordsMatch &&
		kb.Type != types.KnowledgeBaseTypeFAQ {
		logger.Info(ctx, "Keyword retrieval supported, preparing keyword retrieval parameters")
		retrieveParams = append(retrieveParams, types.RetrieveParams{
			Query:            params.QueryText,
			KnowledgeBaseIDs: []string{id},
			TopK:             matchCount,
			Threshold:        params.KeywordThreshold,
			RetrieverType:    types.KeywordsRetrieverType,
			KnowledgeIDs:     params.KnowledgeIDs,
			TagIDs:           params.TagIDs,
		})
		logger.Info(ctx, "Keyword retrieval parameters setup completed")
	}

	if len(retrieveParams) == 0 {
		logger.Error(ctx, "No retrieval parameters available")
		return nil, errors.New("no retrieve params")
	}

	// Execute retrieval using the configured engines
	logger.Infof(ctx, "Starting retrieval, parameter count: %d", len(retrieveParams))
	retrieveResults, err := retrieveEngine.Retrieve(ctx, retrieveParams)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
			"query_text":        params.QueryText,
		})
		return nil, err
	}

	// Collect all results from different retrievers and deduplicate by chunk ID
	logger.Infof(ctx, "Processing retrieval results")

	// Separate results by retriever type for RRF fusion
	var vectorResults []*types.IndexWithScore
	var keywordResults []*types.IndexWithScore
	for _, retrieveResult := range retrieveResults {
		logger.Infof(ctx, "Retrieval results, engine: %v, retriever: %v, count: %v",
			retrieveResult.RetrieverEngineType,
			retrieveResult.RetrieverType,
			len(retrieveResult.Results),
		)
		if retrieveResult.RetrieverType == types.VectorRetrieverType {
			vectorResults = append(vectorResults, retrieveResult.Results...)
		} else {
			keywordResults = append(keywordResults, retrieveResult.Results...)
		}
	}

	// Early return if no results
	if len(vectorResults) == 0 && len(keywordResults) == 0 {
		logger.Info(ctx, "No search results found")
		return nil, nil
	}
	logger.Infof(ctx, "Result count before fusion: vector=%d, keyword=%d", len(vectorResults), len(keywordResults))

	var deduplicatedChunks []*types.IndexWithScore

	// If only vector results (no keyword results), keep original embedding scores
	// This is important for FAQ search which only uses vector retrieval
	if len(keywordResults) == 0 {
		logger.Info(ctx, "Only vector results, keeping original embedding scores")
		chunkInfoMap := make(map[string]*types.IndexWithScore)
		for _, r := range vectorResults {
			// Keep the highest score for each chunk (FAQ may have multiple similar questions)
			if existing, exists := chunkInfoMap[r.ChunkID]; !exists || r.Score > existing.Score {
				chunkInfoMap[r.ChunkID] = r
			}
		}
		deduplicatedChunks = make([]*types.IndexWithScore, 0, len(chunkInfoMap))
		for _, info := range chunkInfoMap {
			deduplicatedChunks = append(deduplicatedChunks, info)
		}
		slices.SortFunc(deduplicatedChunks, func(a, b *types.IndexWithScore) int {
			if a.Score > b.Score {
				return -1
			} else if a.Score < b.Score {
				return 1
			}
			return 0
		})
		logger.Infof(ctx, "Result count after deduplication: %d", len(deduplicatedChunks))
	} else {
		// Use RRF (Reciprocal Rank Fusion) to merge results from multiple retrievers
		// RRF score = sum(1 / (k + rank)) for each retriever where the chunk appears
		// k=60 is a common choice that works well in practice
		const rrfK = 60

		// Build rank maps for each retriever (already sorted by score from retriever)
		vectorRanks := make(map[string]int)
		for i, r := range vectorResults {
			if _, exists := vectorRanks[r.ChunkID]; !exists {
				vectorRanks[r.ChunkID] = i + 1 // 1-indexed rank
			}
		}
		keywordRanks := make(map[string]int)
		for i, r := range keywordResults {
			if _, exists := keywordRanks[r.ChunkID]; !exists {
				keywordRanks[r.ChunkID] = i + 1 // 1-indexed rank
			}
		}

		// Collect all unique chunks and compute RRF scores
		// Keep the highest score for each chunk from each retriever
		chunkInfoMap := make(map[string]*types.IndexWithScore)
		rrfScores := make(map[string]float64)

		// Process vector results - keep highest score per chunk
		for _, r := range vectorResults {
			if existing, exists := chunkInfoMap[r.ChunkID]; !exists || r.Score > existing.Score {
				chunkInfoMap[r.ChunkID] = r
			}
		}
		// Process keyword results - only add if not already from vector
		for _, r := range keywordResults {
			if _, exists := chunkInfoMap[r.ChunkID]; !exists {
				chunkInfoMap[r.ChunkID] = r
			}
		}

		// Compute RRF scores
		for chunkID := range chunkInfoMap {
			rrfScore := 0.0
			if rank, ok := vectorRanks[chunkID]; ok {
				rrfScore += 1.0 / float64(rrfK+rank)
			}
			if rank, ok := keywordRanks[chunkID]; ok {
				rrfScore += 1.0 / float64(rrfK+rank)
			}
			rrfScores[chunkID] = rrfScore
		}

		// Convert to slice and sort by RRF score
		deduplicatedChunks = make([]*types.IndexWithScore, 0, len(chunkInfoMap))
		for chunkID, info := range chunkInfoMap {
			// Store RRF score in the Score field for downstream processing
			info.Score = rrfScores[chunkID]
			deduplicatedChunks = append(deduplicatedChunks, info)
		}
		slices.SortFunc(deduplicatedChunks, func(a, b *types.IndexWithScore) int {
			if a.Score > b.Score {
				return -1
			} else if a.Score < b.Score {
				return 1
			}
			return 0
		})

		logger.Infof(ctx, "Result count after RRF fusion: %d", len(deduplicatedChunks))

		// Log top results after RRF fusion for debugging
		for i, chunk := range deduplicatedChunks {
			if i < 15 {
				vRank, vOk := vectorRanks[chunk.ChunkID]
				kRank, kOk := keywordRanks[chunk.ChunkID]
				logger.Debugf(ctx, "RRF rank %d: chunk_id=%s, rrf_score=%.6f, vector_rank=%v(%v), keyword_rank=%v(%v)",
					i, chunk.ChunkID, chunk.Score, vRank, vOk, kRank, kOk)
			}
		}
	}

	kb.EnsureDefaults()

	// Check if we need iterative retrieval for FAQ with separate indexing
	// Only use iterative retrieval if we don't have enough unique chunks after first deduplication
	needsIterativeRetrieval := len(deduplicatedChunks) < params.MatchCount &&
		kb.Type == types.KnowledgeBaseTypeFAQ && len(vectorResults) == matchCount
	if needsIterativeRetrieval {
		logger.Info(ctx, "Not enough unique chunks, using iterative retrieval for FAQ")
		// Use iterative retrieval to get more unique chunks (with negative question filtering inside)
		deduplicatedChunks = s.iterativeRetrieveWithDeduplication(
			ctx,
			retrieveEngine,
			retrieveParams,
			params.MatchCount,
			params.QueryText,
		)
	} else if kb.Type == types.KnowledgeBaseTypeFAQ {
		// Filter by negative questions if not using iterative retrieval
		deduplicatedChunks = s.filterByNegativeQuestions(ctx, deduplicatedChunks, params.QueryText)
		logger.Infof(ctx, "Result count after negative question filtering: %d", len(deduplicatedChunks))
	}

	// Limit to MatchCount
	if len(deduplicatedChunks) > params.MatchCount {
		deduplicatedChunks = deduplicatedChunks[:params.MatchCount]
	}

	return s.processSearchResults(ctx, deduplicatedChunks)
}

// 在 RAG 系统中，FAQ 往往存在“多个相似问题指向同一个答案”的情况，
// 简单的 Top-K 检索经过按答案去重后，往往会导致返回结果数量不足。
// 该函数通过循环迭代、动态扩大搜索范围、结合负向过滤的方式解决了这一痛点。

// Q: 什么是负向问题?
//
// 在知识库（尤其是 FAQ 问答对）的设计中，负向问题是一种用来精准纠错的机制。
// 简单来说，它是为了告诉 AI：“如果用户问这些看起来很像的问题，千万不要用这个答案去回答。”
//
// 在 AI 检索（向量检索）中，模型是基于“语义相似度”来找答案的。
// 但有时候，语义相似并不代表意图相同，甚至可能完全相反。
//
// 举个例子：
//	标准问题： “如何开启自动续费？”
//	标准答案： “请进入设置页面，点击‘开启续费’按钮...”
//	用户提问： “如何关闭自动续费？”
//	AI 误判：
//		由于“开启自动续费”和“关闭自动续费”这两个句子的关键词几乎一样（只有一字之差），
//		在向量空间里，它们的距离非常近。AI 很可能会把“开启”的答案推送给想“关闭”的用户。
//
//
// 为了解决上述误判，我们会在“如何开启自动续费”这个知识点下面，配置一个负向问题：
//	负向问题： “如何关闭自动续费”、“取消续费”、“不想要自动续费了”
// 检索逻辑就变成了：
//	第一步（模糊匹配）： AI 发现用户问“如何关闭续费”，和“如何开启续费”很像，准备召回这个答案。
//	第二步（精准排除）： 系统检查该答案关联的“负向问题”列表。
//	第三步（逻辑判断）： 发现用户问的内容命中了负向问题列表。
//	结果：系统判定这是一个“陷阱”，立即剔除这个错误的召回结果，去寻找更合适的答案（或者直接告诉用户没搜到）。
//
//
// Q: 如何发现用户问的内容命中了负向问题列表？
//
// 1. 文本精确匹配与模糊匹配（最常用）
//	这是最简单直接的方法，通常在代码层面进行。
//	 - 归一化处理：将用户的提问和负向问题列表全部转为小写（strings.ToLower），去掉首尾空格、标点符号。
//	 - 包含/子串判定：检查用户的核心词是否出现在负向问题中。例如，负向问题是“如何关闭”，用户问“我想知道如何关闭续费”，代码通过 strings.Contains 就能识别出冲突。
//	 - 逻辑：如果用户的问题与负向列表中的任意一项高度相似，该函数就返回 true，触发剔除逻辑。
//
// 2. 向量余弦相似度（语义命中）
//	由于用户不可能每次都问得和负向问题一模一样，系统通常会给负向问题也做一套“小规模向量检索”：
//	 - 预计算：在后台配置 FAQ 时，系统不仅把“标准问题”向量化，也把“负向问题”向量化存储。
//	 - 实时比对：
//		1. 用户问：“我不想再用自动扣费了”。
//		2. 系统匹配到知识点 A（标准问：如何开启自动续费）。
//		3. 系统计算用户提问与 A 的负向问题（如：“取消扣费”）的向量相似度。
//		4. 如果相似度极高（例如 > 0.9），说明命中负向意图，判定为“误召回”。
//
// 3. LLM 意图反转判定（最智能，但开销大）
//	在更高级的系统中，会利用大模型（LLM）来做最后的“裁判”：
//	 - Prompt ：
//	 	给 LLM 输入：
//	 	   “用户问题：‘如何关闭续费’。
//	 		召回的知识点标题：‘开启续费流程’。
//	 		已知该知识点的排除意图（负向问题）包含‘关闭、取消’。
//	 		请问用户的问题是否属于排除意图？”
//	 - 判定：
//	 	LLM 会理解“开启”和“关闭”是反义词，从而给出 Match 的结论，系统据此丢弃该结果。

// 如果用户问了一个问题，第一波搜出来的 10 条里有 8 条因为“命中负向问题”被删了，那结果就不够了。
// 所以代码会自动开启下一轮搜寻（TopK 翻倍），直到绕过这些“负向陷阱”，找到真正正确的、没被过滤掉的答案。

// iterativeRetrieveWithDeduplication 执行迭代检索，直到找到足够数量的唯一分片。
// 此方法主要用于 FAQ 知识库的独立索引模式，在每次迭代后应用负向问题过滤并配合分片数据缓存。
//
// 1. 初始化与变量定义
//   - 最大迭代次数 (maxIterations = 5)：防止在极端情况下进入死循环，平衡了检索深度与系统响应时间。
//   - 起始召回量 (currentTopK)：从初始召回量的 3 倍开始，后续每轮迭代以 2 倍速度指数级增长（currentTopK *= 2）。
//   - 缓存机制 (chunkDataCache)：引入内存缓存记录已获取的 Chunk 数据，避免在循环中对数据库进行重复的 I/O 操作，显著提升性能。
//   - 状态追踪：分别使用 uniqueChunks 存储有效结果，使用 filteredOutChunks 记录因命中负向问题而被剔除的黑名单。
//
//  2. 迭代检索循环 (核心流程)
//     函数进入一个最多 5 次的循环，每轮执行以下操作：
//     - 参数更新：将当前的 currentTopK 注入检索参数，试图从向量库中捞取更多的候选条目。
//     - 执行检索：调用 retrieveEngine.Retrieve 获取原始结果。
//     - 批量数据补全 (Batch Fetch)：
//     	 - 识别出当前结果中哪些 ChunkID 既不在缓存中，也没被过滤过。
//       - 通过 chunkRepo.ListChunksByID 批量从数据库拉取元数据（Metadata），这是为了获取 FAQ 的“负向问题”字段。
//
//  3. 一次性处理：去重、过滤与合并
//     在每轮迭代的结果处理中，系统在一次遍历内完成了三件事：
//     - 负向问题过滤 (Negative Filter)：
//     	 - 解析 Chunk 的 FAQ 元数据，检查用户当前提问是否命中了该知识点的 NegativeQuestions（即“不该回答该问题的反例”）。
//     	 - 如果命中，将该 ID 加入 filteredOutChunks 黑名单，并从现有结果中剔除。
//     - 语义去重：确保每个 ChunkID 即使对应多个相似问法，也只保留一个记录。
//     - 分值优化：在去重过程中，始终保留该 Chunk 在多路检索中出现的最高分。
//
//  4. 退出机制 (Early Stop)
//     为了保证效率，函数设置了两个聪明的“早停”条件：
//     - 数量达标：一旦有效去重后的结果数 >= matchCount，立即结束迭代，返回结果。
//     - 数据耗尽：如果某次检索回来的条数少于请求的 currentTopK，说明向量数据库里已经没有更多相关数据了，强行继续迭代也没有意义，直接退出。
//
// 5. 排序与返回
//   - 最后将 Map 转换回切片，并严格按照分数（Score）进行降序排列，确保交给大模型的是相关度最高的内容。
//
// 设计亮点总结:
//   - 指数级探测策略：通过 currentTopK *= 2 快速扩大搜索半径，能够以最少的迭代次数找到足够的不重复结果。
//   - 性能优异：利用 chunkDataCache 和批量查询（Batch Fetch）将数据库压力降至最低，非常适合高并发的生产环境。
//   - 语义精准度：将“负向过滤”深度嵌入到迭代过程中，确保即便在扩大搜索范围时，也不会引入含义截然相反的错误答案。

// iterativeRetrieveWithDeduplication performs iterative retrieval until enough unique chunks are found
// This is used for FAQ knowledge bases with separate indexing mode
// Negative question filtering is applied after each iteration with chunk data caching
func (s *knowledgeBaseService) iterativeRetrieveWithDeduplication(ctx context.Context,
	retrieveEngine *retriever.CompositeRetrieveEngine,
	retrieveParams []types.RetrieveParams,
	matchCount int,
	queryText string,
) []*types.IndexWithScore {
	maxIterations := 5
	// Start with a larger TopK since we're called when first retrieval wasn't enough
	// The first retrieval already used matchCount*3, so start from there
	currentTopK := matchCount * 3
	uniqueChunks := make(map[string]*types.IndexWithScore)
	// Cache chunk data to avoid repeated DB queries across iterations
	chunkDataCache := make(map[string]*types.Chunk)
	// Track chunks that have been filtered out by negative questions
	filteredOutChunks := make(map[string]struct{})

	queryTextLower := strings.ToLower(strings.TrimSpace(queryText))
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	for i := 0; i < maxIterations; i++ {
		// Update TopK in retrieve params
		updatedParams := make([]types.RetrieveParams, len(retrieveParams))
		for j := range retrieveParams {
			updatedParams[j] = retrieveParams[j]
			updatedParams[j].TopK = currentTopK
		}

		// Execute retrieval
		retrieveResults, err := retrieveEngine.Retrieve(ctx, updatedParams)
		if err != nil {
			logger.Warnf(ctx, "Iterative retrieval failed at iteration %d: %v", i+1, err)
			break
		}

		// Collect results
		iterationResults := []*types.IndexWithScore{}
		for _, retrieveResult := range retrieveResults {
			iterationResults = append(iterationResults, retrieveResult.Results...)
		}

		if len(iterationResults) == 0 {
			logger.Infof(ctx, "No results found at iteration %d", i+1)
			break
		}

		totalRetrieved := len(iterationResults)

		// Collect new chunk IDs that need to be fetched from DB
		newChunkIDs := make([]string, 0)
		for _, result := range iterationResults {
			if _, cached := chunkDataCache[result.ChunkID]; !cached {
				if _, filtered := filteredOutChunks[result.ChunkID]; !filtered {
					newChunkIDs = append(newChunkIDs, result.ChunkID)
				}
			}
		}

		// Batch fetch only new chunks
		if len(newChunkIDs) > 0 {
			newChunks, err := s.chunkRepo.ListChunksByID(ctx, tenantID, newChunkIDs)
			if err != nil {
				logger.Warnf(ctx, "Failed to fetch chunks at iteration %d: %v", i+1, err)
			} else {
				for _, chunk := range newChunks {
					chunkDataCache[chunk.ID] = chunk
				}
			}
		}

		// Deduplicate, merge, and filter in one pass
		for _, result := range iterationResults {
			// Skip if already filtered out
			if _, filtered := filteredOutChunks[result.ChunkID]; filtered {
				continue
			}

			// Check negative questions using cached data
			if chunkData, ok := chunkDataCache[result.ChunkID]; ok {
				if chunkData.ChunkType == types.ChunkTypeFAQ {
					if meta, err := chunkData.FAQMetadata(); err == nil && meta != nil {
						if s.matchesNegativeQuestions(queryTextLower, meta.NegativeQuestions) {
							filteredOutChunks[result.ChunkID] = struct{}{}
							delete(uniqueChunks, result.ChunkID)
							continue
						}
					}
				}
			}

			// Keep highest score for each chunk
			if existing, ok := uniqueChunks[result.ChunkID]; !ok || result.Score > existing.Score {
				uniqueChunks[result.ChunkID] = result
			}
		}

		logger.Infof(
			ctx,
			"After iteration %d: retrieved %d results, found %d valid unique chunks (target: %d)",
			i+1,
			totalRetrieved,
			len(uniqueChunks),
			matchCount,
		)

		// Early stop: Check if we have enough unique chunks after deduplication and filtering
		if len(uniqueChunks) >= matchCount {
			logger.Infof(ctx, "Found enough unique chunks after %d iterations", i+1)
			break
		}

		// Early stop: If we got fewer results than TopK, there are no more results to retrieve
		if totalRetrieved < currentTopK {
			logger.Infof(ctx, "No more results available (got %d < %d), stopping iteration", totalRetrieved, currentTopK)
			break
		}

		// Increase TopK for next iteration
		currentTopK *= 2
	}

	// Convert map to slice and sort by score
	result := make([]*types.IndexWithScore, 0, len(uniqueChunks))
	for _, chunk := range uniqueChunks {
		result = append(result, chunk)
	}

	// Sort by score descending
	slices.SortFunc(result, func(a, b *types.IndexWithScore) int {
		if a.Score > b.Score {
			return -1
		} else if a.Score < b.Score {
			return 1
		}
		return 0
	})

	logger.Infof(ctx, "Iterative retrieval completed: %d unique chunks found after filtering", len(result))
	return result
}

// filterByNegativeQuestions filters out chunks that match negative questions for FAQ knowledge bases.
func (s *knowledgeBaseService) filterByNegativeQuestions(ctx context.Context,
	chunks []*types.IndexWithScore,
	queryText string,
) []*types.IndexWithScore {
	if len(chunks) == 0 {
		return chunks
	}

	queryTextLower := strings.ToLower(strings.TrimSpace(queryText))
	if queryTextLower == "" {
		return chunks
	}

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// Collect chunk IDs
	chunkIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		chunkIDs = append(chunkIDs, chunk.ChunkID)
	}

	// Batch fetch chunks to get negative questions
	allChunks, err := s.chunkRepo.ListChunksByID(ctx, tenantID, chunkIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to fetch chunks for negative question filtering: %v", err)
		// If we can't fetch chunks, return original results
		return chunks
	}

	// Build chunk map for quick lookup
	chunkMap := make(map[string]*types.Chunk, len(allChunks))
	for _, chunk := range allChunks {
		chunkMap[chunk.ID] = chunk
	}

	// Filter out chunks that match negative questions
	filteredChunks := make([]*types.IndexWithScore, 0, len(chunks))
	for _, chunk := range chunks {
		chunkData, ok := chunkMap[chunk.ChunkID]
		if !ok {
			// If chunk not found, keep it (shouldn't happen, but be safe)
			filteredChunks = append(filteredChunks, chunk)
			continue
		}

		// Only filter FAQ type chunks
		if chunkData.ChunkType != types.ChunkTypeFAQ {
			filteredChunks = append(filteredChunks, chunk)
			continue
		}

		// Get FAQ metadata and check negative questions
		meta, err := chunkData.FAQMetadata()
		if err != nil || meta == nil {
			// If we can't parse metadata, keep the chunk
			filteredChunks = append(filteredChunks, chunk)
			continue
		}

		// Check if query matches any negative question
		if s.matchesNegativeQuestions(queryTextLower, meta.NegativeQuestions) {
			logger.Debugf(ctx, "Filtered FAQ chunk %s due to negative question match", chunk.ChunkID)
			continue
		}

		// Keep the chunk
		filteredChunks = append(filteredChunks, chunk)
	}

	return filteredChunks
}

// matchesNegativeQuestions checks if the query text matches any negative questions.
// Returns true if the query matches any negative question, false otherwise.
func (s *knowledgeBaseService) matchesNegativeQuestions(queryTextLower string, negativeQuestions []string) bool {
	if len(negativeQuestions) == 0 {
		return false
	}

	for _, negativeQ := range negativeQuestions {
		negativeQLower := strings.ToLower(strings.TrimSpace(negativeQ))
		if negativeQLower == "" {
			continue
		}
		// Check if query text is exactly the same as the negative question
		if queryTextLower == negativeQLower {
			return true
		}
	}
	return false
}

// processSearchResults handles the processing of search results, optimizing database queries
func (s *knowledgeBaseService) processSearchResults(ctx context.Context,
	chunks []*types.IndexWithScore,
) ([]*types.SearchResult, error) {
	if len(chunks) == 0 {
		return nil, nil
	}

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// Prepare data structures for efficient processing
	var knowledgeIDs []string
	var chunkIDs []string
	chunkScores := make(map[string]float64)
	chunkMatchTypes := make(map[string]types.MatchType)
	chunkMatchedContents := make(map[string]string)
	processedKnowledgeIDs := make(map[string]bool)

	// Collect all knowledge and chunk IDs
	for _, chunk := range chunks {
		if !processedKnowledgeIDs[chunk.KnowledgeID] {
			knowledgeIDs = append(knowledgeIDs, chunk.KnowledgeID)
			processedKnowledgeIDs[chunk.KnowledgeID] = true
		}

		chunkIDs = append(chunkIDs, chunk.ChunkID)
		chunkScores[chunk.ChunkID] = chunk.Score
		chunkMatchTypes[chunk.ChunkID] = chunk.MatchType
		chunkMatchedContents[chunk.ChunkID] = chunk.Content
	}

	// Batch fetch knowledge data (include shared KB so cross-tenant retrieval works)
	logger.Infof(ctx, "Fetching knowledge data for %d IDs", len(knowledgeIDs))
	knowledgeMap, err := s.fetchKnowledgeDataWithShared(ctx, tenantID, knowledgeIDs)
	if err != nil {
		return nil, err
	}

	// Batch fetch chunks (include shared KB chunks: first by tenant, then by ID-only for missing with permission check)
	logger.Infof(ctx, "Fetching chunk data for %d IDs", len(chunkIDs))
	allChunks, err := s.listChunksByIDWithShared(ctx, tenantID, chunkIDs)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
			"chunk_ids": chunkIDs,
		})
		return nil, err
	}
	logger.Infof(ctx, "Chunk data fetched successfully, count: %d", len(allChunks))

	// Build chunk map and collect additional IDs to fetch
	chunkMap := make(map[string]*types.Chunk, len(allChunks))
	var additionalChunkIDs []string
	processedChunkIDs := make(map[string]bool)

	// First pass: Build chunk map and collect parent IDs
	for _, chunk := range allChunks {
		chunkMap[chunk.ID] = chunk
		processedChunkIDs[chunk.ID] = true

		// Collect parent chunks
		if chunk.ParentChunkID != "" && !processedChunkIDs[chunk.ParentChunkID] {
			additionalChunkIDs = append(additionalChunkIDs, chunk.ParentChunkID)
			processedChunkIDs[chunk.ParentChunkID] = true

			// Pass score to parent
			chunkScores[chunk.ParentChunkID] = chunkScores[chunk.ID]
			chunkMatchTypes[chunk.ParentChunkID] = types.MatchTypeParentChunk
		}

		// Collect related chunks
		relationChunkIDs := s.collectRelatedChunkIDs(chunk, processedChunkIDs)
		for _, chunkID := range relationChunkIDs {
			additionalChunkIDs = append(additionalChunkIDs, chunkID)
			chunkMatchTypes[chunkID] = types.MatchTypeRelationChunk
		}

		// Add nearby chunks (prev and next)
		if slices.Contains([]string{types.ChunkTypeText}, chunk.ChunkType) {
			if chunk.NextChunkID != "" && !processedChunkIDs[chunk.NextChunkID] {
				additionalChunkIDs = append(additionalChunkIDs, chunk.NextChunkID)
				processedChunkIDs[chunk.NextChunkID] = true
				chunkMatchTypes[chunk.NextChunkID] = types.MatchTypeNearByChunk
			}
			if chunk.PreChunkID != "" && !processedChunkIDs[chunk.PreChunkID] {
				additionalChunkIDs = append(additionalChunkIDs, chunk.PreChunkID)
				processedChunkIDs[chunk.PreChunkID] = true
				chunkMatchTypes[chunk.PreChunkID] = types.MatchTypeNearByChunk
			}
		}
	}

	// Fetch all additional chunks in one go if needed (include shared KB)
	if len(additionalChunkIDs) > 0 {
		logger.Infof(ctx, "Fetching %d additional chunks", len(additionalChunkIDs))
		additionalChunks, err := s.listChunksByIDWithShared(ctx, tenantID, additionalChunkIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to fetch some additional chunks: %v", err)
			// Continue with what we have
		} else {
			// Add to chunk map
			for _, chunk := range additionalChunks {
				chunkMap[chunk.ID] = chunk
			}
		}
	}

	// Build final search results - preserve original order from input chunks
	var searchResults []*types.SearchResult
	addedChunkIDs := make(map[string]bool)

	// First pass: Add results in the original order from input chunks
	for _, inputChunk := range chunks {
		chunk, exists := chunkMap[inputChunk.ChunkID]
		if !exists {
			logger.Debugf(ctx, "Chunk not found in chunkMap: %s", inputChunk.ChunkID)
			continue
		}
		if !s.isValidTextChunk(chunk) {
			logger.Debugf(ctx, "Chunk is not valid text chunk: %s, type: %s", chunk.ID, chunk.ChunkType)
			continue
		}
		if addedChunkIDs[chunk.ID] {
			continue
		}

		score := chunkScores[chunk.ID]
		if knowledge, ok := knowledgeMap[chunk.KnowledgeID]; ok {
			matchType := chunkMatchTypes[chunk.ID]
			matchedContent := chunkMatchedContents[chunk.ID]
			searchResults = append(searchResults, s.buildSearchResult(chunk, knowledge, score, matchType, matchedContent))
			addedChunkIDs[chunk.ID] = true
		} else {
			logger.Warnf(ctx, "Knowledge not found for chunk: %s, knowledge_id: %s", chunk.ID, chunk.KnowledgeID)
		}
	}

	// Second pass: Add additional chunks (parent, nearby, relation) that weren't in original input
	for chunkID, chunk := range chunkMap {
		if addedChunkIDs[chunkID] || !s.isValidTextChunk(chunk) {
			continue
		}

		score, hasScore := chunkScores[chunkID]
		if !hasScore || score <= 0 {
			score = 0.0
		}

		if knowledge, ok := knowledgeMap[chunk.KnowledgeID]; ok {
			matchType := types.MatchTypeParentChunk
			if specificType, exists := chunkMatchTypes[chunkID]; exists {
				matchType = specificType
			} else {
				logger.Warnf(ctx, "Unkonwn match type for chunk: %s", chunkID)
				continue
			}
			matchedContent := chunkMatchedContents[chunkID]
			searchResults = append(searchResults, s.buildSearchResult(chunk, knowledge, score, matchType, matchedContent))
		}
	}
	logger.Infof(ctx, "Search results processed, total: %d", len(searchResults))
	return searchResults, nil
}

// collectRelatedChunkIDs extracts related chunk IDs from a chunk
func (s *knowledgeBaseService) collectRelatedChunkIDs(chunk *types.Chunk, processedIDs map[string]bool) []string {
	var relatedIDs []string
	// Process direct relations
	if len(chunk.RelationChunks) > 0 {
		var relations []string
		if err := json.Unmarshal(chunk.RelationChunks, &relations); err == nil {
			for _, id := range relations {
				if !processedIDs[id] {
					relatedIDs = append(relatedIDs, id)
					processedIDs[id] = true
				}
			}
		}
	}
	return relatedIDs
}

// buildSearchResult creates a search result from chunk and knowledge
func (s *knowledgeBaseService) buildSearchResult(chunk *types.Chunk,
	knowledge *types.Knowledge,
	score float64,
	matchType types.MatchType,
	matchedContent string,
) *types.SearchResult {
	return &types.SearchResult{
		ID:                chunk.ID,
		Content:           chunk.Content,
		KnowledgeID:       chunk.KnowledgeID,
		ChunkIndex:        chunk.ChunkIndex,
		KnowledgeTitle:    knowledge.Title,
		StartAt:           chunk.StartAt,
		EndAt:             chunk.EndAt,
		Seq:               chunk.ChunkIndex,
		Score:             score,
		MatchType:         matchType,
		Metadata:          knowledge.GetMetadata(),
		ChunkType:         string(chunk.ChunkType),
		ParentChunkID:     chunk.ParentChunkID,
		ImageInfo:         chunk.ImageInfo,
		KnowledgeFilename: knowledge.FileName,
		KnowledgeSource:   knowledge.Source,
		ChunkMetadata:     chunk.Metadata,
		MatchedContent:    matchedContent,
	}
}

// isValidTextChunk checks if a chunk is a valid text chunk
func (s *knowledgeBaseService) isValidTextChunk(chunk *types.Chunk) bool {
	return slices.Contains([]types.ChunkType{
		types.ChunkTypeText, types.ChunkTypeSummary,
		types.ChunkTypeTableColumn, types.ChunkTypeTableSummary,
		types.ChunkTypeFAQ,
	}, chunk.ChunkType)
}

// fetchKnowledgeData gets knowledge data in batch
func (s *knowledgeBaseService) fetchKnowledgeData(ctx context.Context,
	tenantID uint64,
	knowledgeIDs []string,
) (map[string]*types.Knowledge, error) {
	knowledges, err := s.kgRepo.GetKnowledgeBatch(ctx, tenantID, knowledgeIDs)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id":     tenantID,
			"knowledge_ids": knowledgeIDs,
		})
		return nil, err
	}

	knowledgeMap := make(map[string]*types.Knowledge, len(knowledges))
	for _, knowledge := range knowledges {
		knowledgeMap[knowledge.ID] = knowledge
	}

	return knowledgeMap, nil
}

// fetchKnowledgeDataWithShared gets knowledge data in batch, including knowledge from shared KBs the user has access to.
func (s *knowledgeBaseService) fetchKnowledgeDataWithShared(ctx context.Context,
	tenantID uint64,
	knowledgeIDs []string,
) (map[string]*types.Knowledge, error) {
	knowledges, err := s.kgRepo.GetKnowledgeBatch(ctx, tenantID, knowledgeIDs)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id":     tenantID,
			"knowledge_ids": knowledgeIDs,
		})
		return nil, err
	}

	knowledgeMap := make(map[string]*types.Knowledge, len(knowledges))
	for _, k := range knowledges {
		knowledgeMap[k.ID] = k
	}

	// Count how many IDs are missing (not found in current tenant)
	var missingIDs []string
	for _, id := range knowledgeIDs {
		if knowledgeMap[id] == nil {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) == 0 {
		return knowledgeMap, nil
	}
	logger.Infof(ctx, "[fetchKnowledgeDataWithShared] %d knowledge IDs not found in current tenant, attempting shared KB lookup", len(missingIDs))

	userIDVal := ctx.Value(types.UserIDContextKey)
	if userIDVal == nil {
		logger.Warnf(ctx, "[fetchKnowledgeDataWithShared] userID not found in context, skipping shared KB lookup")
		return knowledgeMap, nil
	}
	userID, ok := userIDVal.(string)
	if !ok || userID == "" {
		logger.Warnf(ctx, "[fetchKnowledgeDataWithShared] userID is empty, skipping shared KB lookup")
		return knowledgeMap, nil
	}

	logger.Infof(ctx, "[fetchKnowledgeDataWithShared] Looking up %d missing knowledge IDs with userID=%s", len(missingIDs), userID)
	for _, id := range missingIDs {
		k, err := s.kgRepo.GetKnowledgeByIDOnly(ctx, id)
		if err != nil || k == nil || k.KnowledgeBaseID == "" {
			logger.Debugf(ctx, "[fetchKnowledgeDataWithShared] Knowledge %s not found or has no KB", id)
			continue
		}
		hasPermission, err := s.kbShareService.HasKBPermission(ctx, k.KnowledgeBaseID, userID, types.OrgRoleViewer)
		if err != nil {
			logger.Debugf(ctx, "[fetchKnowledgeDataWithShared] Permission check error for KB %s: %v", k.KnowledgeBaseID, err)
			continue
		}
		if !hasPermission {
			logger.Debugf(ctx, "[fetchKnowledgeDataWithShared] No permission for KB %s", k.KnowledgeBaseID)
			continue
		}
		logger.Debugf(ctx, "[fetchKnowledgeDataWithShared] Found shared knowledge %s in KB %s", id, k.KnowledgeBaseID)
		knowledgeMap[k.ID] = k
	}

	logger.Infof(ctx, "[fetchKnowledgeDataWithShared] After shared lookup, total knowledge found: %d", len(knowledgeMap))
	return knowledgeMap, nil
}

// listChunksByIDWithShared fetches chunks by IDs, including chunks from shared KBs the user has access to.
func (s *knowledgeBaseService) listChunksByIDWithShared(ctx context.Context,
	tenantID uint64,
	chunkIDs []string,
) ([]*types.Chunk, error) {
	chunks, err := s.chunkRepo.ListChunksByID(ctx, tenantID, chunkIDs)
	if err != nil {
		return nil, err
	}

	foundSet := make(map[string]bool)
	for _, c := range chunks {
		if c != nil {
			foundSet[c.ID] = true
		}
	}

	var missing []string
	for _, id := range chunkIDs {
		if !foundSet[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return chunks, nil
	}
	logger.Infof(ctx, "[listChunksByIDWithShared] %d chunks not found in current tenant, attempting shared KB lookup", len(missing))

	userIDVal := ctx.Value(types.UserIDContextKey)
	if userIDVal == nil {
		logger.Warnf(ctx, "[listChunksByIDWithShared] userID not found in context, skipping shared KB lookup")
		return chunks, nil
	}
	userID, ok := userIDVal.(string)
	if !ok || userID == "" {
		logger.Warnf(ctx, "[listChunksByIDWithShared] userID is empty, skipping shared KB lookup")
		return chunks, nil
	}

	logger.Infof(ctx, "[listChunksByIDWithShared] Looking up %d missing chunks with userID=%s", len(missing), userID)
	crossChunks, err := s.chunkRepo.ListChunksByIDOnly(ctx, missing)
	if err != nil {
		logger.Warnf(ctx, "[listChunksByIDWithShared] Failed to fetch chunks by ID only: %v", err)
		return chunks, nil
	}
	logger.Infof(ctx, "[listChunksByIDWithShared] Found %d chunks without tenant filter", len(crossChunks))

	for _, c := range crossChunks {
		if c == nil || c.KnowledgeBaseID == "" {
			continue
		}
		hasPermission, err := s.kbShareService.HasKBPermission(ctx, c.KnowledgeBaseID, userID, types.OrgRoleViewer)
		if err != nil {
			logger.Debugf(ctx, "[listChunksByIDWithShared] Permission check error for KB %s: %v", c.KnowledgeBaseID, err)
			continue
		}
		if !hasPermission {
			logger.Debugf(ctx, "[listChunksByIDWithShared] No permission for KB %s", c.KnowledgeBaseID)
			continue
		}
		chunks = append(chunks, c)
	}

	logger.Infof(ctx, "[listChunksByIDWithShared] After shared lookup, total chunks: %d", len(chunks))
	return chunks, nil
}
