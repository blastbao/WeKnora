package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// CREATE TABLE knowledges (
//    id VARCHAR(36) PRIMARY KEY,                 -- 主键：全局唯一 UUID，标识单个知识条目（可能是文件、网页或手动录入的文本块）
//    tenant_id INT NOT NULL,                     -- 租户 ID：核心隔离字段，确保数据归属正确
//    knowledge_base_id VARCHAR(36) NOT NULL,     -- 所属知识库 ID：逻辑分组标识。多个 `knowledges` (如多个文件) 可归属于同一个 `knowledge_base`
//
//    type VARCHAR(50) NOT NULL,                  -- 知识类型：枚举值，如 'file' (文件), 'url' (网页), 'text' (手动文本), 'faq_import' (FAQ导入)
//    title VARCHAR(255) NOT NULL,                -- 标题：知识的显示名称。对于文件通常是文件名，对于网页是页面标题
//    description TEXT,                           -- 描述：用户对知识的备注或自动生成的摘要，用于检索增强或展示
//
//    source VARCHAR(128) NOT NULL,               -- 来源标识：具体来源路径或标识。如本地上传标记为 'local_upload'，URL 则存链接地址
//    parse_status VARCHAR(50) NOT NULL DEFAULT 'unprocessed', 	-- 解析状态：生命周期状态。
//                                                              -- 枚举：'unprocessed' (待处理), 'parsing' (解析中), 'parsed' (解析完成), 'failed' (失败)
//    enable_status VARCHAR(50) NOT NULL DEFAULT 'enabled',    	-- 启用状态：控制该知识是否参与检索。
//                                                              -- 枚举：'enabled' (启用), 'disabled' (禁用/暂停), 'archived' (归档)
//    embedding_model_id VARCHAR(64),             -- 向量化模型 ID：记录该知识使用哪个 Embedding 模型生成的向量。模型变更时需重新向量化
//    processed_at TIMESTAMP,                     -- 处理完成时间：记录解析和向量化成功的时间点，用于统计耗时
//    error_message TEXT,                         -- 错误信息：当 `parse_status`='failed' 时，存储具体的报错堆栈或原因，便于排查
//
//    file_name VARCHAR(255),                     -- 原始文件名：保留用户上传时的原始名称（含扩展名）
//    file_type VARCHAR(50),                      -- 文件 MIME 类型/扩展名：如 'pdf', 'md', 'docx'。决定使用哪种解析器
//    file_size BIGINT,                           -- 文件大小 (字节)：上传时的原始大小
//    file_path TEXT,                             -- 存储路径：对象存储 (OSS/S3) 的路径或本地文件系统路径
//    file_hash VARCHAR(64),                      -- 文件内容哈希：SHA256 值。用于秒传检测（相同文件不重复上传）和去重
//    storage_size BIGINT NOT NULL DEFAULT 0,     -- 实际占用存储：解析后产生的切片、图片等所有关联资源的总大小（可能大于原文件）
//
//    metadata JSON,                              -- 扩展元数据：存储动态字段。
//                                                -- 示例：
//                                                -- - URL 类型： { "crawl_depth": 1, "domain": "example.com" }
//                                                -- - 文件类型：  { "page_count": 10, "author": "John" }
//                                                -- - 自定义标签：{ "tags": ["HR", "Policy"] }
//) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

var ErrKnowledgeNotFound = errors.New("knowledge not found")

// omitFieldsOnUpdate defines fields to omit when updating knowledge
var omitFieldsOnUpdate = []string{"DeletedAt"}

// knowledgeRepository implements knowledge base and knowledge repository interface
type knowledgeRepository struct {
	db *gorm.DB
}

// NewKnowledgeRepository creates a new knowledge repository
func NewKnowledgeRepository(db *gorm.DB) interfaces.KnowledgeRepository {
	return &knowledgeRepository{db: db}
}

// CreateKnowledge creates knowledge
func (r *knowledgeRepository) CreateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	err := r.db.WithContext(ctx).Create(knowledge).Error
	return err
}

// GetKnowledgeByID gets knowledge
func (r *knowledgeRepository) GetKnowledgeByID(
	ctx context.Context,
	tenantID uint64,
	id string,
) (*types.Knowledge, error) {
	var knowledge types.Knowledge
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&knowledge).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeNotFound
		}
		return nil, err
	}
	return &knowledge, nil
}

// GetKnowledgeByIDOnly returns knowledge by ID without tenant filter (for permission resolution).
func (r *knowledgeRepository) GetKnowledgeByIDOnly(ctx context.Context, id string) (*types.Knowledge, error) {
	var knowledge types.Knowledge
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&knowledge).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeNotFound
		}
		return nil, err
	}
	return &knowledge, nil
}

// ListKnowledgeByKnowledgeBaseID lists all knowledge in a knowledge base
func (r *knowledgeRepository) ListKnowledgeByKnowledgeBaseID(
	ctx context.Context, tenantID uint64, kbID string,
) ([]*types.Knowledge, error) {
	var knowledges []*types.Knowledge
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Order("created_at DESC").Find(&knowledges).Error; err != nil {
		return nil, err
	}
	return knowledges, nil
}

// ListPagedKnowledgeByKnowledgeBaseID lists all knowledge in a knowledge base with pagination
func (r *knowledgeRepository) ListPagedKnowledgeByKnowledgeBaseID(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	page *types.Pagination,
	tagID string,
	keyword string,
	fileType string,
) ([]*types.Knowledge, int64, error) {
	var knowledges []*types.Knowledge
	var total int64

	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID)
	if tagID != "" {
		query = query.Where("tag_id = ?", tagID)
	}
	if keyword != "" {
		query = query.Where("file_name LIKE ?", "%"+keyword+"%")
	}
	if fileType != "" {
		if fileType == "manual" {
			query = query.Where("type = ?", "manual")
		} else if fileType == "url" {
			query = query.Where("type = ?", "url")
		} else {
			query = query.Where("file_type = ?", fileType)
		}
	}

	// Query total count first
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Then query paginated data
	dataQuery := r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID)
	if tagID != "" {
		dataQuery = dataQuery.Where("tag_id = ?", tagID)
	}
	if keyword != "" {
		dataQuery = dataQuery.Where("file_name LIKE ?", "%"+keyword+"%")
	}
	if fileType != "" {
		if fileType == "manual" {
			dataQuery = dataQuery.Where("type = ?", "manual")
		} else if fileType == "url" {
			dataQuery = dataQuery.Where("type = ?", "url")
		} else {
			dataQuery = dataQuery.Where("file_type = ?", fileType)
		}
	}

	if err := dataQuery.
		Order("created_at DESC").
		Offset(page.Offset()).
		Limit(page.Limit()).
		Find(&knowledges).Error; err != nil {
		return nil, 0, err
	}

	return knowledges, total, nil
}

// UpdateKnowledge updates knowledge
func (r *knowledgeRepository) UpdateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	err := r.db.WithContext(ctx).Omit(omitFieldsOnUpdate...).Save(knowledge).Error
	return err
}

// UpdateKnowledgeBatch updates knowledge items in batch
func (r *knowledgeRepository) UpdateKnowledgeBatch(ctx context.Context, knowledgeList []*types.Knowledge) error {
	if len(knowledgeList) == 0 {
		return nil
	}
	return r.db.Debug().WithContext(ctx).Omit(omitFieldsOnUpdate...).Save(knowledgeList).Error
}

// DeleteKnowledge deletes knowledge
func (r *knowledgeRepository) DeleteKnowledge(ctx context.Context, tenantID uint64, id string) error {
	return r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&types.Knowledge{}).Error
}

// DeleteKnowledge deletes knowledge
func (r *knowledgeRepository) DeleteKnowledgeList(ctx context.Context, tenantID uint64, ids []string) error {
	return r.db.WithContext(ctx).Where("tenant_id = ? AND id in ?", tenantID, ids).Delete(&types.Knowledge{}).Error
}

// GetKnowledgeBatch gets knowledge in batch
func (r *knowledgeRepository) GetKnowledgeBatch(
	ctx context.Context, tenantID uint64, ids []string,
) ([]*types.Knowledge, error) {
	var knowledge []*types.Knowledge
	if err := r.db.WithContext(ctx).Debug().
		Where("tenant_id = ? AND id IN ?", tenantID, ids).
		Find(&knowledge).Error; err != nil {
		return nil, err
	}
	return knowledge, nil
}

// CheckKnowledgeExists checks if knowledge already exists
func (r *knowledgeRepository) CheckKnowledgeExists(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	params *types.KnowledgeCheckParams,
) (bool, *types.Knowledge, error) {
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ? AND parse_status <> ?", tenantID, kbID, "failed")

	switch params.Type {
	case "file":
		// If file hash exists, prioritize exact match using hash
		if params.FileHash != "" {
			var knowledge types.Knowledge
			err := query.Where("file_hash = ?", params.FileHash).First(&knowledge).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return false, nil, nil
				}
				return false, nil, err
			}
			return true, &knowledge, nil
		}

		// If no hash or hash doesn't match, use filename and size
		if params.FileName != "" && params.FileSize > 0 {
			var knowledge types.Knowledge
			err := query.Where(
				"file_name = ? AND file_size = ?",
				params.FileName, params.FileSize,
			).First(&knowledge).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return false, nil, nil
				}
				return false, nil, err
			}
			return true, &knowledge, nil
		}
	case "url":
		// If file hash exists, prioritize exact match using hash
		if params.FileHash != "" {
			var knowledge types.Knowledge
			err := query.Where("type = 'url' AND file_hash = ?", params.FileHash).First(&knowledge).Error
			if err == nil && knowledge.ID != "" {
				return true, &knowledge, nil
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil, err
			}
		}

		if params.URL != "" {
			var knowledge types.Knowledge
			err := query.Where("type = 'url' AND source = ?", params.URL).First(&knowledge).Error
			if err == nil && knowledge.ID != "" {
				return true, &knowledge, nil
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil, err
			}
		}
		return false, nil, nil
	}

	// No valid parameters, default to not existing
	return false, nil, nil
}

func (r *knowledgeRepository) AminusB(
	ctx context.Context,
	Atenant uint64, A string,
	Btenant uint64, B string,
) ([]string, error) {
	knowledgeIDs := []string{}
	subQuery := r.db.Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", Btenant, B).Select("file_hash")
	err := r.db.Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", Atenant, A).
		Where("file_hash NOT IN (?)", subQuery).
		Pluck("id", &knowledgeIDs).
		Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledgeIDs, nil
	}
	return knowledgeIDs, err
}

func (r *knowledgeRepository) UpdateKnowledgeColumn(
	ctx context.Context,
	id string,
	column string,
	value interface{},
) error {
	err := r.db.WithContext(ctx).Model(&types.Knowledge{}).Where("id = ?", id).Update(column, value).Error
	return err
}

// CountKnowledgeByKnowledgeBaseID counts the number of knowledge items in a knowledge base
func (r *knowledgeRepository) CountKnowledgeByKnowledgeBaseID(
	ctx context.Context,
	tenantID uint64,
	kbID string,
) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Count(&count).Error
	return count, err
}

// CountKnowledgeByStatus counts the number of knowledge items with the specified parse status
func (r *knowledgeRepository) CountKnowledgeByStatus(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	parseStatuses []string,
) (int64, error) {
	if len(parseStatuses) == 0 {
		return 0, nil
	}

	var count int64
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Where("parse_status IN ?", parseStatuses)

	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}

	return count, nil
}

// SearchKnowledge searches knowledge items by keyword across the tenant
// If keyword is empty, returns recent files
// Only returns documents from document-type knowledge bases (excludes FAQ)
// Returns (results, hasMore, error)
func (r *knowledgeRepository) SearchKnowledge(
	ctx context.Context,
	tenantID uint64,
	keyword string,
	offset, limit int,
	fileTypes []string,
) ([]*types.Knowledge, bool, error) {
	// Use raw query to properly map knowledge_base_name
	type KnowledgeWithKBName struct {
		types.Knowledge
		KnowledgeBaseName string `gorm:"column:knowledge_base_name"`
	}

	var results []KnowledgeWithKBName
	query := r.db.WithContext(ctx).
		Table("knowledges").
		Select("knowledges.*, knowledge_bases.name as knowledge_base_name").
		Joins("JOIN knowledge_bases ON knowledge_bases.id = knowledges.knowledge_base_id").
		Where("knowledges.tenant_id = ?", tenantID).
		Where("knowledge_bases.type = ?", types.KnowledgeBaseTypeDocument).
		Where("knowledges.deleted_at IS NULL")

	// If keyword is provided, filter by file_name or title
	if keyword != "" {
		query = query.Where("knowledges.file_name LIKE ? ", "%"+keyword+"%")
	}

	// If fileTypes is provided, filter by file extension
	if len(fileTypes) > 0 {
		// Build file extension patterns (e.g., "%.csv", "%.xlsx")
		seen := make(map[string]bool)
		var uniquePatterns []string
		for _, ft := range fileTypes {
			ft = strings.ToLower(strings.TrimPrefix(ft, "."))
			pattern := "%." + ft
			if !seen[pattern] {
				seen[pattern] = true
				uniquePatterns = append(uniquePatterns, pattern)
			}
			// Handle common aliases
			var aliases []string
			switch ft {
			case "xlsx":
				aliases = []string{"%.xls"}
			case "xls":
				aliases = []string{"%.xlsx"}
			case "docx":
				aliases = []string{"%.doc"}
			case "doc":
				aliases = []string{"%.docx"}
			case "jpg":
				aliases = []string{"%.jpeg", "%.png"}
			case "jpeg":
				aliases = []string{"%.jpg", "%.png"}
			case "png":
				aliases = []string{"%.jpg", "%.jpeg"}
			}
			for _, alias := range aliases {
				if !seen[alias] {
					seen[alias] = true
					uniquePatterns = append(uniquePatterns, alias)
				}
			}
		}
		// Build OR conditions for file extensions
		if len(uniquePatterns) > 0 {
			orConditions := make([]string, len(uniquePatterns))
			args := make([]interface{}, len(uniquePatterns))
			for i, p := range uniquePatterns {
				orConditions[i] = "LOWER(knowledges.file_name) LIKE ?"
				args[i] = p
			}
			query = query.Where("("+strings.Join(orConditions, " OR ")+")", args...)
		}
	}

	// Fetch limit+1 to check if there are more results
	err := query.Order("knowledges.created_at DESC").
		Offset(offset).
		Limit(limit + 1).
		Scan(&results).Error
	if err != nil {
		return nil, false, err
	}

	// Check if there are more results
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	// Convert to []*types.Knowledge
	knowledges := make([]*types.Knowledge, len(results))
	for i, r := range results {
		k := r.Knowledge
		k.KnowledgeBaseName = r.KnowledgeBaseName
		knowledges[i] = &k
	}
	return knowledges, hasMore, nil
}

// SearchKnowledgeInScopes searches knowledge items by keyword within the given (tenant_id, kb_id) scopes (e.g. own + shared KBs).
func (r *knowledgeRepository) SearchKnowledgeInScopes(
	ctx context.Context,
	scopes []types.KnowledgeSearchScope,
	keyword string,
	offset, limit int,
	fileTypes []string,
) ([]*types.Knowledge, bool, error) {
	if len(scopes) == 0 {
		return nil, false, nil
	}

	type KnowledgeWithKBName struct {
		types.Knowledge
		KnowledgeBaseName string `gorm:"column:knowledge_base_name"`
	}

	placeholders := make([]string, len(scopes))
	args := make([]interface{}, 0, len(scopes)*2)
	for i, s := range scopes {
		placeholders[i] = "(?,?)"
		args = append(args, s.TenantID, s.KBID)
	}
	scopeCondition := "(knowledges.tenant_id, knowledges.knowledge_base_id) IN (" + strings.Join(placeholders, ",") + ")"

	query := r.db.WithContext(ctx).
		Table("knowledges").
		Select("knowledges.*, knowledge_bases.name as knowledge_base_name").
		Joins("JOIN knowledge_bases ON knowledge_bases.id = knowledges.knowledge_base_id AND knowledge_bases.tenant_id = knowledges.tenant_id").
		Where(scopeCondition, args...).
		Where("knowledge_bases.type = ?", types.KnowledgeBaseTypeDocument).
		Where("knowledges.deleted_at IS NULL")

	if keyword != "" {
		query = query.Where("knowledges.file_name LIKE ?", "%"+keyword+"%")
	}

	if len(fileTypes) > 0 {
		seen := make(map[string]bool)
		var uniquePatterns []string
		for _, ft := range fileTypes {
			ft = strings.ToLower(strings.TrimPrefix(ft, "."))
			pattern := "%." + ft
			if !seen[pattern] {
				seen[pattern] = true
				uniquePatterns = append(uniquePatterns, pattern)
			}
			var aliases []string
			switch ft {
			case "xlsx":
				aliases = []string{"%.xls"}
			case "xls":
				aliases = []string{"%.xlsx"}
			case "docx":
				aliases = []string{"%.doc"}
			case "doc":
				aliases = []string{"%.docx"}
			case "jpg":
				aliases = []string{"%.jpeg", "%.png"}
			case "jpeg":
				aliases = []string{"%.jpg", "%.png"}
			case "png":
				aliases = []string{"%.jpg", "%.jpeg"}
			}
			for _, alias := range aliases {
				if !seen[alias] {
					seen[alias] = true
					uniquePatterns = append(uniquePatterns, alias)
				}
			}
		}
		if len(uniquePatterns) > 0 {
			orConditions := make([]string, len(uniquePatterns))
			ftArgs := make([]interface{}, len(uniquePatterns))
			for i, p := range uniquePatterns {
				orConditions[i] = "LOWER(knowledges.file_name) LIKE ?"
				ftArgs[i] = p
			}
			query = query.Where("("+strings.Join(orConditions, " OR ")+")", ftArgs...)
		}
	}

	var results []KnowledgeWithKBName
	err := query.Order("knowledges.created_at DESC").
		Offset(offset).
		Limit(limit + 1).
		Scan(&results).Error
	if err != nil {
		return nil, false, err
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	knowledges := make([]*types.Knowledge, len(results))
	for i, r := range results {
		k := r.Knowledge
		k.KnowledgeBaseName = r.KnowledgeBaseName
		knowledges[i] = &k
	}
	return knowledges, hasMore, nil
}

// ListIDsByTagID returns all knowledge IDs that have the specified tag ID
func (r *knowledgeRepository) ListIDsByTagID(
	ctx context.Context,
	tenantID uint64,
	kbID, tagID string,
) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ? AND tag_id = ?", tenantID, kbID, tagID).
		Pluck("id", &ids).Error
	return ids, err
}
