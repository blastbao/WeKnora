package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// pgRepository implements PostgreSQL-based retrieval operations
type pgRepository struct {
	db *gorm.DB // Database connection
}

// NewPostgresRetrieveEngineRepository creates a new PostgreSQL retriever repository
func NewPostgresRetrieveEngineRepository(db *gorm.DB) interfaces.RetrieveEngineRepository {
	logger.GetLogger(context.Background()).Info("[Postgres] Initializing PostgreSQL retriever engine repository")
	return &pgRepository{db: db}
}

// EngineType returns the retriever engine type (PostgreSQL)
func (r *pgRepository) EngineType() types.RetrieverEngineType {
	return types.PostgresRetrieverEngineType
}

// Support returns supported retriever types (keywords and vector)
func (r *pgRepository) Support() []types.RetrieverType {
	return []types.RetrieverType{types.KeywordsRetrieverType, types.VectorRetrieverType}
}

// calculateIndexStorageSize 计算单个向量索引条目的存储大小（字节数）
//
// 该函数用于估算 PostgreSQL 中存储一个向量索引条目所需的总磁盘空间，
// 包括文本内容、向量数据、元数据开销以及 HNSW 索引结构的开销。
//
// 存储组成：
//   1. 文本内容：实际字符的字节长度
//   2. 向量数据：维度数 × 2 字节（半精度浮点数 halfvec）
//   3. 元数据：固定 200 字节（ID、时间戳等字段开销）
//   4. HNSW 索引：向量大小的 2 倍（近似值）
//
// 参数：
//   - embeddingDB: 向量索引数据库模型指针，包含内容、维度等信息
//
// 返回：
//   - int64: 该索引条目占用的总字节数
//
// 注意：
//   - 使用半精度浮点数（halfvec）而非全精度，可节省 50% 存储
//   - HNSW 索引开销为经验估算值，实际大小取决于索引参数（m/ef_construction）
//   - 元数据 200 字节为经验值，覆盖 UUID、时间戳、外键等字段
//
// 示例：
//   对于 768 维向量 + 1000 字符内容：
//   - 内容：1000 字节
//   - 向量：768 × 2 = 1536 字节
//   - 元数据：200 字节
//   - 索引：1536 × 2 = 3072 字节
//   - 总计：~5.8 KB
//
// 	768 维向量 = 一个包含 768 个浮点数的数组，存储着 768 个特征值：
//		vector := [768]float32{
//		   0.0234, -0.1567, 0.0891, ...,
//		   ...,
//		   0.0456, -0.0789, 0.1234
//		}
//
//  768 维向量的每个数字代表文本的一个语义特征：
//		- 第1个数：可能代表"情感极性"（正/负）
//		- 第50个数：可能代表"专业程度"
//		- 第200个数：可能代表"时间相关性"
//		...
//		- 第768个数：某个抽象语义特征
//
//
// 768 维是 NLP 领域的"黄金标准"维度
//		足够表达丰富的语义信息
//		计算和存储成本可控（1.5KB/向量）
//		与主流模型（BERT 家族）兼容
//		HNSW 索引在此维度上效率良好
//
//	信息容量
//
//		每个 float32 有 2^32 ≈ 4.3 × 10^9 种可能的位模式
//		768 维向量的总状态数 = (2^32)^768 = 2^(32×768) = 2^24576
//		换算为 10 的幂：2^24576 = 10^(24576 × log10(2)) ≈ 10^(24576 × 0.301) ≈ 10^7400
//		考虑有效数值范围（浮点分布密度），实际有意义的数值组合 ≈ 10^4000
//		简单说：768 个浮点数，每个有 40 亿种可能，组合起来是天文数字。
//
//
//		理论状态空间: 10^4000  ← 数学上的可能性
//                    ↓
//            神经网络表达能力限制
//                    ↓
//		实际稳定编码: 10^6 ~ 10^7  ← 工程上的可用性
//
//
//	  	因此，768 个浮点数 × 4 字节（float32）= 3 KB 原始数据，理论上可编码的语义信息量：
//		- 理论上：区分 ~10^4000 种不同的语义状态（天文数字）
//		- 实际上：可稳定编码数百万到数千万级别的语义差异
//
//
//  维度          理论状态空间         实际稳定编码          类比
//	─────────────────────────────────────────────────────────────
//	2 维          10^19              10^2               平面地图城市
//	128 维        10^1200            10^5               小型知识库
//	768 维        10^4000            10^6~10^7          维基百科核心概念
//	1536 维       10^8000            10^7~10^8          大型知识图谱
//
// 实际业务中，768 维支撑的知识库规模：
// 	- 小型: 10万 文档 → 向量存储 ~150MB
// 	- 中型: 100万 文档 → 向量存储 ~1.5GB
// 	- 大型: 1000万 文档 → 向量存储 ~15GB（接近性能边界）

// calculateIndexStorageSize calculates storage size for a single index entry
func (g *pgRepository) calculateIndexStorageSize(embeddingDB *pgVector) int64 {
	// 1. Text content size
	contentSizeBytes := int64(len(embeddingDB.Content))

	// 2. Vector storage size (2 bytes per dimension for half-precision float)
	var vectorSizeBytes int64 = 0
	if embeddingDB.Dimension > 0 {
		vectorSizeBytes = int64(embeddingDB.Dimension * 2)
	}

	// 3. Metadata size (fixed overhead for IDs, timestamps etc.)
	metadataSizeBytes := int64(200)

	// 4. Index overhead (HNSW index is ~2x vector size)
	indexOverheadBytes := vectorSizeBytes * 2

	// Total size in bytes
	totalSizeBytes := contentSizeBytes + vectorSizeBytes + metadataSizeBytes + indexOverheadBytes

	return totalSizeBytes
}

// EstimateStorageSize 估算一批索引条目在 PostgreSQL 中预计占用的总存储空间大小。
// 该函数遍历所有待处理的索引信息，将其转换为数据库模型后，累加单个条目的预估存储大小。
// 估算结果包含文本内容、向量数据、元数据开销以及 HNSW 向量索引的结构开销。
//
// 主要用途:
//   - 在批量导入数据前检查租户或知识库的存储配额是否充足。
//   - 为系统管理员提供容量规划参考。
//	 - 根据存储量计算云服务费用
//
// 参数:
//   - ctx: 上下文对象，用于链路追踪、超时控制及日志记录。
//   - indexInfoList: 待估算的索引信息列表，包含原始文本、向量数据及元数据。
//   - additionalParams: 额外的转换参数映射，用于在将 IndexInfo 转换为数据库模型时补充必要信息
//     (例如：指定的向量维度 dimension、特定的编码格式等)。
//
// 返回:
//   - int64: 所有索引条目的预估总存储字节数。如果列表为空，返回 0。
//
// 计算步骤：
//   1. 初始化总存储大小累加器为 0
//   2. 遍历索引信息列表中的每个条目：
//      a. 调用 toDBVectorEmbedding 将业务层的 IndexInfo 转换为数据库层的 pgVector 模型，转换后才能获取准确的 Dimension (维度)信息。
//      b. 调用 calculateIndexStorageSize ，基于 embeddingDB 的内容长度和向量维度，计算包含“原始数据 + 元数据 + HNSW 索引膨胀”在内的单行总大小。
//      c. 累加到总存储大小
//   3. 记录估算日志，包含索引数量和总字节数
//   4. 返回总存储大小

// EstimateStorageSize estimates total storage size for multiple indices
func (g *pgRepository) EstimateStorageSize(
	ctx context.Context,
	indexInfoList []*types.IndexInfo,
	additionalParams map[string]any,
) int64 {
	var totalStorageSize int64 = 0
	for _, indexInfo := range indexInfoList {
		embeddingDB := toDBVectorEmbedding(indexInfo, additionalParams)
		totalStorageSize += g.calculateIndexStorageSize(embeddingDB)
	}
	logger.GetLogger(ctx).Infof(
		"[Postgres] Estimated storage size for %d indices: %d bytes",
		len(indexInfoList), totalStorageSize,
	)
	return totalStorageSize
}

// Save 存储单个向量索引条目到 PostgreSQL
//
// 将单个文档分块的索引信息（包含文本内容和向量嵌入）持久化到数据库。
// 适用于逐条保存场景，如实时索引更新、单文档导入等。

// Save stores a single index entry
func (g *pgRepository) Save(ctx context.Context, indexInfo *types.IndexInfo, additionalParams map[string]any) error {
	logger.GetLogger(ctx).Debugf("[Postgres] Saving index for source ID: %s", indexInfo.SourceID)
	embeddingDB := toDBVectorEmbedding(indexInfo, additionalParams)
	err := g.db.WithContext(ctx).Create(embeddingDB).Error
	if err != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Failed to save index: %v", err)
		return err
	}
	logger.GetLogger(ctx).Infof("[Postgres] Successfully saved index for source ID: %s", indexInfo.SourceID)
	return nil
}

// BatchSave 批量存储多个向量索引条目到 PostgreSQL
//
// 该函数将多个文档分块的索引信息批量持久化到数据库，通过单次 INSERT 操作
// 减少网络往返和事务开销，显著提升高并发场景下的写入性能。
//
//
//
// 冲突处理策略：
//   - OnConflict{DoNothing: true} 实现 UPSERT 的"插入或忽略"语义
//   - 适用场景：断点续传、重复导入防护、多副本并发写入
//   - 如需更新现有记录，应使用 OnConflict{UpdateAll: true} 或其他策略
//
// 与 Save 对比：
//   | 特性      | Save           | BatchSave            |
//   |-----------|---------------|----------------------|
//   | 调用方式   | 单条调用        | 批量调用              |
//   | 网络开销   | N 次 RTT       | 1 次 RTT             |
//   | 事务边界   | 每条独立        | 批量统一              |
//   | 冲突处理   | 报错           | 静默忽略              |
//   | 日志输出   | 单条详细        | 批量汇总              |
//   | 适用场景   | 实时单条        | 批量导入/初始化        |
//
// 注意事项：
//   - additionalParams["embedding"] 必须包含所有 indexInfoList 中SourceID 对应的向量，否则部分记录将缺失向量数据
//   - 批次过大可能导致：
//     * PostgreSQL 参数数量限制（65535 参数）
//     * 内存占用过高（pgVector 对象 + 序列化缓冲）
//     * 事务超时或锁等待
//   - 建议外层调用按固定大小分块（如每批 1000），而非全量一次性传入

// BatchSave stores multiple index entries in batch
func (g *pgRepository) BatchSave(
	ctx context.Context, indexInfoList []*types.IndexInfo, additionalParams map[string]any,
) error {
	logger.GetLogger(ctx).Infof("[Postgres] Batch saving %d indices", len(indexInfoList))
	indexInfoDBList := make([]*pgVector, len(indexInfoList))
	for i := range indexInfoList {
		indexInfoDBList[i] = toDBVectorEmbedding(indexInfoList[i], additionalParams)
	}
	err := g.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(indexInfoDBList).Error
	if err != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Batch save failed: %v", err)
		return err
	}
	logger.GetLogger(ctx).Infof("[Postgres] Successfully batch saved %d indices", len(indexInfoList))
	return nil
}

// DeleteByChunkIDList 根据分块 ID 列表批量删除向量索引
//
// 参数说明:
//   - ctx: 上下文对象，用于链路追踪和超时控制。
//   - chunkIDList: 待删除的切片 ID 列表。支持空列表 (此时不执行任何操作)。
//   - dimension: [当前未使用] 预留参数。
//     *设计意图*: 可能用于未来扩展，例如仅删除特定维度的向量，或用于审计日志记录。
//     *现状*: 在当前实现中，删除操作仅依赖 chunk_id，此参数被忽略。
//   - knowledgeType: [当前未使用] 预留参数。
//     *设计意图*: 可能用于未来扩展，例如限制只删除特定类型知识库下的切片，增加安全性。
//     *现状*: 在当前实现中，只要 chunk_id 匹配即删除，无论其所属类型，此参数被忽略。
//
// 关于未使用参数 (dimension, knowledgeType):
// 	当前实现采用“宽泛删除”策略：只要 ChunkID 匹配即删除，不校验维度或类型。
// 	这通常是因为 ChunkID 在全局或租户范围内本身就是唯一的。
//	 如果未来需要更严格的校验（例如防止误删其他类型的同名 ID），可以在此处追加 .Where("dimension = ?", dimension) 等条件。
//
// 注意事项:
//   - 幂等性: 该操作是幂等的。重复调用相同的 ID 列表不会报错，只是第二次调用时受影响行数为 0。
//   - 性能: 对于极大的列表 (如 >10,000 个 ID)，SQL 语句长度可能超出限制。建议在调用前对列表进行分片 (Batching) 处理。
//   - 级联删除: 确保数据库层面没有外键约束阻止删除，或者已配置了 ON DELETE CASCADE。

// DeleteByChunkIDList deletes indices by chunk IDs
func (g *pgRepository) DeleteByChunkIDList(ctx context.Context, chunkIDList []string, dimension int, knowledgeType string) error {
	logger.GetLogger(ctx).Infof("[Postgres] Deleting indices by chunk IDs, count: %d", len(chunkIDList))
	result := g.db.WithContext(ctx).Where("chunk_id IN ?", chunkIDList).Delete(&pgVector{})
	if result.Error != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Failed to delete indices by chunk IDs: %v", result.Error)
		return result.Error
	}
	logger.GetLogger(ctx).Infof("[Postgres] Successfully deleted %d indices by chunk IDs", result.RowsAffected)
	return nil
}

// DeleteBySourceIDList deletes indices by source IDs
func (g *pgRepository) DeleteBySourceIDList(ctx context.Context, sourceIDList []string, dimension int, knowledgeType string) error {
	if len(sourceIDList) == 0 {
		return nil
	}
	logger.GetLogger(ctx).Infof("[Postgres] Deleting indices by source IDs, count: %d", len(sourceIDList))
	result := g.db.WithContext(ctx).Where("source_id IN ?", sourceIDList).Delete(&pgVector{})
	if result.Error != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Failed to delete indices by source IDs: %v", result.Error)
		return result.Error
	}
	logger.GetLogger(ctx).Infof("[Postgres] Successfully deleted %d indices by source IDs", result.RowsAffected)
	return nil
}

// DeleteByKnowledgeIDList deletes indices by knowledge IDs
func (g *pgRepository) DeleteByKnowledgeIDList(ctx context.Context, knowledgeIDList []string, dimension int, knowledgeType string) error {
	logger.GetLogger(ctx).Infof("[Postgres] Deleting indices by knowledge IDs, count: %d", len(knowledgeIDList))
	result := g.db.WithContext(ctx).Where("knowledge_id IN ?", knowledgeIDList).Delete(&pgVector{})
	if result.Error != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Failed to delete indices by knowledge IDs: %v", result.Error)
		return result.Error
	}
	logger.GetLogger(ctx).Infof("[Postgres] Successfully deleted %d indices by knowledge IDs", result.RowsAffected)
	return nil
}

// Retrieve handles retrieval requests and routes to appropriate method
func (g *pgRepository) Retrieve(ctx context.Context, params types.RetrieveParams) ([]*types.RetrieveResult, error) {
	logger.GetLogger(ctx).Debugf("[Postgres] Processing retrieval request of type: %s", params.RetrieverType)
	switch params.RetrieverType {
	case types.KeywordsRetrieverType:
		return g.KeywordsRetrieve(ctx, params)
	case types.VectorRetrieverType:
		return g.VectorRetrieve(ctx, params)
	}
	err := errors.New("invalid retriever type")
	logger.GetLogger(ctx).Errorf("[Postgres] %v: %s", err, params.RetrieverType)
	return nil, err
}

// KeywordsRetrieve 使用 PostgreSQL 全文搜索执行关键词检索
//
// 该函数基于 ParadeDB 的 match 函数实现 BM25 全文检索，支持多维度过滤
// （知识库、文档、标签）和启用状态筛选，返回按相关性评分排序的结果。
//
// ParadeDB 不是一个独立的数据库软件，而是一个运行在 PostgreSQL 内部的插件。
// 它让 PostgreSQL 拥有了媲美 Elasticsearch 的全文搜索和实时分析能力，而无需将数据迁移到外部系统。
// 它在 PostgreSQL 内部引入了新的数据类型、索引方法（基于 Rust 编写的 Tantivy 引擎）和函数（如 paradedb.match）。
//
// 虽然 PG 原生支持全文检索（tsvector/tsquery），但在处理海量数据、复杂评分（BM25）、高并发搜索或实时分析时，性能和功能不如专用的搜索引擎（如 Elasticsearch）。
// ParadeDB 在单机性能和核心搜索算法上它与 ES 达到了同一水平线；
// 在架构先进性（消除 ETL、强一致性、混合检索）上，ParadeDB 实际上代表了比传统 ES 架构更先进的方向。
// 但是，如果你的数据量大到单机 PG 扛不住（PB 级），ES 的强大分布式能力还是短期无可替代的。
//
// 当下，大多数 RAG 应用的知识库数据量在 GB 到几百 GB 级别，单节点高性能 PG 完全能扛住，此时 ES 的分布式优势反而成了运维负担。
// 95% 的企业应用（包括大型 RAG 系统）的数据量根本不需要几百台机器的集群。
// 单机或主从架构的 PG 就能搞定，在这些主流场景中，ParadeDB 的性能完全“媲美”ES。

// 参数:
//   - ctx: 上下文对象，用于超时控制和链路追踪。
//   - params: 检索参数对象，包含:
//       * Query: 用户输入的关键词字符串。
//       * TopK: 期望返回的最大结果数量。
//       * KnowledgeBaseIDs/KnowledgeIDs/TagIDs: 可选的过滤条件列表。
//
// 查询过程：
// 	a. 知识库过滤（KnowledgeBaseIDs）：IN 条件，限制搜索范围
// 	b. 文档过滤（KnowledgeIDs）：IN 条件，精确匹配特定文档
//	c. 标签过滤（TagIDs）：IN 条件，按标签筛选分块
// 	d. 全文搜索（ParadeDB）：@@@ match 操作符，BM25 评分
// 	e. 启用状态过滤：is_enabled IS NULL OR is_enabled = true
//	f. 排序规则：按 paradedb.score 降序排列
//
// ParadeDB 全文搜索：
//   - 语法:
//  	`id @@@ paradedb.match(field => 'content', value => ?, distance => 1)`。
//		 - `id` 是 ParadeDB 索引的标识符
//		 - `field => 'content'` 指定搜索文本字段
//		 - `value` 为用户查询词
//		 - distance => 1：允许 1 个编辑距离的模糊匹配（容错）
//   - 算法：
//  	BM25，基于词频-逆文档频率（TF-IDF）改进
//   - 评分：
//  	paradedb.score(id) 返回相关性分数（越高越相关）
//
// SQL 生成示例：
//   SELECT
//     paradedb.score(id) as score,
//     id, content, source_id, ...
//   FROM embeddings
//   WHERE knowledge_base_id IN ('kb-001', 'kb-002')
//     AND knowledge_id IN ('doc-001')
//     AND tag_id IN ('tag-001')
//     AND id @@@ paradedb.match(field => 'content', value => '关键词', distance => 1)
//     AND (is_enabled IS NULL OR is_enabled = true)
//   ORDER BY score DESC
//   LIMIT 10

// KeywordsRetrieve performs keyword-based search using PostgreSQL full-text search
func (g *pgRepository) KeywordsRetrieve(ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	logger.GetLogger(ctx).Infof("[Postgres] Keywords retrieval: query=%s, topK=%d", params.Query, params.TopK)
	conds := make([]clause.Expression, 0)

	// KnowledgeBaseIDs and KnowledgeIDs use AND logic
	// - If only KnowledgeBaseIDs: search entire knowledge bases
	// - If only KnowledgeIDs: search specific documents
	// - If both: search specific documents within the knowledge bases (AND)
	if len(params.KnowledgeBaseIDs) > 0 {
		logger.GetLogger(ctx).Debugf("[Postgres] Filtering by knowledge base IDs: %v", params.KnowledgeBaseIDs)
		conds = append(conds, clause.IN{
			Column: "knowledge_base_id",
			Values: common.ToInterfaceSlice(params.KnowledgeBaseIDs),
		})
	}
	if len(params.KnowledgeIDs) > 0 {
		logger.GetLogger(ctx).Debugf("[Postgres] Filtering by knowledge IDs: %v", params.KnowledgeIDs)
		conds = append(conds, clause.IN{
			Column: "knowledge_id",
			Values: common.ToInterfaceSlice(params.KnowledgeIDs),
		})
	}
	// Filter by tag IDs if specified
	if len(params.TagIDs) > 0 {
		logger.GetLogger(ctx).Debugf("[Postgres] Filtering by tag IDs: %v", params.TagIDs)
		conds = append(conds, clause.IN{
			Column: "tag_id",
			Values: common.ToInterfaceSlice(params.TagIDs),
		})
	}
	conds = append(conds, clause.Expr{
		SQL:  "id @@@ paradedb.match(field => 'content', value => ?, distance => 1)",
		Vars: []interface{}{params.Query},
	})
	// Filter by is_enabled = true or NULL (NULL means enabled for historical data)
	conds = append(conds, clause.Expr{
		SQL:  "(is_enabled IS NULL OR is_enabled = ?)",
		Vars: []interface{}{true},
	})
	conds = append(conds, clause.OrderBy{Columns: []clause.OrderByColumn{
		{Column: clause.Column{Name: "score"}, Desc: true},
	}})

	var embeddingDBList []pgVectorWithScore
	err := g.db.WithContext(ctx).Clauses(conds...).Debug().
		Select([]string{
			"paradedb.score(id) as score",
			"id",
			"content",
			"source_id",
			"source_type",
			"chunk_id",
			"knowledge_id",
			"knowledge_base_id",
			"tag_id",
		}).
		Limit(int(params.TopK)).
		Find(&embeddingDBList).Error

	if err == gorm.ErrRecordNotFound {
		logger.GetLogger(ctx).Warnf("[Postgres] No records found for keywords query: %s", params.Query)
		return nil, nil
	}
	if err != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Keywords retrieval failed: %v", err)
		return nil, err
	}

	logger.GetLogger(ctx).Infof("[Postgres] Keywords retrieval found %d results", len(embeddingDBList))
	results := make([]*types.IndexWithScore, len(embeddingDBList))
	for i := range embeddingDBList {
		results[i] = fromDBVectorEmbeddingWithScore(&embeddingDBList[i], types.MatchTypeKeywords)
		logger.GetLogger(ctx).Debugf("[Postgres] Keywords result %d: chunk=%s, score=%f",
			i, results[i].ChunkID, results[i].Score)
	}
	return []*types.RetrieveResult{
		{
			Results:             results,
			RetrieverEngineType: types.PostgresRetrieverEngineType,
			RetrieverType:       types.KeywordsRetrieverType,
			Error:               nil,
		},
	}, nil
}

// VectorRetrieve 使用 pgvector 执行向量相似度检索
//
// 该函数基于 HNSW（Hierarchical Navigable Small World）索引实现高效的近似最近邻（ANN）搜索，
// 通过子查询优化避免重复计算向量距离，支持多维度过滤和相似度阈值控制。
//
// 参数：
//   - ctx: 上下文，控制超时（建议 5~30s，取决于 TopK 和数据量）
//   - params: 检索参数结构体
//       * Embedding: 查询向量（float32 切片，长度须与存储维度一致）
//       * TopK: 返回结果数量上限（建议 10~100）
//       * Threshold: 相似度阈值（0~1，如 0.7 表示 70% 相似度）
//       * KnowledgeBaseIDs: 知识库白名单（可选，AND 条件）
//       * KnowledgeIDs: 知识文档白名单（可选，AND 条件）
//       * TagIDs: 标签白名单（可选，AND 条件）

// SQL 结构详解：
//   SELECT ..., (1 - distance) as score          -- 外层：距离转相似度
//   FROM (
//       SELECT ..., embedding::halfvec(d) <=> $1::halfvec as distance  -- 内层：距离计算
//       FROM embeddings 								 -- 从存储向量数据的表 embeddings 中查询
//       WHERE dimension = $2 AND ...其他过滤     		 -- 在进行向量距离计算前，先通过标量字段（如 dimension 维度、类别标签、时间范围等）缩小搜索范围
//       ORDER BY embedding::halfvec(d) <=> $1::halfvec  -- HNSW 索引驱动排序
//       LIMIT $expandedTopK                       		 -- 候选集大小
//   ) AS candidates
//   WHERE distance <= $threshold                  		 -- 相似度阈值过滤
//   ORDER BY distance ASC                         		 -- 最终排序
//   LIMIT $TopK                                   		 -- 返回限制
//
// 其中：
//  子查询计算数据库中存储的向量与查询向量之间的“距离”，并将结果命名为 distance。
//	 - embedding::halfvec(d) : 将 embedding 列的数据强制转换为 halfvec（半精度浮点向量）类型，(d) 指定了向量的维度（例如 768, 1536 等）。
//	 - 1::halfvec : 将传入的查询向量 queryVector 转换为半精度类型，这里维度 d 不需要显式指定，而是隐式推导的。
//	 - <=> : pgvector 中的距离运算符（通常表示余弦距离或欧氏距离，取决于配置，这里语境下通常指余弦距离，值越小越相似）。
//   - as distance : 计算出来的向量距离命名为 distance 。
// 	 - ORDER BY ... <=> ... : 按照‘数据库里的向量’与‘查询向量’之间的距离，从小到大进行排序，数据库会利用 HNSW 索引来快速找到距离最近的向量不需要全表扫描。
//	 - LIMIT $expandedTopK : 返回条数限制。
//
//
// 这种 SQL 写法是为了解决 “带过滤条件的向量搜索” 中的经典矛盾：
//	- 如果先过滤再算距离：如果过滤条件筛选后剩余数据很少，可能找不到足够的近邻；如果剩余数据依然很大，全量计算距离太慢。
//	- 如果直接用索引算距离：HNSW 索引很难高效处理复杂的 WHERE 标量过滤（尤其是高选择性过滤），容易导致索引失效或退化为暴力搜索。
// 该方案的平衡点（两阶段法）：
//	- Phase 1 (内层)：利用 HNSW 索引忽略部分标量过滤（或仅做简单过滤），快速捞出 expandedTopK 个最相似的候选者。牺牲一点查准率换取极高的查全速度和索引利用率。
//	- Phase 2 (外层)：在内存中对这少量的候选者进行严格的标量过滤和阈值判断。保证最终结果的精确度。
// 关键参数提示：
//	- $expandedTopK 的设置至关重要。设得太小，可能导致过滤后结果不足；设得太大，会增加内层查询开销。通常设置为 $TopK 的 2~5 倍，或者根据数据分布动态调整。
//
// 在 HNSW 检索过程中，应避免将复杂的标量过滤条件（如范围查询、多字段组合条件）嵌入到图的遍历逻辑中，
// 否则会破坏 HNSW 固有的跳跃式检索结构，引发频繁回溯，显著降低查询性能。
//
// 合理的做法是采用“两阶段过滤”策略：
//  - 预过滤（轻量）：
// 		在 HNSW 遍历阶段，仅对计算成本极低、且不会阻断图连通性的标量条件进行快速判断（如布尔字段、
//		枚举值的简单相等检查），以减少候选集大小，但不对结果做严格约束。
//  - 后过滤（精确）：
// 		在 HNSW 完成检索、捞出一个远大于最终需求（expandedTopK） 的向量候选集后，
//		再在应用层或内存中对这些候选对象执行完整的、复杂的标量条件过滤，最终返回满足条件的前 K 个结果。
//
// 此策略的本质是：用向量索引保证“召回覆盖面”，用后置过滤保证“精确性”，在检索效率和过滤准确性之间取得工程上的平衡。

// 1. 参数初始化与向量转换
//    - 提取查询向量的维度 (dimension)，作为后续类型转换和索引匹配的关键依据。
//    - 使用 pgvector.NewHalfVector 将 float32 切片转换为半精度 (halfvec) 格式。
//
// 2. 动态构建 WHERE 子句与参数绑定
//    - 按顺序构建参数列表 allVars 和占位符字符串 whereParts：
//      * 参数 $1: 查询向量（用于 ORDER BY 距离计算）。
//      * 参数 $2: 维度值 dimension (辅助优化器锁定 HNSW 索引)。
//      * 可选参数：KnowledgeBaseIDs IN 条件（知识库列表）
//      * 可选参数：KnowledgeIDs IN 条件（文档列表）
//      * 可选参数：TagIDs IN 条件（标签列表）
//      * $N+1: is_enabled 标志位 (兼容 NULL 或 true)。
//    - 将所有条件通过 AND 连接，形成完整的过滤子句。
//
// 3. 计算扩展候选集 (Expanded TopK)
//    - 策略：TopK * 2，以抵消外层阈值过滤可能造成的结果丢失。
//    - 边界控制：下限 100 (保证最小搜索广度)，上限 1000 (防止单次查询资源爆炸)。
//
// 4. 构建双层优化 SQL (子查询策略)
//    - 内层子查询 (索引扫描):
//      核心：ORDER BY embedding::halfvec(dim) <=> $1
//      目的：强制 PostgreSQL 使用 HNSW 索引进行近似最近邻搜索。
//      限制：LIMIT expandedTopK，仅获取最相似的候选集，不在此处应用相似度阈值。
//      输出：包含原始字段及计算好的 distance。
//
//    - 外层查询 (过滤与排序):
//      过滤：WHERE distance <= (1 - Threshold)，在候选集上进行精确筛选。
//      转换：计算 score = 1 - distance，将距离转换为直观的相似度分数。
//      排序：ORDER BY distance ASC 确保高分在前，LIMIT TopK 返回最终结果。
//
// 5. 执行查询与异常处理
//    - 使用 g.db.Raw() 执行拼接好的 SQL 和参数列表。
//    - 特殊处理 gorm.ErrRecordNotFound：视为“无匹配数据”而非错误，返回 nil, nil。
//    - 其他错误：记录 Error 日志并直接返回 error。
//
// 6. 结果后处理与格式化
//    - 二次截断：虽然 SQL 已有 LIMIT，代码层再次检查并截断以防万一。
//    - 模型转换：遍历结果，调用 fromDBVectorEmbeddingWithScore 转换为业务模型 IndexWithScore。
//    - 日志记录：记录每个结果的 chunk_id 和 score (Debug 级别)。
//
// 7. 组装最终返回值
//    - 封装为 []*types.RetrieveResult。
//    - 标记元数据：RetrieverEngineType = Postgres, RetrieverType = Vector。
//    - 返回结果集。

// VectorRetrieve performs vector similarity search using pgvector
// Optimized to use HNSW index efficiently and avoid recalculating vector distance
func (g *pgRepository) VectorRetrieve(ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	logger.GetLogger(ctx).Infof("[Postgres] Vector retrieval: dim=%d, topK=%d, threshold=%.4f",
		len(params.Embedding), params.TopK, params.Threshold)

	dimension := len(params.Embedding)
	queryVector := pgvector.NewHalfVector(params.Embedding)

	// Build WHERE conditions for filtering
	whereParts := make([]string, 0)
	allVars := make([]interface{}, 0)

	// Add query vector first (used in ORDER BY for HNSW index)
	allVars = append(allVars, queryVector)

	// Dimension filter (required for HNSW index WHERE clause)
	whereParts = append(whereParts, fmt.Sprintf("dimension = $%d", len(allVars)+1))
	allVars = append(allVars, dimension)

	// KnowledgeBaseIDs and KnowledgeIDs use AND logic
	// - If only KnowledgeBaseIDs: search entire knowledge bases
	// - If only KnowledgeIDs: search specific documents
	// - If both: search specific documents within the knowledge bases (AND)
	if len(params.KnowledgeBaseIDs) > 0 {
		logger.GetLogger(ctx).Debugf(
			"[Postgres] Filtering vector search by knowledge base IDs: %v",
			params.KnowledgeBaseIDs,
		)
		placeholders := make([]string, len(params.KnowledgeBaseIDs))
		paramStart := len(allVars) + 1
		for i := range params.KnowledgeBaseIDs {
			placeholders[i] = fmt.Sprintf("$%d", paramStart+i)
			allVars = append(allVars, params.KnowledgeBaseIDs[i])
		}
		whereParts = append(whereParts, fmt.Sprintf("knowledge_base_id IN (%s)",
			strings.Join(placeholders, ", ")))
	}
	if len(params.KnowledgeIDs) > 0 {
		logger.GetLogger(ctx).Debugf(
			"[Postgres] Filtering vector search by knowledge IDs: %v",
			params.KnowledgeIDs,
		)
		placeholders := make([]string, len(params.KnowledgeIDs))
		paramStart := len(allVars) + 1
		for i := range params.KnowledgeIDs {
			placeholders[i] = fmt.Sprintf("$%d", paramStart+i)
			allVars = append(allVars, params.KnowledgeIDs[i])
		}
		whereParts = append(whereParts, fmt.Sprintf("knowledge_id IN (%s)",
			strings.Join(placeholders, ", ")))
	}
	// Filter by tag IDs if specified
	if len(params.TagIDs) > 0 {
		logger.GetLogger(ctx).Debugf(
			"[Postgres] Filtering vector search by tag IDs: %v",
			params.TagIDs,
		)
		placeholders := make([]string, len(params.TagIDs))
		paramStart := len(allVars) + 1
		for i := range params.TagIDs {
			placeholders[i] = fmt.Sprintf("$%d", paramStart+i)
			allVars = append(allVars, params.TagIDs[i])
		}
		whereParts = append(whereParts, fmt.Sprintf("tag_id IN (%s)",
			strings.Join(placeholders, ", ")))
	}

	// is_enabled filter
	whereParts = append(whereParts, fmt.Sprintf("(is_enabled IS NULL OR is_enabled = $%d)", len(allVars)+1))
	allVars = append(allVars, true)

	// Build WHERE clause string
	whereClause := ""
	if len(whereParts) > 0 {
		whereClause = "WHERE " + strings.Join(whereParts, " AND ")
	}

	// Expand TopK to get more candidates before threshold filtering
	expandedTopK := params.TopK * 2
	if expandedTopK < 100 {
		expandedTopK = 100 // Minimum 100 candidates
	}
	if expandedTopK > 1000 {
		expandedTopK = 1000 // Maximum 1000 candidates
	}

	// Optimized query: Use subquery to calculate distance once
	// Strategy: Use ORDER BY with vector distance to leverage HNSW index,
	// then filter by threshold in outer query
	// This allows PostgreSQL to use HNSW index efficiently
	subqueryLimitParam := len(allVars) + 1
	thresholdParam := len(allVars) + 2
	finalLimitParam := len(allVars) + 3

	querySQL := fmt.Sprintf(`
		SELECT 
			id, content, source_id, source_type, chunk_id, knowledge_id, knowledge_base_id, tag_id,
			(1 - distance) as score
		FROM (
			SELECT 
				id, content, source_id, source_type, chunk_id, knowledge_id, knowledge_base_id, tag_id,
				embedding::halfvec(%d) <=> $1::halfvec as distance
			FROM embeddings
			%s
			ORDER BY embedding::halfvec(%d) <=> $1::halfvec
			LIMIT $%d
		) AS candidates
		WHERE distance <= $%d
		ORDER BY distance ASC
		LIMIT $%d
	`, dimension, whereClause, dimension, subqueryLimitParam, thresholdParam, finalLimitParam)

	allVars = append(allVars, expandedTopK)       // LIMIT in subquery
	allVars = append(allVars, 1-params.Threshold) // Distance threshold
	allVars = append(allVars, params.TopK)        // Final LIMIT

	var embeddingDBList []pgVectorWithScore

	err := g.db.WithContext(ctx).Raw(querySQL, allVars...).Scan(&embeddingDBList).Error

	if err == gorm.ErrRecordNotFound {
		logger.GetLogger(ctx).Warnf("[Postgres] No vector matches found that meet threshold %.4f", params.Threshold)
		return nil, nil
	}
	if err != nil {
		logger.GetLogger(ctx).Errorf("[Postgres] Vector retrieval failed: %v", err)
		return nil, err
	}

	// Apply final TopK limit (in case we got more results than needed)
	if len(embeddingDBList) > int(params.TopK) {
		embeddingDBList = embeddingDBList[:params.TopK]
	}

	logger.GetLogger(ctx).Infof("[Postgres] Vector retrieval found %d results", len(embeddingDBList))
	results := make([]*types.IndexWithScore, len(embeddingDBList))
	for i := range embeddingDBList {
		results[i] = fromDBVectorEmbeddingWithScore(&embeddingDBList[i], types.MatchTypeEmbedding)
		logger.GetLogger(ctx).Debugf("[Postgres] Vector search result %d: chunk_id %s, score %.4f",
			i, results[i].ChunkID, results[i].Score)
	}
	return []*types.RetrieveResult{
		{
			Results:             results,
			RetrieverEngineType: types.PostgresRetrieverEngineType,
			RetrieverType:       types.VectorRetrieverType,
			Error:               nil,
		},
	}, nil
}

// CopyIndices 将向量索引数据从源知识库复制到目标知识库。
// 采用批量处理方式以应对大数据量场景，并直接复制向量嵌入以避免昂贵的重计算开销。
//
// 功能说明：
//   - 批量复制向量数据，避免内存溢出（每批500条）
//   - 直接复制向量嵌入，无需重新调用 Embedding 模型，大幅提升性能
//   - 支持 ID 映射转换，适应不同知识库间的数据关联
//   - 智能处理生成的问题等特殊数据结构
//
// 参数说明：
//   - ctx: 上下文，用于操作取消和日志记录
//   - sourceKnowledgeBaseID: 源知识库 ID
//   - sourceToTargetKBIDMap: 知识 ID 映射表（源知识ID -> 目标知识ID），用于正确关联复制后的向量与目标知识
//   - sourceToTargetChunkIDMap: 文档块 ID 映射表（源块ID -> 目标块ID），用于更新复制后向量的文档块引用
//   - targetKnowledgeBaseID: 目标知识库 ID
//   - dimension: 向量维度（预留参数，暂未使用）
//   - knowledgeType: 知识类型（预留参数，暂未使用，如 "document"、"qa" 等）
//
// 返回值：
//   - error: 复制成功返回 nil，失败返回错误信息。可能因数据库查询失败、插入失败或上下文取消而报错
//
// 处理逻辑：
//   1. 批量分页查询源知识库的向量数据（每批500条）
//   2. 对每条记录进行ID映射转换（ChunkID、KnowledgeID）
//   3. 根据场景智能转换SourceID：
//      - 普通文档块（SourceID == ChunkID）：使用目标ChunkID作为SourceID
//      - 生成的问题（SourceID格式：{chunkID}-{questionID}）：保留问题ID部分，更新块ID前缀
//      - 其他复杂场景：生成新的UUID作为SourceID
//   4. 直接复制向量嵌入，避免调用Embedding模型重算
//   5. 批量插入目标表，使用 ON CONFLICT DO NOTHING 处理冲突，保证幂等性
//
// 注意事项：
//   - 函数会记录详细的执行日志，便于监控进度和排查问题
//   - 映射缺失时跳过对应记录，不影响其他数据的复制
//   - 批量大小固定为500，在内存占用和数据库性能之间取得平衡
//
// 使用示例：
//   err := repo.CopyIndices(ctx,
//       "kb_source_123",                    // 源知识库
//       map[string]string{                  // 知识ID映射
//           "know_old_1": "know_new_1",
//           "know_old_2": "know_new_2",
//       },
//       map[string]string{                  // 块ID映射
//           "chunk_old_1": "chunk_new_1",
//           "chunk_old_2": "chunk_new_2",
//       },
//       "kb_target_456",                    // 目标知识库
//       1536,                               // 向量维度（预留）
//       "document",                         // 知识类型（预留）
//   )

// CopyIndices copies index data
func (g *pgRepository) CopyIndices(ctx context.Context,
	sourceKnowledgeBaseID string,
	sourceToTargetKBIDMap map[string]string,
	sourceToTargetChunkIDMap map[string]string,
	targetKnowledgeBaseID string,
	dimension int,
	knowledgeType string,
) error {
	logger.GetLogger(ctx).Infof(
		"[Postgres] Copying indices, source knowledge base: %s, target knowledge base: %s, mapping count: %d",
		sourceKnowledgeBaseID, targetKnowledgeBaseID, len(sourceToTargetChunkIDMap),
	)

	if len(sourceToTargetChunkIDMap) == 0 {
		logger.GetLogger(ctx).Warnf("[Postgres] Mapping is empty, no need to copy")
		return nil
	}

	// Batch processing parameters
	batchSize := 500 // Number of records to process per batch
	offset := 0      // Offset for pagination
	totalCopied := 0 // Total number of copied records

	for {
		// Paginated query for source data

		// 存放当前批次查询到的向量数据对象
		var sourceVectors []*pgVector
		if err := g.db.WithContext(ctx).
			Where("knowledge_base_id = ?", sourceKnowledgeBaseID). // 只查属于 sourceKnowledgeBaseID 的数据
			Limit(batchSize).                                      // 只取 500 条
			Offset(offset).                                        // 跳过前 offset 条数据（实现分页）
			Find(&sourceVectors).Error; err != nil {               // 执行查询并将结果填充到 sourceVectors 切片中
			logger.GetLogger(ctx).Errorf("[Postgres] Failed to query source index data: %v", err)
			return err
		}

		// If no more data, exit the loop
		if len(sourceVectors) == 0 {
			if offset == 0 {
				logger.GetLogger(ctx).Warnf("[Postgres] No source index data found")
			}
			break
		}

		batchCount := len(sourceVectors)
		logger.GetLogger(ctx).Infof(
			"[Postgres] Found %d source index data, batch start position: %d",
			batchCount, offset,
		)

		// 数据转换

		// Create target vector index
		targetVectors := make([]*pgVector, 0, batchCount)
		for _, sourceVector := range sourceVectors {
			// Get the mapped target chunk ID
			targetChunkID, ok := sourceToTargetChunkIDMap[sourceVector.ChunkID]
			if !ok {
				logger.GetLogger(ctx).Warnf(
					"[Postgres] Source chunk %s not found in target chunk mapping, skipping",
					sourceVector.ChunkID,
				)
				continue
			}

			// Get the mapped target knowledge ID
			targetKnowledgeID, ok := sourceToTargetKBIDMap[sourceVector.KnowledgeID]
			if !ok {
				logger.GetLogger(ctx).Warnf(
					"[Postgres] Source knowledge %s not found in target knowledge mapping, skipping",
					sourceVector.KnowledgeID,
				)
				continue
			}

			// Handle SourceID transformation for generated questions
			// Generated questions have SourceID format: {chunkID}-{questionID}
			// Regular chunks have SourceID == ChunkID
			var targetSourceID string
			if sourceVector.SourceID == sourceVector.ChunkID {
				// 场景 1 (普通块):
				// 如果 SourceID 等于 ChunkID，说明是普通文本块。
				// 直接将 SourceID 更新为新的 targetChunkID。

				// Regular chunk, use targetChunkID as SourceID
				targetSourceID = targetChunkID
			} else if strings.HasPrefix(sourceVector.SourceID, sourceVector.ChunkID+"-") {
				// 场景 2 (生成问题):
				//	如果 SourceID 以 ChunkID- 开头（例如 old_chunk_123-q_001），说明这是基于该块生成的子问题。
				//	 - strings.TrimPrefix: 去掉旧的前缀，提取出 q_001。
				//	 - fmt.Sprintf: 拼接成 new_chunk_456-q_001，这保证了子问题依然挂在新的父块下。

				// This is a generated question, preserve the questionID part
				questionID := strings.TrimPrefix(sourceVector.SourceID, sourceVector.ChunkID+"-")
				targetSourceID = fmt.Sprintf("%s-%s", targetChunkID, questionID)
			} else {
				// 场景 3 (异常/其他):
				//	如果格式都不匹配，生成一个全新的 UUID 作为 SourceID，避免主键冲突或逻辑错误。

				// For other complex scenarios, generate new unique SourceID
				targetSourceID = uuid.New().String()
			}

			// 构建新对象
			// 	Embedding (向量数据) 和 Content (文本内容) 直接从源对象复制，避免重新调用 AI 模型计算向量的关键，极大节省成本。
			//  ChunkID, KnowledgeID, KnowledgeBaseID, SourceID 全部更新为目标值。

			// Create new vector index, copy the content and vector of the source index
			targetVector := &pgVector{
				Content:         sourceVector.Content,
				SourceID:        targetSourceID, // Handle SourceID transformation properly
				SourceType:      sourceVector.SourceType,
				ChunkID:         targetChunkID,         // Update to target chunk ID
				KnowledgeID:     targetKnowledgeID,     // Update to target knowledge ID
				KnowledgeBaseID: targetKnowledgeBaseID, // Update to target knowledge base ID
				Dimension:       sourceVector.Dimension,
				Embedding:       sourceVector.Embedding, // Copy the vector embedding directly, avoid recalculation
			}

			targetVectors = append(targetVectors, targetVector)
		}

		// Batch insert target vector index
		if len(targetVectors) > 0 {
			if err := g.db.WithContext(ctx).
				Clauses(clause.OnConflict{DoNothing: true}).Create(targetVectors).Error; err != nil {
				logger.GetLogger(ctx).Errorf("[Postgres] Failed to batch create target index: %v", err)
				return err
			}

			totalCopied += len(targetVectors)
			logger.GetLogger(ctx).Infof(
				"[Postgres] Successfully copied batch data, batch size: %d, total copied: %d",
				len(targetVectors),
				totalCopied,
			)
		}

		// 将游标向后移动当前实际读取的数量，准备读取下一页。
		// Move to the next batch
		offset += batchCount

		// If the number of returned records is less than the requested size, it means the last page has been reached
		if batchCount < batchSize {
			break
		}
	}

	logger.GetLogger(ctx).Infof("[Postgres] Index copying completed, total copied: %d", totalCopied)
	return nil
}

// 批量更新 Chunk（文本块）启用状态
//
// 根据传入的 chunkID 到 布尔值 的映射表，将数据库中对应记录的 is_enabled 字段更新为 true 或 false 。

// BatchUpdateChunkEnabledStatus updates the enabled status of chunks in batch
func (g *pgRepository) BatchUpdateChunkEnabledStatus(ctx context.Context, chunkStatusMap map[string]bool) error {
	if len(chunkStatusMap) == 0 {
		logger.GetLogger(ctx).Warnf("[Postgres] Chunk status map is empty, skipping update")
		return nil
	}

	logger.GetLogger(ctx).Infof("[Postgres] Batch updating chunk enabled status, count: %d", len(chunkStatusMap))

	// Group chunks by enabled status for batch updates
	enabledChunkIDs := make([]string, 0)
	disabledChunkIDs := make([]string, 0)

	for chunkID, enabled := range chunkStatusMap {
		if enabled {
			enabledChunkIDs = append(enabledChunkIDs, chunkID)
		} else {
			disabledChunkIDs = append(disabledChunkIDs, chunkID)
		}
	}

	// Batch update enabled chunks
	if len(enabledChunkIDs) > 0 {
		result := g.db.WithContext(ctx).Model(&pgVector{}).
			Where("chunk_id IN ?", enabledChunkIDs).
			Update("is_enabled", true)
		if result.Error != nil {
			logger.GetLogger(ctx).Errorf("[Postgres] Failed to update enabled chunks: %v", result.Error)
			return result.Error
		}
		logger.GetLogger(ctx).
			Infof("[Postgres] Updated %d chunks to enabled, rows affected: %d", len(enabledChunkIDs), result.RowsAffected)
	}

	// Batch update disabled chunks
	if len(disabledChunkIDs) > 0 {
		result := g.db.WithContext(ctx).Model(&pgVector{}).
			Where("chunk_id IN ?", disabledChunkIDs).
			Update("is_enabled", false)
		if result.Error != nil {
			logger.GetLogger(ctx).Errorf("[Postgres] Failed to update disabled chunks: %v", result.Error)
			return result.Error
		}
		logger.GetLogger(ctx).
			Infof("[Postgres] Updated %d chunks to disabled, rows affected: %d", len(disabledChunkIDs), result.RowsAffected)
	}

	logger.GetLogger(ctx).Infof("[Postgres] Successfully batch updated chunk enabled status")
	return nil
}

// BatchUpdateChunkTagID updates the tag ID of chunks in batch
func (g *pgRepository) BatchUpdateChunkTagID(ctx context.Context, chunkTagMap map[string]string) error {
	if len(chunkTagMap) == 0 {
		logger.GetLogger(ctx).Warnf("[Postgres] Chunk tag map is empty, skipping update")
		return nil
	}

	logger.GetLogger(ctx).Infof("[Postgres] Batch updating chunk tag ID, count: %d", len(chunkTagMap))

	// Group chunks by tag ID for batch updates
	tagGroups := make(map[string][]string)
	for chunkID, tagID := range chunkTagMap {
		tagGroups[tagID] = append(tagGroups[tagID], chunkID)
	}

	// Batch update chunks for each tag ID
	for tagID, chunkIDs := range tagGroups {
		result := g.db.WithContext(ctx).Model(&pgVector{}).
			Where("chunk_id IN ?", chunkIDs).
			Update("tag_id", tagID)
		if result.Error != nil {
			logger.GetLogger(ctx).Errorf("[Postgres] Failed to update chunks with tag_id %s: %v", tagID, result.Error)
			return result.Error
		}
		logger.GetLogger(ctx).
			Infof("[Postgres] Updated %d chunks to tag_id=%s, rows affected: %d", len(chunkIDs), tagID, result.RowsAffected)
	}

	logger.GetLogger(ctx).Infof("[Postgres] Successfully batch updated chunk tag ID")
	return nil
}
