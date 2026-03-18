package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

// 在处理 Excel 或 CSV 时，系统并不会每次都去读取原始的大文件，而是预先将文件的结构提取并存储为特殊的 Chunk（分块）：
//	- ChunkTypeTableSummary：存储表的总体概况（如：表名、总行数、数据来源、简要描述）。
//	- ChunkTypeTableColumn：存储列的详细定义（如：列名、数据类型、示例值、备注）。
// 该工具的作用就是精准地把这两类信息捞出来。

// 应用场景
//	SQL 生成前置动作：AI 在调用 DuckDB 执行查询前，必须先调用此工具获取 Schema，否则无法写出正确的 SELECT 语句。
//	数据可用性检查：用户问“这个表里有手机号吗？”，AI 调用此工具查看列名定义即可快速回答。
//	跨租户报表分析：当分公司查看总部共享的财务报表时，通过该工具识别总部报表的字段规范。

// Execute 获取指定知识库中表格文件的 Schema 信息。
//
// 流程如下：
// 1. 参数解析：反序列化 LLM 传入的 JSON 参数为 DataSchemaInput。
// 2. 租户鉴权：通过 KnowledgeID 获取 TenantID，以支持跨租户共享并确保数据隔离。
// 3. 精准检索：基于元数据过滤，仅查询类型为 TableSummary 和 TableColumn 的 Chunk。
// 4. 内容组装：提取并校验表摘要与列定义内容，若缺失则返回错误。
// 5. 结果返回：同时输出供 LLM 阅读的自然语言文本 (Output) 和供程序使用的结构化数据 (Data)。

// 设计亮点：
// 1. 解耦数据处理与推理：依赖预处理生成的 Chunk，避免运行时解析文件，以空间换时间降低延迟与 Token 消耗。
// 2. 利用 Chunk 原子化优势：精准提取“表摘要”与“列定义”独立块，无需加载全文，契合 DAG 灵活编排需求。
// 3. 安全与多租户支持：通过 IDOnly 模式获取 TenantID 进行鉴权，确保跨租户共享场景下的数据隔离合规。
// 4. 增强容错性：严格校验关键内容完整性，防止因信息缺失导致 LLM 上下文空洞或产生幻觉。

var dataSchemaTool = BaseTool{
	name:        ToolDataSchema,
	description: "Use this tool to get the schema information of a CSV or Excel file loaded into DuckDB. It returns the table name, columns, and row count.",
	schema:      utils.GenerateSchema[DataSchemaInput](),
}

type DataSchemaInput struct {
	KnowledgeID string `json:"knowledge_id" jsonschema:"id of the knowledge to query"`
}

type DataSchemaTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeService
	chunkRepo        interfaces.ChunkRepository
	targetChunkTypes []types.ChunkType
}

func NewDataSchemaTool(knowledgeService interfaces.KnowledgeService, chunkRepo interfaces.ChunkRepository, targetChunkTypes ...types.ChunkType) *DataSchemaTool {
	if len(targetChunkTypes) == 0 {
		targetChunkTypes = []types.ChunkType{types.ChunkTypeTableSummary, types.ChunkTypeTableColumn}
	}
	return &DataSchemaTool{
		BaseTool:         dataSchemaTool,
		knowledgeService: knowledgeService,
		chunkRepo:        chunkRepo,
		targetChunkTypes: targetChunkTypes,
	}
}

// Execute executes the tool logic
func (t *DataSchemaTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input DataSchemaInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse input args: %v", err),
		}, err
	}

	// Get knowledge to get TenantID (use IDOnly to support cross-tenant shared KB)
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, input.KnowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to get knowledge '%s': %v", input.KnowledgeID, err),
		}, err
	}

	// Get chunks for the knowledge ID using ChunkRepository
	// We only need table summary and column chunks
	chunkTypes := t.targetChunkTypes
	page := &types.Pagination{
		Page:     1,
		PageSize: 100, // Should be enough for schema chunks
	}

	chunks, _, err := t.chunkRepo.ListPagedChunksByKnowledgeID(
		ctx,
		knowledge.TenantID,
		input.KnowledgeID,
		page,
		chunkTypes,
		"", // tagID
		"", // keyword
		"", // searchField
		"", // sortOrder
		"", // knowledgeType
	)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to list chunks for knowledge ID '%s': %v", input.KnowledgeID, err),
		}, err
	}

	var summaryContent, columnContent string
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeTableSummary {
			summaryContent = chunk.Content
		} else if chunk.ChunkType == types.ChunkTypeTableColumn {
			columnContent = chunk.Content
		}
	}

	if summaryContent == "" || columnContent == "" {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("No table schema information found for knowledge ID '%s'", input.KnowledgeID),
		}, fmt.Errorf("no schema info found")
	}

	output := fmt.Sprintf("%s\n\n%s", summaryContent, columnContent)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"summary": summaryContent,
			"columns": columnContent,
		},
	}, nil
}
