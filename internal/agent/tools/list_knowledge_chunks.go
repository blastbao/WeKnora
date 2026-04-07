package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 在 AI 对话中，单纯靠语义搜索召回的 3-5 个分块往往缺乏上下文。用户可能会问：“这段话的前后文在说什么？”
//	功能：通过 knowledge_id（文档 ID）直接拉取该文档的所有分块或指定页码的分块。
//	价值：允许 AI 在锁定目标文档后，进行“浸入式阅读”，获取比向量检索更完整的上下文信息。

// 核心功能
//	全量/分页读取：根据 knowledge_id（文档ID），按页拉取该文档下所有的文本分块（Chunk）。
//	多模态支持：不仅提取文本，还能解析并展示分块中关联的图片信息（URL、描述、OCR 识别出的文字）。
//	跨租户兼容：专门设计了逻辑来支持“共享知识库”场景，即当前用户可以读取属于其他租户但已共享给当前用户的文档内容。
//	上下文还原：提供 chunk_index（分块索引），让用户或 AI 知道当前读到的片段在原文中的位置（开头、中间还是结尾）。

// 执行流程：
//
// 1. 身份识别与权限穿透 (Identity & Permission)
//    - 调用 GetKnowledgeByIDOnly 跨租户获取文档元数据。
//    - 校验该文档所属 KB 是否在当前会话的已授权列表 (SearchTargets) 中，确保合规性。
//
// 2. 物理路由与上下文对齐 (Data Routing)
//    - 提取文档真实的 TenantID，作为物理检索的路由 Key。
//    - 切换至文档真实租户上下文，在正确的数据库表分区中定位分块数据。
//
// 3. 分页内容拉取 (Paged Content Fetch)
//    - 结合 Offset 和 Limit 计算分页参数，调用 Repository 执行结构化查询。
//    - 重点提取：纯文本内容 (Text) 与 问答对 (FAQ) 类型的分块，确保覆盖全量知识。
//
// 4. 多模态数据整合 (Multimodal Enrichment)
//    - 解析 ImageInfo 字段，将分块关联的图片 URL、Caption 及 OCR 文本一并封装。
//    - 为 AI 提供“文+图”的复合上下文，消除长文档阅读中的“信息死角”。
//
// 5. 结构化渲染 (Formatted Output)
//    - 生成 Output：供 LLM 直接阅读的顺序文本报告（含 Chunk Index 序号）。
//    - 生成 Data：供前端渲染的完整 JSON 数据包，包含详细的分页统计与元数据。

// 应用场景
//
// 1. 深度长文档分析与综合问答
//	痛点：用户询问涉及整本书或长篇报告的综合问题（如“总结《2025战略规划》的三大核心风险”），单纯的向量检索（RAG）只能返回零散的片段，缺乏全局视角。
//	工作流：
//		Agent 先用 grep_chunks 定位到目标文档 ID。
//		调用 list_knowledge_chunks 分页拉取该文档的前 N 页内容（或全部关键章节）。
//		LLM 基于获取的完整上下文进行逻辑推理和总结。
//	价值：从“碎片化检索”升级为“系统性阅读”，确保回答的逻辑连贯性和完整性。

// Q: 为什么 dag 系统里，处理的单元是 chunk ，而不是 doc ?
//
// 综述：
// 	Doc 是人类阅读和管理的自然单元（方便归档、命名）；
//	Chunk 是机器（LLM、向量数据库、DAG 引擎）理解和计算的自然单元。
//	RAG 系统的本质就是“用 Chunk 的方式存储和检索，最后组装成 Doc 的逻辑呈现给用户”。
//
// 具体原因
//  1. 突破上下文窗口限制
//     大模型每次对话能读进去的字数是有限的（即 Context Window）。
//     - Doc 模式的问题：一份技术手册可能有 500 页（几十万字），而 GPT-4 或 Claude 虽然窗口在变大，但一次性塞入整本书会极大消耗 Token，甚至直接爆掉窗口。
//     - Chunk 模式的优势：系统只把与问题最相关的 3-5 个片段（约 2000 字）喂给模型。这就像是“开卷考试”，AI 只需要看那几行关键答案，而不是背下整本书。
//
//  2. 提高“语义匹配”的精确度
//     向量检索（Semantic Search）是靠计算“距离”来工作的。
//     - Doc 模式的模糊性：如果一本书涵盖了“财务”、“技术”、“行政”三个主题，整本书的向量会变得非常“平庸”，很难精准匹配具体的财务问题。
//     - Chunk 模式的尖锐性：每个分块只讲一个具体点，当用户问“出差怎么报销”时，这个分块的向量会非常“尖锐”地指向答案，检索精度显著提升。
//
// 	3. 实现细粒度的溯源 (Granular Citation)
//		Chunk 携带了精确的 chunk_index 和 start/end 位置信息，提供了像素级的引用能力，极大提升了用户体验和可信度。
//		- Doc 级：只能告诉用户“出自《员工手册》第 1-200 页”。用户还得自己去翻。
//		- Chunk 级：可以精确告诉用户“出自《员工手册》第 15 页第 3 段”，甚至直接高亮显示那句话。
//
// 	4. 优化向量嵌入的质量 (Embedding Quality)
//		向量模型（Embedding Model）也有输入长度限制（通常 512 或 8192 tokens）。
//		如果把整本书转成一个向量，这个向量会是全书所有概念的“平均混合体”，变得非常模糊，无法准确代表任何一个具体知识点。
//		Chunk 方案：每个 Chunk 表达一个完整的语义单元（如一个概念、一个步骤）。
//		优势：生成的向量语义聚焦。当用户查询“打车标准”时，能精准匹配到包含该语义的 Chunk 向量，而不是匹配到一本混杂了“考勤、招聘、福利”的模糊文档向量。
//
//
//	5. 支持灵活的 DAG 工作流编排 (Workflow Flexibility)
//
//	6. 多模态与异构数据处理
//		场景：一个 Doc 里既有文字，又有表格，还有图片。
//		Chunk 方案：
//			- Chunk A: 纯文本段落。
//			- Chunk B: 解析后的表格（Markdown 格式）。
//			- Chunk C: 图片 + OCR 文本。
//		优势：系统可以对不同类型的 Chunk 采用不同的处理策略（例如：对表格 Chunk 使用专门的 Table-QA 模型，对文本 Chunk 使用通用模型）。
//		如果是 Doc 级别，就很难区分处理。

// Q: 为什么 dag 系统里，处理的单元是 chunk ，而不是 doc ?
//
// 1. 解决“全局语义”与“局部精度”的矛盾
//
// Doc 的局限：
//
//	文档级向量（Embedding）是对全文信息的高维压缩与平均。
//	当文档很长时，具体细节（如某个参数、某条条款）的语义特征会被大量无关文本稀释，导致向量空间中的定位模糊。
//
// Chunk 的优势：
//
//	Chunk 强制将语义收敛在固定长度内（如 500 tokens），保证了每个向量都代表一个高密度的单一语义主题。
//	在向量检索时，能实现从“模糊的相关性匹配”到“精确的语义命中”，大幅提升召回率（Recall）和准确率（Precision）。
//
// 2. 突破“输入窗口”与“计算成本”的硬约束
//
// Doc 的局限：
//
//	LLM 的 Context Window（上下文窗口）是有限的资源。
//	若以 Doc 为单位，一旦文档长度超过阈值，系统必须截断或拒绝处理；
//	即使未超限，将整篇长文输入也会产生巨大的 Token 成本，且引发“迷失”现象，降低模型推理质量。
//
// Chunk 的优势：
//
//	Chunk 是可计量的原子单位。
//	DAG 可以通过 limit/offset 机制，动态计算并仅加载“最相关的 N 个 Chunk”进入 LLM 上下文。
//	这使得系统能够处理无限长度的知识库，同时将每次推理的成本和延迟控制在恒定范围内（O(1) 复杂度而非 O(N)）。
//
// 3. 实现 DAG 节点的“算子解耦”与“数据流控制”
//
// Doc 的局限：
//
//	如果数据流传递的是 Doc，下游节点（如 Rerank、Summary）必须内置复杂的解析逻辑来拆解文档，导致节点功能耦合，无法复用。
//	且无法对文档的局部进行独立操作（例如：只重排序文档的第 3 节而忽略第 1 节）。
//
// Chunk 的优势：
//
//	Chunk 是标准的数据接口协议。
//	  - 路由节点只需读取 Chunk 的 Meta 信息即可决策，无需加载正文。
//	  - 重排序节点可直接对 Chunk 列表进行矩阵运算打分。
//	  - 合并节点可根据索引相邻性，将多个 Chunk 重组为新的逻辑块。
//
// 这种原子化使得 DAG 中的每个算子都能专注于单一逻辑，通过组合不同的算子即可构建复杂的工作流，无需修改底层数据结构。
//
// 4. 满足“细粒度溯源”与“增量更新”的工程需求
//
// Doc 的局限：
//   - 溯源难：回答只能引用到“某文档”，用户需人工二次查找具体位置，体验断层。
//   - 更新贵：文档中修改一句话，会导致整个文档的向量和哈希值变化，必须重新索引整篇文档，计算开销大且实时性差。
//
// Chunk 的优势：
//   - 精准锚点：Chunk 自带 start_index, end_index, page_number 等元数据，可直接映射到原文的具体字符位置，实现像素级引用。
//   - 局部失效：修改文档某处时，仅需重新计算受影响的那几个 Chunk 的向量和索引，其余部分保持不变，极大降低了维护成本和延迟。
var listKnowledgeChunksTool = BaseTool{
	name: ToolListKnowledgeChunks,
	description: `Retrieve full chunk content for a document by knowledge_id.

## Use After grep_chunks or knowledge_search:
1. grep_chunks(["keyword", "变体"]) → get knowledge_id  
2. list_knowledge_chunks(knowledge_id) → read full content

## When to Use:
- Need full content of chunks from a known document
- Want to see context around specific chunks
- Check how many chunks a document has

## Parameters:
- knowledge_id (required): Document ID
- limit (optional): Chunks per page (default 20, max 100)
- offset (optional): Start position (default 0)

## Output:
Full chunk content with chunk_id, chunk_index, and content text.`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "knowledge_id": {
      "type": "string",
      "description": "Document ID to retrieve chunks from"
    },
    "limit": {
      "type": "integer",
      "description": "Chunks per page (default 20, max 100)",
      "default": 20,
      "minimum": 1,
      "maximum": 100
    },
    "offset": {
      "type": "integer",
      "description": "Start position (default 0)",
      "default": 0,
      "minimum": 0
    }
  },
  "required": ["knowledge_id", "limit", "offset"]
}`),
}

// ListKnowledgeChunksInput defines the input parameters for list knowledge chunks tool
type ListKnowledgeChunksInput struct {
	KnowledgeID string `json:"knowledge_id"`
	Limit       int    `json:"limit"`
	Offset      int    `json:"offset"`
}

// ListKnowledgeChunksTool retrieves chunk snapshots for a specific knowledge document.
type ListKnowledgeChunksTool struct {
	BaseTool
	chunkService     interfaces.ChunkService
	knowledgeService interfaces.KnowledgeService
	searchTargets    types.SearchTargets // Pre-computed unified search targets with KB-tenant mapping
}

// NewListKnowledgeChunksTool creates a new tool instance.
func NewListKnowledgeChunksTool(
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
) *ListKnowledgeChunksTool {
	return &ListKnowledgeChunksTool{
		BaseTool:         listKnowledgeChunksTool,
		chunkService:     chunkService,
		knowledgeService: knowledgeService,
		searchTargets:    searchTargets,
	}
}

// Execute 执行知识文档块列表查询工具，获取指定知识文件的文档片段
//
// 功能说明:
//   - 解析并验证输入参数，提取目标知识文件ID
//   - 验证知识文件存在性，并检查当前Agent是否有权限访问（通过searchTargets验证）
//   - 支持跨租户共享知识库场景，使用知识文件的实际租户ID查询
//   - 分页查询文档块（Chunk）列表，支持文本块和FAQ类型
//   - 处理文档块中的图片信息（OCR文本、URL、描述等）
//   - 返回格式化的文档块列表，供Agent详细分析知识内容
//
// 参数:
//   - ctx: 上下文对象，包含租户信息、用户权限等
//   - args: JSON格式的原始参数，包含以下字段：
//     - KnowledgeID: string 知识文件ID（必填）
//     - Limit: int 每页数量（可选，默认20）
//     - Offset: int 偏移量（可选，默认0）
//
// 返回值:
//   - *types.ToolResult: 工具执行结果，包含以下内容：
//     - Success: 查询是否成功完成（true/false）
//     - Output: 格式化的文档块列表文本（Markdown格式，包含文件信息和片段预览）
//     - Data: 结构化数据（map[string]interface{}），包含：
//         * knowledge_id: string 知识文件ID
//         * knowledge_title: string 知识文件标题
//         * total_chunks: int 文档块总数
//         * fetched_chunks: int 本次获取的文档块数量
//         * page: int 当前页码
//         * page_size: int 每页大小
//         * chunks: []map[string]interface{} 文档块详情列表，每个块包含：
//           - seq: int 序号
//           - chunk_id: string 块ID
//           - chunk_index: int 块索引
//           - content: string 文本内容
//           - chunk_type: string 块类型（text/faq）
//           - knowledge_id: string 所属知识文件ID
//           - knowledge_base: string 所属知识库ID
//           - start_at: int 起始位置
//           - end_at: int 结束位置
//           - parent_chunk_id: string 父块ID（如有）
//           - images: []map[string]string 图片信息列表（如有），包含：
//             * url: string 图片URL
//             * caption: string 图片描述
//             * ocr_text: string OCR识别文本
//     - Error: 错误信息（执行失败时）
//   - error: 工具框架层面的错误，业务逻辑错误通过ToolResult.Error返回
//
// 执行流程:
//   1. 解析输入参数 → 2. 验证KnowledgeID必填且非空
//   3. 查询知识文件信息（跨租户支持）→ 4. 验证知识库访问权限（searchTargets检查）
//   5. 确定有效租户ID（支持共享KB场景）→ 6. 计算分页参数
//   7. 分页查询文档块（文本+FAQ类型）→ 8. 验证查询结果
//   9. 查询知识文件标题 → 10. 构建格式化输出
//   11. 处理文档块图片信息（解析ImageInfo JSON）→ 12. 组装返回结果
//
// 权限控制:
//   - 知识库级别：通过t.searchTargets.ContainsKB()验证，确保只能访问Agent授权的知识库
//   - 租户隔离：使用知识文件的实际租户ID查询（effectiveTenantID），支持跨租户共享
//   - 无租户过滤：查询知识文件时跳过租户过滤（GetKnowledgeByIDOnly），支持共享场景
//
// 分页逻辑:
//   - Limit: 默认20，可通过参数指定
//   - Offset: 从0开始，负数自动修正为0
//   - Page计算: offset/limit + 1
//   - 类型过滤: 仅查询ChunkTypeText和ChunkTypeFAQ类型
//
// 图片信息处理:
//   - 解析ImageInfo字段（JSON数组格式）
//   - 提取URL、Caption、OCRText字段
//   - 过滤空值，仅保留有效信息
//   - 组装为结构化列表附加到文档块数据
//
// 错误处理:
//   - 参数解析失败: 返回解析错误
//   - KnowledgeID缺失或为空: 返回"knowledge_id is required"
//   - 知识文件不存在: 返回"Knowledge not found"
//   - 知识库无访问权限: 返回"Knowledge base %s is not accessible"
//   - 查询执行失败: 返回数据库错误
//   - 结果为空: 返回"chunk query returned no data"
//
// 跨租户共享支持:
//   - 查询知识文件时跳过租户过滤（GetKnowledgeByIDOnly）
//   - 使用知识文件.TenantID作为effectiveTenantID查询文档块
//   - 权限检查在知识库级别完成（searchTargets.ContainsKB）
//
// 日志记录:
//   - 无显式日志记录（依赖调用方或底层服务日志）
//
// 使用场景:
//   - Agent需要查看某知识文件的完整内容片段
//   - 用户通过@提及指定文件后，Agent获取详细内容分析
//   - 需要获取文档中的图片OCR信息辅助理解
//
// 示例:
//   result, err := tool.Execute(ctx, []byte(`{
//       "KnowledgeID": "doc-abc123",
//       "Limit": 10,
//       "Offset": 0
//   }`))
//
// 成功返回示例:
// result = &types.ToolResult{
//     Success: true,
//     Output: "## 知识文件: 产品安装手册\n\n**文件ID**: doc-abc123\n" +
//             "**总片段数**: 150\n**本次获取**: 10\n\n" +
//             "### 片段 1\n**内容**: 第一章 环境准备...\n\n" +
//             "### 片段 2\n**内容**: 1.1 系统要求...\n",
//     Data: map[string]interface{}{
//         "knowledge_id":    "doc-abc123",
//         "knowledge_title": "产品安装手册",
//         "total_chunks":    150,
//         "fetched_chunks":  10,
//         "page":            1,
//         "page_size":       10,
//         "chunks": []map[string]interface{}{
//             {
//                 "seq":          1,
//                 "chunk_id":     "chunk-001",
//                 "chunk_index":  0,
//                 "content":      "第一章 环境准备...",
//                 "chunk_type":   "text",
//                 "knowledge_id": "doc-abc123",
//                 "knowledge_base": "kb-product",
//                 "start_at":     0,
//                 "end_at":       500,
//                 "parent_chunk_id": "",
//                 "images": []map[string]string{
//                     {"url": "https://...", "caption": "架构图", "ocr_text": "系统架构..."},
//                 },
//             },
//			   {
//					"seq":             2,
//					"chunk_id":        "chunk-002",
//					"chunk_index":     1,
//					"content":         "1.1 系统要求\n- 操作系统: Linux...",
//					"chunk_type":      "text",
//					"knowledge_id":    "doc-abc123",
//					"knowledge_base":  "kb-product",
//					"start_at":        501,
//					"end_at":          1000,
//					"parent_chunk_id": "",
//					"images":          nil,
//				},
//
//             // ...更多片段
//         },
//     },
//     Error: "",
// }
//
// 失败返回示例:
// result = &types.ToolResult{
//     Success: false,
//     Output:  "",
//     Data:    nil,
//     Error:   "Knowledge base kb-product is not accessible",
// }

// Execute performs the chunk fetch against the chunk service.
func (t *ListKnowledgeChunksTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input ListKnowledgeChunksInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	knowledgeID := input.KnowledgeID
	ok := knowledgeID != ""
	if !ok || strings.TrimSpace(knowledgeID) == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_id is required",
		}, fmt.Errorf("knowledge_id is required")
	}
	knowledgeID = strings.TrimSpace(knowledgeID)

	// Get knowledge info without tenant filter to support shared KB
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Knowledge not found: %v", err),
		}, err
	}

	// Verify the knowledge's KB is in searchTargets (permission check)
	if !t.searchTargets.ContainsKB(knowledge.KnowledgeBaseID) {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Knowledge base %s is not accessible", knowledge.KnowledgeBaseID),
		}, fmt.Errorf("knowledge base not in search targets")
	}

	// Use the knowledge's actual tenant_id for chunk query (supports cross-tenant shared KB)
	effectiveTenantID := knowledge.TenantID

	chunkLimit := 20
	if input.Limit > 0 {
		chunkLimit = input.Limit
	}
	offset := 0
	if input.Offset > 0 {
		offset = input.Offset
	}
	if offset < 0 {
		offset = 0
	}

	pagination := &types.Pagination{
		Page:     offset/chunkLimit + 1,
		PageSize: chunkLimit,
	}

	chunks, total, err := t.chunkService.GetRepository().ListPagedChunksByKnowledgeID(ctx,
		effectiveTenantID, knowledgeID, pagination, []types.ChunkType{types.ChunkTypeText, types.ChunkTypeFAQ}, "", "", "", "", "")
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("failed to list chunks: %v", err),
		}, err
	}
	if chunks == nil {
		return &types.ToolResult{
			Success: false,
			Error:   "chunk query returned no data",
		}, fmt.Errorf("chunk query returned no data")
	}

	totalChunks := total
	fetched := len(chunks)

	knowledgeTitle := t.lookupKnowledgeTitle(ctx, knowledgeID)

	output := t.buildOutput(knowledgeID, knowledgeTitle, totalChunks, fetched, chunks)

	formattedChunks := make([]map[string]interface{}, 0, len(chunks))
	for idx, c := range chunks {
		chunkData := map[string]interface{}{
			"seq":             idx + 1,
			"chunk_id":        c.ID,
			"chunk_index":     c.ChunkIndex,
			"content":         c.Content,
			"chunk_type":      c.ChunkType,
			"knowledge_id":    c.KnowledgeID,
			"knowledge_base":  c.KnowledgeBaseID,
			"start_at":        c.StartAt,
			"end_at":          c.EndAt,
			"parent_chunk_id": c.ParentChunkID,
		}

		// 添加图片信息
		if c.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(c.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				imageList := make([]map[string]string, 0, len(imageInfos))
				for _, img := range imageInfos {
					imgData := make(map[string]string)
					if img.URL != "" {
						imgData["url"] = img.URL
					}
					if img.Caption != "" {
						imgData["caption"] = img.Caption
					}
					if img.OCRText != "" {
						imgData["ocr_text"] = img.OCRText
					}
					if len(imgData) > 0 {
						imageList = append(imageList, imgData)
					}
				}
				if len(imageList) > 0 {
					chunkData["images"] = imageList
				}
			}
		}

		formattedChunks = append(formattedChunks, chunkData)
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"knowledge_id":    knowledgeID,
			"knowledge_title": knowledgeTitle,
			"total_chunks":    totalChunks,
			"fetched_chunks":  fetched,
			"page":            pagination.Page,
			"page_size":       pagination.PageSize,
			"chunks":          formattedChunks,
		},
	}, nil
}

// lookupKnowledgeTitle looks up the title of a knowledge document
// Uses GetKnowledgeByIDOnly to support cross-tenant shared KB
func (t *ListKnowledgeChunksTool) lookupKnowledgeTitle(ctx context.Context, knowledgeID string) string {
	if t.knowledgeService == nil {
		return ""
	}
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil || knowledge == nil {
		return ""
	}
	return strings.TrimSpace(knowledge.Title)
}

// buildOutput builds the output for the list knowledge chunks tool
func (t *ListKnowledgeChunksTool) buildOutput(
	knowledgeID string,
	knowledgeTitle string,
	total int64,
	fetched int,
	chunks []*types.Chunk,
) string {
	builder := &strings.Builder{}
	builder.WriteString("=== 知识文档分块 ===\n\n")

	if knowledgeTitle != "" {
		fmt.Fprintf(builder, "文档: %s (%s)\n", knowledgeTitle, knowledgeID)
	} else {
		fmt.Fprintf(builder, "文档 ID: %s\n", knowledgeID)
	}
	fmt.Fprintf(builder, "总分块数: %d\n", total)

	if fetched == 0 {
		builder.WriteString("未找到任何分块，请确认文档是否已完成解析。\n")
		if total > 0 {
			builder.WriteString("文档存在但当前页数据为空，请检查分页参数。\n")
		}
		return builder.String()
	}
	fmt.Fprintf(
		builder,
		"本次拉取: %d 条， 检索范围: %d - %d\n\n",
		fetched,
		chunks[0].ChunkIndex,
		chunks[len(chunks)-1].ChunkIndex,
	)

	builder.WriteString("=== 分块内容预览 ===\n\n")
	for idx, c := range chunks {
		fmt.Fprintf(builder, "Chunk #%d (Index %d)\n", idx+1, c.ChunkIndex+1)
		fmt.Fprintf(builder, "  chunk_id: %s\n", c.ID)
		fmt.Fprintf(builder, "  类型: %s\n", c.ChunkType)
		fmt.Fprintf(builder, "  内容: %s\n", summarizeContent(c.Content))

		// 输出关联的图片信息
		if c.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(c.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				fmt.Fprintf(builder, "  关联图片 (%d):\n", len(imageInfos))
				for imgIdx, img := range imageInfos {
					fmt.Fprintf(builder, "    图片 %d:\n", imgIdx+1)
					if img.URL != "" {
						fmt.Fprintf(builder, "      URL: %s\n", img.URL)
					}
					if img.Caption != "" {
						fmt.Fprintf(builder, "      描述: %s\n", img.Caption)
					}
					if img.OCRText != "" {
						fmt.Fprintf(builder, "      OCR文本: %s\n", img.OCRText)
					}
				}
			}
		}
		builder.WriteString("\n")
	}

	if int64(fetched) < total {
		builder.WriteString("提示：文档仍有更多分块，可调整 offset 或多次调用以获取全部内容。\n")
	}

	return builder.String()
}

// summarizeContent summarizes the content of a chunk
func summarizeContent(content string) string {
	cleaned := strings.TrimSpace(content)
	if cleaned == "" {
		return "(空内容)"
	}

	return strings.TrimSpace(string(cleaned))
}
