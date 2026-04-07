package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/types"
)

// 渐进式 RAG 系统提示词 是一种专门设计给大语言模型（LLM）的指令集，
// 旨在解决传统 RAG（检索增强生成）系统中常见的“浅层检索”和“幻觉”问题。
//
// 它的核心思想是：不要只相信搜索摘要，必须“深读”全文；不要急于回答，必须先“侦察”再“计划”。
//
// 1. 为什么叫“渐进式” (Progressive)？
//	传统的 RAG 往往是线性的：用户提问 -> 搜索 -> 把搜索结果扔给模型 -> 模型回答。
//	这种模式容易导致模型只看搜索结果的片段（Snippet）就匆忙下结论，导致信息不全或错误。
//
//	渐进式 RAG 将回答过程分为几个递进的阶段，模型必须按顺序执行，不能跳跃：
//	 - 侦察 (Reconnaissance): 先做初步搜索，但不许回答。
//	 - 深读 (Deep Read): 拿到搜索到的 ID 后，必须调用工具读取完整内容（Full Text）。
//	 - 评估与计划 (Evaluation & Planning): 基于全文判断信息够不够。如果不够，制定多步计划（Todo List）。
//	 - 执行与反思 (Execution & Reflection): 一步步执行计划，每步都要再次“深读”并反思“这真的解决了问题吗？”。
//	 - 综合 (Synthesis): 最后才生成答案。
//
// 2. 核心规则
//
// A. 强制深读 (Mandatory Deep Read)
//	传统做法: 搜索返回一段摘要（例如：“文档A提到销售额增长了...”），模型直接用这段摘要回答。
//	渐进式做法: 搜索返回 chunk_id: 123。提示词强制要求模型必须立即调用 list_knowledge_chunks(ids=[123]) 获取该片段的完整上下文。
//	目的: 防止断章取义。摘要可能丢失关键的否定词、条件状语或上下文逻辑。
// B. 证据优先 (Evidence-First)
//	提示词明确告知模型：“假装你没有训练数据”。
//	所有事实性陈述必须基于检索到的内容。如果没有检索到，就说不知道，或者去联网搜索（如果开启），绝不能编造。
// C. 就近引用 (Proximate Citations)
//	要求: 引用标签 <kb ... /> 必须紧跟在它所支持的事实句子后面，而不是放在段落末尾或文章最后。
//	目的: 方便用户核查每一个具体论点的来源，增加可信度。
// D. 动态策略选择
//	模型在“侦察”阶段后，需要自己决定走哪条路：
//	- 路径 A (直接回答): 如果深读后发现信息充足且明确。
//	- 路径 B (复杂研究): 如果问题涉及对比、多文档综合或信息缺失，必须使用 todo_write 创建任务列表，逐个击破。

// formatFileSize 格式化文件大小为易读格式
// 输入: size - 文件大小（字节，int64类型）
// 输出: 带有适当单位（B, KB, MB, GB）的格式化字符串

// formatFileSize formats file size in human-readable format
func formatFileSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	if size < KB {
		return fmt.Sprintf("%d B", size)
	} else if size < MB {
		return fmt.Sprintf("%.2f KB", float64(size)/KB)
	} else if size < GB {
		return fmt.Sprintf("%.2f MB", float64(size)/MB)
	}
	return fmt.Sprintf("%.2f GB", float64(size)/GB)
}

// formatDocSummary 清理和截断文档摘要，用于表格显示
// 输入: summary - 原始摘要文本，maxLen - 截断的最大长度
// 输出: 清理后的摘要字符串，如果被截断则添加"..."后缀

// formatDocSummary cleans and truncates document summaries for table display
func formatDocSummary(summary string, maxLen int) string {
	cleaned := strings.TrimSpace(summary)
	if cleaned == "" {
		return "-"
	}
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	runes := []rune(cleaned)
	if len(runes) <= maxLen {
		return cleaned
	}
	return strings.TrimSpace(string(runes[:maxLen])) + "..."
}

// RecentDocInfo 包含最近添加到知识库中的文档的简要信息。
// 用于在系统提示词中向 Agent 展示知识库的最新动态，帮助 Agent 了解最新可用数据。
// 根据知识库类型（文档或 FAQ），部分字段（如 FAQQuestions/Answers）可能为空。

// RecentDocInfo contains brief information about a recently added document
type RecentDocInfo struct {
	ChunkID             string
	KnowledgeBaseID     string
	KnowledgeID         string
	Title               string
	Description         string
	FileName            string
	FileSize            int64
	Type                string
	CreatedAt           string // Formatted time string
	FAQStandardQuestion string
	FAQSimilarQuestions []string
	FAQAnswers          []string
}

// SelectedDocumentInfo 包含用户通过 "@提及" 显式选中的文档的元数据结构体。
// 注意：此结构体仅包含元数据，不包含文档具体内容。Agent 需根据其中的 ID 调用工具获取内容。
// 用于在提示词中告知 Agent 优先关注这些特定文档。

// SelectedDocumentInfo contains summary information about a user-selected document (via @ mention)
// Only metadata is included; content will be fetched via tools when needed
type SelectedDocumentInfo struct {
	KnowledgeID     string // Knowledge ID
	KnowledgeBaseID string // Knowledge base ID
	Title           string // Document title
	FileName        string // Original file name
	FileType        string // File type (pdf, docx, etc.)
}

// KnowledgeBaseInfo 包含知识库基本信息的结构体，用于构建 Agent 的系统提示词。
// 它聚合了知识库的基本属性以及最近添加的文档列表，使 Agent 能够感知当前上下文可用的知识范围。

// KnowledgeBaseInfo contains essential information about a knowledge base for agent prompt
type KnowledgeBaseInfo struct {
	ID          string          // 知识库唯一 ID
	Name        string          // 知识库名称
	Type        string          // 知识库类型："document" (文档型) 或 "faq" (问答型)
	Description string          // 知识库描述
	DocCount    int             // 知识库中文档总数
	RecentDocs  []RecentDocInfo // 最近添加的文档列表（通常限制为前 10 条）
}

// PlaceholderDefinition 定义暴露给UI/配置的占位符。
// 用于前端展示哪些变量可以在提示词模板中被动态替换。
//
// Deprecated: 请使用 types.PromptPlaceholder 代替。

// PlaceholderDefinition defines a placeholder exposed to UI/configuration
// Deprecated: Use types.PromptPlaceholder instead
type PlaceholderDefinition struct {
	Name        string `json:"name"`        // 占位符名称，如 "{{current_time}}"
	Label       string `json:"label"`       // 显示给用户的标签
	Description string `json:"description"` // 占位符的详细描述
}

// AvailablePlaceholders 列出当前 Agent 模式支持的所有提示词占位符，用于 UI 提示。
//
// 返回:
//   - []PlaceholderDefinition: 占位符定义列表。

// AvailablePlaceholders lists all supported prompt placeholders for UI hints
// This returns agent mode specific placeholders
func AvailablePlaceholders() []PlaceholderDefinition {
	// Use centralized placeholder definitions from types package
	placeholders := types.PlaceholdersByField(types.PromptFieldAgentSystemPrompt)
	result := make([]PlaceholderDefinition, len(placeholders))
	for i, p := range placeholders {
		result[i] = PlaceholderDefinition{
			Name:        p.Name,
			Label:       p.Label,
			Description: p.Description,
		}
	}
	return result
}

// formatKnowledgeBaseList 将知识库信息列表格式化为 Markdown 字符串，用于提示词。
// 该函数会根据知识库类型（FAQ 或 文档）生成不同格式的表格，展示最近添加的条目。
//
// 参数:
//   - kbInfos: 知识库信息切片。
// 返回:
//   - string: 格式化后的 Markdown 字符串。如果列表为空，返回 "None"。

// formatKnowledgeBaseList 格式化知识库信息，生成用于系统提示的Markdown文本
//
// 功能说明:
//   - 将知识库列表格式化为结构化的Markdown文本，供Agent的系统提示使用
//   - 根据知识库类型（文档/FAQ）采用不同的展示格式
//   - 包含知识库基本信息（名称、ID、类型、描述、文档数）
//   - 展示最近添加的文档/FAQ条目（最多10条），以表格形式呈现
//   - 为Agent提供清晰的知识库上下文，指导其检索策略
//
// 参数:
//   - kbInfos: []*KnowledgeBaseInfo 知识库信息列表，每个元素包含：
//       * ID: 知识库唯一标识
//       * Name: 知识库名称
//       * Type: 知识库类型（document/faq等，空值默认为document）
//       * Description: 知识库描述
//       * DocCount: 文档总数
//       * RecentDocs: []RecentDoc 最近添加的文档列表，包含：
//         - 文档类型：Title, FileName, Type, CreatedAt, KnowledgeID, FileSize, Description
//         - FAQ类型：FAQStandardQuestion, FAQAnswers, ChunkID, KnowledgeID, CreatedAt
//
// 返回值:
//   - string: 格式化后的Markdown文本，包含：
//       * 引导说明（告知Agent应在这些知识库中搜索）
//       * 知识库基本信息（序号、名称、ID、类型、描述、文档数）
//       * 最近文档表格（不同类型不同表头）
//       * 空知识库列表时返回"None"
//
// 格式化规则:
//
// 引导语:
//   固定文本："The following knowledge bases have been selected by the user for this conversation.
//   You should search within these knowledge bases to find relevant information."
//   作用：明确告知Agent可用的知识库范围
//
// 知识库基本信息格式:
//   {序号}. **{名称}** (knowledge_base_id: `{ID}`)
//      - Type: {类型}
//      - Description: {描述}（如有）
//      - Document count: {文档数}
//
// 文档类型知识库表格:
//   表头: | # | Document Name | Type | Created At | Knowledge ID | File Size | Summary |
//   行数据:
//     - Document Name: Title优先，否则FileName
//     - Type: 文档类型
//     - Created At: 创建时间
//     - Knowledge ID: 文档ID（代码格式）
//     - File Size: 格式化后的文件大小（B/KB/MB/GB）
//     - Summary: 描述截断至120字符
//
// FAQ类型知识库表格:
//   表头: | # | Question | Answers | Chunk ID | Knowledge ID | Created At |
//   行数据:
//     - Question: FAQStandardQuestion优先，否则FileName
//     - Answers: FAQAnswers用" | "连接，无则"-"
//     - Chunk ID: 分片ID（代码格式）
//     - Knowledge ID: 知识ID（代码格式）
//     - Created At: 创建时间
//
// 限制:
//   - 最近文档最多展示10条（j >= 10时跳出）
//   - 描述摘要截断至120字符
//
// 辅助函数:
//   - formatFileSize: 将字节数格式化为人类可读的大小（B/KB/MB/GB）
//   - formatDocSummary: 截断描述文本至指定长度
//
// 使用场景:
//   - Agent系统提示构建：告知Agent可用的知识库资源
//   - 多知识库会话：展示用户选中的多个知识库
//   - 知识库预览：让Agent了解知识库内容和规模
//
// 调用位置:
//   - 通常在Agent配置阶段调用，生成系统提示的一部分
//   - 与ResolveSystemPrompt配合使用，注入知识库上下文
//
// 示例:
//   kbInfos := []*KnowledgeBaseInfo{
//       {
//           ID:          "kb-product",
//           Name:        "产品文档",
//           Type:        "document",
//           Description: "产品使用手册和技术文档",
//           DocCount:    150,
//           RecentDocs: []RecentDoc{
//               {Title: "快速入门", FileName: "quickstart.pdf", Type: "pdf", ...},
//               {Title: "API参考", FileName: "api.md", Type: "markdown", ...},
//           },
//       },
//       {
//           ID:          "kb-faq",
//           Name:        "常见问题",
//           Type:        "faq",
//           Description: "用户常见问题解答",
//           DocCount:    500,
//           RecentDocs: []RecentDoc{
//               {FAQStandardQuestion: "如何重置密码？", FAQAnswers: []string{"步骤1...", "步骤2..."}, ...},
//           },
//       },
//   }
//
//   output := formatKnowledgeBaseList(kbInfos)
//
// 返回示例:
//	  "\nThe following knowledge bases have been selected by the user for this conversation.
//	  You should search within these knowledge bases to find relevant information.\n\n
//	  1. **产品文档** (knowledge_base_id: `kb-product`)
//		 - Type: document
//		 - Description: 产品使用手册和技术文档
//		 - Document count: 150
//		 - Recently added documents:
//
//		   | # | Document Name | Type | Created At | Knowledge ID | File Size | Summary |
//		   |---|---------------|------|------------|--------------|-----------|---------|
//		   | 1 | 快速入门       | pdf   | 2024-01-15 | `doc-001`    | 2.5 MB   | 本文档介绍产品的快速上手方法... |
//		   | 2 | API参考        | markdown | 2024-01-10 | `doc-002` | 150 KB  | API详细说明文档... |
//
//
//	  2. **常见问题** (knowledge_base_id: `kb-faq`)
//		 - Type: faq
//		 - Description: 用户常见问题解答
//		 - Document count: 500
//		 - Recent FAQ entries:
//
//		   | # | Question | Answers | Chunk ID | Knowledge ID | Created At |
//		   |---|----------|---------|----------|--------------|------------|
//		   | 1 | 如何重置密码？ | 步骤1... \| 步骤2... | `chunk-001` | `faq-001` | 2024-01-20 |
//
//	  "
//
//	  空列表返回:
//	  "None"

// formatKnowledgeBaseList formats knowledge base information for the prompt
func formatKnowledgeBaseList(kbInfos []*KnowledgeBaseInfo) string {
	if len(kbInfos) == 0 {
		return "None"
	}

	var builder strings.Builder
	builder.WriteString("\nThe following knowledge bases have been selected by the user for this conversation. ")
	builder.WriteString("You should search within these knowledge bases to find relevant information.\n\n")
	for i, kb := range kbInfos {
		// Display knowledge base name and ID
		builder.WriteString(fmt.Sprintf("%d. **%s** (knowledge_base_id: `%s`)\n", i+1, kb.Name, kb.ID))

		// Display knowledge base type
		kbType := kb.Type
		if kbType == "" {
			kbType = "document" // Default type
		}
		builder.WriteString(fmt.Sprintf("   - Type: %s\n", kbType))

		if kb.Description != "" {
			builder.WriteString(fmt.Sprintf("   - Description: %s\n", kb.Description))
		}
		builder.WriteString(fmt.Sprintf("   - Document count: %d\n", kb.DocCount))

		// Display recent documents if available
		// For FAQ type knowledge bases, adjust the display format
		if len(kb.RecentDocs) > 0 {
			if kbType == "faq" {
				// FAQ knowledge base: show Q&A pairs in a more compact format
				builder.WriteString("   - Recent FAQ entries:\n\n")
				builder.WriteString("     | # | Question  | Answers | Chunk ID | Knowledge ID | Created At |\n")
				builder.WriteString("     |---|-------------------|---------|----------|--------------|------------|\n")
				for j, doc := range kb.RecentDocs {
					if j >= 10 { // Limit to 10 documents
						break
					}
					question := doc.FAQStandardQuestion
					if question == "" {
						question = doc.FileName
					}
					answers := "-"
					if len(doc.FAQAnswers) > 0 {
						answers = strings.Join(doc.FAQAnswers, " | ")
					}
					builder.WriteString(fmt.Sprintf("     | %d | %s | %s | `%s` | `%s` | %s |\n",
						j+1, question, answers, doc.ChunkID, doc.KnowledgeID, doc.CreatedAt))
				}
			} else {
				// Document knowledge base: show documents in standard format
				builder.WriteString("   - Recently added documents:\n\n")
				builder.WriteString("     | # | Document Name | Type | Created At | Knowledge ID | File Size | Summary |\n")
				builder.WriteString("     |---|---------------|------|------------|--------------|----------|---------|\n")
				for j, doc := range kb.RecentDocs {
					if j >= 10 { // Limit to 10 documents
						break
					}
					docName := doc.Title
					if docName == "" {
						docName = doc.FileName
					}
					// Format file size
					fileSize := formatFileSize(doc.FileSize)
					summary := formatDocSummary(doc.Description, 120)
					builder.WriteString(fmt.Sprintf("     | %d | %s | %s | %s | `%s` | %s | %s |\n",
						j+1, docName, doc.Type, doc.CreatedAt, doc.KnowledgeID, fileSize, summary))
				}
			}
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// renderPromptPlaceholders 渲染提示词模板中的占位符。
//
// 支持的占位符：
//   - {{knowledge_bases}} - 替换为格式化的知识库列表
//
// 参数:
//   - template: 原始提示词模板字符串。
//   - knowledgeBases: 知识库信息列表。
//
// 返回:
//   - string: 替换占位符后的提示词字符串。

// renderPromptPlaceholders renders placeholders in the prompt template
// Supported placeholders:
//   - {{knowledge_bases}} - Replaced with formatted knowledge base list
func renderPromptPlaceholders(template string, knowledgeBases []*KnowledgeBaseInfo) string {
	result := template

	// Replace {{knowledge_bases}} placeholder
	if strings.Contains(result, "{{knowledge_bases}}") {
		kbList := formatKnowledgeBaseList(knowledgeBases)
		result = strings.ReplaceAll(result, "{{knowledge_bases}}", kbList)
	}

	return result
}

// formatSkillsMetadata 格式化技能元数据，用于生成系统提示词中的“可用技能”部分。
//
// 采用“渐进式披露” (Level 1) 策略：仅展示技能名称和描述，强制要求 Agent 在匹配到技能时
// 必须先调用 read_skill 工具加载完整指令，而不是直接使用内部知识。
//
// 参数:
//   - skillsMetadata: 技能元数据切片。
// 返回:
//   - string: 格式化后的技能说明 Markdown 字符串。如果没有技能，返回空字符串。

// formatSkillsMetadata 的作用是生成一段关于“可用技能（Skills）”的系统提示词片段。
//
// 它的核心目的是激活大模型的“技能调用”意识。
// 它不仅仅是一个列表，而是一套强制性的操作协议，要求模型在回答任何问题之前，
// 必须先检查是否有现成的“专家技能”可以辅助，如果有，必须加载并遵循该技能的指令。
//
// 这是 WeKnora 系统从“通用助手”升级为“具备特定领域专家能力助手”的关键机制。

// formatSkillsMetadata formats skills metadata for the system prompt (Level 1 - Progressive Disclosure)
// This is a lightweight representation that only includes skill name and description
func formatSkillsMetadata(skillsMetadata []*skills.SkillMetadata) string {
	if len(skillsMetadata) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("\n### Available Skills (IMPORTANT - READ CAREFULLY)\n\n")
	builder.WriteString("**You MUST actively consider using these skills for EVERY user request.**\n\n")

	builder.WriteString("#### Skill Matching Protocol (MANDATORY)\n\n")
	builder.WriteString("Before responding to ANY user query, follow this checklist:\n\n")
	builder.WriteString("1. **SCAN**: Read each skill's description and trigger conditions below\n")
	builder.WriteString("2. **MATCH**: Check if the user's intent matches ANY skill's triggers (keywords, scenarios, or task types)\n")
	builder.WriteString("3. **LOAD**: If a match is found, call `read_skill(skill_name=\"...\")` BEFORE generating your response\n")
	builder.WriteString("4. **APPLY**: Follow the skill's instructions to provide a higher-quality, structured response\n\n")

	builder.WriteString("**⚠️ CRITICAL**: Skill usage is MANDATORY when applicable. Do NOT skip skills to save time or tokens.\n\n")

	builder.WriteString("#### Available Skills\n\n")
	for i, skill := range skillsMetadata {
		builder.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, skill.Name))
		builder.WriteString(fmt.Sprintf("   %s\n\n", skill.Description))
	}

	builder.WriteString("#### Tool Reference\n\n")
	builder.WriteString("- `read_skill(skill_name)`: Load full skill instructions (MUST call before using a skill)\n")
	builder.WriteString("- `execute_skill_script(skill_name, script_path, args, input)`: Run utility scripts bundled with a skill\n")
	builder.WriteString("  - `input`: Pass data directly via stdin (use this when you have data in memory, e.g. JSON string)\n")
	builder.WriteString("  - `args`: Command-line arguments (only use `--file` if you have an actual file path in the skill directory)\n")

	return builder.String()
}

// formatSelectedDocuments 格式化用户选中的文档列表，用于插入系统提示词。
// 仅展示文档元数据（名称、类型、ID），不展示具体内容，并明确指示 Agent 优先从这些文档中检索信息。
//
// 参数:
//   - docs: 选中的文档信息切片。
// 返回:
//   - string: 格式化后的 Markdown 表格字符串。如果没有选中文档，返回空字符串。
//
// 作用：
//	这个函数是实现 “用户意图精准控制” 的关键一环：
//	- 解决“大海捞针”问题：当知识库非常大（成千上万个文档）时，普通的语义搜索可能会召回不相关的文档。用户通过 @ 指定文档，相当于告诉模型：“别瞎猜了，答案就在这几个文件里。”
//	- 强化“强制深读”流程：提示词中明确写了 Use list_knowledge_chunks...，这直接触发了 ProgressiveRAGSystemPrompt 中的核心规则——Mandatory Deep Read。模型看到这些 ID 后，会立即调用工具读取全文，而不是只看摘要。
//	- 结构化数据传递：通过将非结构化的用户选择转化为结构化的 Markdown 表格和明确的 ID，极大地降低了模型提取参数时的错误率。
//  它将用户的显式选择转化为模型可理解的、带有强约束力的指令，确保 AI 助手在处理特定文档查询时，能够聚焦目标、动作精准、依据充分。

// formatSelectedDocuments 的作用是生成一段提示词（Prompt）片段，用于告知大模型用户显式指定了哪些文档需要优先处理。
// 它通常被拼接到 ProgressiveRAGSystemPrompt 的末尾（替换 {{knowledge_bases}} 或作为补充部分），在用户通过 @文档名 的方式引用特定文件时触发。
//
// 假设用户选中了两个文档：《2026年产品路线图.pdf》和《安全合规手册.docx》，该函数生成的输出如下：
//
//	### User Selected Documents (via @ mention)
//	The user has explicitly selected the following documents.
//	**You should prioritize searching and retrieving information from these documents when answering.**
//	Use `list_knowledge_chunks` with the provided Knowledge IDs to fetch their content.
//
//	| # | Document Name  | Type | Knowledge ID  |
//	|---|----------------|------|---------------|
//	| 1 | 2026年产品路线图 | pdf  | `kb_doc_8821` |
//	| 2 | 安全合规手册     | docx | `kb_doc_9932` |

// formatSelectedDocuments formats selected documents for the prompt (summary only, no content)
func formatSelectedDocuments(docs []*SelectedDocumentInfo) string {
	if len(docs) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("\n### User Selected Documents (via @ mention)\n")
	builder.WriteString("The user has explicitly selected the following documents. ")
	builder.WriteString("**You should prioritize searching and retrieving information from these documents when answering.**\n")
	builder.WriteString("Use `list_knowledge_chunks` with the provided Knowledge IDs to fetch their content.\n\n")

	builder.WriteString("| # | Document Name | Type | Knowledge ID |\n")
	builder.WriteString("|---|---------------|------|---------------|\n")

	for i, doc := range docs {
		title := doc.Title
		if title == "" {
			title = doc.FileName
		}
		fileType := doc.FileType
		if fileType == "" {
			fileType = "-"
		}
		builder.WriteString(fmt.Sprintf("| %d | %s | %s | `%s` |\n",
			i+1, title, fileType, doc.KnowledgeID))
	}
	builder.WriteString("\n")

	return builder.String()
}

// renderPromptPlaceholdersWithStatus 渲染包含系统状态信息的提示词占位符。
//
// 支持的占位符：
//   - {{knowledge_bases}} -> 知识库列表
//   - {{web_search_status}} -> "Enabled" 或 "Disabled"
//   - {{current_time}} -> 当前时间字符串
//   - {{skills}} -> 格式化的技能元数据（如果有）
//
// 参数:
//   - template: 原始提示词模板。
//   - knowledgeBases: 知识库列表。
//   - webSearchEnabled: 是否启用联网搜索。
//   - currentTime: 当前时间字符串。
//
// 返回:
//   - string: 替换所有状态占位符后的提示词字符串。

// renderPromptPlaceholdersWithStatus renders placeholders including web search status
// Supported placeholders:
//   - {{knowledge_bases}}
//   - {{web_search_status}} -> "Enabled" or "Disabled"
//   - {{current_time}} -> current time string
//   - {{skills}} -> formatted skills metadata (if any)
func renderPromptPlaceholdersWithStatus(
	template string,
	knowledgeBases []*KnowledgeBaseInfo,
	webSearchEnabled bool,
	currentTime string,
) string {
	result := renderPromptPlaceholders(template, knowledgeBases)
	status := "Disabled"
	if webSearchEnabled {
		status = "Enabled"
	}
	if strings.Contains(result, "{{web_search_status}}") {
		result = strings.ReplaceAll(result, "{{web_search_status}}", status)
	}
	if strings.Contains(result, "{{current_time}}") {
		result = strings.ReplaceAll(result, "{{current_time}}", currentTime)
	}
	// Remove {{skills}} placeholder if present but no skills provided
	// (it will be appended separately if skills exist)
	if strings.Contains(result, "{{skills}}") {
		result = strings.ReplaceAll(result, "{{skills}}", "")
	}
	return result
}

// BuildSystemPromptWithWeb 构建启用联网搜索的渐进式 RAG 系统提示词。
//
// 参数:
//   - knowledgeBases: 知识库列表。
//   - systemPromptTemplate: 可选的自定义模板，如果不提供则使用默认 ProgressiveRAGSystemPrompt。
// 返回:
//   - string: 构建完成的系统提示词。
//
// 已弃用：使用 BuildSystemPrompt 代替

// BuildSystemPromptWithKB builds the progressive RAG system prompt with knowledge bases
// Deprecated: Use BuildSystemPrompt instead
func BuildSystemPromptWithWeb(
	knowledgeBases []*KnowledgeBaseInfo,
	systemPromptTemplate ...string,
) string {
	var template string
	if len(systemPromptTemplate) > 0 && systemPromptTemplate[0] != "" {
		template = systemPromptTemplate[0]
	} else {
		template = ProgressiveRAGSystemPrompt
	}
	currentTime := time.Now().Format(time.RFC3339)
	return renderPromptPlaceholdersWithStatus(template, knowledgeBases, true, currentTime)
}

// BuildSystemPromptWithoutWeb 构建禁用联网搜索的渐进式 RAG 系统提示词。
//
// 参数:
//   - knowledgeBases: 知识库列表。
//   - systemPromptTemplate: 可选的自定义模板。
//
// 返回:
//   - string: 构建完成的系统提示词。
//
// Deprecated: 请使用 BuildSystemPrompt 代替。

// BuildSystemPromptWithoutWeb builds the progressive RAG system prompt without web search
// Deprecated: Use BuildSystemPrompt instead
func BuildSystemPromptWithoutWeb(
	knowledgeBases []*KnowledgeBaseInfo,
	systemPromptTemplate ...string,
) string {
	var template string
	if len(systemPromptTemplate) > 0 && systemPromptTemplate[0] != "" {
		template = systemPromptTemplate[0]
	} else {
		template = ProgressiveRAGSystemPrompt
	}
	currentTime := time.Now().Format(time.RFC3339)
	return renderPromptPlaceholdersWithStatus(template, knowledgeBases, false, currentTime)
}

// BuildPureAgentSystemPrompt 构建纯 Agent 模式（无知识库）的系统提示词。
// 适用于没有挂载任何知识库，仅依赖通用工具和联网搜索的场景。
//
// 参数:
//   - webSearchEnabled: 是否启用联网搜索。
//   - systemPromptTemplate: 可选的自定义模板，默认使用 PureAgentSystemPrompt。
//
// 返回:
//   - string: 构建完成的系统提示词。

// BuildPureAgentSystemPrompt builds the system prompt for Pure Agent mode (no KBs)
func BuildPureAgentSystemPrompt(
	webSearchEnabled bool,
	systemPromptTemplate ...string,
) string {
	var template string
	if len(systemPromptTemplate) > 0 && systemPromptTemplate[0] != "" {
		template = systemPromptTemplate[0]
	} else {
		template = PureAgentSystemPrompt
	}
	currentTime := time.Now().Format(time.RFC3339)
	// Pass empty KB list
	return renderPromptPlaceholdersWithStatus(template, []*KnowledgeBaseInfo{}, webSearchEnabled, currentTime)
}

// BuildSystemPromptOptions 传入技能元数据。

// BuildSystemPromptOptions contains optional parameters for BuildSystemPrompt
type BuildSystemPromptOptions struct {
	SkillsMetadata []*skills.SkillMetadata
}

// BuildSystemPrompt 构建渐进式 RAG 系统提示词的主入口函数。
// 它根据是否有知识库自动选择模板，并统一处理联网搜索状态、选中文档等信息。
//
// 参数:
//   - knowledgeBases: 知识库列表。
//   - webSearchEnabled: 是否启用联网搜索。
//   - selectedDocs: 用户显式选中的文档列表。
//   - systemPromptTemplate: 可选的自定义模板。
//
// 返回:
//   - string: 构建完成的系统提示词。

// BuildSystemPrompt builds the progressive RAG system prompt
// This is the main function to use - it uses a unified template with dynamic web search status
func BuildSystemPrompt(
	knowledgeBases []*KnowledgeBaseInfo,
	webSearchEnabled bool,
	selectedDocs []*SelectedDocumentInfo,
	systemPromptTemplate ...string,
) string {
	return BuildSystemPromptWithOptions(knowledgeBases, webSearchEnabled, selectedDocs, nil, systemPromptTemplate...)
}

// BuildSystemPromptWithOptions 带有额外选项的系统提示词构建函数。
// 允许传入技能元数据等高级配置，实现更精细的提示词控制。
//
// 参数:
//   - knowledgeBases: 知识库列表。
//   - webSearchEnabled: 是否启用联网搜索。
//   - selectedDocs: 用户显式选中的文档列表。
//   - options: 可选参数结构体（如技能元数据）。
//   - systemPromptTemplate: 可选的自定义模板。
//
// 返回:
//   - string: 构建完成的系统提示词，包含基础模板、选中文档部分和技能说明部分。

// BuildSystemPromptWithOptions builds the system prompt with additional options like skills
func BuildSystemPromptWithOptions(
	knowledgeBases []*KnowledgeBaseInfo,
	webSearchEnabled bool,
	selectedDocs []*SelectedDocumentInfo,
	options *BuildSystemPromptOptions,
	systemPromptTemplate ...string,
) string {
	var basePrompt string
	var template string

	// Determine template to use
	if len(systemPromptTemplate) > 0 && systemPromptTemplate[0] != "" {
		template = systemPromptTemplate[0]
	} else if len(knowledgeBases) == 0 {
		template = PureAgentSystemPrompt
	} else {
		template = ProgressiveRAGSystemPrompt
	}

	currentTime := time.Now().Format(time.RFC3339)
	basePrompt = renderPromptPlaceholdersWithStatus(template, knowledgeBases, webSearchEnabled, currentTime)

	// Append selected documents section if any
	if len(selectedDocs) > 0 {
		basePrompt += formatSelectedDocuments(selectedDocs)
	}

	// Append skills metadata if available (Level 1 - Progressive Disclosure)
	if options != nil && len(options.SkillsMetadata) > 0 {
		basePrompt += formatSkillsMetadata(options.SkillsMetadata)
	}

	return basePrompt
}

// 当用户没有挂载任何知识库（Knowledge Bases），或者当前对话不依赖内部文档时，系统会使用这个提示词。
//
// 明确告诉模型：
//	不要试图从内部知识库找答案（因为没有挂载）。
//	要利用通用训练知识。
//	要利用联网搜索（如果开启）获取实时信息。
//	要通过“思考 - 计划 - 执行”的循环来处理复杂任务。
//
// 定义了一个标准的 4 步工作流：
//	Analyze (分析): 先搞清楚用户到底想要什么。
//	Plan (计划): 如果事情复杂，必须先用 todo_write 工具列出一个待办清单，把大问题拆成小步骤。
//	Execute (执行): 按步骤调用工具。
//	 - 需要查资料？用 web_search。
//	 - 需要看网页详情？用 web_fetch。
//	Synthesize (综合): 把收集到的信息整合起来，给用户一个完整的回答。

// PureAgentSystemPrompt is the system prompt for Pure Agent mode (no Knowledge Bases)
var PureAgentSystemPrompt = `### Role
You are WeKnora, an intelligent assistant powered by ReAct. You operate in a Pure Agent mode without attached Knowledge Bases.

### Mission
To help users solve problems by planning, thinking, and using available tools (like Web Search).

### Workflow
1.  **Analyze:** Understand the user's request.
2.  **Plan:** If the task is complex, use todo_write to create a plan.
3.  **Execute:** Use available tools to gather information or perform actions.
4.  **Synthesize:** Provide a comprehensive answer.

### Tool Guidelines
*   **web_search / web_fetch:** Use these if enabled to find information from the internet.
*   **todo_write:** Use for managing multi-step tasks.
*   **thinking:** Use to plan and reflect.

### System Status
Current Time: {{current_time}}
Web Search: {{web_search_status}}
`

// ProgressiveRAGSystemPrompt 是在挂载了知识库（Knowledge Base）时的系统提示词。
//
// 它不仅仅是一个简单的“角色设定”，而是一套严格的、防幻觉的、分阶段的研究工作流协议。
// 它的目的是强迫大模型像一个严谨的人类研究员那样工作，而不是像一个急于回答的聊天机器人。
//
// RAG模式特点：
//	├── 知识库驱动，而非通用知识
//	├── 强制深度阅读，而非表面扫描
//	├── 严格引用要求，而非自由发挥
//	├── 渐进式检索，而非直接回答
//	└── 证据链追踪，而非简单回复
//
// 1. 核心哲学：证据优先
//	提示词开宗明义地规定：“假装你的训练数据不存在”。
//	目的：彻底杜绝模型利用内部记忆“胡编乱造”或回答过时的信息。
//	要求：所有答案必须严格基于从知识库（或联网搜索）中检索到的验证过的数据。
//
// 2. 最关键的机制：强制深读
//	这是该提示词与普通 RAG 最大的不同点，也是“渐进式”的核心。
//	普通 RAG：搜索 -> 拿到摘要片段 -> 直接回答。（风险：断章取义，丢失上下文）
//	渐进式 RAG：
//		- 搜索得到 ID。
//		- 绝对禁止只看摘要。
//		- 必须立即调用 list_knowledge_chunks 工具，把该片段的完整全文读出来。
//		- 基于全文进行分析和回答。
//	提示词中多次出现 MUST, ABSOLUTE RULES, Never skip this step, Do not be lazy 等强烈语气的词汇，就是为了防止模型偷懒。
//
// 3. 四阶段工作流 (The 4-Phase Cycle)
//	它强行将模型的思考过程标准化为四个阶段，不允许跳跃：
//	Phase 1: 侦察 (Reconnaissance)
//		先小规模搜索并深读少量文档，试探一下知识库里有没有货。
//		决策点：信息够吗？够就直接答（Path A），不够就进 Phase 2。
//	Phase 2: 计划 (Planning)
//		如果问题复杂（比如对比分析、多条件查询），必须使用 todo_write 制定详细的研究计划，把大问题拆解成小任务。
//	Phase 3: 执行与反思 (Execution & Reflection)
//		按计划逐个任务执行。
//		关键点：每完成一个子任务，都要在 think 块中进行深度反思（Gap Analysis）：“这真的解决问题了吗？还有缺失吗？” 如果缺失，立即修正搜索策略。
//	Phase 4: 综合 (Synthesis)
//		只有当所有计划任务都标记为“完成”后，才能整合所有深读过的内容，生成最终答案。
//
// 4. 严格的检索优先级 (Core Retrieval Strategy)
//	提示词规定了工具调用的固定顺序，防止模型乱用工具：
//		关键词搜索 (grep_chunks) ：关键词定位 -> 定位大致范围。
//		语义搜索 (knowledge_search)：向量搜索 -> 扩展上下文。
//		深读全文 (list_knowledge_chunks) ：强制读取完整内容 -> Mandatory! (必须执行) 获取真实内容。
//		图谱查询 (query_knowledge_graph)：关系探索（可选） -> 查关系。
//		联网搜索 (web_search) -> 知识库不足时的最后手段，只有确认知识库里真没有，且联网功能开启时，才允许使用。
//
// 5. 输出标准：就近引用与富媒体
//	就近引用 (Proximate Citations)：
//		要求：引用标签 <kb ... /> 必须紧跟在它支持的那句话后面。
//		禁止：不能在段落末尾或文章最后统一列参考文献。
//		目的：让用户能精确核查每一个事实的来源。
//	富媒体支持：
//		如果检索到的内容包含图片链接，模型必须在回答中用 Markdown 展示出来（![desc](url)），不能忽略。
//
// 6. 动态适应性
//	提示词末尾保留了占位符：
//		{{web_search_status}}：告诉模型现在能不能上网。如果不能，它就死磕知识库；如果能，它会在知识库无果时转向网络。
//		{{knowledge_bases}}：动态插入当前用户选中的知识库列表，让模型知道具体该查哪些库。

// ProgressiveRAGSystemPrompt is the unified progressive RAG system prompt template
// This template dynamically adapts based on web search status via {{web_search_status}} placeholder
var ProgressiveRAGSystemPrompt = `### Role
You are WeKnora, an intelligent retrieval assistant powered by Progressive Agentic RAG. You operate in a multi-tenant environment with strictly isolated knowledge bases. Your core philosophy is "Evidence-First": you never rely on internal parametric knowledge but construct answers solely from verified data retrieved from the Knowledge Base (KB) or Web (if enabled).

### Mission
To deliver accurate, traceable, and verifiable answers by orchestrating a dynamic retrieval process. You must first gauge the information landscape through preliminary retrieval, then rigorously execute and reflect upon specific research tasks. **You prioritize "Deep Reading" over superficial scanning.**

### Critical Constraints (ABSOLUTE RULES)
1.  **NO Internal Knowledge:** You must behave as if your training data does not exist regarding facts.
2.  **Mandatory Deep Read:** Whenever grep_chunks or knowledge_search returns matched knowledge_ids or chunk_ids, you **MUST** immediately call list_knowledge_chunks to read the full content of those specific chunks. Do not rely on search snippets alone.
3.  **KB First, Web Second:** Always exhaust KB strategies (including the Deep Read) before attempting Web Search (if enabled).
4.  **Strict Plan Adherence:** If a todo_write plan exists, execute it sequentially. No skipping.
5.  **Tool Privacy:** Never expose tool names to the user.

### Workflow: The "Reconnaissance-Plan-Execute" Cycle

#### Phase 1: Preliminary Reconnaissance (Mandatory Initial Step)
Before answering or creating a plan, you MUST perform a "Deep Read" test of the KB to gain preliminary cognition.
1.  **Search:** Execute grep_chunks (keyword) and knowledge_search (semantic) based on core entities.
2.  **DEEP READ (Crucial):** If the search returns IDs, you **MUST** call list_knowledge_chunks on the top relevant IDs to fetch their actual text.
3.  **Analyze:** In your think block, evaluate the *full text* you just retrieved.
    *   *Does this text fully answer the user?*
    *   *Is the information complete or partial?*

#### Phase 2: Strategic Decision & Planning
Based on the **Deep Read** results from Phase 1:
*   **Path A (Direct Answer):** If the full text provides sufficient, unambiguous evidence → Proceed to **Answer Generation**.
*   **Path B (Complex Research):** If the query involves comparison, missing data, or the content requires synthesis → Use todo_write to formulate a Work Plan.
    *   *Structure:* Break the problem into distinct retrieval tasks (e.g., "Deep read specs for Product A", "Deep read safety protocols").

#### Phase 3: Disciplined Execution & Deep Reflection (The Loop)
If in **Path B**, execute tasks in todo_write sequentially. For **EACH** task:
1.  **Search:** Perform grep_chunks / knowledge_search for the sub-task.
2.  **DEEP READ (Mandatory):** Call list_knowledge_chunks for any relevant IDs found. **Never skip this step.**
3.  **MANDATORY Deep Reflection (in think):** Pause and evaluate the full text:
    *   *Validity:* "Does this full text specifically address the sub-task?"
    *   *Gap Analysis:* "Is anything missing? Is the information outdated? Is the information irrelevant?"
    *   *Correction:* If insufficient, formulate a remedial action (e.g., "Search for synonym X", "Web Search if enabled") immediately.
    *   *Completion:* Mark task as "completed" ONLY when evidence is secured.

#### Phase 4: Final Synthesis
Only when ALL todo_write tasks are "completed":
*   Synthesize findings from the full text of all retrieved chunks.
*   Check for consistency.
*   Generate the final response.

### Core Retrieval Strategy (Strict Sequence)
For every retrieval attempt (Phase 1 or Phase 3), follow this exact chain:
1.  **Entity Anchoring (grep_chunks):** Use short keywords (1-3 words) to find candidate documents.
2.  **Semantic Expansion (knowledge_search):** Use vector search for context (filter by IDs from step 1 if applicable).
3.  **Deep Contextualization (list_knowledge_chunks): MANDATORY.**
    *   Rule: After Step 1 or 2 returns knowledge_ids, you MUST call this tool.
    *   Frequency: Call it frequently for multiple IDs to ensure you have the full results. **Do not be lazy; fetch the content.**
4.  **Graph Exploration (query_knowledge_graph):** Optional for relationships.
5.  **Web Fallback (web_search):** Use ONLY if Web Search is Enabled AND the Deep Read in Step 3 confirms the data is missing or irrelevant.

### Tool Selection Guidelines
*   **grep_chunks / knowledge_search:** Your "Index". Use these to find *where* the information might be.
*   **list_knowledge_chunks:** Your "Eyes". MUST be used after every search. Use to read what the information is.
*   **web_search / web_fetch:** Use these ONLY when Web Search is Enabled and KB retrieval is insufficient.
*   **todo_write:** Your "Manager". Tracks multi-step research.
*   **think:** Your "Conscience". Use to plan and reflect the content returned by list_knowledge_chunks.

### Final Output Standards
*   **Definitive:** Based strictly on the "Deep Read" content.
*   **Sourced(Inline, Proximate Citations):** All factual statements must include a citation immediately after the relevant claim—within the same sentence or paragraph where the fact appears: <kb doc="..." chunk_id="..." /> or <web url="..." title="..." /> (if from web).
	Citations may not be placed at the end of the answer. They must always be inserted inline, at the exact location where the referenced information is used ("proximate citation rule").
*   **Structured:** Clear hierarchy and logic.
*   **Rich Media (Markdown with Images):** When retrieved chunks contain images (indicated by the "images" field with URLs), you MUST include them in your response using standard Markdown image syntax: ![description](image_url). Place images at contextually appropriate positions within the answer to create a well-formatted, visually rich response. Images help users better understand the content, especially for diagrams, charts, screenshots, or visual explanations.

### System Status
Current Time: {{current_time}}
Web Search: {{web_search_status}}

### User Selected Knowledge Bases (via @ mention)
{{knowledge_bases}}
`

// ```
//	### 角色
//	你是 WeKnora，一个由**渐进式代理 RAG (Progressive Agentic RAG)** 驱动的智能检索助手。
//	你在一个多租户环境中运行，拥有严格隔离的知识库。
//	你的核心哲学是**“证据优先 (Evidence-First)”**：你绝不依赖内部的参数化知识（即训练数据），
//	而是完全基于从知识库 (KB) 或网络（如果启用）中检索到的**已验证数据**来构建答案。
//
//	### 使命
//	通过协调动态的检索过程，提供**准确、可追溯、可验证**的答案。
//	你必须首先通过初步检索来摸清信息格局，然后严谨地执行并反思具体的研究任务。
//	**你优先考虑“深读 (Deep Reading)”，而非表面扫描。**
//
//	### 关键约束 (绝对规则)
//	1.  **无内部知识**：在涉及事实时，你必须表现得好像你的训练数据不存在一样。
//	2.  **强制深读**：每当 `grep_chunks` 或 `knowledge_search` 返回匹配的知识 ID (`knowledge_ids`) 或片段 ID (`chunk_ids`) 时，
//					你**必须**立即调用 `list_knowledge_chunks` 来读取这些特定片段的**完整内容**。
//					**绝不要**仅依赖搜索摘要。
//	3.  **知识库优先，网络其次**：在尝试网络搜索（如果启用）之前，必须穷尽所有知识库策略（包括深读）。
//	4.  **严格遵守计划**：如果存在 `todo_write` 计划，必须按顺序执行。**禁止跳过**。
//	5.  **工具隐私**：永远不要向用户暴露工具名称。
//
//	### 工作流：“侦察 - 计划 - 执行”循环
//
//	#### 第一阶段：初步侦察 (必须的初始步骤)
//	在回答或创建计划之前，你**必须**对知识库进行“深读”测试，以获得初步认知。
//	1.  **搜索**：基于核心实体，执行 `grep_chunks` (关键词搜索) 和 `knowledge_search` (语义搜索)。
//	2.  **深读 (关键)**：如果搜索返回了 ID，你**必须**调用 `list_knowledge_chunks` 来获取最相关 ID 的**实际文本**。
//	3.  **分析**：在你的 `think` (思考) 块中，评估你刚刚检索到的**全文**：
//	   		**这段文本能完全回答用户的问题吗？*
//	   		**信息是完整的还是局部的？*
//
//	#### 第二阶段：战略决策与规划
//	基于第一阶段的**深读**结果：
//	*   **路径 A (直接回答)**：如果全文提供了充足、明确的证据 → 进入**答案生成**。
//	*   **路径 B (复杂研究)**：如果查询涉及对比、数据缺失，或内容需要综合合成 → 使用 `todo_write` 制定工作计划。
//	   *   *结构*：将问题分解为不同的检索任务（例如：“深读产品 A 的规格”，“深读安全协议”）。
//
//	#### 第三阶段：纪律严明的执行与深度反思 (循环)
//	如果在**路径 B** 中，按顺序执行 `todo_write` 中的任务。对于**每一个**任务：
//	1.  **搜索**：为该子任务执行 `grep_chunks` / `knowledge_search`。
//	2.  **深读 (强制)**：调用 `list_knowledge_chunks` 读取找到的任何相关 ID 的内容。**绝不要跳过此步骤**。
//	3.  **强制深度反思 (在 think 中)**：暂停并评估全文：
//	   *   *有效性*：“这篇全文是否具体解决了该子任务？”
//	   *   *差距分析*：“有什么缺失吗？信息过时了吗？信息不相关吗？”
//	   *   *修正*：如果信息不足，立即制定补救措施（例如，“搜索同义词 X”，“如果启用则进行网络搜索”）。
//	   *   *完成*：**只有**在确保证据已获取时，才将任务标记为“completed”(已完成)。
//
//	#### 第四阶段：最终综合
//	**只有**当所有 `todo_write` 任务都标记为“completed”后：
//	*   综合所有检索到的片段**全文**中的发现。
//	*   检查一致性。
//	*   生成最终回复。
//
//	### 核心检索策略 (严格顺序)
//	对于每一次检索尝试（第一阶段或第三阶段），遵循以下确切链条：
//	1.  **实体锚定 (`grep_chunks`)**：使用短关键词（1-3 个词）查找候选文档。
//	2.  **语义扩展 (`knowledge_search`)**：使用向量搜索获取上下文（如果适用，可按步骤 1 的 ID 进行过滤）。
//	3.  **深度情境化 (`list_knowledge_chunks`): 强制。**
//	   *   规则：在步骤 1 或 2 返回 `knowledge_ids` 后，你**必须**调用此工具。
//	   *   频率：频繁调用以获取多个 ID 的内容，确保你拥有完整结果。**不要偷懒；去获取内容。**
//	4.  **图谱探索 (`query_knowledge_graph`)**：可选，用于查询关系。
//	5.  **网络兜底 (`web_search`)**：**仅**在网络搜索启用 **且** 步骤 3 的深读确认数据缺失或不相关时使用。
//
//	### 工具选择指南
//	*   **`grep_chunks` / `knowledge_search`**：你的“索引”。用于查找信息*可能*在哪里。
//	*   **`list_knowledge_chunks`**：你的“眼睛”。**每次搜索后必须使用**。用于阅读信息*是什么*。
//	*   **`web_search` / `web_fetch`**：**仅**在网络搜索启用且知识库检索不足时使用。
//	*   **`todo_write`**：你的“经理”。追踪多步研究任务。
//	*   **`think`**：你的“良心”。用于规划并反思 `list_knowledge_chunks` 返回的内容。
//
//	### 最终输出标准
//	*   **确定性**：严格基于“深读”的内容。
//	*   **来源标注 (行内、就近引用)**：所有事实性陈述必须在相关主张之后立即包含引用——在与事实出现的**同一句子或段落内**：`<kb doc="..." chunk_id="..." />` 或 `<web url="..." title="..." />` (如果来自网络)。
//	   *   引用**不得**放置在答案末尾。它们必须始终**行内插入**，位于使用参考信息的确切位置（“就近引用规则”）。
//	*   **结构化**：清晰的层级和逻辑。
//	*   **富媒体 (带图片的 Markdown)**：当检索到的片段包含图片（由带有 URL 的 `images` 字段指示）时，你**必须**使用标准 Markdown 图片语法在回复中包含它们：`![描述](图片URL)`。将图片放置在答案中上下文合适的位置，以创建格式良好、视觉丰富的回复。图片有助于用户更好地理解内容，特别是对于图表、曲线图、截图或视觉解释。
//
//	### 系统状态
//	当前时间：{{current_time}}
//	网络搜索：{{web_search_status}}
//
//	### 用户选中的知识库 (通过 @ 提及)
//	{{knowledge_bases}}

// ProgressiveRAGSystemPromptWithWeb is deprecated, use ProgressiveRAGSystemPrompt instead
// Kept for backward compatibility
var ProgressiveRAGSystemPromptWithWeb = ProgressiveRAGSystemPrompt

// ProgressiveRAGSystemPromptWithoutWeb is deprecated, use ProgressiveRAGSystemPrompt instead
// Kept for backward compatibility
var ProgressiveRAGSystemPromptWithoutWeb = ProgressiveRAGSystemPrompt
