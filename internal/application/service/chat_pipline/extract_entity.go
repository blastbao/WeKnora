package chatpipline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 从用户查询中提取实体（Entity Extraction），并将其构建成图结构数据。
// 该模块是对话处理流水线（Chat Pipeline）的一部分，旨在通过大模型识别用户意图中的关键实体，
// 为后续的知识图谱检索或对话处理做准备。

// PluginExtractEntity is a plugin for extracting entities from user queries
// It uses historical dialog context and large language models to identify key entities in the user's original query
type PluginExtractEntity struct {
	modelService      interfaces.ModelService         // Model service for calling large language models
	template          *types.PromptTemplateStructured // Template for generating prompts
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository
	knowledgeService  interfaces.KnowledgeService // For shared KB document resolution
	knowledgeRepo     interfaces.KnowledgeRepository
}

// NewPluginExtractEntity creates a new extract-entity plugin instance
// Also registers the plugin with the event manager
func NewPluginExtractEntity(
	eventManager *EventManager,
	modelService interfaces.ModelService,
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository,
	knowledgeService interfaces.KnowledgeService,
	knowledgeRepo interfaces.KnowledgeRepository,
	config *config.Config,
) *PluginExtractEntity {
	res := &PluginExtractEntity{
		modelService:      modelService,
		template:          config.ExtractManager.ExtractEntity,
		knowledgeBaseRepo: knowledgeBaseRepo,
		knowledgeService:  knowledgeService,
		knowledgeRepo:     knowledgeRepo,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the list of event types this plugin responds to
// This plugin only responds to REWRITE_QUERY events
func (p *PluginExtractEntity) ActivationEvents() []types.EventType {
	return []types.EventType{types.REWRITE_QUERY}
}

// PluginExtractEntity 是一个基于图谱的实体抽取插件。
//
// 该插件在用户查询重写（REWRITE_QUERY）阶段介入，利用大语言模型（LLM）从用户原始查询中提取关键实体。
// 它结合了知识库的配置与提示词工程，将非结构化的自然语言转化为结构化的图数据（实体与关系）。
//
// 处理流程详解:
//
// 1. 环境检查与配置初始化
//    - 特性开关：检查环境变量 NEO4J_ENABLE 是否开启。若未开启，直接短路退出，不进行实体抽取。
//    - 模型加载：根据 ChatManage 中的模型 ID 获取可用的对话模型实例。
//
// 2. 上下文感知的资源准备
//    - 知识库关联分析：收集当前会话关联的所有知识库 ID（包括直接指定的 KnowledgeIDs 及其所属的 KnowledgeBaseIDs）。
//    - 批量元数据获取：调用知识库服务批量获取元数据，筛选出配置了 ExtractConfig 且 Enabled 为 true 的知识库。
//    - 上下文注入：将筛选出的启用实体抽取的知识库 ID（EntityKBIDs）及关联映射（EntityKnowledge）写回 ChatManage，
//      为后续的检索插件提供精确的过滤范围，避免全库扫描。
//
// 3. 提示词工程与模型推理
//    - 模板构建：使用预设的 PromptTemplateStructured（包含描述、标签和示例）构建推理上下文。
//    - 对话生成：构造 System（包含抽取规则与示例）和 User（包含用户原始 Query）消息，调用 LLM 进行推理。
//    - 参数控制：设置 Temperature 为 0.3 以平衡创造性和确定性，MaxTokens 限制输出长度。
//
// 4. 结构化解析与清洗
//    - 格式化解析：利用正则表达式提取模型输出中的 JSON/YAML 代码块，并将其反序列化为 GraphData 结构。
//    - 数据清洗：
//        - 去重合并：合并同名实体的属性。
//        - 修复图谱：自动补全关系中缺失的孤立节点，确保图谱结构的完整性。
//        - 过滤无效关系：移除模板中未定义的未知关系类型。
//
// 5. 结果回填
//    - 实体注入：将提取出的实体名称列表（Entity）写回 ChatManage。
//    - 流程传递：调用 next() 将控制权移交至责任链中的下一个插件。
//
// 最终结果：后续的检索插件可以利用这些实体作为关键词或过滤条件，显著提升在大规模知识库中的检索精度。

// OnEvent processes triggered events
// When receiving a REWRITE_QUERY event, it rewrites the user query using conversation history and the language model
func (p *PluginExtractEntity) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	if strings.ToLower(os.Getenv("NEO4J_ENABLE")) != "true" {
		logger.Debugf(ctx, "skipping extract entity, neo4j is disabled")
		return next()
	}

	query := chatManage.Query

	model, err := p.modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get model, session_id: %s, error: %v", chatManage.SessionID, err)
		return next()
	}

	// Collect all knowledge base IDs to query
	kbIDSet := make(map[string]struct{})
	for _, id := range chatManage.KnowledgeBaseIDs {
		kbIDSet[id] = struct{}{}
	}

	// If KnowledgeIDs is specified, retrieve them and collect their knowledge base IDs (include shared KB docs)
	// Also build a mapping from KnowledgeID to KnowledgeBaseID
	knowledgeToKBMap := make(map[string]string)
	if len(chatManage.KnowledgeIDs) > 0 {
		knowledges, err := p.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, chatManage.TenantID, chatManage.KnowledgeIDs)
		if err != nil {
			logger.Errorf(ctx, "failed to get knowledges: %v", err)
			return next()
		}
		for _, k := range knowledges {
			kbIDSet[k.KnowledgeBaseID] = struct{}{}
			knowledgeToKBMap[k.ID] = k.KnowledgeBaseID
		}
	}

	// Convert set to slice
	allKBIDs := make([]string, 0, len(kbIDSet))
	for id := range kbIDSet {
		allKBIDs = append(allKBIDs, id)
	}

	// Batch retrieve all knowledge bases
	kbs, err := p.knowledgeBaseRepo.GetKnowledgeBaseByIDs(ctx, allKBIDs)
	if err != nil {
		logger.Errorf(ctx, "failed to get knowledge bases: %v", err)
		return next()
	}

	// Check if any knowledge base has ExtractConfig enabled and collect their IDs
	enabledKBSet := make(map[string]struct{})
	for _, kb := range kbs {
		if kb.ExtractConfig != nil && kb.ExtractConfig.Enabled {
			enabledKBSet[kb.ID] = struct{}{}
		}
	}
	if len(enabledKBSet) == 0 {
		logger.Debugf(ctx, "no knowledge base has extract config enabled")
		return next()
	}

	// Save enabled knowledge base IDs for later use in search_entity
	enabledKBIDs := make([]string, 0, len(enabledKBSet))
	for id := range enabledKBSet {
		enabledKBIDs = append(enabledKBIDs, id)
	}
	chatManage.EntityKBIDs = enabledKBIDs

	// Filter knowledgeToKBMap to only include files from enabled knowledge bases
	entityKnowledge := make(map[string]string)
	for knowledgeID, kbID := range knowledgeToKBMap {
		if _, ok := enabledKBSet[kbID]; ok {
			entityKnowledge[knowledgeID] = kbID
		}
	}
	chatManage.EntityKnowledge = entityKnowledge

	template := &types.PromptTemplateStructured{
		Description: p.template.Description,
		Examples:    p.template.Examples,
	}
	extractor := NewExtractor(model, template)
	graph, err := extractor.Extract(ctx, query)
	if err != nil {
		logger.Errorf(ctx, "Failed to extract entities, session_id: %s, error: %v", chatManage.SessionID, err)
		return next()
	}
	nodes := []string{}
	for _, node := range graph.Node {
		nodes = append(nodes, node.Name)
	}
	logger.Debugf(ctx, "extracted node: %v", nodes)
	chatManage.Entity = nodes
	return next()
}

// Extractor is a struct for extracting entities
type Extractor struct {
	chat     chat.Chat
	formater *Formater
	template *types.PromptTemplateStructured
	chatOpt  *chat.ChatOptions
}

// NewExtractor creates a new extractor
func NewExtractor(
	chatModel chat.Chat,
	template *types.PromptTemplateStructured,
) Extractor {
	think := false
	return Extractor{
		chat:     chatModel,
		formater: NewFormater(),
		template: template,
		chatOpt: &chat.ChatOptions{
			Temperature: 0.3,
			MaxTokens:   4096,
			Thinking:    &think,
		},
	}
}

// Extract extracts entities from content
func (e *Extractor) Extract(ctx context.Context, content string) (*types.GraphData, error) {
	generator := NewQAPromptGenerator(e.formater, e.template)

	// logger.Debugf(ctx, "chat system: %s", generator.System(ctx))
	// logger.Debugf(ctx, "chat user: %s", generator.User(ctx, content))

	chatResponse, err := e.chat.Chat(ctx, generator.Render(ctx, content), e.chatOpt)
	if err != nil {
		logger.Errorf(ctx, "failed to chat: %v", err)
		return nil, err
	}

	graph, err := e.formater.ParseGraph(ctx, chatResponse.Content)
	if err != nil {
		logger.Errorf(ctx, "failed to parse graph: %v", err)
		return nil, err
	}
	// e.RemoveUnknownRelation(ctx, graph)
	return graph, nil
}

// RemoveUnknownRelation removes unknown relations from graph
func (e *Extractor) RemoveUnknownRelation(ctx context.Context, graph *types.GraphData) {
	relationType := make(map[string]bool)
	for _, tag := range e.template.Tags {
		relationType[tag] = true
	}

	relationNew := make([]*types.GraphRelation, 0)
	for _, relation := range graph.Relation {
		if _, ok := relationType[relation.Type]; ok {
			relationNew = append(relationNew, relation)
		} else {
			logger.Infof(ctx, "Unknown relation type %s with %v, ignore it", relation.Type, e.template.Tags)
		}
	}
	graph.Relation = relationNew
}

// QAPromptGenerator is a struct for generating QA prompts
type QAPromptGenerator struct {
	Formater        *Formater
	Template        *types.PromptTemplateStructured
	ExamplesHeading string
	QuestionHeading string
	QuestionPrefix  string
	AnswerPrefix    string
}

// NewQAPromptGenerator creates a new QA prompt generator
func NewQAPromptGenerator(formater *Formater, template *types.PromptTemplateStructured) *QAPromptGenerator {
	return &QAPromptGenerator{
		Formater:        formater,
		Template:        template,
		ExamplesHeading: "# Examples",
		QuestionHeading: "# Question",
		QuestionPrefix:  "Q: ",
		AnswerPrefix:    "A: ",
	}
}

// System generates a system prompt
func (qa *QAPromptGenerator) System(ctx context.Context) string {
	promptLines := []string{}

	if len(qa.Template.Tags) == 0 {
		promptLines = append(promptLines, qa.Template.Description)
	} else {
		tags, _ := json.Marshal(qa.Template.Tags)
		promptLines = append(promptLines, fmt.Sprintf(qa.Template.Description, string(tags)))
	}
	if len(qa.Template.Examples) > 0 {
		promptLines = append(promptLines, qa.ExamplesHeading)
		for _, example := range qa.Template.Examples {
			// Question
			promptLines = append(promptLines, fmt.Sprintf("%s%s", qa.QuestionPrefix, strings.TrimSpace(example.Text)))

			// Answer
			answer, err := qa.Formater.formatExtraction(example.Node, example.Relation)
			if err != nil {
				return ""
			}
			promptLines = append(promptLines, fmt.Sprintf("%s%s", qa.AnswerPrefix, answer))

			// new line
			promptLines = append(promptLines, "")
		}
	}
	return strings.Join(promptLines, "\n")
}

// User generates a user prompt
func (qa *QAPromptGenerator) User(ctx context.Context, question string) string {
	promptLines := []string{}
	promptLines = append(promptLines, qa.QuestionHeading)
	promptLines = append(promptLines, fmt.Sprintf("%s%s", qa.QuestionPrefix, question))
	promptLines = append(promptLines, qa.AnswerPrefix)
	return strings.Join(promptLines, "\n")
}

// Render renders a prompt
func (qa *QAPromptGenerator) Render(ctx context.Context, question string) []chat.Message {
	return []chat.Message{
		{
			Role:    "system",
			Content: qa.System(ctx),
		},
		{
			Role:    "user",
			Content: qa.User(ctx, question),
		},
	}
}

// FormatType is a type for format types
type FormatType string

const (
	// FormatTypeJSON is a format type for JSON
	FormatTypeJSON FormatType = "json"
	// FormatTypeYAML is a format type for YAML
	FormatTypeYAML FormatType = "yaml"
)

const (
	_FENCE_START   = "```"
	_LANGUAGE_TAG  = `(?P<lang>[A-Za-z0-9_+-]+)?`
	_FENCE_NEWLINE = `(?:\s*\n)?`
	_FENCE_BODY    = `(?P<body>[\s\S]*?)`
	_FENCE_END     = "```"
)

var _FENCE_RE = regexp.MustCompile(
	_FENCE_START + _LANGUAGE_TAG + _FENCE_NEWLINE + _FENCE_BODY + _FENCE_END,
)

// Formater is a struct for formatting entities
type Formater struct {
	attributeSuffix string
	formatType      FormatType
	useFences       bool
	nodePrefix      string

	relationSource string
	relationTarget string
	relationPrefix string
}

// NewFormater creates a new formater
func NewFormater() *Formater {
	return &Formater{
		attributeSuffix: "_attributes",
		formatType:      FormatTypeJSON,
		useFences:       true,
		nodePrefix:      "entity",
		relationSource:  "entity1",
		relationTarget:  "entity2",
		relationPrefix:  "relation",
	}
}

// formatExtraction formats extraction
func (f *Formater) formatExtraction(nodes []*types.GraphNode, relations []*types.GraphRelation) (string, error) {
	items := make([]map[string]interface{}, 0)
	for _, node := range nodes {
		item := map[string]interface{}{
			f.nodePrefix: node.Name,
		}
		if len(node.Attributes) > 0 {
			item[fmt.Sprintf("%s%s", f.nodePrefix, f.attributeSuffix)] = node.Attributes
		}
		items = append(items, item)
	}
	for _, relation := range relations {
		item := map[string]interface{}{
			f.relationSource: relation.Node1,
			f.relationTarget: relation.Node2,
			f.relationPrefix: relation.Type,
		}
		items = append(items, item)
	}
	formatted := ""
	switch f.formatType {
	default:
		formattedBytes, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return "", err
		}
		formatted = string(formattedBytes)
	}
	if f.useFences {
		formatted = f.addFences(formatted)
	}
	return formatted, nil
}

func (f *Formater) parseOutput(ctx context.Context, text string) ([]map[string]interface{}, error) {
	if text == "" {
		return nil, errors.New("empty or invalid input string")
	}
	content := f.extractContent(ctx, text)
	// logger.Debugf(ctx, "Extracted content: %s", content)
	if content == "" {
		return nil, errors.New("empty or invalid input string")
	}

	var parsed interface{}
	var err error
	if f.formatType == FormatTypeJSON {
		err = json.Unmarshal([]byte(content), &parsed)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s content: %s", strings.ToUpper(string(f.formatType)), err.Error())
	}
	if parsed == nil {
		return nil, fmt.Errorf("content must be a list of extractions or a dict")
	}

	var items []interface{}
	if parsedMap, ok := parsed.(map[string]interface{}); ok {
		items = []interface{}{parsedMap}
	} else if parsedList, ok := parsed.([]interface{}); ok {
		items = parsedList
	} else {
		return nil, fmt.Errorf("expected list or dict, got %T", parsed)
	}

	itemsList := make([]map[string]interface{}, 0)
	for _, item := range items {
		if itemMap, ok := item.(map[string]interface{}); ok {
			itemsList = append(itemsList, itemMap)
		} else {
			return nil, fmt.Errorf("each item in the sequence must be a mapping.")
		}
	}
	return itemsList, nil
}

func (f *Formater) ParseGraph(ctx context.Context, text string) (*types.GraphData, error) {
	matchData, err := f.parseOutput(ctx, text)
	if err != nil {
		return nil, err
	}
	if len(matchData) == 0 {
		logger.Debugf(ctx, "received empty extraction data.")
		return &types.GraphData{}, nil
	}
	// mm, _ := json.Marshal(matchData)
	// logger.Debugf(ctx, "Parsed graph data: %s", string(mm))

	var nodes []*types.GraphNode
	var relations []*types.GraphRelation

	for _, group := range matchData {
		switch {
		case group[f.nodePrefix] != nil:
			attributes := make([]string, 0)
			attributesKey := f.nodePrefix + f.attributeSuffix
			if attr, ok := group[attributesKey].([]interface{}); ok {
				for _, v := range attr {
					attributes = append(attributes, fmt.Sprintf("%v", v))
				}
			}
			nodes = append(nodes, &types.GraphNode{
				Name:       fmt.Sprintf("%v", group[f.nodePrefix]),
				Attributes: attributes,
			})
		case group[f.relationSource] != nil && group[f.relationTarget] != nil:
			relations = append(relations, &types.GraphRelation{
				Node1: fmt.Sprintf("%v", group[f.relationSource]),
				Node2: fmt.Sprintf("%v", group[f.relationTarget]),
				Type:  fmt.Sprintf("%v", group[f.relationPrefix]),
			})
		default:
			logger.Warnf(ctx, "Unsupported graph group: %v", group)
			continue
		}
	}
	graph := &types.GraphData{
		Node:     nodes,
		Relation: relations,
	}
	f.rebuildGraph(ctx, graph)
	return graph, nil
}

func (f *Formater) rebuildGraph(ctx context.Context, graph *types.GraphData) {
	nodeMap := make(map[string]*types.GraphNode)
	nodes := make([]*types.GraphNode, 0, len(graph.Node))
	for _, node := range graph.Node {
		if prenode, ok := nodeMap[node.Name]; ok {
			logger.Infof(ctx, "Duplicate node ID: %s, merge attribute", node.Name)
			// 修复panic：检查Attributes是否为nil
			if node.Attributes == nil {
				node.Attributes = make([]string, 0)
			}
			if prenode.Attributes != nil {
				node.Attributes = append(node.Attributes, prenode.Attributes...)
			}
			continue
		}
		nodeMap[node.Name] = node
		nodes = append(nodes, node)
	}

	relations := make([]*types.GraphRelation, 0, len(graph.Relation))
	for _, relation := range graph.Relation {
		if relation.Node1 == relation.Node2 {
			logger.Infof(ctx, "Duplicate relation, ignore it")
			continue
		}

		if _, ok := nodeMap[relation.Node1]; !ok {
			node := &types.GraphNode{Name: relation.Node1}
			nodes = append(nodes, node)
			nodeMap[relation.Node1] = node
			logger.Infof(ctx, "Add unknown source node ID: %s", relation.Node1)
		}
		if _, ok := nodeMap[relation.Node2]; !ok {
			node := &types.GraphNode{Name: relation.Node2}
			nodes = append(nodes, node)
			nodeMap[relation.Node2] = node
			logger.Infof(ctx, "Add unknown target node ID: %s", relation.Node2)
		}

		relations = append(relations, relation)
	}
	*graph = types.GraphData{
		Node:     nodes,
		Relation: relations,
	}
}

func (f *Formater) extractContent(ctx context.Context, text string) string {
	if !f.useFences {
		return strings.TrimSpace(text)
	}
	validTags := map[FormatType]map[string]struct{}{
		FormatTypeYAML: {"yaml": {}, "yml": {}},
		FormatTypeJSON: {"json": {}},
	}
	matches := _FENCE_RE.FindAllStringSubmatch(text, -1)
	var candidates []string
	for _, match := range matches {
		lang := match[1]
		body := match[2]
		if f.isValidLanguageTag(lang, validTags) {
			candidates = append(candidates, body)
		}
	}
	switch {
	case len(candidates) == 1:
		return strings.TrimSpace(candidates[0])

	case len(candidates) > 1:
		logger.Warnf(ctx, "multiple candidates found: %d", len(candidates))
		return strings.TrimSpace(candidates[0])

	case len(matches) == 1:
		logger.Debugf(ctx, "no candidate found, use first match without language tag: %s", matches[0][1])
		return strings.TrimSpace(matches[0][2])

	case len(matches) > 1:
		logger.Warnf(ctx, "multiple matches found: %d", len(matches))
		return strings.TrimSpace(matches[0][2])

	default:
		logger.Warnf(ctx, "no match found")
		return strings.TrimSpace(text)
	}
}

func (f *Formater) addFences(content string) string {
	content = strings.TrimSpace(content)
	return fmt.Sprintf("```%s\n%s\n```", f.formatType, content)
}

func (f *Formater) isValidLanguageTag(lang string, validTags map[FormatType]map[string]struct{}) bool {
	if lang == "" {
		return true
	}
	tag := strings.TrimSpace(strings.ToLower(lang))
	validSet, ok := validTags[f.formatType]
	if !ok {
		return false
	}
	_, exists := validSet[tag]
	return exists
}
