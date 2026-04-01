package chatpipline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// 将搜索、合并、重排序阶段得到的结构化搜索结果（SearchResult），按照特定的模板和业务逻辑，转化为大模型（LLM）能够理解的提示词（Prompt）。

// PluginIntoChatMessage handles the transformation of search results into chat messages
type PluginIntoChatMessage struct{}

// NewPluginIntoChatMessage creates and registers a new PluginIntoChatMessage instance
func NewPluginIntoChatMessage(eventManager *EventManager) *PluginIntoChatMessage {
	res := &PluginIntoChatMessage{}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginIntoChatMessage) ActivationEvents() []types.EventType {
	return []types.EventType{types.INTO_CHAT_MESSAGE}
}

// OnEvent 处理 INTO_CHAT_MESSAGE 事件，将检索到的原始结果集格式化为 LLM 可理解的 Prompt 上下文。
//
// 该函数是 RAG 流程中连接“检索”与“生成”的关键桥梁，主要执行以下核心逻辑：
// 1. 安全校验：调用 utils.ValidateInput 验证用户查询的合法性，防止恶意输入。
// 2. 策略分流（FAQ 优先）：若开启 FAQ 优先模式，将检索结果分离为 FAQ 和文档两类 ，并根据置信度阈值标记“精准匹配” FAQ ，引导模型优先参考。
// 3. 内容增强：调用 getEnrichedPassageForChat 融合文本内容与图片的 OCR/描述信息。
// 4. 模板填充：将用户查询 {{query}} 、构建好的上下文 {{contexts}} 、当前时间 {{current_time}} 等信息填入预设模板，生成最终 UserContent。
// 5. 结果写回：将组装好的最终文本存入 chatManage.UserContent，供后续模型生成环节使用。
//
// 参数:
//   - ctx: 用于链路追踪与日志记录。
//   - eventType: 事件类型，此处固定为 INTO_CHAT_MESSAGE。
//   - chatManage: 聊天管理对象，包含原始检索结果、配置参数，并承载最终生成的 UserContent。
//   - next: 链式调用函数，用于执行流水线中的下一个插件。
//
// 返回:
//   - *PluginError: 若输入校验失败返回错误，成功则返回 next() 的执行结果。

// 当一篇文档（比如 PDF 或 网页）被存入数据库时，后端程序会做两件事：
//	- 提取文字：存入 Content 字段。
//	- 处理图片：如果文档里有图片，程序会跑一遍 OCR（文字识别）和 Caption（AI 描述），
//		然后把这些结构化信息（URL、描述、文字）存入该文档片段的 ImageInfo 字段。
//
// 当 Agent 去数据库里检索相关内容时，后台返回的数据结构是这样的：
// 	{
//  	"id": "DOC-2",
//  	"content": "访客请在正门保安处进行登记。",
//  	"imageInfo": "[{\"url\": \"qr_code.png\", \"caption\": \"正门访客登记二维码\", \"ocr\": \"扫码登记访客信息\"}]"
//	}
//
// 可以看到，这里的 content 没有图片链接，但是在 imageInfo 中关联了一张图片 qr_code.png，以及它的描述。
//
// 造成这种现象的原因主要是：
//
// 	1. 文档拆分（Chunking）导致的断裂
//		这是最常见的原因。
//		背景：长文档在存入数据库前会被切分成多个小段（Chunk）。
//		现象：第一段话提到了“如下图所示”，而图片链接 ![](...) 恰好被切到了第二段。
//		结果：当用户提问命中第一段时，检索回来的 content 只有文字，但数据库记录的该段落关联的元数据中仍然带着这张图片的信息。
//
// 	2. 非标准格式转换（Parser Issues）
//		从原始格式（PDF, Word, PPT）转换为 Markdown 时，解析器并不总是完美的。
//		场景：PDF 中的图片是浮动布局，或者是在表格内部。
//		结果：解析工具提取出了图片并做了 OCR，但它无法确定这张图片在 Markdown 文本中的精准位置，因此不敢乱插 ![]() 链接，以免破坏阅读流。
//		策略：为了保险，解析器会把这张图作为“文档附件”或“片段属性”存储在 imageInfo 结构体中，而不是强行塞进 content。
//
// 	3. 多模态数据的“元数据挂载”
//		在某些高级检索系统中，图片信息是作为增强属性存在的。
//		场景：一个关于“公司前台”的百科页面，正文只写了“公司前台位于一楼大厅”。
//		结果：后台系统可能在入库时，自动将“前台实景图”、“正门方位图”等图片关联到了这个文本片段上。
//		逻辑：这些图片并不是正文的一部分，而是补充素材，因此它们只存在于 imageInfo 这种结构化字段里。
//
// 本插件在拿到上面的数据后，会执行以下逻辑：
//	第一步：找链接
//		代码用正则表达式去 content（"访客请在正门保安处进行登记。"）里找 ![](...) 这种图片链接。
//		结果：没找到。因为正文中根本没写图片链接。
//	第二步：进兜底逻辑
//		代码发现 imageInfo 里有一张 qr_code.png，但是刚才在正文里又没见到这张图。
//		判定：这张图属于“有数据但没在文中展示”的遗漏图片。
//	第三步：追加到文末
//		为了不浪费这张图的信息，代码把它放到了文本的最末尾。

// 示例一：优先级模式 (多文档匹配示例)
//	假设开启了 FAQ 优先级，且检索到 2 条 FAQ（其中第 1 条为高置信度）和 3 条参考文档，最终生成的 userContent 示例如下：
//
//	Markdown
//
// 	### 资料来源 1：标准问答库 (FAQ)
//	【高置信度 - 请优先参考】
// 	[FAQ-1] ⭐ 精准匹配: 提取公积金需通过“公积金中心”官网或APP，在线提交申请表及身份证明，审核周期通常为3个工作日。
// 	[FAQ-2] 公积金提取金额上限为您账户内余额的90%，且需保留至少10元余额。
//
// 	### 资料来源 2：参考文档
//	【补充资料 - 仅在FAQ无法解答时参考】
// 	[DOC-1] 根据《公积金管理条例》第十五条，职工在购买、建造、翻建、大修自住住房时，可以申请提取住房公积金。
//	[DOC-2] 办理公积金提取需准备：身份证原件、银行卡、购房合同（若为购房提取）或租赁备案证明（若为租房提取）。
// 	[DOC-3] 2024年新规调整了租房提取限额，月度提取额度上限提高至2000元。
//
// 	用户问题：怎么提取公积金，能提多少钱？
// 	当前时间：2026-03-30 17:08:15
// 	当前星期：星期一

// 示例二：平铺模式 (FAQPriorityEnabled = false)
//	若检索到 3 条普通搜索结果（或未开启优先级），生成的 userContent 示例：
//
//	Markdown
//
//	[1] 北京今日晴转多云，气温 15°C - 22°C，建议着轻便春秋装。
//	[2] 气象台提醒，午后紫外线较强，出门请注意防晒。
//	[3] 明日将有小幅降温，请市民关注天气变化，预防感冒。
//
//	用户问题：北京今天天气怎么样？
//	当前时间：2026-03-30 17:06:59
//	当前星期：星期一

// 示例三：优先级模式，含图片增强示例
//	在这种模式下，系统不仅对 FAQ 进行了置信度分层，还通过 enrichContentWithImageInfo 将图片中的视觉信息转化为了文本，供 LLM 参考。
//  要点说明：
//	 - 行内注入（[FAQ-1] / [DOC-1]）：
//		正则表达式匹配到了 ![办公区图](...)，紧跟其后插入了 图片描述 和 图片文本。
//		这让 LLM 能够直接关联图片与上下文，知道“财务部在北侧”这个信息来源于这张图。
//	 - 兜底追加（[DOC-2]）：
//		[DOC-2] 的原始文本中没有图片链接，但后台返回了 qr_code.png 的信息。
//		代码判定该图片未被处理，于是将其放置在 附加图片信息 栏目下，确保 LLM 知道存在一个“正门访客登记二维码”。
//	 - LLM 的预期表现：
//		看到这个 Prompt 后，模型可以准确回答：“财务部在15楼北侧，报销流程需要经过部门主管、财务和出纳。”，
//		即使原始文档里全是图片而没有这些文字描述。
//
//	Markdown
//
//	### 资料来源 1：标准问答库 (FAQ)
//	【高置信度 - 请优先参考】
//	[FAQ-1] ⭐ 精准匹配: 办公区平面图及工位分布如下。
//	![办公区图](https://cdn.com/map_001.jpg)
//	图片描述: 科技大厦15层行政办公区平面示意图
//	图片文本: 财务部(北侧)、研发部(南侧)、茶歇区(电梯厅旁)
//
//	### 资料来源 2：参考文档
//	【补充资料 - 仅在FAQ无法解答时参考】
//	[DOC-1] 报销流程需严格遵守公司财务制度。
//	![流程图](https://cdn.com/flow_99.png)
//	图片描述: 差旅报销审批流转节点图
//	图片文本: 员工提交 -> 部门主管审批 -> 财务复核 -> 出纳付款
//
//	[DOC-2] 外部访客入场流程：
//	请引导访客在正门扫码登记。
//	附加图片信息:
//	图片 https://cdn.com/qr_code.png 的描述信息: 正门访客登记二维码
//	图片 https://cdn.com/qr_code.png 的文本: 欢迎光临，请扫码实名登记
//
//	用户问题：财务部在几楼，报销找谁？
//	当前时间：2026-03-30 17:15:22
//	当前星期：星期一

// OnEvent processes the INTO_CHAT_MESSAGE event to format chat message content
func (p *PluginIntoChatMessage) OnEvent(
	ctx context.Context,
	eventType types.EventType,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "IntoChatMessage", "input", map[string]interface{}{
		"session_id":       chatManage.SessionID,
		"merge_result_cnt": len(chatManage.MergeResult),
		"template_len":     len(chatManage.SummaryConfig.ContextTemplate),
	})

	// Separate FAQ and document results when FAQ priority is enabled
	var faqResults, docResults []*types.SearchResult
	var hasHighConfidenceFAQ bool

	if chatManage.FAQPriorityEnabled {
		// 遍历合并后的检索结果，根据 ChunkType 字段判断是 FAQ 类型还是普通文档类型，分别存入 faqResults 和 docResults 切片。
		// 在遍历 FAQ 时，检查 result.Score（相似度分数）是否大于等于配置中的 FAQDirectAnswerThreshold（直接回答阈值）。
		// 一旦发现任意一个高置信度的 FAQ，就将 hasHighConfidenceFAQ 设为 true。
		// 这个标志位后续用于在 Prompt 中添加“⭐ 精准匹配”标记，提示大模型优先参考该条信息。
		for _, result := range chatManage.MergeResult {
			if result.ChunkType == string(types.ChunkTypeFAQ) {
				faqResults = append(faqResults, result)
				// Check if this FAQ has high confidence (above direct answer threshold)
				if result.Score >= chatManage.FAQDirectAnswerThreshold && !hasHighConfidenceFAQ {
					hasHighConfidenceFAQ = true
					pipelineInfo(ctx, "IntoChatMessage", "high_confidence_faq", map[string]interface{}{
						"chunk_id":  result.ID,
						"score":     fmt.Sprintf("%.4f", result.Score),
						"threshold": chatManage.FAQDirectAnswerThreshold,
					})
				}
			} else {
				docResults = append(docResults, result)
			}
		}
		pipelineInfo(ctx, "IntoChatMessage", "faq_separation", map[string]interface{}{
			"faq_count":           len(faqResults),
			"doc_count":           len(docResults),
			"has_high_confidence": hasHighConfidenceFAQ,
		})
	}

	// 对用户原始查询 chatManage.Query 进行检查（检测非法字符、SQL 注入、XSS 攻击或敏感词等等）。
	// 如果校验失败，立即记录警告日志，并返回错误，终止后续的流程，防止恶意内容进入大模型。

	// 验证用户查询的安全性
	safeQuery, isValid := utils.ValidateInput(chatManage.Query)
	if !isValid {
		pipelineWarn(ctx, "IntoChatMessage", "invalid_query", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return ErrTemplateExecute.WithError(fmt.Errorf("用户查询包含非法内容"))
	}

	// Prepare weekday names
	weekdayName := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

	var contextsBuilder strings.Builder

	// Build contexts string based on FAQ priority strategy
	if chatManage.FAQPriorityEnabled && len(faqResults) > 0 {
		// Build structured context with FAQ prioritization
		contextsBuilder.WriteString("### 资料来源 1：标准问答库 (FAQ)\n")
		contextsBuilder.WriteString("【高置信度 - 请优先参考】\n")
		for i, result := range faqResults {
			passage := getEnrichedPassageForChat(ctx, result)
			if hasHighConfidenceFAQ && i == 0 {
				contextsBuilder.WriteString(fmt.Sprintf("[FAQ-%d] ⭐ 精准匹配: %s\n", i+1, passage))
			} else {
				contextsBuilder.WriteString(fmt.Sprintf("[FAQ-%d] %s\n", i+1, passage))
			}
		}

		if len(docResults) > 0 {
			contextsBuilder.WriteString("\n### 资料来源 2：参考文档\n")
			contextsBuilder.WriteString("【补充资料 - 仅在FAQ无法解答时参考】\n")
			for i, result := range docResults {
				passage := getEnrichedPassageForChat(ctx, result)
				contextsBuilder.WriteString(fmt.Sprintf("[DOC-%d] %s\n", i+1, passage))
			}
		}
	} else {
		// Original behavior: simple numbered list
		passages := make([]string, len(chatManage.MergeResult))
		for i, result := range chatManage.MergeResult {
			passages[i] = getEnrichedPassageForChat(ctx, result)
		}
		for i, passage := range passages {
			if i > 0 {
				contextsBuilder.WriteString("\n\n")
			}
			contextsBuilder.WriteString(fmt.Sprintf("[%d] %s", i+1, passage))
		}
	}

	// Replace placeholders in context template
	userContent := chatManage.SummaryConfig.ContextTemplate
	userContent = strings.ReplaceAll(userContent, "{{query}}", safeQuery)
	userContent = strings.ReplaceAll(userContent, "{{contexts}}", contextsBuilder.String())
	userContent = strings.ReplaceAll(userContent, "{{current_time}}", time.Now().Format("2006-01-02 15:04:05"))
	userContent = strings.ReplaceAll(userContent, "{{current_week}}", weekdayName[time.Now().Weekday()])

	// Set formatted content back to chat management
	chatManage.UserContent = userContent
	pipelineInfo(ctx, "IntoChatMessage", "output", map[string]interface{}{
		"session_id":       chatManage.SessionID,
		"user_content_len": len(chatManage.UserContent),
		"faq_priority":     chatManage.FAQPriorityEnabled,
	})
	return next()
}

// getEnrichedPassageForChat 合并Content和ImageInfo的文本内容，为聊天消息准备
func getEnrichedPassageForChat(ctx context.Context, result *types.SearchResult) string {
	// 如果没有图片信息，直接返回内容
	if result.Content == "" && result.ImageInfo == "" {
		return ""
	}

	// 如果只有内容，没有图片信息
	if result.ImageInfo == "" {
		return result.Content
	}

	// 处理图片信息并与内容合并
	return enrichContentWithImageInfo(ctx, result.Content, result.ImageInfo)
}

// 在 RAG 中，检索到的原始文本通常只包含 Markdown 格式的图片链接（如 ![](http://...)），LLM 本身无法直接通过这个 URL 看到图片内容。
// 本函数通过将图片的元数据（描述、OCR 文字）注入到文本中，完成了从多媒体数据到纯文本语义的桥接。

// enrichContentWithImageInfo 将图片元数据（描述与OCR文本）注入到 Markdown 文本内容中，使 LLM 能够理解图片内容。
//
// 该函数主要用于增强 RAG 上下文的语义信息，使纯文本大模型能够理解图片内容。
// 处理逻辑如下：
// 1. 解析：将 JSON 格式的图片信息反序列化为结构体。
// 2. 行内增强：利用正则匹配内容中的 Markdown 图片链接，并在链接下方直接追加“图片描述”和“图片文本”信息。
// 3. 兜底补充：遍历所有图片元数据，若发现图片未在当前内容中被引用（链接丢失或未展示），则将其信息汇总并追加到文本末尾的“附加图片信息”板块，确保信息不遗漏。
//
// 参数:
//   - ctx: 上下文对象，用于链路追踪与日志记录。
//   - content: 包含 Markdown 图片链接的原始文本内容。
//   - imageInfoJSON: 包含图片 URL、描述(Caption)及 OCR 文本的 JSON 字符串。
//
// 返回:
//   - string: 注入图片信息后的增强文本内容。若解析失败则返回原始内容。

// 正则表达式用于匹配Markdown图片链接
var markdownImageRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// enrichContentWithImageInfo 将图片信息与文本内容合并
func enrichContentWithImageInfo(ctx context.Context, content string, imageInfoJSON string) string {
	// 解析ImageInfo
	var imageInfos []types.ImageInfo
	err := json.Unmarshal([]byte(imageInfoJSON), &imageInfos)
	if err != nil {
		pipelineWarn(ctx, "IntoChatMessage", "image_parse_error", map[string]interface{}{
			"error": err.Error(),
		})
		return content
	}

	if len(imageInfos) == 0 {
		return content
	}

	// 创建图片URL到信息的映射
	imageInfoMap := make(map[string]*types.ImageInfo)
	for i := range imageInfos {
		if imageInfos[i].URL != "" {
			imageInfoMap[imageInfos[i].URL] = &imageInfos[i]
		}
		// 同时检查原始URL
		if imageInfos[i].OriginalURL != "" {
			imageInfoMap[imageInfos[i].OriginalURL] = &imageInfos[i]
		}
	}

	// 查找内容中的所有Markdown图片链接
	matches := markdownImageRegex.FindAllStringSubmatch(content, -1)

	// 用于存储已处理的图片URL
	processedURLs := make(map[string]bool)

	pipelineInfo(ctx, "IntoChatMessage", "image_markdown_links", map[string]interface{}{
		"match_count": len(matches),
	})

	// 替换每个图片链接，添加描述和OCR文本
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}

		// 提取图片URL，忽略alt文本
		imgURL := match[2]

		// 标记该URL已处理
		processedURLs[imgURL] = true

		// 查找匹配的图片信息
		imgInfo, found := imageInfoMap[imgURL]

		// 如果找到匹配的图片信息，添加描述和OCR文本
		if found && imgInfo != nil {
			replacement := match[0] + "\n"
			if imgInfo.Caption != "" {
				replacement += fmt.Sprintf("图片描述: %s\n", imgInfo.Caption)
			}
			if imgInfo.OCRText != "" {
				replacement += fmt.Sprintf("图片文本: %s\n", imgInfo.OCRText)
			}
			content = strings.Replace(content, match[0], replacement, 1)
		}
	}

	// 处理未在内容中找到但存在于ImageInfo中的图片
	var additionalImageTexts []string
	for _, imgInfo := range imageInfos {
		// 如果图片URL已经处理过，跳过
		if processedURLs[imgInfo.URL] || processedURLs[imgInfo.OriginalURL] {
			continue
		}

		var imgTexts []string
		if imgInfo.Caption != "" {
			imgTexts = append(imgTexts, fmt.Sprintf("图片 %s 的描述信息: %s", imgInfo.URL, imgInfo.Caption))
		}
		if imgInfo.OCRText != "" {
			imgTexts = append(imgTexts, fmt.Sprintf("图片 %s 的文本: %s", imgInfo.URL, imgInfo.OCRText))
		}

		if len(imgTexts) > 0 {
			additionalImageTexts = append(additionalImageTexts, imgTexts...)
		}
	}

	// 如果有额外的图片信息，添加到内容末尾
	if len(additionalImageTexts) > 0 {
		if content != "" {
			content += "\n\n"
		}
		content += "附加图片信息:\n" + strings.Join(additionalImageTexts, "\n")
	}

	pipelineInfo(ctx, "IntoChatMessage", "image_enrich_summary", map[string]interface{}{
		"markdown_images": len(matches),
		"additional_imgs": len(additionalImageTexts),
	})

	return content
}
