package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

var webSearchTool = BaseTool{
	name: ToolWebSearch,
	description: `Search the web for current information and news. This tool searches the internet to find up-to-date information that may not be in the knowledge base.

## CRITICAL - KB First Rule
**ABSOLUTE RULE**: You MUST complete KB retrieval (grep_chunks AND knowledge_search) FIRST before using this tool.
- NEVER use web_search without first trying grep_chunks and knowledge_search
- ONLY use web_search if BOTH grep_chunks AND knowledge_search return insufficient/no results
- KB retrieval is MANDATORY - you CANNOT skip it

## Features
- Real-time web search: Search the internet for current information
- RAG compression: Automatically compresses and extracts relevant content from search results
- Session-scoped caching: Maintains temporary knowledge base for session to avoid re-indexing

## Usage

**Use when**:
- **ONLY after** completing grep_chunks AND knowledge_search
- KB retrieval returned insufficient or no results
- Need current or real-time information (news, events, recent updates)
- Information is not available in knowledge bases
- Need to verify or supplement information from knowledge bases
- Searching for recent developments or trends

**Parameters**:
- query (required): Search query string

**Returns**: Web search results with title, URL, snippet, and content (up to %d results)

## Examples

` + "`" + `
# Search for current information
{
  "query": "latest developments in AI"
}

# Search for recent news
{
  "query": "Python 3.12 release notes"
}
` + "`" + `

## Tips

- Results are automatically compressed using RAG to extract relevant content
- Search results are stored in a temporary knowledge base for the session
- Use this tool when knowledge bases don't have the information you need
- Results include URL, title, snippet, and content snippet (may be truncated)
- **CRITICAL**: If content is truncated or you need full details, use **web_fetch** to fetch complete page content
- Maximum %d results will be returned per search`,
	schema: utils.GenerateSchema[WebSearchInput](),
}

// WebSearchInput defines the input parameters for web search tool
type WebSearchInput struct {
	Query string `json:"query" jsonschema:"Search query string"`
}

// WebSearchTool performs web searches and returns results
type WebSearchTool struct {
	BaseTool
	webSearchService      interfaces.WebSearchService
	knowledgeBaseService  interfaces.KnowledgeBaseService
	knowledgeService      interfaces.KnowledgeService
	webSearchStateService interfaces.WebSearchStateService
	sessionID             string
	maxResults            int
}

// NewWebSearchTool creates a new web search tool
func NewWebSearchTool(
	webSearchService interfaces.WebSearchService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	webSearchStateService interfaces.WebSearchStateService,
	sessionID string,
	maxResults int,
) *WebSearchTool {
	tool := webSearchTool
	tool.description = fmt.Sprintf(tool.description, maxResults, maxResults)

	return &WebSearchTool{
		BaseTool:              tool,
		webSearchService:      webSearchService,
		knowledgeBaseService:  knowledgeBaseService,
		knowledgeService:      knowledgeService,
		webSearchStateService: webSearchStateService,
		sessionID:             sessionID,
		maxResults:            maxResults,
	}
}

// 1. 输入解析与校验：解析 query ，如果为空，直接报错。
// 2. 租户识别：从上下文 (ctx) 中获取 TenantID ，确保权限受控。
// 3. 检查该租户是否开启了 Web Search 功能（即配置了 Provider，如 Google, Bing, Tavily 等）。如果未配置，直接拒绝服务。
// 4. 执行网络搜索：通过 webSearchService.Search 向配置好的搜索引擎发起 HTTP 请求。
// 5. RAG 压缩与状态持久化
//  - 状态恢复：从 Redis 读取当前会话的“临时知识库 ID”和“已处理文档列表”。这意味着如果在同一会话中再次搜索相似内容，系统可以识别并去重。
//  - RAG 压缩：调用 CompressWithRAG ，这通常利用一个小模型或提取算法，从搜索结果中提炼出直接回答问题的内容，去除广告、导航栏等噪音。
//  - 状态持久化：将新生成的压缩内容索引到一个临时的知识库中，并更新 Redis 中的状态。这使得后续的对话可以引用这次搜索的结果，而无需重新搜索。
// 6. 结果格式化封装
//  - 数据截断：为了防止网页正文过长导致 AI 上下文溢出，函数对 Content 进行了强制截断，并加上省略号。
//  - 结构化输出：同时返回给前端 Data（用于 UI 渲染表格/卡片）和 Output（给 AI 阅读的文本）。
//  - Next Steps 引导：在返回给 AI 的文本末尾明确写道：“内容已截断，如需详情请使用 web_fetch”，引导 AI 针对特定的高价值 URL 发起深度抓取请求。

// 设计优势
//  - 成本与效率优化: 通过 RAG 压缩，大幅减少了传递给 LLM 的 Token 数量，降低了成本，同时提高了回答的精准度（去除了噪音）。
//  - 会话记忆: 利用 Redis 存储会话状态，实现了跨轮次的搜索记忆和去重，让 Agent 显得更“聪明”。
//  - 鲁棒性: 具备完善的降级机制（压缩失败用原数据）和参数校验，确保在各种异常情况下都能给出明确的反馈。
//  - 引导性: 输出中内置的 Next Steps 指导，有效地管理了 LLM 的预期和行为，促使其在需要时自动调用 web_fetch。
//  - 多租户安全: 严格的租户配置检查，确保了资源使用的隔离性和可控性。

// 函数概览：
//
//	输入：一堆网页链接 + 用户的问题。
//	内部动作：
//	  - 爬取网页正文。
//	  - 切成小块。
//	  - 计算相关性（留下有用的，删掉没用的）。
//	  - 存入临时知识库。
//	输出（即左边的返回值）：
//	  - compressed: 提纯后的文字段落。
//	  - kbID: 存放这些段落的临时仓库地址。
//	  - newSeen/newIDs: 更新后的“已阅”清单，传回给外层下次使用。

// Execute executes the web search tool
func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][WebSearch] Execute started")

	// Parse args from json.RawMessage
	var input WebSearchInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][WebSearch] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Parse query
	query := input.Query
	ok := query != ""
	if !ok || query == "" {
		logger.Errorf(ctx, "[Tool][WebSearch] Query is required")
		return &types.ToolResult{
			Success: false,
			Error:   "query parameter is required",
		}, fmt.Errorf("query parameter is required")
	}

	logger.Infof(ctx, "[Tool][WebSearch] Searching with query: %s, max_results: %d", query, t.maxResults)

	// Get tenant ID from context
	tenantID := uint64(0)
	if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		tenantID = tid
	}

	if tenantID == 0 {
		logger.Errorf(ctx, "[Tool][WebSearch] Tenant ID not found in context")
		return &types.ToolResult{
			Success: false,
			Error:   "tenant ID not found in context",
		}, fmt.Errorf("tenant ID not found in context")
	}

	// Get tenant info from context (same approach as search.go)
	tenant := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant == nil || tenant.WebSearchConfig == nil || tenant.WebSearchConfig.Provider == "" {
		logger.Errorf(ctx, "[Tool][WebSearch] Web search not configured for tenant %d", tenantID)
		return &types.ToolResult{
			Success: false,
			Error:   "web search is not configured for this tenant",
		}, fmt.Errorf("web search is not configured for tenant %d", tenantID)
	}

	// Create a copy of web search config with maxResults from agent config
	searchConfig := *tenant.WebSearchConfig
	searchConfig.MaxResults = t.maxResults

	// Perform web search
	logger.Infof(
		ctx,
		"[Tool][WebSearch] Performing web search with provider: %s, maxResults: %d",
		searchConfig.Provider,
		searchConfig.MaxResults,
	)
	webResults, err := t.webSearchService.Search(ctx, &searchConfig, query)
	if err != nil {
		logger.Errorf(ctx, "[Tool][WebSearch] Web search failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("web search failed: %v", err),
		}, fmt.Errorf("web search failed: %w", err)
	}

	logger.Infof(ctx, "[Tool][WebSearch] Web search returned %d results", len(webResults))

	// Apply RAG compression if configured
	//
	// 如果有搜索结果，且租户配置中开启了压缩功能，就执行 Dag 压缩。
	if len(webResults) > 0 && tenant.WebSearchConfig.CompressionMethod != "none" &&
		tenant.WebSearchConfig.CompressionMethod != "" {

		// Load session-scoped temp KB state from Redis using WebSearchStateRepository
		//
		// 通过 sessionID 从 Redis 中取回该用户当前的回话状态。
		//
		// 返回值含义:
		//	- tempKBID: 当前会话专用的“临时知识库”ID。如果这是该会话第一次搜索，可能为空或需要新建。
		//	- seen: 一个集合，记录了已经处理过的 URL 或文档 ID。用于去重。
		//	- ids: 已存入临时知识库的文档 ID 列表。
		//
		// 如果用户在同一个会话中搜索了两次相似的内容，系统可以识别出哪些结果是新的，哪些是旧的，避免重复处理和重复索引。
		tempKBID, seen, ids := t.webSearchStateService.GetWebSearchTempKBState(ctx, t.sessionID)

		// Build questions for RAG compression
		//
		// 先把用户的搜索词（query）作为一个“问题”，后续 Rag 压缩程序会根据这些问题去网页里“划重点”。
		//
		// Rag 压缩程序：
		//	先把搜索到的几个网页内容全部抓下来，切成一小块一小块的文字（Chunks）；
		//	对比这些小块文字和你的“问题清单”，只留下那些真正能回答问题的段落。
		//	把这些有用的段落存进一个临时知识库（tempKBID）。

		questions := []string{strings.TrimSpace(query)}

		logger.Infof(ctx, "[Tool][WebSearch] Applying RAG compression")
		compressed, kbID, newSeen, newIDs, err := t.webSearchService.CompressWithRAG(
			ctx,
			t.sessionID,            // 会话 ID ，系统会根据这个 ID 在内存或 Redis 里建立一个临时的小仓库，专门放这次搜到的内容。
			tempKBID,               // 临时知识库的 ID ，如果这是该会话的第二次搜索，它会把新搜到的内容追加到之前已经创建好的那个小仓库里。
			questions,              // 用户的问题列表，RAG 压缩时会根据这些问题，去计算网页中哪些段落与问题最相关，不相关的废话（广告、页脚）会被直接扔掉。
			webResults,             // 搜索引擎刚刚给出的原始结果列表（包含 URL 和初步摘要），系统会顺着这些 URL 去抓取更详细的正文内容。
			tenant.WebSearchConfig, // 租户的搜索配置，包含：压缩算法，最大长度...
			t.knowledgeBaseService, // 数据库操作
			t.knowledgeService,     // 数据库操作
			seen,                   // 已经抓取过的 URL 集合，
			ids,                    // 已经处理过的 数据分片（Chunk）ID
		)
		if err != nil {
			logger.Warnf(ctx, "[Tool][WebSearch] RAG compression failed, using raw results: %v", err)
		} else {
			webResults = compressed
			// Persist temp KB state back into Redis using WebSearchStateRepository
			t.webSearchStateService.SaveWebSearchTempKBState(ctx, t.sessionID, kbID, newSeen, newIDs)
			logger.Infof(ctx, "[Tool][WebSearch] RAG compression completed, %d results", len(webResults))
		}
	}

	// Format output
	if len(webResults) == 0 {
		return &types.ToolResult{
			Success: true,
			Output:  fmt.Sprintf("No web search results found for query: %s", query),
			Data: map[string]interface{}{
				"query":   query,
				"results": []interface{}{},
				"count":   0,
			},
		}, nil
	}

	// Build output text
	output := "=== Web Search Results ===\n"
	output += fmt.Sprintf("Query: %s\n", query)
	output += fmt.Sprintf("Found %d result(s)\n\n", len(webResults))

	// Format results
	formattedResults := make([]map[string]interface{}, 0, len(webResults))
	for i, result := range webResults {
		output += fmt.Sprintf("Result #%d:\n", i+1)
		output += fmt.Sprintf("  Title: %s\n", result.Title)
		output += fmt.Sprintf("  URL: %s\n", result.URL)
		if result.Snippet != "" {
			output += fmt.Sprintf("  Snippet: %s\n", result.Snippet)
		}
		if result.Content != "" {
			// Truncate content if too long
			content := result.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			output += fmt.Sprintf("  Content: %s\n", content)
		}
		if result.PublishedAt != nil {
			output += fmt.Sprintf("  Published: %s\n", result.PublishedAt.Format(time.RFC3339))
		}
		output += "\n"

		resultData := map[string]interface{}{
			"result_index": i + 1,
			"title":        result.Title,
			"url":          result.URL,
			"snippet":      result.Snippet,
			"content":      result.Content,
			"source":       result.Source,
		}
		if result.PublishedAt != nil {
			resultData["published_at"] = result.PublishedAt.Format(time.RFC3339)
		}
		formattedResults = append(formattedResults, resultData)
	}

	// Add guidance for next steps
	output += "\n=== Next Steps ===\n"
	if len(webResults) > 0 {
		output += "- ⚠️ Content may be truncated (showing first 500 chars). Use web_fetch to get full page content.\n"
		output += "- Extract URLs from results above and use web_fetch with appropriate prompts to get detailed information.\n"
		output += "- Synthesize information from multiple sources for comprehensive answers.\n"
	} else {
		output += "- No web search results found. Consider:\n"
		output += "  - Try different search queries or keywords\n"
		output += "  - Check if question can be answered from knowledge base instead\n"
		output += "  - Verify if the topic requires real-time information\n"
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"query":        query,
			"results":      formattedResults,
			"count":        len(webResults),
			"display_type": "web_search_results",
		},
	}, nil
}
