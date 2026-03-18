package types

const (
	TypeChunkExtract        = "chunk:extract"
	TypeDocumentProcess     = "document:process"      // 文档处理任务
	TypeFAQImport           = "faq:import"            // FAQ导入任务（包含dry run模式）
	TypeQuestionGeneration  = "question:generation"   // 问题生成任务
	TypeSummaryGeneration   = "summary:generation"    // 摘要生成任务
	TypeKBClone             = "kb:clone"              // 知识库复制任务
	TypeIndexDelete         = "index:delete"          // 索引删除任务
	TypeKBDelete            = "kb:delete"             // 知识库删除任务
	TypeKnowledgeListDelete = "knowledge:list_delete" // 批量删除知识任务
	TypeDataTableSummary    = "datatable:summary"     // 表格摘要任务
)

// ExtractChunkPayload represents the extract chunk task payload
//
// 当需要触发一个 “对文本片段进行信息抽取” 的异步任务时，这个结构体就是被序列化并发送的消息内容。
// 它不包含具体的文本数据本身，而是包含定位数据所需的元数据。
//
// ChunkID
// 含义: 文本片段 ID。
// 用途: 数据定位，根据 TenantID 和 ChunkID 可以从数据库、对象存储（如 S3）或向量数据库中检索实际的文本内容。
//
// ModelID
// 含义: 模型 ID。
// 用途: 指定执行此次抽取任务具体使用哪个模型。允许在同一租户内，对不同的文本片段使用不同的模型策略。
type ExtractChunkPayload struct {
	TenantID uint64 `json:"tenant_id"`
	ChunkID  string `json:"chunk_id"`
	ModelID  string `json:"model_id"`
}

// DocumentProcessPayload represents the document process task payload
//
// 文档处理载荷：把它理解为一个“任务说明书”，告诉后端处理程序：哪个租户、哪份文档、用什么方式、以及需要开启哪些增强功能。
//
// 1. 基础标识
// RequestId: 请求的唯一标识，用于全链路日志追踪（Tracing）和排查问题。
// TenantID: 租户 ID。在多租户架构中，确保数据隔离，知道这份文档属于哪个客户。
// KnowledgeID & KnowledgeBaseID: 文档 ID 和知识库 ID。明确这份文件处理完后应该存放到哪个具体的知识库位置。
//
// 2. 数据来源
// 文件上传: FilePath（路径）、FileName（文件名）、FileType（后缀名，如 pdf, docx）。
// 网络来源: URL（网页抓取）或 FileURL（直接的文件下载链接）。
// 纯文本: Passages，如果用户直接输入一段话，数据会放在这个字符串切片里。
//
// 3. 增强功能
// EnableMultimodel: 是否开启多模态。如果开启，系统不仅提取文本，还会提取图片、表格等非文本元素，并可能调用多模态大模型（如 LLaVA, GPT-4V）来生成图片的描述（Caption）或理解图表内容，以便后续进行多模态检索。
// EnableQuestionGeneration: 是否开启问题生成。系统在将文档切片存入知识库时，利用 LLM 根据切片内容自动生成若干个小问题。当用户提问时，可以直接匹配这些预生成的问题，往往比直接匹配文本片段更准确，能提高检索召回率。
// QuestionCount: 每个分块（Chunk）生成的题目数量，配合上一个字段使用。例如设置为 3，表示对每一个文档切片，LLM 需要尝试生成 3 个不同角度的潜在问题。
//
// 工作流示例：
// 用户上传一个 PDF 到 S3 -> 前端调用 API 发送 DocumentProcessPayload（填入 FileURL, KnowledgeBaseID, 设置 EnableQuestionGeneration=true） -> 后端接收任务 -> 下载文件 -> 解析文本/图片 -> 切片 -> 调用 LLM 生成问题和图片描述 -> 将文本、问题、图片描述一起向量化并存入向量数据库。
type DocumentProcessPayload struct {
	RequestId                string   `json:"request_id"`
	TenantID                 uint64   `json:"tenant_id"`
	KnowledgeID              string   `json:"knowledge_id"`
	KnowledgeBaseID          string   `json:"knowledge_base_id"`   // 知识库 ID
	FilePath                 string   `json:"file_path,omitempty"` // 文件路径（文件导入时使用）
	FileName                 string   `json:"file_name,omitempty"` // 文件名（文件导入时使用）
	FileType                 string   `json:"file_type,omitempty"` // 文件类型（文件导入时使用）
	URL                      string   `json:"url,omitempty"`       // URL（URL导入时使用）
	FileURL                  string   `json:"file_url,omitempty"`  // 文件资源链接（file_url导入时使用）
	Passages                 []string `json:"passages,omitempty"`  // 文本段落（文本导入时使用）
	EnableMultimodel         bool     `json:"enable_multimodel"`
	EnableQuestionGeneration bool     `json:"enable_question_generation"` // 是否启用问题生成
	QuestionCount            int      `json:"question_count,omitempty"`   // 每个chunk生成的问题数量
}

// FAQImportPayload represents the FAQ import task payload (including dry run mode)
//
// 用于处理 FAQ（常见问题解答）的批量导入任务。
//
//  1. 基础上下文与标识 (Context & Identity)
//     TenantID (uint64): 租户 ID，用于多租户隔离。
//     TaskID (string): 任务唯一标识。用于追踪整个导入流程的状态（如：进行中、成功、失败）。
//     KBID (string): 知识库 ID (Knowledge Base ID)。指定 FAQ 数据最终要写入哪个知识库。
//     KnowledgeID (string): 知识 ID。
//
//     含义: 在“空跑/验证”模式下，系统只检查数据格式而不真正写入数据库，因此不需要指定具体的知识逻辑 ID；只有在真实导入时才需要。
//
//  2. 数据来源与大小写策略 (Data Source & Scalability)
//     它根据数据量的大小提供了两种传输方式，避免大 Payload 导致消息队列（如 Kafka, RabbitMQ）阻塞或超时。
//
//     - 小数据量场景:
//     -- Entries ([]FAQEntryPayload): 这是一个切片，直接包含所有的问答对数据（问题、答案、分类等）。适用于几十或几百条数据的快速导入。
//
//     - 大数据量场景:
//     -- EntriesURL (string): 当数据量巨大（如成千上万条）时，数据会被预先上传到对象存储（如 S3/OSS）。这里只传递文件的下载链接，Worker 收到任务后先去下载文件再处理。
//     -- EntryCount (int): 配合 EntriesURL 使用。告知 Worker 预计有多少条数据，用于进度条展示、资源预分配或日志统计。
//
//  3. 执行模式控制 (Execution Control)
//     Mode (string): 定义导入的具体模式（例如：append 追加, overwrite 覆盖, update 更新）。具体枚举值取决于业务逻辑。
//     DryRun (bool): 空跑/试运行模式。true: 系统只进行数据校验（格式检查、重复性检查、敏感词过滤等），不执行实际的数据库写入操作；false: 执行完整导入流程。
//
//     用途: 让用户在正式导入大量数据前，先确认数据是否合法，避免污染知识库。
//
//  4. 并发与幂等性控制 (Concurrency & Idempotency)
//     EnqueuedAt (int64): 任务入队的时间戳（Unix Timestamp）。用于区分同一 TaskID 的不同次提交。
//
//  5. 依赖结构 (Nested Structure)
//     FAQEntryPayload: 它包含单个 FAQ 的核心信息，通常包括： Question、Answer、Category、，SimilarQuestions。
type FAQImportPayload struct {
	TenantID    uint64            `json:"tenant_id"`
	TaskID      string            `json:"task_id"`
	KBID        string            `json:"kb_id"`
	KnowledgeID string            `json:"knowledge_id,omitempty"` // 仅非 dry run 模式需要
	Entries     []FAQEntryPayload `json:"entries,omitempty"`      // 小数据量时直接存储在 payload 中
	EntriesURL  string            `json:"entries_url,omitempty"`  // 大数据量时存储到对象存储，这里存储 URL
	EntryCount  int               `json:"entry_count,omitempty"`  // 条目总数（使用 EntriesURL 时需要）
	Mode        string            `json:"mode"`
	DryRun      bool              `json:"dry_run"`     // dry run 模式只验证不导入
	EnqueuedAt  int64             `json:"enqueued_at"` // 任务入队时间戳，用于区分同一 TaskID 的不同次提交
}

// QuestionGenerationPayload represents the question generation task payload
//
// 在 RAG（检索增强生成）或智能客服场景中，这个任务通常是指：给定一个已有的文档（Knowledge），让 AI 自动挖掘并生成若干个用户可能会问的问题，以增强搜索的覆盖面或用于 FAQ 的冷启动。
//
// 应用场景：
//
//   - 存量数据优化 (Backfilling):
//     系统刚上线时导入了大量文档，当时为了速度关闭了问题生成功能（EnableQuestionGeneration=false）。
//     现在希望提升检索效果，可以构造此 Payload 发送一个后台任务，批量补全历史数据的问题索引。
//
//   - 策略调整 (Re-processing):
//
//     原本每个切片只生成 1 个问题，发现召回率不够。
//     管理员将 QuestionCount 调整为 5，重新提交此任务，让系统对现有数据进行全覆盖的重新生成。
//
//   - 独立异步任务:
//     问题生成是一个耗时且昂贵的操作（涉及 LLM 调用）。
//     将其设计为独立的任务 Payload，允许它在后台队列中慢慢处理，而不会阻塞用户的文档上传或编辑操作。
type QuestionGenerationPayload struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	KnowledgeID     string `json:"knowledge_id"`
	QuestionCount   int    `json:"question_count"`
}

// SummaryGenerationPayload represents the summary generation task payload
//
// 在知识库（RAG）或文档管理系统中，摘要生成通常用于为长文档提供精炼的概括，帮助用户在检索时快速预览内容，或者作为大模型在处理超长文本时的“预缩略”参考。
//
// 应用场景
//
//   - 文档预览：当用户在知识库列表中点击某个文档时，右侧弹窗显示的“内容简介”。
//
//   - 搜索增强：在向量检索时，除了匹配原文分片，还可以匹配摘要，提高召回的准确性。
//
//   - 自动标签：有时摘要生成的下一步就是“关键词提取”或“自动打标”。
type SummaryGenerationPayload struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	KnowledgeID     string `json:"knowledge_id"`
}

// KBClonePayload represents the knowledge base clone task payload
//
// 将一个现有的知识库（源知识库）完整地复制成一个新的知识库（目标知识库）。通常包括复制文档切片、向量数据、元数据、配置信息以及可能关联的问答对（FAQ）。
//
// 当后端收到这个 Payload 时，它通常需要完成以下几步操作，而不仅仅是简单的数据库拷贝：
//
//	-- 关系映射：需要将原有的文档关系、标签、FAQ 等在目标库重新建立。
//	-- 向量迁移：如果向量数据库支持，可以直接复制 Collection；如果不支持，可能需要重新计算或从源库提取 Embedding 写入新库。
//	-- 状态同步：克隆过程中，SourceID 的知识库通常应该进入“只读”状态，或者在克隆完成后记录一个时间戳切片。
//
// 应用场景
//
//   - 环境隔离与测试 (Staging vs Production):
//
//     场景：用户有一个正在运行的生产环境知识库 (SourceID)，想要测试新的分段策略或清理脏数据，但不想影响线上服务。
//     操作：克隆一份到新的 TargetID，在副本上进行实验。验证无误后，再切换流量或删除原库。
//
//   - 模板化快速部署:
//
//     平台方维护一个“标准知识库模板”（包含标准的分类、预设的 FAQ、基础的文档结构）。
//     当新客户入驻时，直接将模板库 (SourceID) 克隆给客户，生成专属的 TargetID。
//     这比逐个上传文件要快得多。
//
//   - 灾备与版本回滚:
//
//     在进行大规模数据清洗或重构前，先克隆一份当前状态作为“快照”。
//     如果新操作失败，可以基于克隆出的副本快速恢复，或者以副本为基准重新操作。
//
//   - 多语言/多区域分发:
//
//     拥有一个中文主知识库。
//     克隆一份作为基础，然后针对克隆出的副本进行翻译或本地化调整，形成英文版知识库，而无需从头构建结构。
type KBClonePayload struct {
	TenantID uint64 `json:"tenant_id"`
	TaskID   string `json:"task_id"`
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
}

// IndexDeletePayload represents the index delete task payload
//
// 在 RAG（检索增强生成）系统中，删除数据远比想象中复杂。
// 不仅要删掉数据库里的记录，更关键的是要同步删除向量数据库和全文检索引擎中的索引分片。
//
// 基础定位字段 (Context)
//
//   - TenantID (uint64): 租户 ID，确保操作隔离。
//   - KnowledgeBaseID (string): 知识库 ID，定位操作范围。
//   - KBType (string): 知识库类型（如 general, faq, document），不同类型可能采用不同的删除或清理策略。
//
// 核心过滤维度 (Filtering Dimensions)
//
//   - EmbeddingModelID (string): 关键字段。
//
//     含义: 指定要删除哪个嵌入模型对应的向量索引。
//     场景: 一个知识库可能同时存在多个版本的向量索引。
//     备注: 如果不指定（或为空），意味着删除该 Chunk 在所有模型下的索引。
//
//   - ChunkIDs ([]string):
//
//     含义: 需要删除的具体文本切片（Chunk）ID 列表。
//     粒度: 切片级删除。这比“删除整个文档”更灵活。
//     场景: 当文档内容发生局部修改（如修正了某一段落），系统可能只重新生成受影响的 Chunk，然后提交此 Payload 删除旧的 Chunk 索引，再插入新的。
//
// 执行引擎控制 (Execution Scope)
//   - EffectiveEngines ([]RetrieverEngineParams):
//     含义: 指定需要在哪些检索引擎中执行删除操作。
//     背景: 现代 RAG 系统通常是混合检索（Hybrid Search），数据可能同时存储在：向量数据库、全文搜索引擎、图数据库。
//     用途: 允许调用方精确控制只清理某个引擎的数据，或者同步清理所有引擎。RetrieverEngineParams 可能包含引擎类型、分片信息或连接配置。
//
// 应用场景
//
// 增量更新与重索引：
//
//	用户修改了文档中的某一段落。
//	系统流程：
//		生成新的 Chunk ID 和新向量。
//		构造 IndexDeletePayload，填入旧 Chunk ID 和 旧 Model ID，提交删除任务。
//		构造插入 Payload，写入新数据。
//	这样可以保证检索结果中不会出现“新旧版本共存”导致的幻觉或冲突。
//
// 模型迁移与清理：
//
//	租户决定将知识库的嵌入模型从 Model-A 切换到 Model-B ，切换完成后，Model-A 生成的向量数据变成了垃圾数据，占用昂贵的显存/内存。
//	提交此 Payload：ChunkIDs 为全量（或分批），EmbeddingModelID 指定为 Model-A，EffectiveEngines 指向向量库。
//	系统将精准清理旧模型数据，保留其他元数据。
//
// 权限回收：
//
//	某个敏感文档片段（Chunk）的访问权限被收回。
//	系统立即提交删除任务，将该 ChunkID 从所有检索引擎的索引中移除，确保即使用户发起搜索，也无法检索到该片段。
//
// 混合存储一致性维护：
//
//	如果向量数据库删除成功但 Elasticsearch 删除失败，会导致搜索结果有标题无摘要（或反之）。
//	通过 EffectiveEngines 参数，任务调度器可以确保在所有指定的引擎中重试删除操作，直到达成一致。
type IndexDeletePayload struct {
	TenantID         uint64                  `json:"tenant_id"`
	KnowledgeBaseID  string                  `json:"knowledge_base_id"`
	EmbeddingModelID string                  `json:"embedding_model_id"`
	KBType           string                  `json:"kb_type"`
	ChunkIDs         []string                `json:"chunk_ids"`
	EffectiveEngines []RetrieverEngineParams `json:"effective_engines"`
}

// KBDeletePayload represents the knowledge base delete task payload
//
// 与 IndexDeletePayload（针对特定切片或模型的细粒度删除）不同，KBDeletePayload 的目的是彻底销毁一个指定的知识库实例及其所有关联资源。
// 这是一个高风险、高权重的操作，通常涉及跨多个存储系统的级联清理。
//
// 应用场景
//   - 用户主动删除: 用户在管理界面点击“删除知识库”。前端构造此 Payload 发送给后端，触发异步删除任务。
//   - 租户注销/资源回收: 当 SaaS 租户停止订阅或账号被封禁时，系统批量遍历该租户下的所有 KnowledgeBaseID，并发发送 KBDeletePayload 以释放存储空间和计算资源。
//   - 测试环境清理: 自动化测试脚本在运行结束后，调用此接口清理生成的临时知识库，保持环境整洁。
//   - 合规性数据擦除 (GDPR/Right to be Forgotten): 当用户要求彻底删除其所有数据时，此 Payload 确保数据不仅从应用层消失，还从底层的向量库和搜索引擎中被物理删除或标记为不可恢复。
type KBDeletePayload struct {
	TenantID         uint64                  `json:"tenant_id"`
	KnowledgeBaseID  string                  `json:"knowledge_base_id"`
	EffectiveEngines []RetrieverEngineParams `json:"effective_engines"`
}

// KnowledgeListDeletePayload represents the batch knowledge delete task payload
type KnowledgeListDeletePayload struct {
	TenantID     uint64   `json:"tenant_id"`
	KnowledgeIDs []string `json:"knowledge_ids"`
}

// KBCloneTaskStatus represents the status of a knowledge base clone task
type KBCloneTaskStatus string

const (
	KBCloneStatusPending    KBCloneTaskStatus = "pending"
	KBCloneStatusProcessing KBCloneTaskStatus = "processing"
	KBCloneStatusCompleted  KBCloneTaskStatus = "completed"
	KBCloneStatusFailed     KBCloneTaskStatus = "failed"
)

// KBCloneProgress represents the progress of a knowledge base clone task
//
// KBCloneProgress 用于实时追踪和报告 “知识库克隆” 任务的执行状态。
// 它通常存储在 Redis 或数据库中，供前端通过 API 轮询或 WebSocket 实时推送给用户。
//
// 1. 进度量化 (Quantification)
// Progress (0-100): 直接给前端进度条组件使用的百分比。
// Total：源知识库中的文档总数或分片总数。
// Processed：已处理的文档总数。
//
// 2. 状态机管理 (State Machine)
// Status： 枚举值，如 Pending, Running, Success, Failed。
// Message: 用于展示当前正在做什么，例如：“正在迁移向量索引...”、“正在同步文档元数据...”。
// Error: 只有在 Status == Failed 时才会有值。它记录了导致克隆中断的具体原因（如“目标库空间不足”或“源库已不存在”）。
//
// 3. 时间线追踪 (Timeline)
// 心跳检查：如果 UpdatedAt 长时间没有更新，系统监控可以判定该克隆任务已“卡死（Stuck）”，从而触发自动重试或报警。
// 耗时统计：UpdatedAt - CreatedAt 可以计算出克隆一个特定规模知识库所需的平均时长，用于优化系统性能。
type KBCloneProgress struct {
	TaskID    string            `json:"task_id"`
	SourceID  string            `json:"source_id"`
	TargetID  string            `json:"target_id"`
	Status    KBCloneTaskStatus `json:"status"`
	Progress  int               `json:"progress"`   // 0-100
	Total     int               `json:"total"`      // 总知识数
	Processed int               `json:"processed"`  // 已处理数
	Message   string            `json:"message"`    // 状态消息
	Error     string            `json:"error"`      // 错误信息
	CreatedAt int64             `json:"created_at"` // 任务创建时间
	UpdatedAt int64             `json:"updated_at"` // 最后更新时间
}

// ChunkContext represents chunk content with surrounding context
//
// 在 RAG（检索增强生成）系统中，为了节省 Token 或适应向量数据库的限制，长文档通常被切分成较小的片段（Chunks）。
// 然而，单独的一个 Chunk 往往缺乏前后的完整语义（例如：代词指代不明、句子被截断）。
// 本结构体通过携带前一个和后一个切片的内容，恢复语义的完整性，从而让大模型（LLM）能更准确地回答问题。
//
// 参数说明
//
//	ChunkID: 当前切片的唯一标识符。
//	Content: 当前切片的核心文本内容。
//	PrevContent: 前一个切片的文本内容。
//	NextContent: 后一个切片的文本内容。
//
// 实践建议
//
//	通常 PrevContent 和 NextContent 不需要包含完整的相邻 Chunk，有时只需包含相邻 Chunk 的最后/最前 100-200 个字符即可达到良好的平衡。
//
// 应用场景
//
//  1. 提升 LLM 回答的准确性 (Context-Aware Generation)
//     问题: 直接投喂孤立的 Chunk 给 LLM，容易产生幻觉或逻辑断裂。
//     方案: 将 PrevContent + Content + NextContent 拼接成一个更大的 Prompt 窗口发送给 LLM。
//     效果: LLM 能看到完整的段落逻辑，准确理解代词（他/她/它）、指代关系和因果链条。
//
//  2. 智能高亮与用户体验 (Smart Highlighting)
//     场景: 前端展示搜索结果时。
//     用法: 虽然主要高亮 Content 中的关键词，但可以利用 PrevContent 和 NextContent 在 UI 上展示“更多上下文”的折叠面板，让用户在不点击原文的情况下就能读懂片段的前因后果。
//
//  3. 重排序优化 (Reranking Optimization)
//     场景: 使用 Cross-Encoder 模型对检索到的 Top-K 个 Chunk 进行重排序。
//     用法: 将带有上下文的完整文本输入重排序模型。模型能更准确地判断该片段与用户查询（Query）的相关性，因为语义更完整。
//     例子: 用户问“苹果公司的财报如何？”。孤立 Chunk 包含“营收增长了 20%”，但不知道是谁的营收。带上上下文后，重排序模型能确认这是关于“苹果公司”的，从而提高评分。
//
//  4. 滑动窗口检索策略的实现结果
//     这个结构体通常是后端执行滑动窗口（Sliding Window）或邻域检索策略后的产物。
//     检索时可能只匹配了 Content，但在构建返回给前端或 LLM 的对象时，系统去数据库查了该 ChunkID 的 prev_id 和 next_id，填入此结构体。
//
// 示例
//
//	假设原始文档是：
//		"...苹果发布了新款手机。它采用了最新的A17芯片，性能大幅提升。电池续航也增加了2小时，深受用户喜爱..."
//
//	切分后 (Size=15 chars):
//		Chunk A: "苹果发布了新款手机。"
//		Chunk B: "它采用了最新的A17芯片，" (匹配命中)
//		Chunk C: "性能大幅提升。电池续航也增加了2小时，深受用户喜爱..."
//
//	检索到 Chunk B 时，构建 ChunkContext:
//
//		{
//		 	"chunk_id": "chunk_b_001",
//		 	"content": "它采用了最新的A17芯片，",
//		 	"prev_content": "苹果发布了新款手机。",
//		 	"next_content": "性能大幅提升。电池续航也增加了2小时，深受用户喜爱..."
//		}
//
// 发送给 LLM 的 Prompt:
//
//	"参考以下上下文回答问题：
//	[前文]: 苹果发布了新款手机。
//	[核心]: 它采用了最新的A17芯片，
//	[后文]: 性能大幅提升。电池续航也增加了2小时...
//	问题：这款手机有什么特点？"
//
// LLM 回答:
//
//	"这款手机采用了最新的 A17 芯片，性能大幅提升，且电池续航增加了 2 小时。" (完美回答)
//
// 总结
//
//	ChunkContext 是解决文本切片碎片化问题的关键数据结构。
//	它通过在检索结果中动态注入邻域信息，以极小的存储和传输代价，显著提升了 RAG 系统的语义理解能力和回答质量。
//	它是连接“向量检索的粒度”与“大语言模型的上下文需求”之间的桥梁。
type ChunkContext struct {
	ChunkID     string `json:"chunk_id"`
	Content     string `json:"content"`
	PrevContent string `json:"prev_content,omitempty"` // Previous chunk content for context
	NextContent string `json:"next_content,omitempty"` // Next chunk content for context
}

// PromptTemplateStructured represents the prompt template structured
//
//  1. Description (描述)
//     作用：记录这个 Prompt 模板的设计意图。
//     场景：例如“用于法律文档的敏感信息脱敏”或“针对医疗报告的专业词汇提取”。这对于团队协作至关重要，让其他开发者一眼看出这个模板该怎么用。
//
//  2. Tags (标签)
//     作用：分类与检索。
//     维度：
//     - 功能维：#摘要、#分类、#纠错。
//     - 模型维：#GPT-4、#Claude-3（不同模型对 Prompt 的敏感度不同）。
//     - 领域维：#金融、#电商。
//
//  3. Examples ([]GraphData)
//     当 AI 需要根据一段文字生成图谱时，这些 Examples 能教导 AI 正确的 JSON 格式或拓扑结构。
type PromptTemplateStructured struct {
	Description string      `json:"description"`
	Tags        []string    `json:"tags"`
	Examples    []GraphData `json:"examples"`
}

type GraphNode struct {
	Name       string   `json:"name,omitempty"`       // 节点名称
	Chunks     []string `json:"chunks,omitempty"`     // 节点关联的源文档片段
	Attributes []string `json:"attributes,omitempty"` // 节点属性
}

// GraphRelation represents the relation of the graph
type GraphRelation struct {
	Node1 string `json:"node1,omitempty"`
	Node2 string `json:"node2,omitempty"`
	Type  string `json:"type,omitempty"` // 关系类型
}

type GraphData struct {
	Text     string           `json:"text,omitempty"`
	Node     []*GraphNode     `json:"node,omitempty"`     // 节点
	Relation []*GraphRelation `json:"relation,omitempty"` // 边(关系)
}

// NameSpace represents the name space of the knowledge base and knowledge
type NameSpace struct {
	KnowledgeBase string `json:"knowledge_base"`
	Knowledge     string `json:"knowledge"`
}

// Labels returns the labels of the name space
func (n NameSpace) Labels() []string {
	res := make([]string, 0)
	if n.KnowledgeBase != "" {
		res = append(res, n.KnowledgeBase)
	}
	if n.Knowledge != "" {
		res = append(res, n.Knowledge)
	}
	return res
}
