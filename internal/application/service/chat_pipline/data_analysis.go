package chatpipline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

// 大模型的“算术短板”
//
//	现状：像 GPT-4 或 Claude 这样的大语言模型，本质上是“文字接龙”高手，而不是数学家。
//	问题：
//		如果你问它“这张表里所有销售额的总和是多少？”，它可能会通过概率去“猜”一个数字，
//		或者因为上下文太长而数错，导致幻觉（一本正经地胡说八道）。
//	解决方案：
//		这段代码引入了 Text-to-SQL 技术。
//		它不让 AI 直接算，而是让 AI 写 SQL 语句，交给专业的数据库引擎（DuckDB）去算，确保结果 100% 准确。

// 场景一：企业销售/运营报表查询
//	用户：销售经理。
//	上传文件：2023年销售记录.csv（包含日期、产品、金额、地区等列）。
//	用户提问：“上个季度华东地区的销售总额是多少？”
//	代码行为：
//		识别到 .csv 文件。
//		将文件加载到 DuckDB。
//		LLM 生成 SQL：SELECT SUM(amount) FROM table WHERE region='华东' AND date...
//		返回计算结果给经理。
//
// 场景二：个人财务/账单分析
//	用户：普通个人用户。
//	上传文件：我的年度账单.xlsx。
//	用户提问：“我今年在餐饮上花费最多的是哪个月？”
//	代码行为：
//		识别 Excel 文件。
//		生成 SQL：SELECT month, SUM(cost) ... GROUP BY month ORDER BY sum DESC LIMIT 1。
//		返回具体的月份和金额。
//
// 场景三：混合问答（文档 + 数据）
//	用户：研究员。
//	上传文件：一份《年度市场分析报告.pdf》和一个《原始调查数据.csv》。
//	用户提问：“根据数据表，样本中有多少女性？并结合报告分析她们的消费习惯。”
//	代码行为：
//		针对数据部分：这段代码介入，计算出女性样本数量（例如：500人）。
//		针对文档部分：普通的检索插件介入，从 PDF 中找到关于“消费习惯”的文字描述。
//		最终回答：系统将这两部分信息结合，生成一个完整的回答。
//

type PluginDataAnalysis struct {
	modelService     interfaces.ModelService
	knowledgeService interfaces.KnowledgeService
	fileService      interfaces.FileService
	chunkRepo        interfaces.ChunkRepository
	db               *sql.DB
}

func NewPluginDataAnalysis(
	eventManager *EventManager,
	modelService interfaces.ModelService,
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
	chunkRepo interfaces.ChunkRepository,
	db *sql.DB,
) *PluginDataAnalysis {
	p := &PluginDataAnalysis{
		modelService:     modelService,
		knowledgeService: knowledgeService,
		fileService:      fileService,
		chunkRepo:        chunkRepo,
		db:               db,
	}
	eventManager.Register(p)
	return p
}

func (p *PluginDataAnalysis) ActivationEvents() []types.EventType {
	return []types.EventType{types.DATA_ANALYSIS}
}

// 执行流程：
// 	[用户提问] →
//	[发现表格] →
//	[提取表结构] →
//	[AI 生成 SQL] →
//	[数据库算数] →
//	[结果回填结果集] →
//	[AI 最终根据算出的结果说话]
//
// 函数说明：
//
// OnEvent 是数据分析插件的核心入口，负责实现基于 Text-to-SQL 的自动化数据处理管道。
//
// 该函数旨在解决大模型在数值计算与复杂聚合任务中的幻觉问题。它通过拦截用户的自然语言查询，
// 结合上下文中的结构化数据文件（CSV/Excel），利用 LLM 生成并执行 SQL 语句，最终将精确的计算结果
// 注入到检索上下文中，供后续对话模块使用。
//
// 处理流程详解:
//
// 1. 资源扫描与上下文净化
//   - 识别结构化数据：遍历当前的检索结果集 (MergeResult)，通过文件后缀匹配机制提取 CSV/Excel 文件。
//   - 噪声过滤：调用 filterOutTableChunks 移除检索阶段产生的表格元数据切片（如表头描述、摘要文本），
//     防止非数据性的文本碎片干扰后续语义分析并节省 Token 窗口。
//   - 短路逻辑：若未检测到有效数据文件，立即终止当前逻辑并调用 next()，实现无侵入式的降级处理。
//
// 2. 运行时环境初始化与 ETL
//   - 元数据获取：根据目标文件 ID ，通过 knowledgeService 定位文件物理路径及详细属性。
//   - 沙箱构建：创建基于 SessionID 隔离的 DuckDB 实例。
//   - 数据装载与内省：将外部文件映射为内存数据库中的临时表，自动提取表名、列名及数据类型，
//     为后续生成准确的 SQL 语句提供必要的上下文基础。
//
// 3. 语义解析与代码生成
//   - 提示词工程：
//     构建包含“用户查询”、“知识库 ID”及“表结构 Schema”的结构化提示词，调用 LLM 进行推理。
//   - 意图识别与转换：调用聊天模型 (chatModel) 进行推理，模型在此承担双重任务：
//     a. 意图判定：分析用户问题是否涉及统计、聚合或过滤等需要数据库处理的场景。
//     b. 代码生成：若判定需要分析，则将自然语言转换为符合 DuckDB 语法的 SQL 语句。
//   - 结构化输出：
//     利用 utils.GenerateSchema 强制模型输出标准化的 JSON 格式，确保程序能可靠解析出 sql 字段。
//
// 4. 确定性执行与计算落地
//   - 指令执行：解析模型返回的 JSON 响应，提取 SQL 语句并在 DuckDB 环境中执行，获取执行结果（如统计值、筛选集）。
//   - 确定性保障：
//     通过数据库引擎处理具体的数学运算（如 SUM, AVG, COUNT），
//     将概率性的语言模型推理转化为确定性的数据查询，从根本上规避 AI 在大数值计算上的“幻觉”问题。
//
// 5. 结果封装与上下文注入
//   - 结果标准化：
//     将 SQL 执行得到的原始数据（如标量值或结果集）封装为标准的 SearchResult 对象。
//   - 类型标记：
//     设置 MatchType 为 DATA_ANALYSIS 并赋予高置信度评分 (Score: 1.0)，以此区分该结果来源于精确计算而非模糊检索。
//   - 上下文增强：
//     将分析结果追加至 MergeResult 列表，使后续的对话生成模块能够将此计算结果作为“事实依据”纳入最终的回答生成中。
//   - 流程控制：
//     最终调用 next()，确保无论分析成功与否，对话处理流水线都能继续流转，维持系统的健壮性。

func (p *PluginDataAnalysis) OnEvent(
	ctx context.Context,
	eventType types.EventType,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	// 1. Check if there are any CSV/Excel files in MergeResult
	var dataFiles []*types.SearchResult
	for _, result := range chatManage.MergeResult {
		if isDataFile(result.KnowledgeFilename) {
			dataFiles = append(dataFiles, result)
		}
	}

	// Filter out table column and table summary chunks from MergeResult
	chatManage.MergeResult = filterOutTableChunks(chatManage.MergeResult)

	if len(dataFiles) == 0 {
		return next()
	}

	// 2. Ask LLM if data analysis is needed
	// We only process the first data file for now to avoid complexity
	targetFile := dataFiles[0]

	// Get Knowledge details to get file path
	knowledge, err := p.knowledgeService.GetKnowledgeByID(ctx, targetFile.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge %s: %v", targetFile.KnowledgeID, err)
		return next()
	}

	// Initialize DataAnalysisTool
	tool := tools.NewDataAnalysisTool(p.knowledgeService, p.fileService, p.db, chatManage.SessionID)
	defer tool.Cleanup(ctx)

	// Load data into DuckDB
	schema, err := tool.LoadFromKnowledge(ctx, knowledge)
	if err != nil {
		logger.Errorf(ctx, "Failed to get data schema: %v", err)
		return next()
	}

	// Ask LLM to generate SQL for data analysis
	chatModel, err := p.modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		return ErrGetChatModel.WithError(err)
	}

	// Use utils.GenerateSchema to generate format schema for DataAnalysisInput
	formatSchema := utils.GenerateSchema[tools.DataAnalysisInput]()

	analysisPrompt := fmt.Sprintf(`
User Question: %s
Knowledge ID: %s
Table Schema: %s

Determine if the user's question requires data analysis (e.g., statistics, aggregation, filtering) on this table.
If YES, generate a DuckDB SQL query to answer the user's question and fill in the knowledge_id and sql fields.
If NO, leave the sql field empty.

Return your response in the specified JSON format.`, chatManage.Query, knowledge.ID, schema.Description())

	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "user", Content: analysisPrompt},
	}, &chat.ChatOptions{
		Temperature: 0.1,
		Format:      formatSchema,
	})
	if err != nil {
		logger.Errorf(ctx, "Failed to generate analysis response: %v", err)
		return next()
	}
	// logger.Debugf(ctx, "Data analysis LLM response: %s", response.Content)

	// Execute SQL using the tool
	// Initialize DataAnalysisTool
	toolResult, err := tool.Execute(ctx, json.RawMessage(response.Content))
	if err != nil {
		logger.Errorf(ctx, "Failed to execute SQL: %v", err)
		return next()
	}

	// 5. Store result
	// Create a new SearchResult for the analysis output
	analysisResult := &types.SearchResult{
		ID:                "analysis_" + knowledge.ID,
		Content:           toolResult.Output,
		Score:             1.0,
		MatchType:         types.MatchTypeDataAnalysis,
		KnowledgeID:       knowledge.ID,
		KnowledgeTitle:    knowledge.Title,
		KnowledgeFilename: knowledge.FileName,
	}

	chatManage.MergeResult = append(chatManage.MergeResult, analysisResult)

	return next()
}

func isDataFile(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.HasSuffix(lower, ".csv") || strings.HasSuffix(lower, ".xlsx") || strings.HasSuffix(lower, ".xls")
}

// filterOutTableChunks filters out table column and table summary chunks from search results
func filterOutTableChunks(results []*types.SearchResult) []*types.SearchResult {
	filtered := make([]*types.SearchResult, 0, len(results))
	filterList := []string{string(types.ChunkTypeTableColumn), string(types.ChunkTypeTableSummary)}
	for _, result := range results {
		if slices.Contains(filterList, result.ChunkType) {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}
