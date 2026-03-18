package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

var queryKnowledgeGraphTool = BaseTool{
	name: ToolQueryKnowledgeGraph,
	description: `Query knowledge graph to explore entity relationships and knowledge networks.

## Core Function
Explores relationships between entities in knowledge bases that have graph extraction configured.

## When to Use
✅ **Use for**:
- Understanding relationships between entities (e.g., "relationship between Docker and Kubernetes")
- Exploring knowledge networks and concept associations
- Finding related information about specific entities
- Understanding technical architecture and system relationships

❌ **Don't use for**:
- General text search → use knowledge_search
- Knowledge base without graph extraction configured
- Need exact document content → use knowledge_search

## Parameters
- **knowledge_base_ids** (required): Array of knowledge base IDs (1-10). Only KBs with graph extraction configured will be effective.
- **query** (required): Query content - can be entity name, relationship query, or concept search.

## Graph Configuration
Knowledge graph must be pre-configured in knowledge bases:
- **Entity types** (Nodes): e.g., "Technology", "Tool", "Concept"
- **Relationship types** (Relations): e.g., "depends_on", "uses", "contains"

If KB is not configured with graph, tool will return regular search results.

## Workflow
1. **Relationship exploration**: query_knowledge_graph → list_knowledge_chunks (for detailed content)
2. **Network analysis**: query_knowledge_graph → knowledge_search (for comprehensive understanding)
3. **Topic research**: knowledge_search → query_knowledge_graph (for deep entity relationships)

## Notes
- Results indicate graph configuration status
- Cross-KB results are automatically deduplicated
- Results are sorted by relevance`,
	schema: utils.GenerateSchema[QueryKnowledgeGraphInput](),
}

// QueryKnowledgeGraphInput defines the input parameters for query knowledge graph tool
type QueryKnowledgeGraphInput struct {
	KnowledgeBaseIDs []string `json:"knowledge_base_ids" jsonschema:"Array of knowledge base IDs to query"`
	Query            string   `json:"query" jsonschema:"查询内容（实体名称或查询文本）"`
}

// QueryKnowledgeGraphTool queries the knowledge graph for entities and relationships
type QueryKnowledgeGraphTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeBaseService
}

// NewQueryKnowledgeGraphTool creates a new query knowledge graph tool
func NewQueryKnowledgeGraphTool(knowledgeService interfaces.KnowledgeBaseService) *QueryKnowledgeGraphTool {
	return &QueryKnowledgeGraphTool{
		BaseTool:         queryKnowledgeGraphTool,
		knowledgeService: knowledgeService,
	}
}

//  输入参数
//		- knowledge_base_ids (必填): 字符串数组，指定要查询的知识库 ID 列表（限制 1-10 个）。
//		- query (必填): 查询内容，可以是实体名称、关系描述或概念关键词。

// 代码逻辑
//
//	第一步：输入校验
//		检查 knowledge_base_ids 是否为空，且不能超过 10 个。
//		确保 query 查询内容不为空。
//
//	第二步：并发检索与图谱检查
//		对于每一个知识库 ID，代码会：
//		 - 获取知识库元数据：检查该知识库是否配置了 ExtractConfig（包含 Nodes 实体和 Relations 关系定义）。
//			- 若未配置：记录错误信息“未配置知识图谱抽取”，跳过该知识库的图谱查询逻辑。
//			- 若已配置：调用 HybridSearch 进行检索。这里虽然叫 HybridSearch，但在图谱上下文中，它旨在利用图谱索引或语义匹配来找到相关实体/片段。
//		 - 执行搜索：调用 HybridSearch 获取相关数据。
//		 - 异常捕获：如果某个知识库查询失败（如 ID 不存在），它会记录错误但不会中断其他知识库的查询。
//
//	第三步：结果去重与排序
//		跨库去重：使用 seenChunks map 过滤掉不同知识库中重复出现的文档块（ID 唯一）。
//		评分排序：根据 Score（相关度分数）对所有结果进行降序排列，确保最相关的结果排在最前面。
//
//	第四步：人性化的报告生成
//		代码花费了大量篇幅构建 output 字符串（Markdown 格式），这是为了让 AI 更好地理解搜索结果：
//		 - 状态可视化：列出每个知识库配置了哪些“实体类型”和“关系类型”。
//		 - 来源标记：清晰地展示每个结果来自哪个知识库和哪篇文档。
//		 - 分级建议：根据分数计算相关度等级（如：极高、高、中）。

// 业务场景
//	用户目标：想了解 "Kubernetes" 和 "Docker" 在内部技术文档中的关系。
//	输入参数：
//		- knowledge_base_ids: ["kb-tech-docs", "kb-ops-manual"] (两个知识库)
//		- query: "Kubernetes 和 Docker 的关系"
//	知识库状态：
//		- kb-ops-manual：未配置知识图谱（只是普通的文档库）。
//		- kb-tech-docs：已配置知识图谱。
//			- 实体类型：Technology, Tool
//			- 关系类型：depends_on, replaces
//
//			{
//			 	"node_1": { "name": "Kubernetes", "type": "Technology" },
//			 	"node_2": { "name": "Docker", "type": "Tool" },
//			 	"edge":   { "type": "depends_on", "from": "Kubernetes", "to": "Docker" }
//			}
//
//			{
//			 	"node_1": { "name": "Containerd", "type": "Technology" },
//			 	"node_2": { "name": "Docker", "type": "Tool" },
//			 	"edge": { "type": "replaces", "from": "Containerd", "to": "Docker", "description": "Containerd 替代了 Docker 作为 Kubernetes 的默认容器运行时" }
//			}
//
// 执行流程详解
//	第 1 步：参数解析与校验
//		解析 JSON 输入。
//		检查知识库 ID 列表长度为 2（符合 <10 的限制）。
//		检查查询文本 Query 非空。
//		结果：校验通过，准备并发执行。
//	第 2 步：并发查询 (Goroutines 启动)
//		系统同时启动两个协程（线程）分别处理两个知识库。
//		- 协程 A (处理 kb-tech-docs)
//			获取配置：调用 GetKnowledgeBaseByID("kb-tech-docs")。
//			检查图谱：发现 ExtractConfig 存在，且配置了 Nodes 和 Relations。
//			判定：图谱已启用。
//			执行搜索：调用 HybridSearch，查询 "Kubernetes 和 Docker 的关系"。
//			返回结果：找到 2 条高相关度的片段（Chunk）。
//				- Chunk A1: "Kubernetes 依赖 Docker 作为容器运行时..." (Score: 0.95)
//				- Chunk A2: "Docker 被 Kubernetes 编排..." (Score: 0.88)
//			存入结果池：将 KB 信息和 2 条结果存入 kbResults["kb-tech-docs"]。
//		- 协程 B (处理 kb-ops-manual)
//			获取配置：调用 GetKnowledgeBaseByID("kb-ops-manual")。
//			检查图谱：发现 ExtractConfig 为 nil 或为空。
//			判定：未配置知识图谱抽取。
//			跳过搜索：不执行 HybridSearch（避免浪费资源或在错误模式下搜索）。
//			记录错误：生成错误信息 "未配置知识图谱抽取"。
//			存入结果池：将错误信息存入 kbResults["kb-ops-manual"]。
//	第 3 步：等待与聚合 (Main Thread)
//		主协程等待所有协程结束，然后开始处理数据：
//		收集错误：发现 kb-ops-manual 有错误，将其加入 errors 列表：["KB kb-ops-manual: 未配置知识图谱抽取"]。
//		提取配置信息：
//			- 从 kb-tech-docs 提取配置：nodes: ["Technology", "Tool"], relations: ["depends_on", "replaces"]。
//			- 标记 hasGraphConfig = true。
//		去重与排序：
//			- 收集到 2 条结果 (A1, A2)。
//			- 假设没有重复 ID。
//			- 按分数排序：A1 (0.95) > A2 (0.88)。
//	第 4 步：构建输出报告
//		系统开始组装最终返回给用户的字符串 (Output) 和数据 (Data)。
//
//		{
//		 "success": true,
//		 "data": {
//		   "knowledge_base_ids": ["kb-tech-docs", "kb-ops-manual"],
//		   "query": "Kubernetes 和 Docker 的关系",
//		   "count": 2,
//		   "has_graph_config": true,
//		   "errors": ["KB kb-ops-manual: 未配置知识图谱抽取"],
//		   "graph_configs": {
//			 "kb-tech-docs": {
//			   "nodes": [{"name": "Technology"}, {"name": "Tool"}],
//			   "relations": [{"name": "depends_on"}, {"name": "replaces"}]
//			 }
//		   },
//		   "results": [
//			 {
//			   "result_index": 1,
//			   "chunk_id": "chunk_001",
//			   "content": "Kubernetes 依赖 Docker...",
//			   "score": 0.95,
//			   "knowledge_title": "容器架构指南.md"
//			 },
//			 {
//			   "result_index": 2,
//			   "chunk_id": "chunk_002",
//			   "content": "Docker 被 Kubernetes...",
//			   "score": 0.88,
//			   "knowledge_title": "容器架构指南.md"
//			 }
//		   ],
//		   "graph_data": {
//			 "nodes": [
//			   {"id": "chunk_001", "label": "Chunk 1", "type": "chunk"},
//			   {"id": "chunk_002", "label": "Chunk 2", "type": "chunk"}
//			 ],
//			 "edges": []
//		   }
//		 }
//		}
//
//
// 输出示例：
//
//	=== 知识图谱查询 ===
//	📊 查询: Kubernetes 和 Docker 的关系
//	🎯 目标知识库: [kb-tech-docs, kb-ops-manual]
//	✓ 找到 2 条相关结果（已去重）
//
//	=== ⚠️ 部分失败 ===
//	KB kb-ops-manual: 未配置知识图谱抽取
//
//	=== 📈 图谱配置状态 ===
//	知识库【kb-tech-docs】:
//	✓ 实体类型 (2): ["Technology", "Tool"]
//	✓ 关系类型 (1): ["depends_on"]
//
//	=== 📚 知识库覆盖 ===
//	kb-tech-docs: 2 条结果
//
//	=== 🔍 查询结果 ===
//	💡 基于图谱配置的相关内容检索
//
//	【来源文档: 云原生架构白皮书】
//
//	结果 #1:
//	📍 相关度: 0.95 (极高)
//	🔗 匹配方式: 语义匹配
//	📄 内容: Kubernetes 通过 CRI 接口调用 Docker 容器运行时，形成强依赖关系。
//	🆔 chunk_id: chunk_abc_123
//
//	结果 #2:
//	📍 相关度: 0.88 (高)
//	🔗 匹配方式: 全文匹配
//	📄 内容: 在早期版本中，Docker 是 K8s 唯一的容器引擎选项。
//	🆔 chunk_id: chunk_def_456
//
//	=== 💡 使用提示 ===
//	✓ 结果已跨知识库去重并按相关度排序
//	✓ 使用 get_chunk_detail 获取完整内容

// Execute performs the knowledge graph query with concurrent KB processing
func (t *QueryKnowledgeGraphTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input QueryKnowledgeGraphInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Extract knowledge_base_ids array
	if len(input.KnowledgeBaseIDs) == 0 {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_base_ids is required and must be a non-empty array",
		}, fmt.Errorf("knowledge_base_ids is required")
	}

	// Validate max 10 KBs
	if len(input.KnowledgeBaseIDs) > 10 {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_base_ids must contain at most 10 KB IDs",
		}, fmt.Errorf("too many KB IDs")
	}

	query := input.Query
	if query == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "query is required",
		}, fmt.Errorf("invalid query")
	}

	// Concurrently query all knowledge bases
	type graphQueryResult struct {
		kbID    string
		kb      *types.KnowledgeBase
		results []*types.SearchResult
		err     error
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	kbResults := make(map[string]*graphQueryResult)

	searchParams := types.SearchParams{
		QueryText:  query,
		MatchCount: 10,
	}

	for _, kbID := range input.KnowledgeBaseIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			// Get knowledge base to check graph configuration
			kb, err := t.knowledgeService.GetKnowledgeBaseByID(ctx, id)
			if err != nil {
				mu.Lock()
				kbResults[id] = &graphQueryResult{kbID: id, err: fmt.Errorf("获取知识库失败: %v", err)}
				mu.Unlock()
				return
			}

			// Check if graph extraction is enabled
			if kb.ExtractConfig == nil || (len(kb.ExtractConfig.Nodes) == 0 && len(kb.ExtractConfig.Relations) == 0) {
				mu.Lock()
				kbResults[id] = &graphQueryResult{kbID: id, err: fmt.Errorf("未配置知识图谱抽取")}
				mu.Unlock()
				return
			}

			// Query graph
			results, err := t.knowledgeService.HybridSearch(ctx, id, searchParams)
			if err != nil {
				mu.Lock()
				kbResults[id] = &graphQueryResult{kbID: id, kb: kb, err: fmt.Errorf("查询失败: %v", err)}
				mu.Unlock()
				return
			}

			mu.Lock()
			kbResults[id] = &graphQueryResult{kbID: id, kb: kb, results: results}
			mu.Unlock()
		}(kbID)
	}

	wg.Wait()

	// Collect and deduplicate results
	seenChunks := make(map[string]*types.SearchResult)
	var errors []string
	graphConfigs := make(map[string]map[string]interface{})
	kbCounts := make(map[string]int)

	for _, kbID := range input.KnowledgeBaseIDs {
		result := kbResults[kbID]
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("KB %s: %v", kbID, result.err))
			continue
		}

		if result.kb != nil && result.kb.ExtractConfig != nil {
			graphConfigs[kbID] = map[string]interface{}{
				"nodes":     result.kb.ExtractConfig.Nodes,
				"relations": result.kb.ExtractConfig.Relations,
			}
		}

		kbCounts[kbID] = len(result.results)
		for _, r := range result.results {
			if _, seen := seenChunks[r.ID]; !seen {
				seenChunks[r.ID] = r
			}
		}
	}

	// Convert map to slice and sort by score
	allResults := make([]*types.SearchResult, 0, len(seenChunks))
	for _, result := range seenChunks {
		allResults = append(allResults, result)
	}

	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})

	if len(allResults) == 0 {
		return &types.ToolResult{
			Success: true,
			Output:  "未找到相关的图谱信息。",
			Data: map[string]interface{}{
				"knowledge_base_ids": input.KnowledgeBaseIDs,
				"query":              query,
				"results":            []interface{}{},
				"graph_configs":      graphConfigs,
				"errors":             errors,
			},
		}, nil
	}

	// Format output with enhanced graph information
	output := "=== 知识图谱查询 ===\n\n"
	output += fmt.Sprintf("📊 查询: %s\n", query)
	output += fmt.Sprintf("🎯 目标知识库: %v\n", input.KnowledgeBaseIDs)
	output += fmt.Sprintf("✓ 找到 %d 条相关结果（已去重）\n\n", len(allResults))

	if len(errors) > 0 {
		output += "=== ⚠️ 部分失败 ===\n"
		for _, errMsg := range errors {
			output += fmt.Sprintf("  - %s\n", errMsg)
		}
		output += "\n"
	}

	// Display graph configuration status
	hasGraphConfig := false
	output += "=== 📈 图谱配置状态 ===\n\n"
	for kbID, config := range graphConfigs {
		hasGraphConfig = true
		output += fmt.Sprintf("知识库【%s】:\n", kbID)

		nodes, _ := config["nodes"].([]interface{})
		relations, _ := config["relations"].([]interface{})

		if len(nodes) > 0 {
			output += fmt.Sprintf("  ✓ 实体类型 (%d): ", len(nodes))
			nodeNames := make([]string, 0, len(nodes))
			for _, n := range nodes {
				if nodeMap, ok := n.(map[string]interface{}); ok {
					if name, ok := nodeMap["name"].(string); ok {
						nodeNames = append(nodeNames, name)
					}
				}
			}
			output += fmt.Sprintf("%v\n", nodeNames)
		} else {
			output += "  ⚠️ 未配置实体类型\n"
		}

		if len(relations) > 0 {
			output += fmt.Sprintf("  ✓ 关系类型 (%d): ", len(relations))
			relNames := make([]string, 0, len(relations))
			for _, r := range relations {
				if relMap, ok := r.(map[string]interface{}); ok {
					if name, ok := relMap["name"].(string); ok {
						relNames = append(relNames, name)
					}
				}
			}
			output += fmt.Sprintf("%v\n", relNames)
		} else {
			output += "  ⚠️ 未配置关系类型\n"
		}
		output += "\n"
	}

	if !hasGraphConfig {
		output += "⚠️ 所查询的知识库均未配置图谱抽取\n"
		output += "💡 提示: 需要在知识库设置中配置实体和关系类型\n\n"
	}

	// Display result counts by KB
	if len(kbCounts) > 0 {
		output += "=== 📚 知识库覆盖 ===\n"
		for kbID, count := range kbCounts {
			output += fmt.Sprintf("  - %s: %d 条结果\n", kbID, count)
		}
		output += "\n"
	}

	// Display search results
	output += "=== 🔍 查询结果 ===\n\n"
	if !hasGraphConfig {
		output += "💡 当前返回相关文档片段（知识库未配置图谱）\n\n"
	} else {
		output += "💡 基于图谱配置的相关内容检索\n\n"
	}

	formattedResults := make([]map[string]interface{}, 0, len(allResults))
	currentKB := ""

	for i, result := range allResults {
		// Group by knowledge base
		if result.KnowledgeID != currentKB {
			currentKB = result.KnowledgeID
			if i > 0 {
				output += "\n"
			}
			output += fmt.Sprintf("【来源文档: %s】\n\n", result.KnowledgeTitle)
		}

		relevanceLevel := GetRelevanceLevel(result.Score)

		output += fmt.Sprintf("结果 #%d:\n", i+1)
		output += fmt.Sprintf("  📍 相关度: %.2f (%s)\n", result.Score, relevanceLevel)
		output += fmt.Sprintf("  🔗 匹配方式: %s\n", FormatMatchType(result.MatchType))
		output += fmt.Sprintf("  📄 内容: %s\n", result.Content)
		output += fmt.Sprintf("  🆔 chunk_id: %s\n\n", result.ID)

		formattedResults = append(formattedResults, map[string]interface{}{
			"result_index":    i + 1,
			"chunk_id":        result.ID,
			"content":         result.Content,
			"score":           result.Score,
			"relevance_level": relevanceLevel,
			"knowledge_id":    result.KnowledgeID,
			"knowledge_title": result.KnowledgeTitle,
			"match_type":      FormatMatchType(result.MatchType),
		})
	}

	output += "=== 💡 使用提示 ===\n"
	output += "- ✓ 结果已跨知识库去重并按相关度排序\n"
	output += "- ✓ 使用 get_chunk_detail 获取完整内容\n"
	output += "- ✓ 使用 list_knowledge_chunks 探索上下文\n"
	if !hasGraphConfig {
		output += "- ⚠️ 配置图谱抽取以获得更精准的实体关系结果\n"
	}
	output += "- ⏳ 完整的图查询语言（Cypher）支持开发中\n"

	// Build structured graph data for frontend visualization
	graphData := buildGraphVisualizationData(allResults, graphConfigs)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"knowledge_base_ids": input.KnowledgeBaseIDs,
			"query":              query,
			"results":            formattedResults,
			"count":              len(allResults),
			"kb_counts":          kbCounts,
			"graph_configs":      graphConfigs,
			"graph_data":         graphData,
			"has_graph_config":   hasGraphConfig,
			"errors":             errors,
			"display_type":       "graph_query_results",
		},
	}, nil
}

// buildGraphVisualizationData builds structured data for graph visualization
func buildGraphVisualizationData(
	results []*types.SearchResult,
	graphConfigs map[string]map[string]interface{},
) map[string]interface{} {
	// Build a simple graph structure for frontend visualization
	nodes := make([]map[string]interface{}, 0)
	edges := make([]map[string]interface{}, 0)

	// Create nodes from results
	seenEntities := make(map[string]bool)
	for i, result := range results {
		if !seenEntities[result.ID] {
			nodes = append(nodes, map[string]interface{}{
				"id":       result.ID,
				"label":    fmt.Sprintf("Chunk %d", i+1),
				"content":  result.Content,
				"kb_id":    result.KnowledgeID,
				"kb_title": result.KnowledgeTitle,
				"score":    result.Score,
				"type":     "chunk",
			})
			seenEntities[result.ID] = true
		}
	}

	return map[string]interface{}{
		"nodes":       nodes,
		"edges":       edges,
		"total_nodes": len(nodes),
		"total_edges": len(edges),
	}
}
