package milvus

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	client "github.com/milvus-io/milvus/client/v2/milvusclient"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	envQdrantCollection   = "MILVUS_COLLECTION"
	defaultCollectionName = "weknora_embeddings"
	fieldContent          = "content"
	fieldSourceID         = "source_id"
	fieldSourceType       = "source_type"
	fieldChunkID          = "chunk_id"
	fieldKnowledgeID      = "knowledge_id"
	fieldKnowledgeBaseID  = "knowledge_base_id"
	fieldTagID            = "tag_id"
	fieldEmbedding        = "embedding"
	fieldIsEnabled        = "is_enabled"
	fieldID               = "id"
	fieldContentSparse    = "content_sparse"
)

var (
	allFields = []string{fieldID, fieldContent, fieldSourceID, fieldSourceType, fieldChunkID,
		fieldKnowledgeID, fieldKnowledgeBaseID, fieldTagID, fieldIsEnabled, fieldEmbedding}
)

// NewMilvusRetrieveEngineRepository creates and initializes a new Milvus repository
func NewMilvusRetrieveEngineRepository(client *client.Client) interfaces.RetrieveEngineRepository {
	log := logger.GetLogger(context.Background())
	log.Info("[Milvus] Initializing Milvus retriever engine repository")

	collectionBaseName := os.Getenv(envQdrantCollection)
	if collectionBaseName == "" {
		log.Warn("[Milvus] MILVUS_COLLECTION environment variable not set, using default collection name")
		collectionBaseName = defaultCollectionName
	}

	res := &milvusRepository{
		filter:             filter{},
		client:             client,
		collectionBaseName: collectionBaseName,
	}

	log.Info("[Milvus] Successfully initialized repository")
	return res
}

// getCollectionName returns the collection name for a specific dimension
func (m *milvusRepository) getCollectionName(dimension int) string {
	return fmt.Sprintf("%s_%d", m.collectionBaseName, dimension)
}

// 集合（Collection） 是 Milvus 中类似于关系型数据库"表"的概念，用于存储向量数据和对应的标量字段。
//
// “指定维度的集合” 是指代码为每一种向量维度单独创建一个集合，通常在集合名中包含维度信息，例如：
//	- weknora_embeddings_768  - 存储 768 维向量
//	- weknora_embeddings_1536 - 存储 1536 维向量
//	- weknora_embeddings_1024 - 存储 1024 维向量
//
// 在 Milvus 中，向量的维度（Dimension）是在创建集合（Collection）时确定的，
// 一旦创建，该集合中所有向量的维度必须完全一致，且无法修改。
//
// 由于 Milvus 不允许在一个集合里混存不同维度的向量，存入向量前需要根据维度信息，动态地查找或创建对应的集合。

// 稠密向量（Dense Vector）是通过神经网络模型（如 BERT、OpenAI Embedding）将文本映射到高维连续空间中的向量表示。
//
//	embedding := []float32{					// 稠密向量示例（768维，每个维度都有值）
//	   0.123, -0.456, 0.789, ..., 0.567, 	// 768 个非零值
//	}
//
// 当你把一句话变成稠密向量（例如 768 个浮点数）时，神经网络模型（如 BERT, Embedding 模型）做的是语义编码。
// 它把“苹果”、“水果”、“红色”、“好吃”这些概念压缩成一串数字。
//	优点：它能理解“汽车”和“轿车”是相似的，即使字面上完全不同。
//	缺点：在这个过程中，具体的字词信息被“模糊化”甚至丢失了。向量空间中的距离代表“意思相近”，而不是“字面相同”。
//
// 具体场景演示：
//	文档：“iPhone 15 Pro Max 的电池容量是 4422 mAh。”
//	查询：“iPhone 15 Pro Max 电池容量”
//	稠密向量表现：✅完美匹配。因为语义完全一致。
//
//	查询：“型号 XQ-72B 的电压参数” (假设 XQ-72B 是一个特定的、生僻的产品型号)
//	稠密向量表现：❌极差。
//	原因：
//		模型在训练时可能从未见过 “XQ-72B” 这个词，或者它把这个生僻词映射到了一个很普通的向量位置。
//		它可能会返回关于“电压”的通用文档，或者完全不相关的其他型号文档，
//		因为它无法通过向量距离精确锁定这个特定的字符串。
//
//	查询：“错误代码 0x80070005”
//	稠密向量表现：❌失效。这种毫无语义规律的随机字符串，在向量空间中很难找到精确的最近邻。
//
// 结论：
//	稠密向量擅长“意会”（Semantic Search），但不擅长“言传”（Keyword Search）。
//	对于专有名词、版本号、错误码、特定人名等精确匹配需求，稠密向量往往无能为力。

// 稀疏向量是通过统计方法（如 TF-IDF、BM25）将文本映射到高维稀疏空间中的向量表示。
//
// 稀疏向量本质是一个高维空间的索引结构，每个维度对应一个词汇，值代表该词汇在文档中的重要性权重。
// 当你把一句话变成稀疏向量时，算法做的是“词频统计”与“逆文档频率加权”。
// 它把“iPhone”、“15”、“Pro”、“Max”这些具体的词，映射到词典中对应的唯一 ID 上，并赋予权重。
// 	优点：它能精确锁定“XQ-72B”和“0x80070005”，只要文档里有这个词，分数就高，绝无模糊。
// 	缺点：它不懂同义词，搜“轿车”匹配不到“汽车”，因为它只认字面，不懂语义。
//
// 稀疏向量示例（词汇表大小 100 万，只有少数维度有值）：
//
// sparseVector := map[int]float32{
//     12345: 2.5,  // 词汇 "Milvus" 的 BM25 分数
//     67890: 1.8,  // 词汇 "向量" 的 BM25 分数
//     11111: 0.5,  // 词汇 "数据库" 的 BM25 分数
//     				// 其他 999,997 个维度都是 0（不存储）
// }
//
// 当你把一句话变成稀疏向量时，你实际上是在构建一个“词汇-权重”映射表。
// 每个维度对应词汇表中的一个词，维度值代表这个词在文档中的重要程度。
// 	优点：它能精确匹配关键词，保留所有词汇信息，不会丢失字面细节。
// 	缺点：它不理解语义，无法识别同义词，"汽车"和"轿车"被认为是完全不同的词。
//
// 具体场景演示：
// 	文档："iPhone 15 Pro Max 的电池容量是 4422 mAh。"
// 	查询："iPhone 15 Pro Max 电池容量"
// 	稀疏向量表现：✅完美匹配。因为查询中的每个词都在文档中出现。
//
// 	查询："型号 XQ-72B 的电压参数"（假设 XQ-72B 是一个特定的、生僻的产品型号）
// 	稀疏向量表现：✅完美匹配。只要文档中包含精确的 "XQ-72B" 字符串。
// 	原因：
//     稀疏向量把 "XQ-72B" 当作一个独立的词汇项，在索引中记录哪些文档包含这个词。
//     搜索时，会精确返回所有包含这个词的文档，不会混淆。
//
// 	查询："错误代码 0x80070005"
// 	稀疏向量表现：✅完美。这种看似随机的字符串，对稀疏向量来说只是另一个词汇项。 只要索引中有这个词，就能精确找到。
//
// 	查询："电动汽车"
// 	文档A："特斯拉电动车续航600公里"
// 	文档B："新能源汽车充电桩"
// 	稀疏向量表现：❌较差。因为"电动汽车"和"电动车"被认为是不同的词。
//     如果文档中只写"电动车"而不写"电动汽车"，可能搜不到。它不知道"汽车"和"轿车"是类似的。
//
// 结论：
// 	稀疏向量擅长“言传”（Keyword Search），是解决专有名词、版本号、错误码、特定术语检索的银弹。
// 	但它无法“意会”，必须与稠密向量配合（混合检索），才能同时兼顾语义理解和精确匹配。

// ensureCollection 确保指定维度的 Milvus 集合存在、schema 正确且已加载至内存。
//
// 执行流程：
// 1. 检查本地缓存：如果该维度已初始化成功，直接返回，避免重复操作。
// 2. 检查集合存在性：调用 Milvus API 确认集合是否已创建。
// 3. 自动创建集合（若不存在）：
//    - 定义包含稠密向量、稀疏向量（用于 BM25 全文检索）、文本内容及业务元数据的 Schema。
//    - 配置 BM25 函数，自动将文本内容转换为稀疏向量。
//    - 建立索引：稠密向量使用 HNSW 索引，稀疏向量使用 AutoIndex(BM25)，标量字段建立过滤索引。
// 4. 加载集合：调用 LoadCollection 并将数据加载到查询节点内存中，确保可被搜索。
// 5. 更新缓存：标记该维度为已初始化状态。
//
// Schema 设计：
//   - 主键：id (VarChar, 最大 1024 字符)
//   - 稠密向量：embedding (FloatVector)，用于语义相似度搜索
//   - 稀疏向量：content_sparse (SparseVector)，通过 BM25 函数从 content 自动生成
//   - 全文字段：content (VarChar, 最大 65535)，启用分词器和匹配功能
//   - 业务字段：source_id, chunk_id, knowledge_id, knowledge_base_id, tag_id, is_enabled 等
//
// 索引配置：
//   - embedding: HNSW 索引，相似度度量 IP，efConstruction=16, M=128
//   - content_sparse: AutoIndex，使用 BM25 算法
//   - 过滤字段：chunk_id, knowledge_id, knowledge_base_id, source_id, is_enabled 均建立标量索引
//
// 参数：
//   - ctx: 上下文，用于控制超时和取消
//   - dimension: 向量维度（如 768、1536），决定集合名称后缀
//
// 返回：
//   - error: 如创建失败、加载失败等情况返回具体错误
//
// 线程安全：
//   本函数内部使用 sync.Map 进行缓存，支持并发调用，但 Milvus 服务端操作是串行的。
//
// 示例：
//
//	if err := repo.ensureCollection(ctx, 1536); err != nil {
//	    log.Fatalf("初始化集合失败: %v", err)
//	}

//	┌─────────────────────────────────────────────────────────────┐
//	│                     应用层插入数据                             │
//	│  embedding := &MilvusVectorEmbedding{                       │
//	│      ID: "uuid-123",                                        │
//	│      Content: "Milvus 向量数据库",                            │
//	│      Embedding: [0.1, 0.2, ...],  // 手动提供                │
//	│      ChunkID: "chunk-001",                                  │
//	│      KnowledgeBaseID: "kb-123",                             │
//	│      IsEnabled: true,                                       │
//	│  }                                                          │
//	└─────────────────────────┬───────────────────────────────────┘
//	│
//	▼
//	┌─────────────────────────────────────────────────────────────┐
//	│                 Milvus 插入处理                              │
//	│  1. 存储 id 字段                                             │
//	│  2. 存储 embedding 字段（稠密向量）                            │
//	│  3. 存储 content 字段（原始文本）                              │
//	│  4. 自动触发 BM25 函数                                        │
//	│  5. 生成 content_sparse 字段（稀疏向量）                       │
//	│  6. 存储所有业务字段                                          │
//	│  7. 更新倒排索引（稀疏向量）                                    │
//	│  8. 更新 HNSW 索引（稠密向量）                                 │
//	│  9. 更新标量索引（业务字段）                                    │
//	└─────────────────────────────────────────────────────────────┘
//	│
//	▼
//	┌─────────────────────────────────────────────────────────────┐
//	│                     可被检索                                 │
//	│  向量检索：使用 embedding 字段 + HNSW 索引                      │
//	│  关键词检索：使用 content_sparse 字段 + 倒排索引                 │
//	│  过滤查询：使用业务字段 + 标量索引                               │
//	└─────────────────────────────────────────────────────────────┘

// ensureCollection ensures the collection exists for the given dimension
func (m *milvusRepository) ensureCollection(ctx context.Context, dimension int) error {
	collectionName := m.getCollectionName(dimension)

	// Check cache first
	if _, ok := m.initializedCollections.Load(dimension); ok {
		return nil
	}

	log := logger.GetLogger(ctx)

	// Check if collection exists
	hasCollection, err := m.client.HasCollection(ctx, client.NewHasCollectionOption(collectionName))
	if err != nil {
		log.Errorf("[Milvus] Failed to check collection existence: %v", err)
		return fmt.Errorf("failed to check collection existence: %w", err)
	}

	if !hasCollection {
		log.Infof("[Milvus] Creating collection %s with dimension %d", collectionName, dimension)

		// Define schema
		schema := &entity.Schema{
			CollectionName: collectionName,
			Description:    fmt.Sprintf("WeKnora embeddings collection with dimension %d", dimension),
			AutoID:         false,
			Fields: []*entity.Field{
				entity.NewField().
					WithName(fieldID).                     // 字段名: "id"
					WithDataType(entity.FieldTypeVarChar). // 字符串类型
					WithIsPrimaryKey(true).                // 设为主键
					WithMaxLength(1024),                   // 最大长度 1024 字符
				entity.NewField().
					WithName(fieldEmbedding).                  // 字段名: "embedding"
					WithDataType(entity.FieldTypeFloatVector). // 稠密向量类型
					WithDim(int64(dimension)),                 // 稠密向量维度（如 768、1536）
				entity.NewField().
					WithName(fieldContent). // 全文字段：content
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(65535).     // 最大长度 65535 字符（约 2 万汉字）
					WithEnableAnalyzer(true). // 启用分词器（支持中文、英文等）
					WithEnableMatch(true),    // 启用文本匹配功能
				entity.NewField().
					WithName(fieldContentSparse).               // 稀疏向量字段：content_sparse
					WithDataType(entity.FieldTypeSparseVector), // 稀疏向量类型
				entity.NewField().
					WithName(fieldSourceID). // 来源标识
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(255),
				entity.NewField().
					WithName(fieldSourceType).
					WithDataType(entity.FieldTypeInt64),
				entity.NewField().
					WithName(fieldChunkID). // 文本块标识
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(255),
				entity.NewField().
					WithName(fieldKnowledgeID). // 知识条目标识
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(255),
				entity.NewField().
					WithName(fieldKnowledgeBaseID). // 知识库标识（多租户隔离）
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(255),
				entity.NewField().
					WithName(fieldTagID). // 标签标识
					WithDataType(entity.FieldTypeVarChar).
					WithMaxLength(255),
				entity.NewField().
					WithName(fieldIsEnabled). // 启用状态（软删除）
					WithDataType(entity.FieldTypeBool),
			},
		}

		// 服务端自动计算
		//  当你向 fieldContent 插入一段文本时，Milvus 服务器内部会自动触发这个函数。
		//	它会自动对文本进行分词、计算词频（TF）和逆文档频率（IDF），生成一个稀疏向量，并直接填入 fieldContentSparse 字段。
		//  应用层代码完全不需要调用额外的 NLP 服务来计算稀疏向量。你只管存文本，数据库自动帮你准备好关键词检索能力，这极大地简化了架构。
		//
		// Add BM25 function for content sparse vector
		// ref: https://milvus.io/docs/zh/full-text-search.md
		schema.WithFunction(entity.NewFunction().
			WithName("text_bm25_emb").            // 函数名称
			WithInputFields(fieldContent).        // 输入字段：content
			WithOutputFields(fieldContentSparse). // 输出字段：content_sparse
			WithType(entity.FunctionTypeBM25))    // 函数类型：BM25

		// 稠密向量索引 (fieldEmbedding):
		//	方式:
		//		index.NewHNSWIndex(entity.IP, 16, 128):
		//	算法:
		//		HNSW (Hierarchical Navigable Small World)。
		//		这是目前业界公认精度和速度平衡最好的近似最近邻搜索算法。
		//	度量方式:
		//		entity.IP (Inner Product, 内积)。
		//		对于归一化后的向量，内积等价于余弦相似度，适合语义搜索。
		//	参数:
		//		M=16 (最大连接数), efConstruction=128 (构建时的搜索范围)。
		//		这些参数平衡了建库速度和查询精度。

		// 稀疏向量索引 (fieldContentSparse):
		//	方式:
		//		index.NewAutoIndex(entity.BM25):
		//	算法:
		//		BM25。
		//		Milvus 针对稀疏向量优化的专用索引，能够极快地计算关键词匹配得分。

		// 标量过滤索引 (Payload Indexes):
		// 	为所有用于 filter 条件的业务字段建立索引（通常是倒排索引或布隆过滤器）。
		// 	如果没有这些索引，Milvus 可能需要全表扫描来筛选数据，速度会极慢。
		// 	有了索引，过滤操作可以在毫秒级完成，确保“先过滤，再搜索”的高效流程。
		//
		// 在 Milvus 中，标量过滤索引（Scalar Index） 是确保“带条件向量搜索”高性能的关键。
		// Milvus 会根据字段的数据类型（Bool, Int, Varchar），自动选择底层最适合的索引算法，Bitmap/Inverted Index 等等。

		// entity.IP 是 Inner Product（内积） 的缩写。
		// 在向量数据库（如 Milvus）和机器学习中，它是衡量两个向量相似度的一种数学度量方式（Metric Type），值越大越相似。
		// 当向量被“归一化”（Normalized）后，内积 (IP) 等价于 余弦相似度 (Cosine Similarity)。
		// 大多数 Embedding 模型（如 OpenAI, BERT）输出的向量通常已经是归一化的，或者我们在存入 Milvus 前会手动归一化。
		// 因此，直接使用 IP 既能得到和余弦相似度一样的排序结果，又能获得最快的计算速度。
		//
		// 对于稠密向量，entity.IP 告诉 HNSW 索引：“请使用内积来计算向量之间的距离，寻找最相似的向量”。
		// 对于标量字段，entity.IP 仅仅是为了通过编译/接口检查，标量索引（倒排索引、位图索引）根本不计算距离，只做精确或范围匹配，这个参数会被忽略。

		indexOpts := make([]client.CreateIndexOption, 0)
		// hnsw index for embedding field
		indexOpts = append(indexOpts, client.NewCreateIndexOption(collectionName, fieldEmbedding, index.NewHNSWIndex(entity.IP, 16, 128)))
		indexOpts = append(indexOpts, client.NewCreateIndexOption(collectionName, fieldContentSparse, index.NewAutoIndex(entity.BM25)))
		// Create payload indexes for filtering
		indexFields := []string{fieldChunkID, fieldKnowledgeID, fieldKnowledgeBaseID, fieldSourceID, fieldIsEnabled}
		for _, fieldName := range indexFields {
			indexOpts = append(indexOpts, client.NewCreateIndexOption(collectionName, fieldName, index.NewAutoIndex(entity.IP)))
		}

		// 阶段1：创建集合（Create Collection）
		// 只在磁盘上创建元数据和结构，不可检索

		// Create collection
		err = m.client.CreateCollection(ctx, client.NewCreateCollectionOption(collectionName, schema).WithIndexOptions(indexOpts...))
		if err != nil {
			log.Errorf("[Milvus] Failed to create collection: %v", err)
			return fmt.Errorf("failed to create collection: %w", err)
		}

		log.Infof("[Milvus] Successfully created collection %s", collectionName)
	}

	// Milvus 采用存储计算分离架构，“创建集合”不等于“可以搜索”。
	// 集合创建后默认只存在于对象存储（如 MinIO/S3），查询节点内存中没有数据。
	//
	// Milvus 采用 存算分离 和 按需加载 的架构：
	//	持久化存储 (Disk/Object Storage)：
	//		当你 Insert 数据或 CreateCollection 时，数据主要保存在磁盘或对象存储（如 AWS S3, MinIO）上。
	//		此时数据是“冷”的，无法进行高性能向量检索。
	//	内存计算 (RAM / Query Nodes)：
	//		向量检索（特别是 HNSW 算法）极度依赖内存随机访问。
	//		只有将索引结构加载到 Query Node 的内存中，才能实现毫秒级的搜索延迟。
	//
	// LoadCollection 把集合数据加载到查询节点的内存中，之后才能执行向量搜索。
	// LoadCollection 并不无脑全量载入原始数据，而是通过索引压缩、分布式分摊、只加载索引结构以及智能内存管理来实现加载。
	//

	// 发起加载请求 (非阻塞)
	//	动作：向 Milvus 服务端发送一个异步指令：“请把 collectionName 这个集合加载到内存中”。
	//	返回值：任务对象（Task），代表了这次加载操作的句柄。
	//	状态：此时函数立即返回，不会等待加载完成。因为加载大量数据（尤其是向量索引）可能需要几秒到几分钟，如果阻塞在这里，程序会假死。
	//	错误：如果连请求都发不出去（比如网络断了、集合不存在、权限不足），err 不为空，直接记录日志并返回错误。
	loadTask, err := m.client.LoadCollection(ctx, client.NewLoadCollectionOption(collectionName))
	if err != nil {
		log.Errorf("[Milvus] Failed to load collection: %v", err)
		return fmt.Errorf("failed to load collection: %w", err)
	}

	// 等待加载完成 (阻塞)
	// 	动作：调用 Await 方法，程序进入阻塞等待状态。
	//	机制：
	//		客户端会轮询（Poll）或服务端推送状态，检查 loadTask 的进度。
	//		Milvus 后台正在做繁重的工作：
	//		 - 从对象存储（如 MinIO/S3）或本地磁盘读取数据段（Segments）。
	//		 - 将稠密向量索引（HNSW 图结构）加载到 RAM。
	//		 - 将稀疏向量索引和标量倒排索引加载到 RAM。
	//		 - 构建内存中的搜索数据结构。
	//	完成标志：当所有数据段都成功加载且索引构建完毕，Await 返回 nil，程序继续向下执行。此时集合处于 Loaded 状态，可以立即响应搜索请求。
	//	超时/失败：如果加载过程中出错（如内存不足 OOM、节点宕机），Await 会返回错误，程序捕获并报错。
	if err := loadTask.Await(ctx); err != nil {
		log.Errorf("[Milvus] Failed to await load collection: %v", err)
		return fmt.Errorf("failed to await load collection: %w", err)
	}

	// Mark as initialized
	m.initializedCollections.Store(dimension, true)
	return nil
}

func (m *milvusRepository) EngineType() types.RetrieverEngineType {
	return types.MilvusRetrieverEngineType
}

func (m *milvusRepository) Support() []types.RetrieverType {
	return []types.RetrieverType{types.KeywordsRetrieverType, types.VectorRetrieverType}
}

// EstimateStorageSize calculates the estimated storage size for a list of indices
func (m *milvusRepository) EstimateStorageSize(ctx context.Context,
	indexInfoList []*types.IndexInfo, params map[string]any,
) int64 {
	var totalStorageSize int64
	for _, embedding := range indexInfoList {
		embeddingDB := toMilvusVectorEmbedding(embedding, params)
		totalStorageSize += m.calculateStorageSize(embeddingDB)
	}
	logger.GetLogger(ctx).Infof(
		"[Milvus] Storage size for %d indices: %d bytes", len(indexInfoList), totalStorageSize,
	)
	return totalStorageSize
}

// Save stores a single point in Milvus
func (m *milvusRepository) Save(ctx context.Context,
	embedding *types.IndexInfo,
	additionalParams map[string]any,
) error {
	log := logger.GetLogger(ctx)
	log.Debugf("[Milvus] Saving index for chunk ID: %s", embedding.ChunkID)

	embeddingDB := toMilvusVectorEmbedding(embedding, additionalParams)
	if len(embeddingDB.Embedding) == 0 {
		err := fmt.Errorf("empty embedding vector for chunk ID: %s", embedding.ChunkID)
		log.Errorf("[Milvus] %v", err)
		return err
	}

	dimension := len(embeddingDB.Embedding)
	if err := m.ensureCollection(ctx, dimension); err != nil {
		return err
	}

	collectionName := m.getCollectionName(dimension)

	embeddingDB.ID = uuid.New().String()
	opts := createUpsert(collectionName, []*MilvusVectorEmbedding{embeddingDB})

	_, err := m.client.Upsert(ctx, opts)
	if err != nil {
		log.Errorf("[Milvus] Failed to save index: %v", err)
		return err
	}

	log.Infof("[Milvus] Successfully saved index for chunk ID: %s", embedding.ChunkID)
	return nil
}

// BatchSave stores multiple points in Milvus using batch insert
func (m *milvusRepository) BatchSave(ctx context.Context,
	embeddingList []*types.IndexInfo, additionalParams map[string]any,
) error {
	log := logger.GetLogger(ctx)
	if len(embeddingList) == 0 {
		log.Warn("[Milvus] Empty list provided to BatchSave, skipping")
		return nil
	}

	log.Infof("[Milvus] Batch saving %d indices", len(embeddingList))

	// Group points by dimension
	embeddingsByDimension := make(map[int][]*types.IndexInfo)

	for _, embedding := range embeddingList {
		embeddingDB := toMilvusVectorEmbedding(embedding, additionalParams)
		if len(embeddingDB.Embedding) == 0 {
			log.Warnf("[Milvus] Skipping empty embedding for chunk ID: %s", embedding.ChunkID)
			continue
		}

		dimension := len(embeddingDB.Embedding)
		embeddingsByDimension[dimension] = append(embeddingsByDimension[dimension], embedding)
		log.Debugf("[Milvus] Added chunk ID %s to batch request (dimension: %d)", embedding.ChunkID, dimension)
	}

	if len(embeddingsByDimension) == 0 {
		log.Warn("[Milvus] No valid points to save after filtering")
		return nil
	}

	// Save points to each dimension-specific collection
	totalSaved := 0
	for dimension, embeddings := range embeddingsByDimension {
		if err := m.ensureCollection(ctx, dimension); err != nil {
			return err
		}

		collectionName := m.getCollectionName(dimension)
		n := len(embeddings)
		embeddingDBList := make([]*MilvusVectorEmbedding, 0, n)

		for _, embedding := range embeddings {
			embeddingDB := toMilvusVectorEmbedding(embedding, additionalParams)
			embeddingDB.ID = uuid.New().String()
			embeddingDBList = append(embeddingDBList, embeddingDB)
		}
		opts := createUpsert(collectionName, embeddingDBList)
		_, err := m.client.Upsert(ctx, opts)
		if err != nil {
			log.Errorf("[Milvus] Failed to execute batch operation for dimension %d: %v", dimension, err)
			return fmt.Errorf("failed to batch save (dimension %d): %w", dimension, err)
		}
		totalSaved += n
		log.Infof("[Milvus] Saved %d points to collection %s", n, collectionName)
	}

	log.Infof("[Milvus] Successfully batch saved %d indices", totalSaved)
	return nil
}

// DeleteByChunkIDList removes points from the collection based on chunk IDs
func (m *milvusRepository) DeleteByChunkIDList(ctx context.Context, chunkIDList []string, dimension int, knowledgeType string) error {
	log := logger.GetLogger(ctx)
	if len(chunkIDList) == 0 {
		log.Warn("[Milvus] Empty chunk ID list provided for deletion, skipping")
		return nil
	}

	collectionName := m.getCollectionName(dimension)
	log.Infof("[Milvus] Deleting indices by chunk IDs from %s, count: %d", collectionName, len(chunkIDList))

	deleteOpt := client.NewDeleteOption(collectionName)
	deleteOpt.WithStringIDs(fieldChunkID, chunkIDList)
	_, err := m.client.Delete(ctx, deleteOpt)
	if err != nil {
		log.Errorf("[Milvus] Failed to delete by chunk IDs: %v", err)
		return fmt.Errorf("failed to delete by chunk IDs: %w", err)
	}

	log.Infof("[Milvus] Successfully deleted documents by chunk IDs")
	return nil
}

// DeleteByKnowledgeIDList removes points from the collection based on knowledge IDs
func (m *milvusRepository) DeleteByKnowledgeIDList(ctx context.Context,
	knowledgeIDList []string, dimension int, knowledgeType string,
) error {
	log := logger.GetLogger(ctx)
	if len(knowledgeIDList) == 0 {
		log.Warn("[Milvus] Empty knowledge ID list provided for deletion, skipping")
		return nil
	}

	collectionName := m.getCollectionName(dimension)
	log.Infof("[Milvus] Deleting indices by knowledge IDs from %s, count: %d", collectionName, len(knowledgeIDList))

	deleteOpt := client.NewDeleteOption(collectionName)
	deleteOpt.WithStringIDs(fieldKnowledgeID, knowledgeIDList)
	_, err := m.client.Delete(ctx, deleteOpt)
	if err != nil {
		log.Errorf("[Milvus] Failed to delete by knowledge IDs: %v", err)
		return fmt.Errorf("failed to delete by knowledge IDs: %w", err)
	}

	log.Infof("[Milvus] Successfully deleted documents by knowledge IDs")
	return nil
}

// DeleteBySourceIDList removes points from the collection based on source IDs
func (m *milvusRepository) DeleteBySourceIDList(ctx context.Context,
	sourceIDList []string, dimension int, knowledgeType string,
) error {
	log := logger.GetLogger(ctx)
	if len(sourceIDList) == 0 {
		log.Warn("[Milvus] Empty source ID list provided for deletion, skipping")
		return nil
	}

	collectionName := m.getCollectionName(dimension)
	log.Infof("[Milvus] Deleting indices by source IDs from %s, count: %d", collectionName, len(sourceIDList))

	deleteOpt := client.NewDeleteOption(collectionName)
	deleteOpt.WithStringIDs(fieldSourceID, sourceIDList)
	_, err := m.client.Delete(ctx, deleteOpt)
	if err != nil {
		log.Errorf("[Milvus] Failed to delete by source IDs: %v", err)
		return fmt.Errorf("failed to delete by source IDs: %w", err)
	}

	log.Infof("[Milvus] Successfully deleted documents by source IDs")
	return nil
}

// BatchUpdateChunkEnabledStatus updates the enabled status of chunks in batch
func (m *milvusRepository) BatchUpdateChunkEnabledStatus(ctx context.Context, chunkStatusMap map[string]bool) error {
	log := logger.GetLogger(ctx)
	if len(chunkStatusMap) == 0 {
		log.Warn("[Milvus] Empty chunk status map provided, skipping")
		return nil
	}

	log.Infof("[Milvus] Batch updating chunk enabled status, count: %d", len(chunkStatusMap))

	// Get all collections
	collections, err := m.client.ListCollections(ctx, client.NewListCollectionOption())
	if err != nil {
		log.Errorf("[Milvus] Failed to list collections: %v", err)
		return fmt.Errorf("failed to list collections: %w", err)
	}

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

	// Update in all matching collections
	for _, collectionName := range collections {
		// Only process collections that start with our base name
		if len(collectionName) <= len(m.collectionBaseName) ||
			collectionName[:len(m.collectionBaseName)] != m.collectionBaseName {
			continue
		}
		enabledEmbeddings, _, err := m.searchByFilter(ctx, collectionName, &universalFilterCondition{
			Field:    fieldChunkID,
			Operator: operatorIn,
			Value:    enabledChunkIDs,
		}, nil, nil)
		if err != nil {
			log.Warnf("[Milvus] Failed to search enabled chunks in %s: %v", collectionName, err)
			continue
		}
		upsertEmbeddings := make([]*MilvusVectorEmbedding, 0, len(enabledEmbeddings))
		for _, embedding := range enabledEmbeddings {
			embedding.IsEnabled = true
			upsertEmbeddings = append(upsertEmbeddings, &embedding.MilvusVectorEmbedding)
		}
		if len(upsertEmbeddings) > 0 {
			enabledReq := createUpsert(collectionName, upsertEmbeddings)
			_, err := m.client.Upsert(ctx, enabledReq)
			if err != nil {
				log.Warnf("[Milvus] Failed to update enabled chunks in %s: %v", collectionName, err)
				continue
			}
		}

		disabledEmbeddings, _, err := m.searchByFilter(ctx, collectionName, &universalFilterCondition{
			Field:    fieldChunkID,
			Operator: operatorIn,
			Value:    disabledChunkIDs,
		}, nil, nil)
		if err != nil {
			log.Warnf("[Milvus] Failed to search disabled chunks in %s: %v", collectionName, err)
			continue
		}
		upsertEmbeddings = make([]*MilvusVectorEmbedding, 0, len(disabledEmbeddings))
		for _, embedding := range disabledEmbeddings {
			embedding.IsEnabled = false
			upsertEmbeddings = append(upsertEmbeddings, &embedding.MilvusVectorEmbedding)
		}
		if len(upsertEmbeddings) > 0 {
			disabledReq := createUpsert(collectionName, upsertEmbeddings)
			_, err := m.client.Upsert(ctx, disabledReq)
			if err != nil {
				log.Warnf("[Milvus] Failed to update disabled chunks in %s: %v", collectionName, err)
				continue
			}
		}
	}

	log.Infof("[Milvus] Batch update chunk enabled status completed")
	return nil
}

func (m *milvusRepository) searchByFilter(ctx context.Context, collectionName string, filter *universalFilterCondition, limit, offset *int) ([]*MilvusVectorEmbeddingWithScore, int, error) {
	params, err := m.filter.Convert(filter)
	if err != nil {
		return nil, 0, err
	}
	queryOpt := client.NewQueryOption(collectionName)
	if params.exprStr != "" {
		queryOpt.WithFilter(params.exprStr)
		for k, v := range params.params {
			queryOpt.WithTemplateParam(k, v)
		}
	}
	queryOpt.WithOutputFields("*")
	if limit != nil {
		queryOpt.WithLimit(*limit)
	}
	if offset != nil {
		queryOpt.WithOffset(*offset)
	}
	resultSet, err := m.client.Query(ctx, queryOpt)
	if err != nil {
		return nil, 0, err
	}
	embeddings, _, err := convertResultSet([]client.ResultSet{resultSet})
	if err != nil {
		return nil, 0, err
	}
	return embeddings, resultSet.ResultCount, nil
}

// BatchUpdateChunkTagID updates the tag ID of chunks in batch
func (m *milvusRepository) BatchUpdateChunkTagID(ctx context.Context, chunkTagMap map[string]string) error {
	log := logger.GetLogger(ctx)
	if len(chunkTagMap) == 0 {
		log.Warn("[Milvus] Empty chunk tag map provided, skipping")
		return nil
	}

	log.Infof("[Milvus] Batch updating chunk tag ID, count: %d", len(chunkTagMap))

	// Get all collections
	collections, err := m.client.ListCollections(ctx, client.NewListCollectionOption())
	if err != nil {
		log.Errorf("[Milvus] Failed to list collections: %w", err)
		return fmt.Errorf("failed to list collections: %w", err)
	}

	// Group chunks by tag ID for batch updates
	tagGroups := make(map[string][]string)
	for chunkID, tagID := range chunkTagMap {
		tagGroups[tagID] = append(tagGroups[tagID], chunkID)
	}

	// Update in all matching collections
	for _, collectionName := range collections {
		// Only process collections that start with our base name
		if len(collectionName) <= len(m.collectionBaseName) ||
			collectionName[:len(m.collectionBaseName)] != m.collectionBaseName {
			continue
		}
		// Update chunks for each tag ID
		for tagID, chunkIDs := range tagGroups {
			embeddings, _, err := m.searchByFilter(ctx, collectionName, &universalFilterCondition{
				Field:    fieldChunkID,
				Operator: operatorIn,
				Value:    chunkIDs,
			}, nil, nil)
			if err != nil {
				log.Warnf("[Milvus] Failed to search chunks in %s: %v", collectionName, err)
				continue
			}
			upsertEmbeddings := make([]*MilvusVectorEmbedding, 0, len(embeddings))
			for _, embedding := range embeddings {
				embedding.TagID = tagID
				upsertEmbeddings = append(upsertEmbeddings, &embedding.MilvusVectorEmbedding)
			}
			if len(upsertEmbeddings) > 0 {
				req := createUpsert(collectionName, upsertEmbeddings)
				_, err := m.client.Upsert(ctx, req)
				if err != nil {
					log.Warnf("[Milvus] Failed to update chunks in %s: %v", collectionName, err)
					continue
				}
			}
		}

	}

	log.Infof("[Milvus] Batch update chunk tag ID completed")
	return nil
}

func (m *milvusRepository) getBaseFilterForQuery(params types.RetrieveParams) (string, map[string]any, error) {
	filters := make([]*universalFilterCondition, 0)
	if len(params.KnowledgeBaseIDs) > 0 {
		filters = append(filters, &universalFilterCondition{
			Field:    fieldKnowledgeBaseID,
			Operator: operatorIn,
			Value:    params.KnowledgeBaseIDs,
		})
	}
	if len(params.KnowledgeIDs) > 0 {
		filters = append(filters, &universalFilterCondition{
			Field:    fieldKnowledgeID,
			Operator: operatorIn,
			Value:    params.KnowledgeIDs,
		})
	}
	if len(params.TagIDs) > 0 {
		filters = append(filters, &universalFilterCondition{
			Field:    fieldTagID,
			Operator: operatorIn,
			Value:    params.TagIDs,
		})
	}
	if len(params.ExcludeKnowledgeIDs) > 0 {
		filters = append(filters, &universalFilterCondition{
			Field:    fieldKnowledgeID,
			Operator: operatorNotIn,
			Value:    params.ExcludeKnowledgeIDs,
		})
	}
	if len(params.ExcludeChunkIDs) > 0 {
		filters = append(filters, &universalFilterCondition{
			Field:    fieldChunkID,
			Operator: operatorNotIn,
			Value:    params.ExcludeChunkIDs,
		})
	}
	if len(filters) == 0 {
		return "", nil, nil
	}
	f, err := m.filter.Convert(&universalFilterCondition{
		Operator: operatorAnd,
		Value:    filters,
	})
	if err != nil {
		return "", nil, err
	}
	return f.exprStr, f.params, nil
}

// Retrieve dispatches the retrieval operation to the appropriate method based on retriever type
func (m *milvusRepository) Retrieve(ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	log := logger.GetLogger(ctx)
	log.Debugf("[Milvus] Processing retrieval request of type: %s", params.RetrieverType)

	switch params.RetrieverType {
	case types.VectorRetrieverType:
		return m.VectorRetrieve(ctx, params)
	case types.KeywordsRetrieverType:
		return m.KeywordsRetrieve(ctx, params)
	}

	err := fmt.Errorf("invalid retriever type: %v", params.RetrieverType)
	log.Errorf("[Milvus] %v", err)
	return nil, err
}

// VectorRetrieve performs vector similarity search
func (m *milvusRepository) VectorRetrieve(ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	log := logger.GetLogger(ctx)
	dimension := len(params.Embedding)
	log.Infof("[Milvus] Vector retrieval: dim=%d, topK=%d, threshold=%.4f",
		dimension, params.TopK, params.Threshold)

	// Get collection name based on embedding dimension
	collectionName := m.getCollectionName(dimension)

	// Check if collection exists
	hasCollection, err := m.client.HasCollection(ctx, client.NewHasCollectionOption(collectionName))
	if err != nil {
		log.Errorf("[Milvus] Failed to check collection existence: %v", err)
		return nil, fmt.Errorf("failed to check collection: %w", err)
	}
	if !hasCollection {
		log.Warnf("[Milvus] Collection %s does not exist, returning empty results", collectionName)
		return buildRetrieveResult(nil, types.VectorRetrieverType), nil
	}

	expr, paramsMap, err := m.getBaseFilterForQuery(params)
	if err != nil {
		log.Errorf("[Milvus] Failed to build base filter: %v", err)
		return nil, fmt.Errorf("failed to build filter: %w", err)
	}
	var sp *index.CustomAnnParam
	if params.Threshold > 0 {
		ann := index.NewCustomAnnParam()
		ann.WithRadius(params.Threshold)
		sp = &ann
	}
	searchOption := client.NewSearchOption(collectionName, params.TopK, []entity.Vector{entity.FloatVector(params.Embedding)})
	searchOption.WithANNSField(fieldEmbedding)
	if sp != nil {
		searchOption.WithAnnParam(sp)
	}
	if expr != "" {
		searchOption.WithFilter(expr)
		for k, v := range paramsMap {
			searchOption.WithTemplateParam(k, v)
		}
	}
	searchOption.WithOutputFields("*")
	resultSet, err := m.client.Search(ctx, searchOption)
	if err != nil {
		log.Errorf("[Milvus] Vector search failed: %v", err)
		return nil, fmt.Errorf("failed to search: %w", err)
	}
	sets, scores, err := convertResultSet(resultSet)
	if err != nil {
		log.Errorf("[Milvus] Failed to convert result set: %v", err)
		return nil, fmt.Errorf("failed to convert result set: %w", err)
	}
	var results []*types.IndexWithScore
	for i, set := range sets {
		set.Score = scores[i]
		results = append(results, fromMilvusVectorEmbedding(set.ID, set, types.MatchTypeEmbedding))
	}
	if len(results) == 0 {
		log.Warnf("[Milvus] No vector matches found that meet threshold %.4f", params.Threshold)
	} else {
		log.Infof("[Milvus] Vector retrieval found %d results", len(results))
		log.Debugf("[Milvus] Top result score: %.4f", results[0].Score)
	}
	return buildRetrieveResult(results, types.VectorRetrieverType), nil
}

// KeywordsRetrieve performs keyword-based search in document content
func (m *milvusRepository) KeywordsRetrieve(ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	log := logger.GetLogger(ctx)
	log.Infof("[Milvus] Performing keywords retrieval with query: %s, topK: %d", params.Query, params.TopK)

	// Get all collections
	collections, err := m.client.ListCollections(ctx, client.NewListCollectionOption())
	if err != nil {
		log.Errorf("[Milvus] Failed to list collections: %v", err)
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}

	var allResults []*types.IndexWithScore

	// Search in all matching collections
	for _, collectionName := range collections {
		// Only process collections that start with our base name
		if len(collectionName) <= len(m.collectionBaseName) ||
			collectionName[:len(m.collectionBaseName)] != m.collectionBaseName {
			continue
		}

		expr, paramsMap, err := m.getBaseFilterForQuery(params)
		if err != nil {
			log.Errorf("[Milvus] Failed to build base filter: %v", err)
			continue
		}
		searchOpt := client.NewSearchOption(collectionName, params.TopK, []entity.Vector{entity.Text(params.Query)})
		searchOpt.WithANNSField(fieldContentSparse)
		if expr != "" {
			searchOpt.WithFilter(expr)
			for k, v := range paramsMap {
				searchOpt.WithTemplateParam(k, v)
			}
		}
		searchOpt.WithOutputFields("*")
		resultSet, err := m.client.Search(ctx, searchOpt)
		if err != nil {
			log.Errorf("[Milvus] Keywords search failed: %v", err)
			continue
		}
		sets, _, err := convertResultSet(resultSet)
		if err != nil {
			log.Errorf("[Milvus] Failed to convert result set: %v", err)
			continue
		}
		for _, set := range sets {
			set.Score = 1.0
			allResults = append(allResults, fromMilvusVectorEmbedding(set.ID, set, types.MatchTypeKeywords))
		}
	}

	// Limit results to topK
	if len(allResults) > params.TopK {
		allResults = allResults[:params.TopK]
	}

	if len(allResults) == 0 {
		log.Warnf("[Milvus] No keyword matches found for query: %s", params.Query)
	} else {
		log.Infof("[Milvus] Keywords retrieval found %d results", len(allResults))
	}

	return buildRetrieveResult(allResults, types.KeywordsRetrieverType), nil
}

// CopyIndices copies index data from source knowledge base to target knowledge base
func (m *milvusRepository) CopyIndices(ctx context.Context,
	sourceKnowledgeBaseID string,
	sourceToTargetKBIDMap map[string]string,
	sourceToTargetChunkIDMap map[string]string,
	targetKnowledgeBaseID string,
	dimension int,
	knowledgeType string,
) error {
	log := logger.GetLogger(ctx)
	log.Infof(
		"[Milvus] Copying indices from source knowledge base %s to target knowledge base %s, count: %d, dimension: %d",
		sourceKnowledgeBaseID, targetKnowledgeBaseID, len(sourceToTargetChunkIDMap), dimension,
	)

	if len(sourceToTargetChunkIDMap) == 0 {
		log.Warn("[Milvus] Empty mapping, skipping copy")
		return nil
	}

	collectionName := m.getCollectionName(dimension)
	// Ensure target collection exists
	if err := m.ensureCollection(ctx, dimension); err != nil {
		return err
	}

	batchSize := 64
	totalCopied := 0
	var offset *int
	for {
		sourceEmbeddings, count, err := m.searchByFilter(ctx, collectionName, &universalFilterCondition{
			Field:    fieldKnowledgeBaseID,
			Operator: operatorEqual,
			Value:    sourceKnowledgeBaseID,
		}, &batchSize, offset)
		if err != nil {
			log.Errorf("[Milvus] Failed to query source points: %v", err)
			return err
		}
		if len(sourceEmbeddings) == 0 {
			break
		}
		targetEmbeddings := make([]*MilvusVectorEmbedding, 0, len(sourceEmbeddings))
		for _, sourceEmbedding := range sourceEmbeddings {
			sourceChunkID := sourceEmbedding.ChunkID
			sourceKnowledgeID := sourceEmbedding.KnowledgeID
			originalSourceID := sourceEmbedding.SourceID

			targetChunkID, ok := sourceToTargetChunkIDMap[sourceChunkID]
			if !ok {
				log.Warnf("[Milvus] Source chunk %s not found in target mapping, skipping", sourceChunkID)
				continue
			}
			targetKnowledgeID, ok := sourceToTargetKBIDMap[sourceKnowledgeID]
			if !ok {
				log.Warnf("[Milvus] Source knowledge %s not found in target mapping, skipping", sourceKnowledgeID)
				continue
			}
			var targetSourceID string
			if originalSourceID == sourceChunkID {
				targetSourceID = targetChunkID
			} else if strings.HasPrefix(originalSourceID, sourceChunkID+"-") {
				questionID := strings.TrimPrefix(originalSourceID, sourceChunkID+"-")
				targetSourceID = fmt.Sprintf("%s-%s", targetChunkID, questionID)
			} else {
				targetSourceID = uuid.New().String()
			}
			targetEmbedding := &MilvusVectorEmbedding{
				ID:              uuid.New().String(),
				Content:         sourceEmbedding.Content,
				SourceID:        targetSourceID,
				SourceType:      sourceEmbedding.SourceType,
				ChunkID:         targetChunkID,
				KnowledgeID:     targetKnowledgeID,
				KnowledgeBaseID: targetKnowledgeBaseID,
				TagID:           sourceEmbedding.TagID,
				Embedding:       sourceEmbedding.Embedding,
				IsEnabled:       sourceEmbedding.IsEnabled,
			}
			targetEmbeddings = append(targetEmbeddings, targetEmbedding)
		}
		if len(targetEmbeddings) > 0 {
			opts := createUpsert(collectionName, targetEmbeddings)
			_, err := m.client.Upsert(ctx, opts)
			if err != nil {
				log.Errorf("[Milvus] Failed to batch upsert target points: %v", err)
				return err
			}
			totalCopied += len(targetEmbeddings)
			log.Infof("[Milvus] Successfully copied batch, batch size: %d, total copied: %d",
				len(targetEmbeddings), totalCopied)
		}

		if count < batchSize {
			break
		}
		if offset == nil {
			offset = new(int)
		}
		*offset += count
	}

	log.Infof("[Milvus] Index copy completed, total copied: %d", totalCopied)
	return nil
}

func buildRetrieveResult(results []*types.IndexWithScore, retrieverType types.RetrieverType) []*types.RetrieveResult {
	return []*types.RetrieveResult{
		{
			Results:             results,
			RetrieverEngineType: types.MilvusRetrieverEngineType,
			RetrieverType:       retrieverType,
			Error:               nil,
		},
	}
}

// 计算单条向量数据在 Milvus 中占用的总存储空间（字节数）。
// 它不仅仅计算了原始数据的大小，还估算了索引结构带来的额外开销。
//
//
// Payload 字段（业务数据）
//	payloadSizeBytes := int64(0)
//	payloadSizeBytes += int64(len(embedding.Content))         // content string
//	payloadSizeBytes += int64(len(embedding.SourceID))        // source_id string
//	payloadSizeBytes += int64(len(embedding.ChunkID))         // chunk_id string
//	payloadSizeBytes += int64(len(embedding.KnowledgeID))     // knowledge_id string
//	payloadSizeBytes += int64(len(embedding.KnowledgeBaseID)) // knowledge_base_id string
//	payloadSizeBytes += 8                                     // source_type int64
//
// 向量存储
//	向量维度：768
//	每个浮点数：4 字节（float32），Milvus 使用 float32 存储向量
//	向量大小 = 768 × 4 = 3,072 字节 ≈ 3KB
//
//	不同维度的向量大小：
//	384 维：384 × 4 = 1,536 字节
//	768 维：768 × 4 = 3,072 字节
//	1536 维：1536 × 4 = 6,144 字节
//
// 索引存储
// 	IVF (Inverted File) 索引原理：
// 		1. 将向量空间划分为 nlist 个聚类（cells）
// 		2. 每个向量分配到最近的聚类中心
// 		3. 索引存储：每个聚类的中心向量 + 聚类内向量列表
// 	索引存储公式：
//		indexBytes = dimensions * (nlist * 4 + 4)
// 		其中：
// 			dimensions: 向量维度
// 			nlist: 聚类数量（默认 16384）
// 			4: 每个浮点数的字节数
// 			+4: 每个聚类的额外元数据
//
// 元数据
//	  共 32byte ，包含：
//		主键 ID
//		时间戳（创建时间、修改时间）
//		删除标记
//		行号映射
//		其他信息

func (m *milvusRepository) calculateStorageSize(embedding *MilvusVectorEmbedding) int64 {
	// Payload fields
	payloadSizeBytes := int64(0)
	payloadSizeBytes += int64(len(embedding.Content))         // content string
	payloadSizeBytes += int64(len(embedding.SourceID))        // source_id string
	payloadSizeBytes += int64(len(embedding.ChunkID))         // chunk_id string
	payloadSizeBytes += int64(len(embedding.KnowledgeID))     // knowledge_id string
	payloadSizeBytes += int64(len(embedding.KnowledgeBaseID)) // knowledge_base_id string
	payloadSizeBytes += 8                                     // source_type int64

	// Vector storage and index
	var vectorSizeBytes int64 = 0
	var indexBytes int64 = 0
	if embedding.Embedding != nil {
		dimensions := int64(len(embedding.Embedding))
		vectorSizeBytes = dimensions * 4

		// IVF_FLAT index: dimensions × (nlist × 4 + 4) bytes
		// Default nlist=16384, so: dimensions × (65536 + 4) ≈ dimensions × 65540
		const nlist = 16384
		indexBytes = dimensions * (nlist*4 + 4)
	}

	// ID tracker and metadata: ~32 bytes per vector
	const metadataBytes int64 = 32

	totalSizeBytes := payloadSizeBytes + vectorSizeBytes + indexBytes + metadataBytes
	return totalSizeBytes
}

// toMilvusVectorEmbedding converts IndexInfo to Milvus format
func toMilvusVectorEmbedding(embedding *types.IndexInfo, additionalParams map[string]interface{}) *MilvusVectorEmbedding {
	vector := &MilvusVectorEmbedding{
		Content:         embedding.Content,
		SourceID:        embedding.SourceID,
		SourceType:      int(embedding.SourceType),
		ChunkID:         embedding.ChunkID,
		KnowledgeID:     embedding.KnowledgeID,
		KnowledgeBaseID: embedding.KnowledgeBaseID,
		TagID:           embedding.TagID,
		IsEnabled:       true, // Default to enabled
	}
	if additionalParams != nil && slices.Contains(slices.Collect(maps.Keys(additionalParams)), fieldEmbedding) {
		if embeddingMap, ok := additionalParams[fieldEmbedding].(map[string][]float32); ok {
			vector.Embedding = embeddingMap[embedding.SourceID]
		}
	}
	return vector
}

// fromMilvusVectorEmbedding converts Milvus result to IndexWithScore domain model
func fromMilvusVectorEmbedding(id string,
	embedding *MilvusVectorEmbeddingWithScore,
	matchType types.MatchType,
) *types.IndexWithScore {
	return &types.IndexWithScore{
		ID:              id,
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

func createUpsert(collectionName string, embeddings []*MilvusVectorEmbedding) client.UpsertOption {
	ids := make([]string, 0, len(embeddings))
	embeddingsData := make([][]float32, 0, len(embeddings))
	contents := make([]string, 0, len(embeddings))
	sourceIDs := make([]string, 0, len(embeddings))
	sourceTypes := make([]int64, 0, len(embeddings))
	chunkIDs := make([]string, 0, len(embeddings))
	knowledgeIDs := make([]string, 0, len(embeddings))
	knowledgeBaseIDs := make([]string, 0, len(embeddings))
	tagIDs := make([]string, 0, len(embeddings))
	isEnableds := make([]bool, 0, len(embeddings))
	var dimension int
	for _, embedding := range embeddings {
		ids = append(ids, embedding.ID)
		embeddingsData = append(embeddingsData, embedding.Embedding)
		contents = append(contents, embedding.Content)
		sourceIDs = append(sourceIDs, embedding.SourceID)
		sourceTypes = append(sourceTypes, int64(embedding.SourceType))
		chunkIDs = append(chunkIDs, embedding.ChunkID)
		knowledgeIDs = append(knowledgeIDs, embedding.KnowledgeID)
		knowledgeBaseIDs = append(knowledgeBaseIDs, embedding.KnowledgeBaseID)
		tagIDs = append(tagIDs, embedding.TagID)
		isEnableds = append(isEnableds, embedding.IsEnabled)
		dimension = len(embedding.Embedding)
	}
	opt := client.NewColumnBasedInsertOption(collectionName).
		WithVarcharColumn(fieldID, ids).
		WithFloatVectorColumn(fieldEmbedding, dimension, embeddingsData).
		WithVarcharColumn(fieldContent, contents).
		WithVarcharColumn(fieldSourceID, sourceIDs).
		WithInt64Column(fieldSourceType, sourceTypes).
		WithVarcharColumn(fieldChunkID, chunkIDs).
		WithVarcharColumn(fieldKnowledgeID, knowledgeIDs).
		WithVarcharColumn(fieldKnowledgeBaseID, knowledgeBaseIDs).
		WithVarcharColumn(fieldTagID, tagIDs).
		WithBoolColumn(fieldIsEnabled, isEnableds)
	return opt
}

func convertResultSet(resultSet []client.ResultSet) ([]*MilvusVectorEmbeddingWithScore, []float64, error) {
	var results []*MilvusVectorEmbeddingWithScore
	var scores []float64
	if len(resultSet) == 0 {
		return results, scores, nil
	}
	set := resultSet[0]
	resultLen := set.Fields[0].Len()
	for _, score := range set.Scores {
		scores = append(scores, float64(score))
	}
	docs := make([]*MilvusVectorEmbeddingWithScore, 0, resultLen)
	for i := 0; i < resultLen; i++ {
		docs = append(docs, &MilvusVectorEmbeddingWithScore{})
	}
	for _, field := range allFields {
		columns := set.GetColumn(field)
		if columns == nil || columns.Len() == 0 {
			continue
		}
		if field == fieldID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].ID = val
			}
		}
		if field == fieldContent {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].Content = val
			}
		}
		if field == fieldSourceID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].SourceID = val
			}
		}
		if field == fieldSourceType {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsInt64(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].SourceType = int(val)
			}
		}
		if field == fieldChunkID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].ChunkID = val
			}
		}
		if field == fieldKnowledgeID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].KnowledgeID = val
			}
		}
		if field == fieldKnowledgeBaseID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].KnowledgeBaseID = val
			}
		}
		if field == fieldTagID {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsString(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].TagID = val
			}
		}
		if field == fieldIsEnabled {
			for i := 0; i < columns.Len(); i++ {
				val, err := columns.GetAsBool(i)
				if err != nil {
					return nil, nil, err
				}
				docs[i].IsEnabled = val
			}
		}
		if field == fieldEmbedding {
			vectorColumn, ok := columns.(*column.ColumnDoubleArray)
			if !ok {
				continue
			}
			for i := 0; i < vectorColumn.Len(); i++ {
				val, err := vectorColumn.Value(i)
				if err != nil {
					return nil, nil, fmt.Errorf("get vector failed: %w", err)
				}
				embedding := make([]float32, len(val))
				for j, v := range val {
					embedding[j] = float32(v)
				}
				docs[i].Embedding = embedding
			}
		}
	}
	return docs, scores, nil
}
