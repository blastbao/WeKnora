package repository

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// CREATE TABLE knowledge_bases (
//    id VARCHAR(36) PRIMARY KEY,                 -- 主键：全局唯一 UUID，标识一个独立的知识库实例
//    name VARCHAR(255) NOT NULL,                 -- 知识库名称：用户在前端看到的显示名称（如“公司规章制度库”）
//    description TEXT,                           -- 知识库描述：详细说明该知识库的用途、范围或包含的内容类型
//    tenant_id INT NOT NULL,                     -- 租户 ID：核心隔离字段，确保不同企业/用户的数据完全隔离
//
//    -- 1. 文本切片策略
//    chunking_config JSON NOT NULL,              -- 切片配置：定义如何将长文档切分为小块。
//                                                -- 示例结构：
//                                                -- {
//                                                --   "strategy": "recursive",          // 策略：fixed (固定长度), recursive (递归语义), markdown
//                                                --   "chunk_size": 512,                // 目标块大小 (字符数)
//                                                --   "chunk_overlap": 50,              // 重叠窗口 (保持上下文连贯)
//                                                --   "separator": ["\n\n", "\n", "."], // 分隔符优先级
//                                                --   "include_metadata": true          // 是否保留标题等元数据
//                                                -- }
//
//    -- 2. 图像处理策略
//    image_processing_config JSON NOT NULL,      -- 图像处理配置：定义如何解析文档中的图片或纯图片文件。
//                                                -- 示例结构：
//                                                -- {
//                                                --   "enabled": true,                  // 是否启用
//                                                --   "ocr_engine": "paddleocr",        // OCR 引擎选择
//                                                --   "caption_model": "blip2",         // 图片描述生成模型
//                                                --   "extract_text_from_image": true,  // 是否提取图中文字
//                                                --   "max_resolution": 2048            // 最大处理分辨率
//                                                -- }
//
//    -- 3. 信息抽取策略
//    extract_config JSON NULL,                   -- 信息抽取配置 (可选)：定义是否从文本中抽取结构化实体 (如日期、人名、金额)。
//                                                -- 示例结构：
//                                                -- {
//                                                --   "enabled": false,
//                                                --   "schema": ["date", "amount"],     // 需要抽取的实体类型
//                                                --   "llm_prompt": "..."               // 自定义抽取提示词
//                                                -- }
//
//    embedding_model_id VARCHAR(64) NOT NULL,    -- 向量化模型 ID：指定将该知识库内容转换为向量时使用的 Embedding 模型 (如 "bge-large-zh", "text-embedding-3-small")
//    summary_model_id VARCHAR(64) NOT NULL,      -- 摘要模型 ID：指定生成知识库或文档摘要时使用的 LLM (如 "gpt-3.5-turbo", "qwen-7b")
//    rerank_model_id VARCHAR(64) NOT NULL,       -- 重排序模型 ID：指定在初步检索后，对结果进行精排的相关性模型 (如 "bge-reranker-large")
//                                                -- 设计意图：允许不同知识库使用不同精度/成本的模型组合 (例如：测试库用免费模型，生产库用付费高精度模型)
//
//    cos_config JSON NOT NULL,                   -- 对象存储配置 (Cloud Object Storage)：允许为特定知识库覆盖全局存储配置。
//                                                -- 用途：支持多租户隔离存储桶，或针对敏感数据使用独立的加密密钥/存储区域。
//                                                -- 示例：{ "bucket": "tenant-a-private", "region": "cn-shanghai", "encrypted": true }
//
//    vlm_config JSON NOT NULL,                   -- 视觉语言模型配置 (Vision-Language Model)：针对多模态检索的高级配置。
//                                                -- 用途：定义如何处理图文混合检索，是否启用“以图搜图”或“图文跨模态匹配”。
//                                                -- 示例：{ "enabled": true, "model": "clip-vit-large", "threshold": 0.75 }
//) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
//

var ErrKnowledgeBaseNotFound = errors.New("knowledge base not found")

// knowledgeBaseRepository implements the KnowledgeBaseRepository interface
type knowledgeBaseRepository struct {
	db *gorm.DB
}

// NewKnowledgeBaseRepository creates a new knowledge base repository
func NewKnowledgeBaseRepository(db *gorm.DB) interfaces.KnowledgeBaseRepository {
	return &knowledgeBaseRepository{db: db}
}

// CreateKnowledgeBase creates a new knowledge base
func (r *knowledgeBaseRepository) CreateKnowledgeBase(ctx context.Context, kb *types.KnowledgeBase) error {
	return r.db.WithContext(ctx).Create(kb).Error
}

// GetKnowledgeBaseByID gets a knowledge base by id (no tenant scope; caller must enforce isolation where needed)
func (r *knowledgeBaseRepository) GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	var kb types.KnowledgeBase
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeBaseNotFound
		}
		return nil, err
	}
	return &kb, nil
}

// GetKnowledgeBaseByIDAndTenant gets a knowledge base by id only if it belongs to the given tenant (enforces tenant isolation)
func (r *knowledgeBaseRepository) GetKnowledgeBaseByIDAndTenant(ctx context.Context, id string, tenantID uint64) (*types.KnowledgeBase, error) {
	var kb types.KnowledgeBase
	if err := r.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, tenantID).First(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeBaseNotFound
		}
		return nil, err
	}
	return &kb, nil
}

// GetKnowledgeBaseByIDs gets knowledge bases by multiple ids
func (r *knowledgeBaseRepository) GetKnowledgeBaseByIDs(ctx context.Context, ids []string) ([]*types.KnowledgeBase, error) {
	if len(ids) == 0 {
		return []*types.KnowledgeBase{}, nil
	}
	var kbs []*types.KnowledgeBase
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&kbs).Error; err != nil {
		return nil, err
	}
	return kbs, nil
}

// ListKnowledgeBases lists all knowledge bases
func (r *knowledgeBaseRepository) ListKnowledgeBases(ctx context.Context) ([]*types.KnowledgeBase, error) {
	var kbs []*types.KnowledgeBase
	if err := r.db.WithContext(ctx).Find(&kbs).Error; err != nil {
		return nil, err
	}
	return kbs, nil
}

// ListKnowledgeBasesByTenantID lists all knowledge bases by tenant id
func (r *knowledgeBaseRepository) ListKnowledgeBasesByTenantID(
	ctx context.Context, tenantID uint64,
) ([]*types.KnowledgeBase, error) {
	var kbs []*types.KnowledgeBase
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND is_temporary = ?", tenantID, false).
		Order("created_at DESC").Find(&kbs).Error; err != nil {
		return nil, err
	}
	return kbs, nil
}

// UpdateKnowledgeBase updates a knowledge base
func (r *knowledgeBaseRepository) UpdateKnowledgeBase(ctx context.Context, kb *types.KnowledgeBase) error {
	return r.db.WithContext(ctx).Save(kb).Error
}

// DeleteKnowledgeBase deletes a knowledge base
func (r *knowledgeBaseRepository) DeleteKnowledgeBase(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&types.KnowledgeBase{}).Error
}
