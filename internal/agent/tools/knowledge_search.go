package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// knowledgeSearchTool 在知识库中查找与用户意图最相关的内容。
// 它不依赖简单的关键词匹配，而是通过向量嵌入（Embedding）理解语义，并结合混合搜索、重排序（Rerank）和多样性优化（MMR）算法，确保返回结果既准确又丰富。

// 1. 参数解析与查询目标确定
//	输入解析：接收 JSON 参数，提取 queries 和可选的 knowledge_base_ids（指定搜索范围）。
//	目标过滤：
//		- 若指定了知识库 ID，则从指定知识库中搜索。
//		- 若未指定，则使用所有配置的可搜索知识库。
//		- 若无可用目标，直接返回错误。
//	配置加载：从租户配置或全局配置中加载搜索参数（如 topK、向量阈值、关键词阈值），若无配置则使用默认值。
//
// 2. 并发混合搜索 (Concurrent Hybrid Search)
//	并行执行：针对每个查询语句（Query）和每个搜索目标（Target），启动独立的 Goroutine 并发执行混合搜索（Hybrid Search）。
//	混合机制：内部同时执行向量搜索（Vector Search）和关键词搜索（Keyword Search）。
//	结果融合：使用 RRF (Reciprocal Rank Fusion) 算法将两路搜索结果合并，生成归一化的综合得分。
//	元数据封装：将原始搜索结果封装，附加来源查询、匹配类型（hybrid）、知识库 ID 及类型等元数据。
//
// 3. 去重预处理
//	多维去重：在重排序之前，先对原始结果进行去重。
//	去重策略：基于 Chunk ID、父 Chunk ID、知识库+索引位置以及内容指纹（Content Signature）进行多重校验，保留得分最高的条目，减少后续计算开销。
//
// 4. 智能重排序 (Reranking)
//	系统优先使用 LLM 进行重排序，若不可用则降级使用专用 Rerank 模型。
//	 - FAQ 特殊处理：FAQ 类型的结果被视为精确匹配，保留原始高分，不参与重排序模型计算，直接并入最终列表。
//	 - LLM 重排序 (首选)：
//		- 分批处理：为避免 Token 超限，将结果每 15 条分为一批。
//		- Prompt 评分：构造 Prompt 让 LLM 根据相关性（0.0-1.0）对每一批结果打分。
//		- 解析与 fallback：解析 LLM 返回的分数，若解析失败或调用出错，则该批次回退到原始得分。
//	 - 模型重排序 (备选)：若 LLM 不可用，调用专用的 Rerank 模型接口进行打分。
//	 - 复合得分计算：在重排序得分基础上，结合原始得分、数据源权重（Web 搜索略降权）和位置优先因子（文档前部内容略加权），计算最终复合得分。
//
// 5. 多样性优化 (MMR)
//	使用 MMR 算法，在保持高相关性的同时，剔除内容高度重复的条目，确保返回结果的多样性。
//	参数：设定 Lambda=0.7，平衡相关性与多样性；选取前 topK 个结果。
//
// 6. 最终整理与输出格式化
//	 - 二次去重与排序：对经过 MMR 处理的结果再次去重，并按最终得分降序排列。
//	 - 内容增强：
//		- 图片信息：若 Chunk 关联图片，解析并提取图片描述（Caption）和 OCR 文本，拼接到正文中。
//		- FAQ 信息：若为 FAQ 类型，提取标准问题、相似问法及选项答案。
//	 - 统计信息：计算每个文档的召回率（已召回 Chunk 数 / 总 Chunk 数），生成检索统计建议。
//	 - 空结果处理：若未找到任何结果，返回明确的引导信息，提示模型不要编造答案，并根据配置建议使用联网搜索或直接告知用户未找到信息。
//	 - 构建返回：生成包含人类可读文本（Output）和结构化数据（Data）的 ToolResult。

// 执行示例
//
//	场景：用户询问“RAG 架构是如何工作的？”，系统配置了内部文档库和 FAQ 库。
//	输入参数：
//		{
//		 	"queries": ["How does RAG architecture work?", "RAG workflow explanation"],
//		 	"knowledge_base_ids": []
//		}
//
//	执行过程：
//		- 解析参数：提取出两个语义查询，确定搜索所有可用知识库。加载配置 topK=10, vector_threshold=0.6。
//		- 并发搜索：
//			- 启动多个 Goroutine，分别在“技术文档库”和“常见问题库”中执行混合搜索。
//			- 底层返回 50 条原始匹配记录（包含向量和关键词匹配）。
//	预处理：去除完全相同的 Chunk，剩余 35 条唯一记录。
//	重排序：
//		识别出 2 条 FAQ 记录（精确匹配），直接保留。
//		将其余 33 条记录分 3 批发送给 LLM。
//		LLM 返回新的相关性分数（例如某条原始得分低但语义极佳的记录被提权）。
//		计算复合得分，融合 FAQ 记录。
//	MMR 优化：
//		发现前 5 条中有 3 条内容高度相似（都在讲定义）。
//		MMR 算法剔除冗余，替换为一条讲“具体实施步骤”的记录，确保前 10 条结果覆盖定义、流程、优缺点等不同维度。
//	格式化：
//		提取每条结果中的图片描述（如有）。
//		计算统计信息：“技术文档库”共 1000 个 Chunk，召回 8 个（0.8%）。
//		组装最终文本。

//	最终输出内容 (Output 片段)：
//
//	=== Search Results ===
//	Found 10 relevant results
//
//	Knowledge Base Coverage:
//	 - tech-docs-kb: 8 results
//	 - faq-kb: 2 results
//
//	=== Detailed Results ===
//
//	[Source Document: RAG Architecture Guide]
//
//	Result #1:
//	 [chunk_id: c_101][chunk_index: 5]
//	Content: RAG (Retrieval-Augmented Generation) works by first retrieving relevant documents from a vector database based on the user query, then injecting these documents into the LLM's context window to ground the generation...
//	 Related Images (1):
//	   Image 1:
//		 Caption: Diagram of RAG flow
//		 OCR Text: Query -> Retriever -> Context -> LLM -> Answer
//
//	Result #2:
//	 [chunk_id: f_05][chunk_index: 0]
//	Content: Standard Question: How does RAG work?
//	 FAQ Standard Question: How does RAG work?
//	 FAQ Answers: It combines retrieval and generation...
//
//	=== 检索统计与建议 ===
//
//	文档: RAG Architecture Guide (tech-docs-kb)
//	 总 Chunk 数: 1000
//	 已召回: 8 个 (0.8%)
//	 未召回: 992 个

var knowledgeSearchTool = BaseTool{
	name: ToolKnowledgeSearch,
	description: `Semantic/vector search tool for retrieving knowledge by meaning, intent, and conceptual relevance.

This tool uses embeddings to understand the user's query and find semantically similar content across knowledge base chunks.

## Purpose
Designed for high-level understanding tasks, such as:
- conceptual explanations
- topic overviews
- reasoning-based information needs
- contextual or intent-driven retrieval
- queries that cannot be answered with literal keyword matching

The tool searches by MEANING rather than exact text. It identifies chunks that are conceptually relevant even when the wording differs.

## What the Tool Does NOT Do
- Does NOT perform exact keyword matching
- Does NOT search for specific named entities
- Should NOT be used for literal lookup tasks
- Should NOT receive long raw text or user messages as queries
- Should NOT be used to locate specific strings or error codes

For literal/keyword/entity search, another tool should be used.

## Required Input Behavior
"queries" must contain **1–5 short, well-formed semantic questions or conceptual statements** that clearly express the meaning the model is trying to retrieve.

Each query should represent a **concept, idea, topic, explanation, or intent**, such as:
- abstract topics
- definitions
- mechanisms
- best practices
- comparisons
- how/why questions

Avoid:
- keyword lists
- raw text from user messages
- full paragraphs
- unprocessed input

## Examples of valid query shapes (not content):
- "What is the main idea of..."
- "How does X work in general?"
- "Explain the purpose of..."
- "What are the key principles behind..."
- "Overview of ..."

## Parameters
- queries (required): 1–5 semantic questions or conceptual statements.
  These should reflect the meaning or topic you want embeddings to capture.
- knowledge_base_ids (optional): limit the search scope.

## Output
Returns chunks ranked by semantic similarity, reranked when applicable.  
Results represent conceptual relevance, not literal keyword overlap.`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "queries": {
      "type": "array",
      "description": "REQUIRED: 1-5 semantic questions/topics (e.g., ['What is RAG?', 'RAG benefits'])",
      "items": {
        "type": "string"
      },
      "minItems": 1,
      "maxItems": 5
    },
    "knowledge_base_ids": {
      "type": "array",
      "description": "Optional: KB IDs to search",
      "items": {
        "type": "string"
      },
      "minItems": 0,
      "maxItems": 10
    }
  },
  "required": ["queries"]
}`),
}

// KnowledgeSearchInput defines the input parameters for knowledge search tool
type KnowledgeSearchInput struct {
	Queries          []string `json:"queries"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids,omitempty"`
}

// searchResultWithMeta wraps search result with metadata about which query matched it
type searchResultWithMeta struct {
	*types.SearchResult
	SourceQuery       string
	QueryType         string // "vector" or "keyword"
	KnowledgeBaseID   string // ID of the knowledge base this result came from
	KnowledgeBaseType string // Type of the knowledge base (document, faq, etc.)
}

// KnowledgeSearchTool searches knowledge bases with flexible query modes
type KnowledgeSearchTool struct {
	BaseTool
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	chunkService         interfaces.ChunkService
	searchTargets        types.SearchTargets // Pre-computed unified search targets
	rerankModel          rerank.Reranker
	chatModel            chat.Chat      // Optional chat model for LLM-based reranking
	config               *config.Config // Global config for fallback values
}

// NewKnowledgeSearchTool creates a new knowledge search tool
func NewKnowledgeSearchTool(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
	rerankModel rerank.Reranker,
	chatModel chat.Chat,
	cfg *config.Config,
) *KnowledgeSearchTool {
	return &KnowledgeSearchTool{
		BaseTool:             knowledgeSearchTool,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		chunkService:         chunkService,
		searchTargets:        searchTargets,
		rerankModel:          rerankModel,
		chatModel:            chatModel,
		config:               cfg,
	}
}

// Execute 执行知识库搜索工具，检索与查询相关的文档片段
//
// 功能说明:
//   - 解析并验证工具输入参数，提取查询语句和目标知识库
//   - 支持用户指定知识库范围，或复用预配置的搜索目标
//   - 执行多路并发检索（向量搜索+关键词搜索的混合检索）
//   - 对检索结果进行去重、重排序、多样性优化（MMR）
//   - 返回格式化的搜索结果，供Agent推理使用
//
// 参数:
//   - ctx: 上下文对象，包含租户信息、配置等
//   - args: JSON格式的原始参数，包含以下字段：
//     - Queries: []string 查询语句列表（必填，支持多查询）
//     - KnowledgeBaseIDs: []string 指定搜索的知识库ID列表（可选）
//
// 返回值:
//   - *types.ToolResult: 工具执行结果，包含以下内容：
//     - Success: 搜索是否成功完成（true/false）
//     - Output: 格式化的搜索结果文本（Markdown格式，包含引用来源）
//     - Data: 结构化数据，包含：
//         * results: []SearchResult 搜索结果列表
//         * queries: []string 实际执行的查询
//         * knowledge_base_ids: []string 搜索的知识库ID
//         * total_results: int 总结果数
//         * search_params: map 搜索参数（topK、阈值等）
//     - Error: 错误信息（执行失败时）
//   - error: 工具框架层面的错误，业务逻辑错误通过ToolResult.Error返回
//
// 执行流程:
//   1. 解析输入参数 → 2. 确定搜索目标（过滤指定KB）→ 3. 验证搜索目标
//   4. 获取搜索参数（租户配置→全局配置→默认值）→ 5. 执行并发混合检索
//   6. 初次去重 → 7. 重排序（LLM或专用模型）→ 8. MMR多样性优化
//   9. 最终去重 → 10. 按分数排序 → 11. 格式化输出
//
// 搜索策略:
//   - 混合检索: 向量相似度 + 关键词匹配，使用RRF融合排序
//   - 并发执行: 多查询、多知识库并行检索
//   - 智能重排: 优先使用LLM-based重排，其次使用专用重排模型
//   - 多样性优化: MMR算法平衡相关性与多样性，避免结果冗余
//
// 参数优先级（搜索参数获取）:
//   租户ConversationConfig > 全局Config > 硬编码默认值
//   默认: topK=5, vectorThreshold=0.6, keywordThreshold=0.5, minScore=0.3
//
// 结果处理流程:
//   原始结果 → 去重 → 重排序 → MMR筛选 → 最终去重 → 排序 → 格式化
//
// 日志记录:
//   - Info: 执行开始、参数详情、搜索目标、结果统计、Top5结果预览
//   - Debug: 输入参数JSON、搜索参数、MMR过程、去重过程
//   - Error: 参数解析失败、搜索目标缺失、查询缺失、格式化失败
//   - Warn: 重排序失败（降级使用原始结果）、MMR无结果
//
// 错误处理:
//   - 参数解析失败: 返回解析错误
//   - 无搜索目标: 返回"no search targets available"
//   - 无查询语句: 返回"queries parameter is required"
//   - 重排序失败: 降级使用去重后的原始结果，记录警告
//   - MMR失败: 降级使用重排序结果，记录警告
//   - 格式化失败: 返回格式化错误
//
// 注意事项:
//   - 重排序阶段优先使用chatModel（LLM-based），其次使用rerankModel
//   - RRF分数范围约为[0, 0.033]，与传统[0,1]分数不同，阈值过滤在HybridSearch内部完成
//   - MMR的k值取min(结果数, topK)，lambda固定为0.7
//   - 多查询场景下，重排序使用合并后的查询语句
//
// 示例:
//   result, err := tool.Execute(ctx, []byte(`{
//       "Queries": ["产品功能介绍", "价格方案"],
//       "KnowledgeBaseIDs": ["kb-sales", "kb-product"]
//   }`))
//
// 成功返回示例:
// result = &types.ToolResult{
//    Success: true,
//    Output:  `## 搜索结果
//				**查询**: 产品功能介绍
//
//				### 结果 1 (相关度: 0.85)
//				**来源**: kb-product / 产品手册 v2.1
//				**内容**: 我们的核心产品支持多租户架构，提供灵活的权限管理...
//
//				### 结果 2 (相关度: 0.72)
//				**来源**: kb-sales / 销售培训资料
//				**内容**: 产品主要功能包括：1.智能检索 2.知识图谱 3.对话分析...`,
//	   Data: map[string]interface{}{
//		   "results": []SearchResult{
//			   {
//				   ID:            "chunk-abc123",
//				   KnowledgeID:   "doc-456",
//				   KnowledgeName: "产品手册 v2.1",
//				   Content:       "我们的核心产品支持多租户架构...",
//				   Score:         0.85,
//				   QueryType:     "vector",
//			   },
//			   // ...更多结果
//		   },
//		   "queries":            []string{"产品功能介绍"},
//		   "knowledge_base_ids": []string{"kb-product", "kb-sales"},
//		   "total_results":      8,
//		   "search_params": map[string]interface{}{
//			   "top_k":             5,
//			   "vector_threshold":  0.6,
//			   "keyword_threshold": 0.5,
//			   "min_score":         0.3,
//		   },
//	   },
//	   Error: "",
//	}

// Execute executes the knowledge search tool
func (t *KnowledgeSearchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Execute started")

	// Parse args from json.RawMessage
	var input KnowledgeSearchInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Log input arguments
	argsJSON, _ := json.MarshalIndent(input, "", "  ")
	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Input args:\n%s", string(argsJSON))

	// Determine which KBs to search - user can optionally filter to specific KBs
	var userSpecifiedKBs []string
	if len(input.KnowledgeBaseIDs) > 0 {
		userSpecifiedKBs = input.KnowledgeBaseIDs
		logger.Infof(ctx, "[Tool][KnowledgeSearch] User specified %d knowledge bases: %v", len(userSpecifiedKBs), userSpecifiedKBs)
	}

	// Use pre-computed search targets, optionally filtered by user-specified KBs
	searchTargets := t.searchTargets
	if len(userSpecifiedKBs) > 0 {
		// Filter search targets to only include user-specified KBs
		userKBSet := make(map[string]bool)
		for _, kbID := range userSpecifiedKBs {
			userKBSet[kbID] = true
		}
		var filteredTargets types.SearchTargets
		for _, target := range t.searchTargets {
			if userKBSet[target.KnowledgeBaseID] {
				filteredTargets = append(filteredTargets, target)
			}
		}
		searchTargets = filteredTargets
	}

	// Validate search targets
	if len(searchTargets) == 0 {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] No search targets available")
		return &types.ToolResult{
			Success: false,
			Error:   "no knowledge bases specified and no search targets configured",
		}, fmt.Errorf("no search targets available")
	}

	kbIDs := searchTargets.GetAllKnowledgeBaseIDs()
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Using %d search targets across %d KBs", len(searchTargets), len(kbIDs))

	// Parse query parameter
	queries := input.Queries

	// Validate: query must be provided
	if len(queries) == 0 {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] No queries provided")
		return &types.ToolResult{
			Success: false,
			Error:   "queries parameter is required",
		}, fmt.Errorf("no queries provided")
	}

	logger.Infof(ctx, "[Tool][KnowledgeSearch] Queries: %v", queries)

	// Get search parameters from tenant conversation config, fallback to global config
	var topK int
	var vectorThreshold, keywordThreshold, minScore float64

	// Try to get from tenant conversation config
	if tenantVal := ctx.Value(types.TenantInfoContextKey); tenantVal != nil {
		if tenant, ok := tenantVal.(*types.Tenant); ok && tenant != nil && tenant.ConversationConfig != nil {
			cc := tenant.ConversationConfig
			if cc.EmbeddingTopK > 0 {
				topK = cc.EmbeddingTopK
			}
			if cc.VectorThreshold > 0 {
				vectorThreshold = cc.VectorThreshold
			}
			if cc.KeywordThreshold > 0 {
				keywordThreshold = cc.KeywordThreshold
			}
			// minScore is not in ConversationConfig, use default or config
			minScore = 0.3
		}
	}

	// Fallback to global config if not set
	if topK == 0 && t.config != nil {
		topK = t.config.Conversation.EmbeddingTopK
	}
	if vectorThreshold == 0 && t.config != nil {
		vectorThreshold = t.config.Conversation.VectorThreshold
	}
	if keywordThreshold == 0 && t.config != nil {
		keywordThreshold = t.config.Conversation.KeywordThreshold
	}

	// Final fallback to hardcoded defaults if config is not available
	if topK == 0 {
		topK = 5
	}
	if vectorThreshold == 0 {
		vectorThreshold = 0.6
	}
	if keywordThreshold == 0 {
		keywordThreshold = 0.5
	}
	if minScore == 0 {
		minScore = 0.3
	}

	logger.Infof(
		ctx,
		"[Tool][KnowledgeSearch] Search params: top_k=%d, vector_threshold=%.2f, keyword_threshold=%.2f, min_score=%.2f",
		topK,
		vectorThreshold,
		keywordThreshold,
		minScore,
	)

	// Execute concurrent search using pre-computed search targets
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Starting concurrent search with %d search targets",
		len(searchTargets))
	kbTypeMap := t.getKnowledgeBaseTypes(ctx, kbIDs)

	allResults := t.concurrentSearchByTargets(ctx, queries, searchTargets,
		topK, vectorThreshold, keywordThreshold, kbTypeMap)
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Concurrent search completed: %d raw results", len(allResults))

	// Note: HybridSearch now uses RRF (Reciprocal Rank Fusion) which produces normalized scores
	// RRF scores are in range [0, ~0.033] (max when rank=1 on both sides: 2/(60+1))
	// Threshold filtering is already done inside HybridSearch before RRF, so we skip it here

	// Deduplicate before reranking to reduce processing overhead
	deduplicatedBeforeRerank := t.deduplicateResults(allResults)

	// Apply ReRank if model is configured
	// Prefer chatModel (LLM-based reranking) over rerankModel if both are available
	// Use first query for reranking (or combine all queries if needed)
	rerankQuery := ""
	if len(queries) > 0 {
		rerankQuery = queries[0]
		if len(queries) > 1 {
			// Combine multiple queries for reranking
			rerankQuery = strings.Join(queries, " ")
		}
	}

	// Variable to hold results through reranking and MMR stages
	var filteredResults []*searchResultWithMeta

	if t.chatModel != nil && len(deduplicatedBeforeRerank) > 0 && rerankQuery != "" {
		logger.Infof(
			ctx,
			"[Tool][KnowledgeSearch] Applying LLM-based rerank with model: %s, input: %d results, queries: %v",
			t.chatModel.GetModelName(),
			len(deduplicatedBeforeRerank),
			queries,
		)
		rerankedResults, err := t.rerankResults(ctx, rerankQuery, deduplicatedBeforeRerank)
		if err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] LLM rerank failed, using original results: %v", err)
			filteredResults = deduplicatedBeforeRerank
		} else {
			filteredResults = rerankedResults
			logger.Infof(ctx, "[Tool][KnowledgeSearch] LLM rerank completed successfully: %d results",
				len(filteredResults))
		}
	} else if t.rerankModel != nil && len(deduplicatedBeforeRerank) > 0 && rerankQuery != "" {
		logger.Infof(ctx, "[Tool][KnowledgeSearch] Applying rerank with model: %s, input: %d results, queries: %v",
			t.rerankModel.GetModelName(), len(deduplicatedBeforeRerank), queries)
		rerankedResults, err := t.rerankResults(ctx, rerankQuery, deduplicatedBeforeRerank)
		if err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] Rerank failed, using original results: %v", err)
			filteredResults = deduplicatedBeforeRerank
		} else {
			filteredResults = rerankedResults
			logger.Infof(ctx, "[Tool][KnowledgeSearch] Rerank completed successfully: %d results",
				len(filteredResults))
		}
	} else {
		// No reranking, use deduplicated results
		filteredResults = deduplicatedBeforeRerank
	}

	// Apply MMR (Maximal Marginal Relevance) to reduce redundancy and improve diversity
	// Note: composite scoring is already applied inside rerankResults
	if len(filteredResults) > 0 {
		// Calculate k for MMR: use min(len(results), max(1, topK))
		mmrK := len(filteredResults)
		if topK > 0 && mmrK > topK {
			mmrK = topK
		}
		if mmrK < 1 {
			mmrK = 1
		}
		// Apply MMR with lambda=0.7 (balance between relevance and diversity)
		logger.Debugf(
			ctx,
			"[Tool][KnowledgeSearch] Applying MMR: k=%d, lambda=0.7, input=%d results",
			mmrK,
			len(filteredResults),
		)
		mmrResults := t.applyMMR(ctx, filteredResults, mmrK, 0.7)
		if len(mmrResults) > 0 {
			filteredResults = mmrResults
			logger.Infof(ctx, "[Tool][KnowledgeSearch] MMR completed: %d results selected", len(filteredResults))
		} else {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] MMR returned no results, using original results")
		}
	}

	// Note: minScore filter is skipped because HybridSearch now uses RRF scores
	// RRF scores are in range [0, ~0.033], not [0, 1], so old thresholds don't apply
	// Threshold filtering is already done inside HybridSearch before RRF fusion

	// Final deduplication after rerank (in case rerank changed scores/order but duplicates remain)
	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Final deduplication after rerank...")
	deduplicatedResults := t.deduplicateResults(filteredResults)
	logger.Infof(ctx, "[Tool][KnowledgeSearch] After final deduplication: %d results (from %d)",
		len(deduplicatedResults), len(filteredResults))

	// Sort results by score (descending)
	sort.Slice(deduplicatedResults, func(i, j int) bool {
		if deduplicatedResults[i].Score != deduplicatedResults[j].Score {
			return deduplicatedResults[i].Score > deduplicatedResults[j].Score
		}
		// If scores are equal, sort by knowledge ID for consistency
		return deduplicatedResults[i].KnowledgeID < deduplicatedResults[j].KnowledgeID
	})

	// Log top results
	if len(deduplicatedResults) > 0 {
		for i := 0; i < len(deduplicatedResults) && i < 5; i++ {
			r := deduplicatedResults[i]
			logger.Infof(ctx, "[Tool][KnowledgeSearch][Top %d] score=%.3f, type=%s, kb=%s, chunk_id=%s",
				i+1, r.Score, r.QueryType, r.KnowledgeID, r.ID)
		}
	}

	// Build output
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Formatting output with %d final results", len(deduplicatedResults))
	result, err := t.formatOutput(ctx, deduplicatedResults, kbIDs, queries)
	if err != nil {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] Failed to format output: %v", err)
		return result, err
	}
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Output: %s", result.Output)
	return result, nil
}

// getKnowledgeBaseTypes fetches knowledge base types for the given IDs
func (t *KnowledgeSearchTool) getKnowledgeBaseTypes(ctx context.Context, kbIDs []string) map[string]string {
	kbTypeMap := make(map[string]string, len(kbIDs))

	for _, kbID := range kbIDs {
		if kbID == "" {
			continue
		}
		if _, exists := kbTypeMap[kbID]; exists {
			continue
		}

		kb, err := t.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to fetch knowledge base %s info: %v", kbID, err)
			continue
		}

		kbTypeMap[kbID] = kb.Type
	}

	return kbTypeMap
}

// concurrentSearchByTargets executes hybrid search using pre-computed search targets
// This avoids duplicate searches when a knowledge file is already covered by its KB's full search
func (t *KnowledgeSearchTool) concurrentSearchByTargets(
	ctx context.Context,
	queries []string,
	searchTargets types.SearchTargets,
	topK int,
	vectorThreshold, keywordThreshold float64,
	kbTypeMap map[string]string,
) []*searchResultWithMeta {
	var wg sync.WaitGroup
	var mu sync.Mutex
	allResults := make([]*searchResultWithMeta, 0)

	for _, query := range queries {
		q := query
		for _, target := range searchTargets {
			st := target
			wg.Add(1)
			go func() {
				defer wg.Done()

				searchParams := types.SearchParams{
					QueryText:        q,
					MatchCount:       topK,
					VectorThreshold:  vectorThreshold,
					KeywordThreshold: keywordThreshold,
				}

				// If target has specific knowledge IDs, add them to search params
				if st.Type == types.SearchTargetTypeKnowledge {
					searchParams.KnowledgeIDs = st.KnowledgeIDs
				}

				kbResults, err := t.knowledgeBaseService.HybridSearch(ctx, st.KnowledgeBaseID, searchParams)
				if err != nil {
					logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to search KB %s: %v", st.KnowledgeBaseID, err)
					return
				}

				// Wrap results with metadata
				mu.Lock()
				for _, r := range kbResults {
					allResults = append(allResults, &searchResultWithMeta{
						SearchResult:      r,
						SourceQuery:       q,
						QueryType:         "hybrid",
						KnowledgeBaseID:   st.KnowledgeBaseID,
						KnowledgeBaseType: kbTypeMap[st.KnowledgeBaseID],
					})
				}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	return allResults
}

// rerankResults applies reranking to search results using LLM prompt scoring or rerank model
func (t *KnowledgeSearchTool) rerankResults(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	// Separate FAQ and normal results.
	// FAQ results keep original scores and bypass reranking model.
	faqResults := make([]*searchResultWithMeta, 0)
	rerankCandidates := make([]*searchResultWithMeta, 0, len(results))

	for _, result := range results {
		// Skip reranking for FAQ results (they are explicitly matched Q&A pairs)
		if result.KnowledgeBaseType == types.KnowledgeBaseTypeFAQ {
			faqResults = append(faqResults, result)
		} else {
			rerankCandidates = append(rerankCandidates, result)
		}
	}

	// If there are no candidates to rerank, return original list (already all FAQ)
	if len(rerankCandidates) == 0 {
		return results, nil
	}

	var (
		rerankedCandidates []*searchResultWithMeta
		err                error
	)

	// Apply reranking only to candidates
	// Try rerankModel first, fallback to chatModel if rerankModel fails or returns no results
	if t.rerankModel != nil {
		rerankedCandidates, err = t.rerankWithModel(ctx, query, rerankCandidates)
		// If rerankModel fails or returns no results, fallback to chatModel
		if err != nil || len(rerankedCandidates) == 0 {
			if err != nil {
				logger.Warnf(ctx, "[Tool][KnowledgeSearch] Rerank model failed, falling back to chat model: %v", err)
			} else {
				logger.Warnf(ctx, "[Tool][KnowledgeSearch] Rerank model returned no results, falling back to chat model")
			}
			// Reset error to allow fallback
			err = nil
			// Try chatModel if available
			if t.chatModel != nil {
				rerankedCandidates, err = t.rerankWithLLM(ctx, query, rerankCandidates)
			} else {
				// No fallback available, use original results
				rerankedCandidates = rerankCandidates
			}
		}
	} else if t.chatModel != nil {
		// No rerankModel, use chatModel directly
		rerankedCandidates, err = t.rerankWithLLM(ctx, query, rerankCandidates)
	} else {
		// No reranking available, use original results
		rerankedCandidates = rerankCandidates
	}

	if err != nil {
		return nil, err
	}

	// Apply composite scoring to reranked results
	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Applying composite scoring")

	// Store base scores before composite scoring
	for _, result := range rerankedCandidates {
		baseScore := result.Score
		// Apply composite score
		result.Score = t.compositeScore(result, result.Score, baseScore)
	}

	// Combine FAQ results (with original order) and reranked candidates
	combined := make([]*searchResultWithMeta, 0, len(results))
	combined = append(combined, faqResults...)
	combined = append(combined, rerankedCandidates...)

	// Sort by score (descending) to keep consistent output order
	sort.Slice(combined, func(i, j int) bool {
		return combined[i].Score > combined[j].Score
	})

	return combined, nil
}

func (t *KnowledgeSearchTool) getFAQMetadata(
	ctx context.Context,
	chunkID string,
	cache map[string]*types.FAQChunkMetadata,
) (*types.FAQChunkMetadata, error) {
	if chunkID == "" || t.chunkService == nil {
		return nil, nil
	}

	if meta, ok := cache[chunkID]; ok {
		return meta, nil
	}

	chunk, err := t.chunkService.GetChunkByID(ctx, chunkID)
	if err != nil {
		cache[chunkID] = nil
		return nil, err
	}
	if chunk == nil {
		cache[chunkID] = nil
		return nil, nil
	}

	meta, err := chunk.FAQMetadata()
	if err != nil {
		cache[chunkID] = nil
		return nil, err
	}
	cache[chunkID] = meta
	return meta, nil
}

// rerankWithLLM uses LLM prompt to score and rerank search results
// Uses batch processing to handle large result sets efficiently
func (t *KnowledgeSearchTool) rerankWithLLM(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Using LLM for reranking %d results", len(results))

	if len(results) == 0 {
		return results, nil
	}

	// Batch size: process 15 results at a time to balance quality and token usage
	// This prevents token overflow and improves processing efficiency
	const batchSize = 15
	const maxContentLength = 800 // Maximum characters per passage to avoid excessive tokens

	// Process in batches
	allScores := make([]float64, len(results))
	allReranked := make([]*searchResultWithMeta, 0, len(results))

	for batchStart := 0; batchStart < len(results); batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > len(results) {
			batchEnd = len(results)
		}

		batch := results[batchStart:batchEnd]
		logger.Debugf(ctx, "[Tool][KnowledgeSearch] Processing rerank batch %d-%d of %d results",
			batchStart+1, batchEnd, len(results))

		// Build prompt with query and batch passages
		var passagesBuilder strings.Builder
		for i, result := range batch {
			// Get enriched passage (content + image info)
			enrichedContent := t.getEnrichedPassage(ctx, result.SearchResult)
			// Truncate content if too long to save tokens
			content := enrichedContent
			if len([]rune(content)) > maxContentLength {
				runes := []rune(content)
				content = string(runes[:maxContentLength]) + "..."
			}
			// Use clear separators to distinguish each passage
			if i > 0 {
				passagesBuilder.WriteString("\n")
			}
			passagesBuilder.WriteString("─────────────────────────────────────────────────────────────\n")
			passagesBuilder.WriteString(fmt.Sprintf("Passage %d:\n", i+1))
			passagesBuilder.WriteString("─────────────────────────────────────────────────────────────\n")
			passagesBuilder.WriteString(content + "\n")
		}

		// Optimized prompt focused on retrieval matching and reranking
		prompt := fmt.Sprintf(
			`You are a search result reranking expert. Your task is to evaluate how well each retrieved passage matches the user's search query and information need.

User Query: %s

Your task: Rerank these search results by evaluating their retrieval relevance - how well each passage answers or relates to the query.

Scoring Criteria (0.0 to 1.0):
- 1.0 (0.9-1.0): Directly answers the query, contains key information needed, highly relevant
- 0.8 (0.7-0.8): Strongly related, provides substantial relevant information
- 0.6 (0.5-0.6): Moderately related, contains some relevant information but may be incomplete
- 0.4 (0.3-0.4): Weakly related, minimal relevance to the query
- 0.2 (0.1-0.2): Barely related, mostly irrelevant
- 0.0 (0.0): Completely irrelevant, no relation to the query

Evaluation Factors:
1. Query-Answer Match: Does the passage directly address what the user is asking?
2. Information Completeness: Does it provide sufficient information to answer the query?
3. Semantic Relevance: Does the content semantically relate to the query intent?
4. Key Term Coverage: Does it cover important terms/concepts from the query?
5. Information Accuracy: Is the information accurate and trustworthy?

Retrieved Passages:
%s

IMPORTANT: Return exactly %d scores, one per line, in this exact format:
Passage 1: X.XX
Passage 2: X.XX
Passage 3: X.XX
...
Passage %d: X.XX

Output only the scores, no explanations or additional text.`,
			query,
			passagesBuilder.String(),
			len(batch),
			len(batch),
		)

		messages := []chat.Message{
			{
				Role:    "system",
				Content: "You are a professional search result reranking expert specializing in information retrieval. You evaluate how well retrieved passages match user queries in search scenarios. Focus on retrieval relevance: whether the passage answers the query, provides needed information, and matches the user's information need. Always respond with scores only, no explanations.",
			},
			{
				Role:    "user",
				Content: prompt,
			},
		}

		// Calculate appropriate max tokens based on batch size
		// Each score line is ~15 tokens, add buffer for safety
		maxTokens := len(batch)*20 + 100

		response, err := t.chatModel.Chat(ctx, messages, &chat.ChatOptions{
			Temperature: 0.1, // Low temperature for consistent scoring
			MaxTokens:   maxTokens,
		})
		if err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] LLM rerank batch %d-%d failed: %v, using original scores",
				batchStart+1, batchEnd, err)
			// Use original scores for this batch on error
			for i := batchStart; i < batchEnd; i++ {
				allScores[i] = results[i].Score
			}
			continue
		}

		logger.Infof(ctx, "[Tool][KnowledgeSearch] LLM rerank batch %d-%d response: %s",
			batchStart+1, batchEnd, response.Content)

		// Parse scores from response
		batchScores, err := t.parseScoresFromResponse(response.Content, len(batch))
		if err != nil {
			logger.Warnf(
				ctx,
				"[Tool][KnowledgeSearch] Failed to parse LLM scores for batch %d-%d: %v, using original scores",
				batchStart+1,
				batchEnd,
				err,
			)
			// Use original scores for this batch on parsing error
			for i := batchStart; i < batchEnd; i++ {
				allScores[i] = results[i].Score
			}
			continue
		}

		// Store scores for this batch
		for i, score := range batchScores {
			if batchStart+i < len(allScores) {
				allScores[batchStart+i] = score
			}
		}
	}

	// Create reranked results with new scores
	for i, result := range results {
		newResult := *result
		if i < len(allScores) {
			newResult.Score = allScores[i]
		}
		allReranked = append(allReranked, &newResult)
	}

	// Sort by new scores (descending)
	sort.Slice(allReranked, func(i, j int) bool {
		return allReranked[i].Score > allReranked[j].Score
	})

	logger.Infof(ctx, "[Tool][KnowledgeSearch] LLM reranked %d results from %d original results (processed in batches)",
		len(allReranked), len(results))
	return allReranked, nil
}

// parseScoresFromResponse parses scores from LLM response text
func (t *KnowledgeSearchTool) parseScoresFromResponse(responseText string, expectedCount int) ([]float64, error) {
	lines := strings.Split(strings.TrimSpace(responseText), "\n")
	scores := make([]float64, 0, expectedCount)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Try to extract score from various formats:
		// "Passage 1: 0.85"
		// "1: 0.85"
		// "0.85"
		// etc.
		parts := strings.Split(line, ":")
		var scoreStr string
		if len(parts) >= 2 {
			scoreStr = strings.TrimSpace(parts[len(parts)-1])
		} else {
			scoreStr = strings.TrimSpace(line)
		}

		// Remove any non-numeric characters except decimal point
		scoreStr = strings.TrimFunc(scoreStr, func(r rune) bool {
			return (r < '0' || r > '9') && r != '.'
		})

		if scoreStr == "" {
			continue
		}

		score, err := strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			continue // Skip invalid scores
		}

		// Clamp score to [0.0, 1.0]
		if score < 0.0 {
			score = 0.0
		}
		if score > 1.0 {
			score = 1.0
		}

		scores = append(scores, score)
	}

	if len(scores) == 0 {
		return nil, fmt.Errorf("no valid scores found in response")
	}

	// If we got fewer scores than expected, pad with last score or 0.5
	for len(scores) < expectedCount {
		if len(scores) > 0 {
			scores = append(scores, scores[len(scores)-1])
		} else {
			scores = append(scores, 0.5)
		}
	}

	// Truncate if we got more scores than expected
	if len(scores) > expectedCount {
		scores = scores[:expectedCount]
	}

	return scores, nil
}

// rerankWithModel uses the rerank model for reranking (fallback)
func (t *KnowledgeSearchTool) rerankWithModel(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	// Prepare passages for reranking (with enriched content including image info)
	passages := make([]string, len(results))
	for i, result := range results {
		passages[i] = t.getEnrichedPassage(ctx, result.SearchResult)
	}

	// Call rerank model
	rerankResp, err := t.rerankModel.Rerank(ctx, query, passages)
	if err != nil {
		return nil, fmt.Errorf("rerank call failed: %w", err)
	}

	// Map reranked results back with new scores
	reranked := make([]*searchResultWithMeta, 0, len(rerankResp))
	for _, rr := range rerankResp {
		if rr.Index >= 0 && rr.Index < len(results) {
			// Create new result with reranked score
			newResult := *results[rr.Index]
			newResult.Score = rr.RelevanceScore
			reranked = append(reranked, &newResult)
		}
	}

	logger.Infof(
		ctx,
		"[Tool][KnowledgeSearch] Reranked %d results from %d original results",
		len(reranked),
		len(results),
	)
	return reranked, nil
}

// deduplicateResults removes duplicate chunks, keeping the highest score
// Uses multiple keys (ID, parent chunk ID, knowledge+index) and content signature for deduplication
func (t *KnowledgeSearchTool) deduplicateResults(results []*searchResultWithMeta) []*searchResultWithMeta {
	seen := make(map[string]bool)
	contentSig := make(map[string]bool)
	uniqueResults := make([]*searchResultWithMeta, 0)

	for _, r := range results {
		// Build multiple keys for deduplication
		keys := []string{r.ID}
		if r.ParentChunkID != "" {
			keys = append(keys, "parent:"+r.ParentChunkID)
		}
		if r.KnowledgeID != "" {
			keys = append(keys, fmt.Sprintf("kb:%s#%d", r.KnowledgeID, r.ChunkIndex))
		}

		// Check if any key is already seen
		dup := false
		for _, k := range keys {
			if seen[k] {
				dup = true
				break
			}
		}
		if dup {
			continue
		}

		// Check content signature for near-duplicate content
		sig := t.buildContentSignature(r.Content)
		if sig != "" {
			if contentSig[sig] {
				continue
			}
			contentSig[sig] = true
		}

		// Mark all keys as seen
		for _, k := range keys {
			seen[k] = true
		}

		uniqueResults = append(uniqueResults, r)
	}

	// If we have duplicates by ID but different scores, keep the highest score
	// This handles cases where the same chunk appears multiple times with different scores
	seenByID := make(map[string]*searchResultWithMeta)
	for _, r := range uniqueResults {
		if existing, ok := seenByID[r.ID]; ok {
			// Keep the result with higher score
			if r.Score > existing.Score {
				seenByID[r.ID] = r
			}
		} else {
			seenByID[r.ID] = r
		}
	}

	// Convert back to slice
	deduplicated := make([]*searchResultWithMeta, 0, len(seenByID))
	for _, r := range seenByID {
		deduplicated = append(deduplicated, r)
	}

	return deduplicated
}

// buildContentSignature creates a normalized signature for content to detect near-duplicates
func (t *KnowledgeSearchTool) buildContentSignature(content string) string {
	return searchutil.BuildContentSignature(content)
}

// formatOutput formats the search results for display
func (t *KnowledgeSearchTool) formatOutput(
	ctx context.Context,
	results []*searchResultWithMeta,
	kbsToSearch []string,
	queries []string,
) (*types.ToolResult, error) {
	if len(results) == 0 {
		data := map[string]interface{}{
			"knowledge_base_ids": kbsToSearch,
			"results":            []interface{}{},
			"count":              0,
		}
		if len(queries) > 0 {
			data["queries"] = queries
		}
		output := fmt.Sprintf("No relevant content found in %d knowledge base(s).\n\n", len(kbsToSearch))
		output += "=== ⚠️ CRITICAL - Next Steps ===\n"
		output += "- ❌ DO NOT use training data or general knowledge to answer\n"
		output += "- ✅ If web_search is enabled: You MUST use web_search to find information\n"
		output += "- ✅ If web_search is disabled: State 'I couldn't find relevant information in the knowledge base'\n"
		output += "- NEVER fabricate or infer answers - ONLY use retrieved content\n"

		return &types.ToolResult{
			Success: true,
			Output:  output,
			Data:    data,
		}, nil
	}

	// Build output header
	output := "=== Search Results ===\n"
	output += fmt.Sprintf("Found %d relevant results", len(results))
	output += "\n\n"

	// Count results by KB
	kbCounts := make(map[string]int)
	for _, r := range results {
		kbCounts[r.KnowledgeID]++
	}

	output += "Knowledge Base Coverage:\n"
	for kbID, count := range kbCounts {
		output += fmt.Sprintf("  - %s: %d results\n", kbID, count)
	}
	output += "\n=== Detailed Results ===\n\n"

	// Format individual results
	formattedResults := make([]map[string]interface{}, 0, len(results))
	currentKB := ""

	faqMetadataCache := make(map[string]*types.FAQChunkMetadata)

	// Track chunks per knowledge for statistics
	knowledgeChunkMap := make(map[string]map[int]bool) // knowledge_id -> set of chunk_index
	knowledgeTotalMap := make(map[string]int64)        // knowledge_id -> total chunks
	knowledgeTitleMap := make(map[string]string)       // knowledge_id -> title

	for i, result := range results {
		var faqMeta *types.FAQChunkMetadata
		if result.KnowledgeBaseType == types.KnowledgeBaseTypeFAQ {
			meta, err := t.getFAQMetadata(ctx, result.ID, faqMetadataCache)
			if err != nil {
				logger.Warnf(
					ctx,
					"[Tool][KnowledgeSearch] Failed to load FAQ metadata for chunk %s: %v",
					result.ID,
					err,
				)
			} else {
				faqMeta = meta
			}
		}

		// Track chunk indices per knowledge
		if knowledgeChunkMap[result.KnowledgeID] == nil {
			knowledgeChunkMap[result.KnowledgeID] = make(map[int]bool)
		}
		knowledgeChunkMap[result.KnowledgeID][result.ChunkIndex] = true
		knowledgeTitleMap[result.KnowledgeID] = result.KnowledgeTitle

		// Group by knowledge base
		if result.KnowledgeID != currentKB {
			currentKB = result.KnowledgeID
			if i > 0 {
				output += "\n"
			}
			output += fmt.Sprintf("[Source Document: %s]\n", result.KnowledgeTitle)

			// Get total chunk count for this knowledge (cache it)
			// Use KB's tenant_id from searchTargets to support cross-tenant shared KB
			if _, exists := knowledgeTotalMap[result.KnowledgeID]; !exists {
				// Get tenant_id from searchTargets using the KB ID from the result
				effectiveTenantID := t.searchTargets.GetTenantIDForKB(result.KnowledgeBaseID)
				if effectiveTenantID == 0 {
					logger.Warnf(ctx, "[Tool][KnowledgeSearch] KB %s not found in searchTargets, skipping chunk count", result.KnowledgeBaseID)
					knowledgeTotalMap[result.KnowledgeID] = 0
				} else {
					_, total, err := t.chunkService.GetRepository().ListPagedChunksByKnowledgeID(ctx,
						effectiveTenantID, result.KnowledgeID,
						&types.Pagination{Page: 1, PageSize: 1},
						[]types.ChunkType{types.ChunkTypeText}, "", "", "", "", "",
					)
					if err != nil {
						logger.Warnf(
							ctx,
							"[Tool][KnowledgeSearch] Failed to get total chunks for knowledge %s: %v",
							result.KnowledgeID,
							err,
						)
						knowledgeTotalMap[result.KnowledgeID] = 0
					} else {
						knowledgeTotalMap[result.KnowledgeID] = total
					}
				}
			}
		}

		// relevanceLevel := GetRelevanceLevel(result.Score)
		output += fmt.Sprintf("\nResult #%d:\n", i+1)
		output += fmt.Sprintf(
			"  [chunk_id: %s][chunk_index: %d]\nContent: %s\n",
			result.ID,
			result.ChunkIndex,
			result.Content,
		)

		// 解析并输出关联的图片信息
		if result.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				output += fmt.Sprintf("  Related Images (%d):\n", len(imageInfos))
				for imgIdx, img := range imageInfos {
					output += fmt.Sprintf("    Image %d:\n", imgIdx+1)
					if img.URL != "" {
						output += fmt.Sprintf("      URL: %s\n", img.URL)
					}
					if img.Caption != "" {
						output += fmt.Sprintf("      Caption: %s\n", img.Caption)
					}
					if img.OCRText != "" {
						output += fmt.Sprintf("      OCR Text: %s\n", img.OCRText)
					}
				}
			}
		}

		if faqMeta != nil {
			if faqMeta.StandardQuestion != "" {
				output += fmt.Sprintf("  FAQ Standard Question: %s\n", faqMeta.StandardQuestion)
			}
			if len(faqMeta.SimilarQuestions) > 0 {
				output += fmt.Sprintf("  FAQ Similar Questions: %s\n", strings.Join(faqMeta.SimilarQuestions, "; "))
			}
			if len(faqMeta.Answers) > 0 {
				output += "  FAQ Answers:\n"
				for ansIdx, ans := range faqMeta.Answers {
					output += fmt.Sprintf("    Answer Choice %d: %s\n", ansIdx+1, ans)
				}
			}
		}

		formattedResults = append(formattedResults, map[string]interface{}{
			"result_index": i + 1,
			"chunk_id":     result.ID,
			"content":      result.Content,
			// "score":        result.Score,
			// "relevance_level":     relevanceLevel,
			"knowledge_id":        result.KnowledgeID,
			"knowledge_title":     result.KnowledgeTitle,
			"match_type":          result.MatchType,
			"source_query":        result.SourceQuery,
			"query_type":          result.QueryType,
			"knowledge_base_type": result.KnowledgeBaseType,
		})

		last := formattedResults[len(formattedResults)-1]

		// 添加图片信息到结构化数据
		if result.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				// 构建简化的图片信息列表
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
					last["images"] = imageList
				}
			}
		}

		if faqMeta != nil {
			if faqMeta.StandardQuestion != "" {
				last["faq_standard_question"] = faqMeta.StandardQuestion
			}
			if len(faqMeta.SimilarQuestions) > 0 {
				last["faq_similar_questions"] = faqMeta.SimilarQuestions
			}
			if len(faqMeta.Answers) > 0 {
				last["faq_answers"] = faqMeta.Answers
			}
		}
	}

	// Add statistics and recommendations for each knowledge
	output += "\n=== 检索统计与建议 ===\n\n"
	for knowledgeID, retrievedChunks := range knowledgeChunkMap {
		totalChunks := knowledgeTotalMap[knowledgeID]
		retrievedCount := len(retrievedChunks)
		title := knowledgeTitleMap[knowledgeID]

		if totalChunks > 0 {
			percentage := float64(retrievedCount) / float64(totalChunks) * 100
			remaining := totalChunks - int64(retrievedCount)

			output += fmt.Sprintf("文档: %s (%s)\n", title, knowledgeID)
			output += fmt.Sprintf("  总 Chunk 数: %d\n", totalChunks)
			output += fmt.Sprintf("  已召回: %d 个 (%.1f%%)\n", retrievedCount, percentage)
			output += fmt.Sprintf("  未召回: %d 个\n", remaining)

		}
	}

	// // Add usage guidance
	// output += "\n\n=== Usage Guidelines ===\n"
	// output += "- High relevance (>=0.8): directly usable for answering\n"
	// output += "- Medium relevance (0.6-0.8): use as supplementary reference\n"
	// output += "- Low relevance (<0.6): use with caution, may not be accurate\n"
	// if totalBeforeFilter > len(results) {
	// 	output += "- Results below threshold have been automatically filtered\n"
	// }
	// output += "- Full content is already included in search results above\n"
	// output += "- Results are deduplicated across knowledge bases and sorted by relevance\n"
	// output += "- Use list_knowledge_chunks to expand context if needed\n"

	data := map[string]interface{}{
		"knowledge_base_ids": kbsToSearch,
		"results":            formattedResults,
		"count":              len(formattedResults),
		"kb_counts":          kbCounts,
		"display_type":       "search_results",
	}

	if len(queries) > 0 {
		data["queries"] = queries
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data:    data,
	}, nil
}

// chunkRange represents a continuous range of chunk indices
type chunkRange struct {
	start int
	end   int
}

// getEnrichedPassage 将非结构化的图片信息（描述、OCR 文字）转化为纯文本，并与原文内容合并，
// 从而让后续的文本处理流程（如分词、向量化、LLM 理解）能够“看见”并理解图片中的信息。
//
// 在传统的文本检索系统中，图片是“黑盒”，无法被检索。
//	场景 1：检索图表数据
//		用户问：“Q4 的销售额是多少？”
//		如果只有正文，可能找不到答案（因为数据在图表里）。
//		经过此函数处理后，OCR 提取的 "Q4: 200万" 变成了可检索的文本，系统能命中该片段。
//	场景 2：理解流程图/架构图
//		用户问：“登录流程的第一步是什么？”
//		正文可能只说“见下图”，而图片的 Caption 或 OCR 包含了具体步骤。
//		合并后，LLM 能根据提取的文本回答问题。

// getEnrichedPassage 合并Content和ImageInfo的文本内容
func (t *KnowledgeSearchTool) getEnrichedPassage(ctx context.Context, result *types.SearchResult) string {
	if result.ImageInfo == "" {
		return result.Content
	}

	// 解析ImageInfo
	var imageInfos []types.ImageInfo
	err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos)
	if err != nil {
		logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to parse image info: %v", err)
		return result.Content
	}

	if len(imageInfos) == 0 {
		return result.Content
	}

	// 提取所有图片的描述和OCR文本
	var imageTexts []string
	for _, img := range imageInfos {
		if img.Caption != "" {
			imageTexts = append(imageTexts, fmt.Sprintf("图片描述: %s", img.Caption))
		}
		if img.OCRText != "" {
			imageTexts = append(imageTexts, fmt.Sprintf("图片文本: %s", img.OCRText))
		}
	}

	if len(imageTexts) == 0 {
		return result.Content
	}

	// 组合内容和图片信息
	combinedText := result.Content
	if combinedText != "" {
		combinedText += "\n\n"
	}
	combinedText += strings.Join(imageTexts, "\n")

	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Enriched passage: content_len=%d, image_texts=%d",
		len(result.Content), len(imageTexts))

	return combinedText
}

// compositeScore calculates a composite score considering multiple factors
func (t *KnowledgeSearchTool) compositeScore(
	result *searchResultWithMeta,
	modelScore, baseScore float64,
) float64 {
	// Source weight: web_search results get slightly lower weight
	sourceWeight := 1.0
	if strings.ToLower(result.KnowledgeSource) == "web_search" {
		sourceWeight = 0.95
	}

	// Position prior: slightly favor chunks earlier in the document
	positionPrior := 1.0
	if result.StartAt >= 0 && result.EndAt > result.StartAt {
		// Calculate position ratio and apply small boost for earlier positions
		positionRatio := 1.0 - float64(result.StartAt)/float64(result.EndAt+1)
		positionPrior += t.clampFloat(positionRatio, -0.05, 0.05)
	}

	// Composite formula: weighted combination of model score, base score, and source weight
	composite := 0.6*modelScore + 0.3*baseScore + 0.1*sourceWeight
	composite *= positionPrior

	// Clamp to [0, 1]
	if composite < 0 {
		composite = 0
	}
	if composite > 1 {
		composite = 1
	}

	return composite
}

// clampFloat clamps a float value to the specified range
func (t *KnowledgeSearchTool) clampFloat(v, minV, maxV float64) float64 {
	return searchutil.ClampFloat(v, minV, maxV)
}

// applyMMR applies Maximal Marginal Relevance algorithm to reduce redundancy
func (t *KnowledgeSearchTool) applyMMR(
	ctx context.Context,
	results []*searchResultWithMeta,
	k int,
	lambda float64,
) []*searchResultWithMeta {
	if k <= 0 || len(results) == 0 {
		return nil
	}

	logger.Infof(ctx, "[Tool][KnowledgeSearch] Applying MMR: lambda=%.2f, k=%d, candidates=%d",
		lambda, k, len(results))

	selected := make([]*searchResultWithMeta, 0, k)
	candidates := make([]*searchResultWithMeta, len(results))
	copy(candidates, results)

	// Pre-compute token sets for all candidates
	tokenSets := make([]map[string]struct{}, len(candidates))
	for i, r := range candidates {
		tokenSets[i] = t.tokenizeSimple(t.getEnrichedPassage(ctx, r.SearchResult))
	}

	// MMR selection loop
	for len(selected) < k && len(candidates) > 0 {
		bestIdx := 0
		bestScore := -1.0

		for i, r := range candidates {
			relevance := r.Score
			redundancy := 0.0

			// Calculate maximum redundancy with already selected results
			for _, s := range selected {
				selectedTokens := t.tokenizeSimple(t.getEnrichedPassage(ctx, s.SearchResult))
				redundancy = math.Max(redundancy, t.jaccard(tokenSets[i], selectedTokens))
			}

			// MMR score: balance relevance and diversity
			mmr := lambda*relevance - (1.0-lambda)*redundancy
			if mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}

		// Add best candidate to selected and remove from candidates
		selected = append(selected, candidates[bestIdx])
		candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)
		// Remove corresponding token set
		tokenSets = append(tokenSets[:bestIdx], tokenSets[bestIdx+1:]...)
	}

	// Compute average redundancy among selected results
	avgRed := 0.0
	if len(selected) > 1 {
		pairs := 0
		for i := 0; i < len(selected); i++ {
			for j := i + 1; j < len(selected); j++ {
				si := t.tokenizeSimple(t.getEnrichedPassage(ctx, selected[i].SearchResult))
				sj := t.tokenizeSimple(t.getEnrichedPassage(ctx, selected[j].SearchResult))
				avgRed += t.jaccard(si, sj)
				pairs++
			}
		}
		if pairs > 0 {
			avgRed /= float64(pairs)
		}
	}

	logger.Infof(ctx, "[Tool][KnowledgeSearch] MMR completed: selected=%d, avg_redundancy=%.4f",
		len(selected), avgRed)

	return selected
}

// tokenizeSimple tokenizes text into a set of words (simple whitespace-based)
func (t *KnowledgeSearchTool) tokenizeSimple(text string) map[string]struct{} {
	return searchutil.TokenizeSimple(text)
}

// jaccard calculates Jaccard similarity between two token sets
func (t *KnowledgeSearchTool) jaccard(a, b map[string]struct{}) float64 {
	return searchutil.Jaccard(a, b)
}
