package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

// 代码执行流程 (Execute)
//	上下文提取: 获取当前请求的 tenantID。
//	参数解析: 将 AI 传来的 json.RawMessage 解析为 DatabaseQueryInput 结构体，提取 SQL 字符串。
//	基础校验: 检查 SQL 是否为空。
//	安全加固 (核心步骤):
//		调用 validateAndSecureSQL，内部委托给 utils.ValidateAndSecureSQL，传入安全选项（默认注入租户信息、注入检查条件）。
//		如果验证失败（如包含非 SELECT 语句、语法错误、未通过白名单），直接返回错误，不执行查询。
//	执行查询:
//		使用 GORM 的 Raw(securedSQL).Rows() 执行最终的安全 SQL。
//	结果处理:
//		动态获取列名 (rows.Columns())。
//		遍历每一行，将 []byte 类型转换为 string ，构建 map[string]interface{} 列表。
//		处理 NULL 值。
//	格式化输出:
//		调用 formatQueryResults 生成人类可读的文本报告（包含执行 SQL、行数、详细记录）。
//		同时构建 Data 字段，包含原始列、行数据、行数等结构化信息，方便程序化使用。
//	日志记录:
//		全程记录关键步骤（原始 SQL、安全 SQL、行数、错误信息），便于审计和调试。

var databaseQueryTool = BaseTool{
	name: ToolDatabaseQuery,
	description: `Execute SQL queries to retrieve information from the database.

## Security Features
- Automatic tenant_id injection: All queries are automatically filtered by the logged-in user's tenant_id
- Read-only queries: Only SELECT statements are allowed
- Safe tables: Only allow queries on authorized tables (knowledge_bases, knowledges, chunks)

## Available Tables and Columns

### knowledge_bases
- id (VARCHAR): Knowledge base ID
- name (VARCHAR): Knowledge base name
- description (TEXT): Description
- tenant_id (INTEGER): Owner tenant ID
- embedding_model_id, summary_model_id, rerank_model_id (VARCHAR): Model IDs
- vlm_config (JSON): Includes VLM settings such as enabled flag and model_id
- created_at, updated_at (TIMESTAMP)

### knowledges (documents)
- id (VARCHAR): Document ID
- tenant_id (INTEGER): Owner tenant ID
- knowledge_base_id (VARCHAR): Parent knowledge base ID
- type (VARCHAR): Document type
- title (VARCHAR): Document title
- description (TEXT): Description
- source (VARCHAR): Source location
- parse_status (VARCHAR): Processing status (unprocessed/processing/completed/failed)
- enable_status (VARCHAR): Enable status (enabled/disabled)
- file_name, file_type (VARCHAR): File information
- file_size, storage_size (BIGINT): Size in bytes
- created_at, updated_at, processed_at (TIMESTAMP)



### chunks
- id (VARCHAR): Chunk ID
- tenant_id (INTEGER): Owner tenant ID
- knowledge_base_id (VARCHAR): Parent knowledge base ID
- knowledge_id (VARCHAR): Parent document ID
- content (TEXT): Chunk content
- chunk_index (INTEGER): Index in document
- is_enabled (BOOLEAN): Enable status
- chunk_type (VARCHAR): Type (text/image/table)
- created_at, updated_at (TIMESTAMP)

## Usage Examples

Query knowledge base information:
{
  "sql": "SELECT id, name, description FROM knowledge_bases ORDER BY created_at DESC LIMIT 10"
}

Count documents by status:
{
  "sql": "SELECT parse_status, COUNT(*) as count FROM knowledges GROUP BY parse_status"
}

Find recent sessions:
{
  "sql": "SELECT id, title, created_at FROM sessions ORDER BY created_at DESC LIMIT 5"
}

Get storage usage:
{
  "sql": "SELECT SUM(storage_size) as total_storage FROM knowledges"
}

Join knowledge bases and documents:
{
  "sql": "SELECT kb.name as kb_name, COUNT(k.id) as doc_count FROM knowledge_bases kb LEFT JOIN knowledges k ON kb.id = k.knowledge_base_id GROUP BY kb.id, kb.name"
}

## Important Notes
- DO NOT include tenant_id in WHERE clause - it's automatically added
- Only SELECT queries are allowed
- Limit results with LIMIT clause for better performance
- Use appropriate JOINs when querying across tables
- All timestamps are in UTC with time zone`,
	schema: utils.GenerateSchema[DatabaseQueryInput](),
}

type DatabaseQueryInput struct {
	SQL string `json:"sql" jsonschema:"The SELECT SQL query to execute. DO NOT include tenant_id condition - it will be automatically added for security."`
}

// DatabaseQueryTool allows AI to query the database with auto-injected tenant_id for security
type DatabaseQueryTool struct {
	BaseTool
	db *gorm.DB
}

// NewDatabaseQueryTool creates a new database query tool
func NewDatabaseQueryTool(db *gorm.DB) *DatabaseQueryTool {
	return &DatabaseQueryTool{
		BaseTool: databaseQueryTool,
		db:       db,
	}
}

// Execute 执行数据库查询工具，在租户隔离的安全环境下执行 SQL 查询
//
// 功能说明:
//   - 解析并验证工具输入参数，提取待执行的 SQL 语句
//   - 从上下文获取当前租户 ID，实现多租户数据隔离
//   - 对 SQL 进行安全验证和加固处理（注入防护、租户过滤等）
//   - 执行加固后的 SQL 查询并获取结果集
//   - 处理查询结果（类型转换、格式化），返回结构化数据
//
// 参数:
//   - ctx: 上下文，包含租户 ID 等信息，用于控制执行超时和权限验证
//   - args: JSON 格式的原始参数，包含以下字段：
//     - SQL: 待执行的 SQL 查询语句（必填）
//
// 返回值:
//   - *types.ToolResult: 工具执行结果，包含以下内容：
//     - Success: 查询是否成功执行
//     - Output: 格式化的查询结果文本（Markdown 表格格式）
//     - Data: 结构化数据，包含列名、行数据、行数、SQL 语句、租户 ID 等
//     - Error: 错误信息（执行失败时）
//   - error: 工具框架层面的错误，业务逻辑错误通过 ToolResult.Error 返回
//
// 执行流程:
//   1. 提取租户 ID → 2. 解析输入参数 → 3. 验证 SQL 参数
//   4. SQL 安全加固 → 5. 执行查询 → 6. 处理结果集 → 7. 格式化输出
//
// 安全机制:
//   - 租户隔离: 自动注入租户 ID 过滤条件，确保数据访问范围限制在当前租户
//   - SQL 验证: 通过 validateAndSecureSQL 进行注入攻击防护和语法检查
//   - 参数化查询: 使用 GORM 的 Raw 方法执行，防止 SQL 注入
//
// 数据处理:
//   - 列值类型转换: 将 []byte 自动转换为 string，提升可读性
//   - 结果集映射: 将每行数据转换为 map[string]interface{} 格式
//   - 格式化输出: 生成 Markdown 表格形式的查询结果展示
//
// 日志记录:
//   - Info: 执行开始、原始 SQL、加固后 SQL、执行结果统计
//   - Debug: 列信息、首行数据样本、格式化过程
//   - Error: 参数解析失败、SQL 验证失败、查询执行失败、行扫描失败
//
// 错误处理:
//   - 参数解析失败: 返回解析错误详情
//   - SQL 参数缺失: 返回参数缺失错误
//   - SQL 验证失败: 返回验证失败原因
//   - 查询执行失败: 返回数据库错误信息
//   - 列获取/行扫描失败: 返回数据处理错误
//
// 注意事项:
//   - 必须确保上下文包含有效的租户 ID，否则租户隔离可能失效
//   - 所有 SQL 都会经过 validateAndSecureSQL 处理，原始 SQL 不会直接执行
//   - 查询结果中的二进制数据会自动转换为字符串
//   - 结果集较大时建议分页处理，避免内存占用过高
//
// 示例:
//   result, err := tool.Execute(ctx, []byte(`{
//       "SQL": "SELECT id, name FROM users WHERE status = 'active'"
//   }`))
//
//
// 成功返回示例:
// result = &types.ToolResult{
//     Success: true,
//     Output: "| id | name |\n|---|---|\n| 1 | Alice |\n| 2 | Bob |",
//     Data: map[string]interface{}{
//         "columns":   []string{"id", "name"},
//         "rows":      []map[string]interface{}{
//             {"id": 1, "name": "Alice"},
//             {"id": 2, "name": "Bob"},
//         },
//         "row_count": 2,
//         "query":     "SELECT id, name FROM users WHERE status = 'active' AND tenant_id = 42",
//         "tenant_id": 42,
//         "display_type": "database_query",
//     },
//     Error: "",
// }
//
// 失败返回示例:
// result = &types.ToolResult{
//     Success: false,
//     Output:  "",
//     Data:    nil,
//     Error:   "SQL validation failed: detected potential SQL injection",
// }

// Execute executes the database query tool
func (t *DatabaseQueryTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][DatabaseQuery] Execute started")

	tenantID := uint64(0)
	if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		tenantID = tid
	}

	// Parse args from json.RawMessage
	var input DatabaseQueryInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][DatabaseQuery] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Extract SQL from input
	if input.SQL == "" {
		logger.Errorf(ctx, "[Tool][DatabaseQuery] Missing or invalid SQL parameter")
		return &types.ToolResult{
			Success: false,
			Error:   "Missing or invalid 'sql' parameter",
		}, fmt.Errorf("missing sql parameter")
	}

	logger.Infof(ctx, "[Tool][DatabaseQuery] Original SQL query:\n%s", input.SQL)
	logger.Infof(ctx, "[Tool][DatabaseQuery] Tenant ID: %d", tenantID)

	// Validate and secure the SQL query
	logger.Debugf(ctx, "[Tool][DatabaseQuery] Validating and securing SQL...")
	securedSQL, err := t.validateAndSecureSQL(input.SQL, tenantID)
	if err != nil {
		logger.Errorf(ctx, "[Tool][DatabaseQuery] SQL validation failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("SQL validation failed: %v", err),
		}, err
	}

	logger.Infof(ctx, "[Tool][DatabaseQuery] Secured SQL query:\n%s", securedSQL)
	logger.Infof(ctx, "Executing secured SQL query - original: %s, secured: %s, tenant_id: %d",
		input.SQL, securedSQL, tenantID)

	// Execute the query
	logger.Infof(ctx, "[Tool][DatabaseQuery] Executing query against database...")
	rows, err := t.db.WithContext(ctx).Raw(securedSQL).Rows()
	if err != nil {
		logger.Errorf(ctx, "[Tool][DatabaseQuery] Query execution failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Query execution failed: %v", err),
		}, err
	}
	defer rows.Close()

	logger.Debugf(ctx, "[Tool][DatabaseQuery] Query executed successfully, processing rows...")

	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to get columns: %v", err),
		}, err
	}

	// Process results
	results := make([]map[string]interface{}, 0)
	for rows.Next() {
		// Create a slice of interface{} to hold each column value
		columnValues := make([]interface{}, len(columns))
		columnPointers := make([]interface{}, len(columns))
		for i := range columnValues {
			columnPointers[i] = &columnValues[i]
		}

		// Scan the row
		if err := rows.Scan(columnPointers...); err != nil {
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("Failed to scan row: %v", err),
			}, err
		}

		// Create a map for this row
		rowMap := make(map[string]interface{})
		for i, colName := range columns {
			val := columnValues[i]
			// Convert []byte to string for better readability
			if b, ok := val.([]byte); ok {
				rowMap[colName] = string(b)
			} else {
				rowMap[colName] = val
			}
		}
		results = append(results, rowMap)
	}

	if err := rows.Err(); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Error iterating rows: %v", err),
		}, err
	}

	logger.Infof(ctx, "[Tool][DatabaseQuery] Retrieved %d rows with %d columns", len(results), len(columns))
	logger.Debugf(ctx, "[Tool][DatabaseQuery] Columns: %v", columns)

	// Log first few rows for debugging
	if len(results) > 0 {
		logger.Debugf(ctx, "[Tool][DatabaseQuery] First row sample:")
		for key, value := range results[0] {
			logger.Debugf(ctx, "[Tool][DatabaseQuery]   %s: %v", key, value)
		}
	}

	// Format output
	logger.Debugf(ctx, "[Tool][DatabaseQuery] Formatting query results...")
	output := t.formatQueryResults(columns, results, securedSQL)

	logger.Infof(ctx, "[Tool][DatabaseQuery] Execute completed successfully: %d rows returned", len(results))
	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"columns":      columns,
			"rows":         results,
			"row_count":    len(results),
			"query":        securedSQL,
			"tenant_id":    tenantID,
			"display_type": "database_query",
		},
	}, nil
}

// validateAndSecureSQL validates the SQL query and injects tenant_id conditions
func (t *DatabaseQueryTool) validateAndSecureSQL(sqlQuery string, tenantID uint64) (string, error) {
	// Use the new ValidateAndSecureSQL with comprehensive security options
	securedSQL, validationResult, err := utils.ValidateAndSecureSQL(
		sqlQuery,
		utils.WithSecurityDefaults(tenantID),
		utils.WithInjectionRiskCheck(),
	)
	if err != nil {
		return "", err
	}

	if !validationResult.Valid {
		// Build error message from validation errors
		var errMsgs []string
		for _, valErr := range validationResult.Errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %s", valErr.Type, valErr.Message))
		}
		return "", fmt.Errorf("validation failed: %s", strings.Join(errMsgs, "; "))
	}

	return securedSQL, nil
}

// formatQueryResults formats query results into readable text
func (t *DatabaseQueryTool) formatQueryResults(
	columns []string,
	results []map[string]interface{},
	query string,
) string {
	output := "=== 查询结果 ===\n\n"
	output += fmt.Sprintf("执行的SQL: %s\n\n", query)
	output += fmt.Sprintf("返回 %d 行数据\n\n", len(results))

	if len(results) == 0 {
		output += "未找到匹配的记录。\n"
		return output
	}

	output += "=== 数据详情 ===\n\n"

	// Format each row
	for i, row := range results {
		output += fmt.Sprintf("--- 记录 #%d ---\n", i+1)
		for _, col := range columns {
			value := row[col]
			// Format the value
			var formattedValue string
			if value == nil {
				formattedValue = "<NULL>"
			} else if jsonData, err := json.Marshal(value); err == nil {
				// Check if it's a complex type
				switch v := value.(type) {
				case string:
					formattedValue = v
				case []byte:
					formattedValue = string(v)
				default:
					formattedValue = string(jsonData)
				}
			} else {
				formattedValue = fmt.Sprintf("%v", value)
			}

			output += fmt.Sprintf("  %s: %s\n", col, formattedValue)
		}
		output += "\n"
	}

	// Add summary statistics if applicable
	if len(results) > 10 {
		output += fmt.Sprintf("注意: 显示了前 %d 条记录，共 %d 条。建议使用 LIMIT 子句限制结果数量。\n", len(results), len(results))
	}

	return output
}
