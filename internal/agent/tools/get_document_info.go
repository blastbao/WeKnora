package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

// 在 RAG（检索增强生成）系统中，搜索结果通常只包含散碎的文本切片（Chunks）。
// 如果用户问“这份文件的格式是什么？”或“这个文档处理完了吗？”，单纯的搜索无法回答。
// 工具 getDocumentInfoTool 能够提供文档级别的全景视图。
//
// 参数
//	输入： 知识点 ID 列表 (knowledge_ids)。
//	输出： 包含标题、描述、文件名、大小、处理进度（解析状态）及自定义元数据。
//
// 执行流程
//	参数解析：将 JSON 参数解析为 GetDocumentInfoInput 对象。
//	校验：确保 ID 列表不为空且不超过 10 个。
//	并发抓取：
//		获取文档基本元数据：调用 Service 层获取详细信息，通常是从数据库中读取。
//		校验访问权限：确保该文档所属的知识库在授权列表中。
//		统计该文档下的分块总数。
//	结果汇总：区分成功和失败的文档，并将失败信息记录在错误列表中。
//	格式化返回：生成 ToolResult，包含用于前端展示的 Data 和用于模型理解的 Output。

// “跨租户共享” 是如何实现的？
//
// 跨租户共享知识库 允许系统在保持底层数据隔离的前提下，实现逻辑上的数据流动。
//	- 所有权不变：文档依然属于原作者，占用原作者的存储配额。
//	- 访问权受控：原作者可以随时收回分享链接，接收方（跨租户用户）只能读，不能删改。
//	- 搜索一体化：AI 助手能在一个搜索接口里，同时搜到“我自己的私有文档”和“合作伙伴分享给我的公共文档”，并把它们聚合成一个回答。
//
// 跨租户共享知识库的意思就是：数据的所有权属于租户 A，但数据的读取/搜索权被授予了租户 B。
// 这意味着系统不能简单地“只看当前登录用户的数据”，而必须：
//	- 检查有没有授权（searchTargets 检查）。
//	- 如果有授权，要伪装成或切换到数据所有者的上下文（使用源租户的 ID 和模型配置）去真正获取数据。

// 在多租户系统中，如果不切换到数据所有者（源租户）的上下文，你不仅获取不到数据，获取到的数据也极有可能是无效的。
//
// 在 RAG（检索增强生成）系统中，数据不仅仅是文本，它还绑定了特定的处理规则和数学空间。
// 如果不用源租户的上下文去查，会导致两个致命问题：“查不到数据” 和 “算不准分数”。
//
// 向量检索依赖于特定的 Embedding 模型。
//	- 租户 A 可能使用的是 OpenAI-text-embedding-3-small
//  - 租户 B（源租户）使用的是 Tencent-Hunyuan-Embedding。
// 如果你用租户 A 的模型去对用户问题向量化，然后跑到租户 B 的向量数据库里去搜，搜出来的结果无语义关联。
// 必须获取源租户的 EmbeddingModelID，并使用源租户配置的模型来生成查询向量，再去租户 B 的向量数据库里去搜。

// Q: 提取属于该文档的分块数目，有啥用？
//
// 统计文档分块 (Chunk) 数目的核心作用：
//
// 	辅助决策（量级感知）
//		让 LLM 判断文档规模：是“未解析（0块）”、“短篇速读”还是“长篇深究”，从而决定检索策略（如是否需扩大搜索范围）。
//	健康诊断（状态校验）
//		戳穿“假成功”：若状态显示“完成”但块数为 0，直接判定为解析失败或文件损坏，避免无效问答。
//	透明反馈（用户感知）
//		前端展示：让用户感知系统处理进度；
//		后台调优：若大文件仅有少量分块，提示需优化分块策略 (Chunk Strategy)。
//	链路验证（跨租户确权）
//		在跨租户场景下，能成功查出块数，即证明“权限授权 -> 路由切换 -> 物理隔离突破”的全链路已彻底打通。

// Execute 并发获取多个文档的详细元数据及分块统计信息。
//
// 流程概述：
//	系统先并行获取文档元数据以识别其归属，接着通过白名单机制校验当前用户是否有权访问该共享知识库，
//	最后显式指定源租户 ID 绕过数据库的默认隔离过滤，精准提取属于该文档的分块统计信息。
//
// 流程细节：
// 1. 初始化与参数校验
//    - 解析输入中的知识点 ID 列表 (KnowledgeIDs)，限制单次并发查询上限（10个），以平衡性能与系统负载。
//
// 2. 并发数据获取 (Concurrent Fetching)
//    - 针对每个 ID 开启独立的 Goroutine 进行检索，利用 Mutex 保证结果集并发写安全。
//    - A. 基础元数据提取: 调用 GetKnowledgeByIDOnly 绕过当前租户的物理过滤。
//       * 原因：共享文档的归属权（TenantID）属于源租户，当前租户环境无法直接通过标准接口查到。
//    - B. 逻辑权限校验 (RBAC Check): 核心合规性判断。
//       * 校验文档所属的 KnowledgeBaseID 是否在 SearchTargets（允许访问的知识库列表）中。
//       * 确保“能查到 ID”不代表“能看到信息”，只有显式分享过的库才允许通过。
//    - C. 物理数据路由: 使用文档真实的 TenantID 统计分块。
//       * 向量检索系统中，分块数据通常按租户分表或分库存储。
//       * 必须切换至“源租户”的 ID 才能在正确的数据库分区中执行 ListPagedChunksByKnowledgeID，统计该文档的实际分块数。
//
// 3. 结果汇总与多维度处理
//    - 容错处理: 区分“查询成功”和“查询失败”的文档，确保部分文档损坏不影响整体任务。
//    - 数据格式化:
//       * 生成 Output: 友好的纯文本，包含处理状态 Emoji 和易读的文件大小，直接供 LLM 阅读。
//       * 生成 Data: 结构化 JSON，包含完整的元数据 Map，供前端 UI 进行文档详情展示。
//
// 4. 返回结果
//    - 包含成功统计数、失败原因列表以及首个文档标题（用于前端 Tab 标签页显示）。

var getDocumentInfoTool = BaseTool{
	name: ToolGetDocumentInfo,
	description: `Retrieve detailed metadata information about documents.

## When to Use

Use this tool when:
- Need to understand document basic information (title, type, size, etc.)
- Check if document exists and is available
- Batch query metadata for multiple documents
- Understand document processing status

Do not use when:
- Need document content (use knowledge_search)
- Need specific text chunks (search results already contain full content)


## Returned Information

- Basic info: title, description, source type
- File info: filename, type, size
- Processing status: whether processed, chunk count
- Metadata: custom tags and properties


## Notes

- Concurrent query for multiple documents provides better performance
- Returns complete document metadata, not just title
- Can check document processing status (parse_status)`,
	schema: utils.GenerateSchema[GetDocumentInfoInput](),
}

// GetDocumentInfoInput defines the input parameters for get document info tool
type GetDocumentInfoInput struct {
	KnowledgeIDs []string `json:"knowledge_ids" jsonschema:"Array of document/knowledge IDs, obtained from knowledge_id field in search results, supports concurrent batch queries"`
}

// GetDocumentInfoTool retrieves detailed information about a document/knowledge
type GetDocumentInfoTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeService
	chunkService     interfaces.ChunkService
	searchTargets    types.SearchTargets // Pre-computed unified search targets with KB-tenant mapping
}

// NewGetDocumentInfoTool creates a new get document info tool
func NewGetDocumentInfoTool(
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
) *GetDocumentInfoTool {
	return &GetDocumentInfoTool{
		BaseTool:         getDocumentInfoTool,
		knowledgeService: knowledgeService,
		chunkService:     chunkService,
		searchTargets:    searchTargets,
	}
}

// Execute retrieves document information with concurrent processing
func (t *GetDocumentInfoTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input GetDocumentInfoInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Extract knowledge_ids array
	knowledgeIDs := input.KnowledgeIDs
	if len(knowledgeIDs) == 0 {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_ids is required and must be a non-empty array",
		}, fmt.Errorf("knowledge_ids is required")
	}

	// Validate max 10 documents
	if len(knowledgeIDs) > 10 {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_ids must contain at least one valid knowledge ID",
		}, fmt.Errorf("no valid knowledge IDs provided")
	}

	// Concurrently get info for each knowledge ID
	type docInfo struct {
		knowledge  *types.Knowledge
		chunkCount int
		err        error
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[string]*docInfo)

	// Concurrently get info for each knowledge ID
	for _, knowledgeID := range knowledgeIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			// Get knowledge metadata without tenant filter to support shared KB
			knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, id)
			if err != nil {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("无法获取文档信息: %v", err),
				}
				mu.Unlock()
				return
			}

			// Verify the knowledge's KB is in searchTargets (permission check)
			if !t.searchTargets.ContainsKB(knowledge.KnowledgeBaseID) {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("知识库 %s 不可访问", knowledge.KnowledgeBaseID),
				}
				mu.Unlock()
				return
			}

			// Use knowledge's actual tenant_id for chunk query (supports cross-tenant shared KB)
			_, total, err := t.chunkService.GetRepository().
				ListPagedChunksByKnowledgeID(ctx, knowledge.TenantID, id, &types.Pagination{
					Page:     1,
					PageSize: 1000,
				}, []types.ChunkType{"text"}, "", "", "", "", "")
			if err != nil {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("无法获取文档信息: %v", err),
				}
				mu.Unlock()
				return
			}
			chunkCount := int(total)

			mu.Lock()
			results[id] = &docInfo{
				knowledge:  knowledge,
				chunkCount: chunkCount,
			}
			mu.Unlock()
		}(knowledgeID)
	}

	wg.Wait()

	// Collect successful results and errors
	successDocs := make([]*docInfo, 0)
	var errors []string

	for _, knowledgeID := range knowledgeIDs {
		result := results[knowledgeID]
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", knowledgeID, result.err))
		} else if result.knowledge != nil {
			successDocs = append(successDocs, result)
		}
	}

	if len(successDocs) == 0 {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("无法获取任何文档信息。错误: %v", errors),
		}, fmt.Errorf("all document retrievals failed")
	}

	// Format output
	output := "=== 文档信息 ===\n\n"
	output += fmt.Sprintf("成功获取 %d / %d 个文档信息\n\n", len(successDocs), len(knowledgeIDs))

	if len(errors) > 0 {
		output += "=== 部分失败 ===\n"
		for _, errMsg := range errors {
			output += fmt.Sprintf("  - %s\n", errMsg)
		}
		output += "\n"
	}

	formattedDocs := make([]map[string]interface{}, 0, len(successDocs))
	for i, doc := range successDocs {
		k := doc.knowledge

		output += fmt.Sprintf("【文档 #%d】\n", i+1)
		output += fmt.Sprintf("  ID:       %s\n", k.ID)
		output += fmt.Sprintf("  标题:     %s\n", k.Title)

		if k.Description != "" {
			output += fmt.Sprintf("  描述:     %s\n", k.Description)
		}

		output += fmt.Sprintf("  来源:     %s\n", formatSource(k.Type, k.Source))

		if k.FileName != "" {
			output += fmt.Sprintf("  文件名:   %s\n", k.FileName)
			output += fmt.Sprintf("  文件类型: %s\n", k.FileType)
			output += fmt.Sprintf("  文件大小: %s\n", formatFileSize(k.FileSize))
		}

		output += fmt.Sprintf("  处理状态: %s\n", formatParseStatus(k.ParseStatus))
		output += fmt.Sprintf("  分块数量: %d 个\n", doc.chunkCount)

		if k.Metadata != nil {
			if metadata, err := k.Metadata.Map(); err == nil && len(metadata) > 0 {
				output += "  元数据:\n"
				for key, value := range metadata {
					output += fmt.Sprintf("    - %s: %v\n", key, value)
				}
			}
		}

		output += "\n"

		formattedDocs = append(formattedDocs, map[string]interface{}{
			"knowledge_id": k.ID,
			"title":        k.Title,
			"description":  k.Description,
			"type":         k.Type,
			"source":       k.Source,
			"file_name":    k.FileName,
			"file_type":    k.FileType,
			"file_size":    k.FileSize,
			"parse_status": k.ParseStatus,
			"chunk_count":  doc.chunkCount,
			"metadata":     k.GetMetadata(),
		})
	}

	// Extract first document title for summary
	var firstTitle string
	if len(successDocs) > 0 && successDocs[0].knowledge != nil {
		firstTitle = successDocs[0].knowledge.Title
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"documents":    formattedDocs,
			"total_docs":   len(successDocs),
			"requested":    len(knowledgeIDs),
			"errors":       errors,
			"display_type": "document_info",
			"title":        firstTitle, // For frontend summary display
		},
	}, nil
}

func formatSource(knowledgeType, source string) string {
	switch knowledgeType {
	case "file":
		return "文件上传"
	case "url":
		return fmt.Sprintf("URL: %s", source)
	case "passage":
		return "文本输入"
	default:
		return knowledgeType
	}
}

func formatFileSize(size int64) string {
	if size == 0 {
		return "未知"
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func formatParseStatus(status string) string {
	switch status {
	case "pending":
		return "⏳ 待处理"
	case "processing":
		return "🔄 处理中"
	case "completed", "success":
		return "✅ 已完成"
	case "failed":
		return "❌ 失败"
	default:
		return status
	}
}
