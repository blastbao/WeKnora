package postgres

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/pgvector/pgvector-go"
)

// pgVector defines the database model for vector embeddings storage
type pgVector struct {
	ID              uint                `json:"id"                gorm:"primarykey"`
	CreatedAt       time.Time           `json:"created_at"        gorm:"column:created_at"`
	UpdatedAt       time.Time           `json:"updated_at"        gorm:"column:updated_at"`
	SourceID        string              `json:"source_id"         gorm:"column:source_id;not null"`
	SourceType      int                 `json:"source_type"       gorm:"column:source_type;not null"`
	ChunkID         string              `json:"chunk_id"          gorm:"column:chunk_id"`
	KnowledgeID     string              `json:"knowledge_id"      gorm:"column:knowledge_id"`
	KnowledgeBaseID string              `json:"knowledge_base_id" gorm:"column:knowledge_base_id"`
	TagID           string              `json:"tag_id"            gorm:"column:tag_id;index"`
	Content         string              `json:"content"           gorm:"column:content;not null"`
	Dimension       int                 `json:"dimension"         gorm:"column:dimension;not null"`
	Embedding       pgvector.HalfVector `json:"embedding"         gorm:"column:embedding;not null"`
	IsEnabled       bool                `json:"is_enabled"        gorm:"column:is_enabled;default:true;index"`
}

// pgVectorWithScore extends pgVector with similarity score field
type pgVectorWithScore struct {
	ID              uint                `json:"id"                gorm:"primarykey"`
	CreatedAt       time.Time           `json:"created_at"        gorm:"column:created_at"`
	UpdatedAt       time.Time           `json:"updated_at"        gorm:"column:updated_at"`
	SourceID        string              `json:"source_id"         gorm:"column:source_id;not null"`
	SourceType      int                 `json:"source_type"       gorm:"column:source_type;not null"`
	ChunkID         string              `json:"chunk_id"          gorm:"column:chunk_id"`
	KnowledgeID     string              `json:"knowledge_id"      gorm:"column:knowledge_id"`
	KnowledgeBaseID string              `json:"knowledge_base_id" gorm:"column:knowledge_base_id"`
	TagID           string              `json:"tag_id"            gorm:"column:tag_id;index"`
	Content         string              `json:"content"           gorm:"column:content;not null"`
	Dimension       int                 `json:"dimension"         gorm:"column:dimension;not null"`
	Embedding       pgvector.HalfVector `json:"embedding"         gorm:"column:embedding;not null"`
	IsEnabled       bool                `json:"is_enabled"        gorm:"column:is_enabled;default:true;index"`
	Score           float64             `json:"score"             gorm:"column:score"`
}

// TableName specifies the database table name for pgVectorWithScore
func (pgVectorWithScore) TableName() string {
	return "embeddings"
}

// TableName specifies the database table name for pgVector
func (pgVector) TableName() string {
	return "embeddings"
}

// toDBVectorEmbedding 将业务层的 IndexInfo 转换为数据库层的 pgVector 模型
//
// 该函数负责对象映射和数据清洗，将索引信息转换为 PostgreSQL 可存储的格式。
// 处理内容包括：基础字段映射、UTF-8 文本清洗、向量嵌入提取、启用状态设置。
//
// 字段映射规则：
//   - SourceID        → source_id (唯一标识)
//   - SourceType      → source_type (枚举转整数)
//   - ChunkID         → chunk_id (分块标识)
//   - KnowledgeID     → knowledge_id (知识文档ID)
//   - KnowledgeBaseID → knowledge_base_id (知识库ID)
//   - TagID           → tag_id (标签ID)
//   - Content         → content (经UTF-8清洗的文本)
//   - IsEnabled       → is_enabled (默认true，可被覆盖)
//
// 处理步骤：
//   1. 创建 pgVector 对象，映射基础字段
//   2. 清洗 Content 字段，移除无效 UTF-8 字符
//   3. 设置默认启用状态 IsEnabled = true
//   4. 从 additionalParams 提取 embedding：
//      a. 检查 additionalParams 是否存在 "embedding" 键
//      b. 类型断言为 map[string][]float32
//      c. 根据 SourceID 获取对应向量
//      d. 转换为 pgvector.HalfVector（半精度）
//      e. 计算并设置 Dimension 维度
//   5. 从 additionalParams 提取 chunk_enabled 覆盖默认状态：
//      a. 检查是否存在 "chunk_enabled" 键
//      b. 类型断言为 map[string]bool
//      c. 根据 ChunkID 查找对应启用状态
//      d. 存在则覆盖 IsEnabled 默认值
//   6. 返回构建完成的 pgVector 对象
//
// 参数：
//   - indexInfo: 业务层索引信息，包含文档内容和元数据
//   - additionalParams: 附加参数映射，可选包含：
//       * "embedding": map[string][]float32 - SourceID 到向量数组的映射
//       * "chunk_enabled": map[string]bool - ChunkID 到启用状态的映射
//
// 返回：
//   - *pgVector: 数据库模型对象，可直接用于 GORM 的 Create/Update 操作
//
// 依赖函数：
//   - common.CleanInvalidUTF8: 清洗无效 UTF-8 字节序列，防止入库失败
//   - pgvector.NewHalfVector: 将 float32 切片转换为半精度向量
//
// 注意：
//   - embedding 通过 additionalParams 传递而非 indexInfo，支持批量场景下向量数据与元数据分离处理（如向量由外部服务生成）
//   - HalfVector 使用 2 字节/维度，比 float32 节省 50% 存储
//   - 若 embedding 不存在，Dimension 将为 0，需在后续逻辑中处理
//   - chunk_enabled 优先级高于默认值，支持批量启用/禁用控制
//
// 示例：
//   info := &types.IndexInfo{
//       SourceID: "doc-001",
//       Content: "文本内容", ...
//   }
//   params := map[string]any{
//       "embedding": map[string][]float32{
//           "doc-001": {0.1, 0.2, ...}, // 768维
//       },
//       "chunk_enabled": map[string]bool{
//           "chunk-001": false, // 禁用该分块
//       },
//   }
//   vec := toDBVectorEmbedding(info, params)
//
//
// 关键设计点解析
//	- 数据清洗 (UTF-8 安全)：入库前自动剔除非法字符，防止因编码错误导致 PostgreSQL 写入失败，确保数据落库的鲁棒性。
//	- 向量解耦 (灵活透传)：通过 additionalParams 动态传递向量数据，避免污染核心业务结构体 (IndexInfo)；利用 SourceID 精准匹配，兼顾类型安全与架构灵活性。
//	- 存储优化 (半精度转换)：显式将 float32 转为 halfvec (FP16)，使向量存储与传输开销减半，并加速相似度计算。
//	- 状态策略 (默认启用 + 按需覆盖)：默认新数据为“启用”状态，同时支持通过参数批量标记特定片段为“禁用”，满足精细化控制需求。

// toDBVectorEmbedding converts IndexInfo to pgVector database model
func toDBVectorEmbedding(indexInfo *types.IndexInfo, additionalParams map[string]any) *pgVector {
	pgVector := &pgVector{
		SourceID:        indexInfo.SourceID,
		SourceType:      int(indexInfo.SourceType),
		ChunkID:         indexInfo.ChunkID,
		KnowledgeID:     indexInfo.KnowledgeID,
		KnowledgeBaseID: indexInfo.KnowledgeBaseID,
		TagID:           indexInfo.TagID,
		Content:         common.CleanInvalidUTF8(indexInfo.Content),
		IsEnabled:       true, // Default to enabled
	}
	// Add embedding data if available in additionalParams
	if additionalParams != nil && slices.Contains(slices.Collect(maps.Keys(additionalParams)), "embedding") {
		if embeddingMap, ok := additionalParams["embedding"].(map[string][]float32); ok {
			pgVector.Embedding = pgvector.NewHalfVector(embeddingMap[indexInfo.SourceID])
			pgVector.Dimension = len(pgVector.Embedding.Slice())
		}
	}
	// Get is_enabled from additionalParams if available
	if additionalParams != nil {
		if chunkEnabledMap, ok := additionalParams["chunk_enabled"].(map[string]bool); ok {
			if enabled, exists := chunkEnabledMap[indexInfo.ChunkID]; exists {
				pgVector.IsEnabled = enabled
			}
		}
	}
	return pgVector
}

// fromDBVectorEmbeddingWithScore converts pgVectorWithScore to IndexWithScore domain model
func fromDBVectorEmbeddingWithScore(embedding *pgVectorWithScore, matchType types.MatchType) *types.IndexWithScore {
	return &types.IndexWithScore{
		ID:              strconv.FormatInt(int64(embedding.ID), 10),
		SourceID:        embedding.SourceID,
		SourceType:      types.SourceType(embedding.SourceType),
		ChunkID:         embedding.ChunkID,
		KnowledgeID:     embedding.KnowledgeID,
		KnowledgeBaseID: embedding.KnowledgeBaseID,
		TagID:           embedding.TagID,
		Content:         embedding.Content,
		Score:           embedding.Score,
		MatchType:       matchType,
	}
}
