package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Agent 调用工具流程（AgentQA 主循环）
//	- 用户提问
//	- WeKnora 把 MCP 工具列表注入 Prompt
//	- LLM 决策：是否调用工具、调用哪个、参数是什么
//	- WeKnora 解析工具调用（JSON 结构）
//	- 通过 MCP Client 转发调用：client.CallTool(ctx, toolName, params)
//	- MCP Server 执行工具，返回结果
//	- WeKnora 把结果塞回上下文
//	- LLM 继续推理 → 生成回答

// MCPClient defines the interface for MCP client implementations
type MCPClient interface {
	// Connect establishes connection to the MCP service
	Connect(ctx context.Context) error

	// Disconnect closes the connection to the MCP service
	Disconnect() error

	// Initialize performs the MCP initialize handshake
	Initialize(ctx context.Context) (*InitializeResult, error)

	// ListTools retrieves the list of available tools from the MCP service
	ListTools(ctx context.Context) ([]*types.MCPTool, error)

	// ListResources retrieves the list of available resources from the MCP service
	ListResources(ctx context.Context) ([]*types.MCPResource, error)

	// CallTool calls a tool on the MCP service
	CallTool(ctx context.Context, name string, args map[string]interface{}) (*CallToolResult, error)

	// ReadResource reads a resource from the MCP service
	ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error)

	// IsConnected returns true if the client is connected
	IsConnected() bool

	// GetServiceID returns the service ID this client is connected to
	GetServiceID() string
}

// ClientConfig represents configuration for creating an MCP client
//
// ClientConfig 包含创建 MCP 客户端所需的所有配置信息
type ClientConfig struct {
	Service *types.MCPService // MCP 服务配置（传输类型、URL、认证信息、超时等）
}

// mcpGoClient wraps mark3labs/mcp-go client to implement our MCPClient interface
//
// mcpGoClient 是基于 mcp-go 库的 MCP 客户端实现，它实现了 MCPClient 接口，支持 SSE 和 HTTP Streamable 两种传输方式。
type mcpGoClient struct {
	service     *types.MCPService // MCP 服务配置
	client      *client.Client    // mcp-go 底层客户端
	connected   bool              // 连接状态（是否已启动）
	initialized bool              // 初始化状态（是否完成握手）
}

// NewMCPClient 根据传输类型创建 MCP 客户端
//
// 功能说明：
//   1. 设置 HTTP 超时时间（默认 30 秒）
//   2. 构建请求头（包含认证头、自定义头）
//   3. 根据传输类型创建对应的客户端：
//      - SSE: Server-Sent Events 单向推送
//      - HTTP Streamable: HTTP 流式双向通信
//      - Stdio: 禁用（安全原因）
//   4. 注册连接丢失回调函数
//
// 安全限制：
//   - Stdio 传输类型被明确禁止，因为涉及本地进程执行存在安全风险
//   - 只允许使用 SSE 和 HTTP Streamable 传输
//
// 参数：
//   config - 客户端配置（包含服务信息、认证配置等）
//
// 示例：
//   config := &ClientConfig{
//       Service: &types.MCPService{
//           TransportType: types.MCPTransportSSE,
//           URL:           stringPtr("http://localhost:8080/sse"),
//           Headers:       map[string]string{"Authorization": "Bearer token"},
//       },
//   }
//   client, err := NewMCPClient(config)
//   if err != nil {
//       return fmt.Errorf("failed to create client: %w", err)
//   }

// 三种网络传输协议
//
// SSE 工作流程
//	1. 客户端建立 SSE 连接：GET /sse
//	2. 服务器返回 session ID
//	3. 客户端通过 HTTP POST 发送请求（携带 session ID）
//	4. 服务器通过 SSE 流推送响应
//
// 		# MCP 服务配置
//		name: weather-service
//		transport: sse
//		url: https://api.weather.com/mcp/sse
//
//      注意，传统 SSE 是单向的，MCP SSE 是双向的。
//		准确来说，MCP 的 SSE 传输类型使用 SSE 作为下行通道（服务器→客户端），HTTP POST 作为上行通道（客户端→服务器），共同实现双向通信。
//
// HTTP Streaming 工作流程
//	1. 客户端发起 HTTP 请求（Accept: text/event-stream）
//	2. 服务器保持连接打开
//	3. 双方可以持续发送数据块
//	4. 任一方可以关闭连接
//
//		# MCP 服务配置
//		name: code-runner
//		transport: http-streamable
//		url: https://sandbox.code-runner.com/mcp
//
// Stdin 工作流程
//	1. WeKnora 启动子进程：python3 mcp_server.py
//	2. 通过 stdin 发送 JSON-RPC 请求
//	3. 从 stdout 读取 JSON-RPC 响应
//	4. 子进程退出时关闭通信
//
//		# Stdin 服务配置
//		name: file-manager
//		transport: stdio  	# WeKnora 会拒绝
//		command: /bin/ls
//		args: ["-la"] 		# 安全风险：可以读取任意文件
//
//		Stdin 被禁用的原因：
//			- 命令注入漏洞：攻击者可以构造恶意参数执行任意命令
//			- 权限过大：子进程继承 WeKnora 的权限，可能访问敏感数据
//			- 难以隔离：无法限制子进程的资源使用
//			- 持久化风险：子进程可能常驻后台成为后门
//			- 日志污染：子进程的输出可能包含恶意内容

// NewMCPClient creates a new MCP client based on the transport type
func NewMCPClient(config *ClientConfig) (MCPClient, error) {
	// Create HTTP client with timeout
	timeout := 30 * time.Second
	if config.Service.AdvancedConfig != nil && config.Service.AdvancedConfig.Timeout > 0 {
		timeout = time.Duration(config.Service.AdvancedConfig.Timeout) * time.Second
	}

	httpClient := &http.Client{
		Timeout: timeout,
	}

	// Build headers
	headers := make(map[string]string)
	for key, value := range config.Service.Headers {
		headers[key] = value
	}

	// Add auth headers
	if config.Service.AuthConfig != nil {
		if config.Service.AuthConfig.APIKey != "" {
			headers["X-API-Key"] = config.Service.AuthConfig.APIKey
		}
		if config.Service.AuthConfig.Token != "" {
			headers["Authorization"] = "Bearer " + config.Service.AuthConfig.Token
		}
		if config.Service.AuthConfig.CustomHeaders != nil {
			for key, value := range config.Service.AuthConfig.CustomHeaders {
				headers[key] = value
			}
		}
	}

	// Create client based on transport type
	var mcpClient *client.Client
	var err error
	switch config.Service.TransportType {
	case types.MCPTransportSSE:
		if config.Service.URL == nil || *config.Service.URL == "" {
			return nil, fmt.Errorf("URL is required for SSE transport")
		}
		mcpClient, err = client.NewSSEMCPClient(*config.Service.URL,
			client.WithHTTPClient(httpClient),
			client.WithHeaders(headers),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create SSE client: %w", err)
		}
	case types.MCPTransportHTTPStreamable:
		if config.Service.URL == nil || *config.Service.URL == "" {
			return nil, fmt.Errorf("URL is required for HTTP Streamable transport")
		}
		// For HTTP streamable, we need to use transport options
		mcpClient, err = client.NewStreamableHttpClient(*config.Service.URL,
			transport.WithHTTPBasicClient(httpClient),
			transport.WithHTTPHeaders(headers),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP streamable client: %w", err)
		}
	case types.MCPTransportStdio:
		// Stdio transport is disabled for security reasons (potential command injection vulnerabilities)
		return nil, fmt.Errorf("stdio transport is disabled for security reasons; please use SSE or HTTP Streamable transport instead")
	default:
		return nil, ErrUnsupportedTransport
	}

	instance := &mcpGoClient{
		service: config.Service,
		client:  mcpClient,
	}
	mcpClient.OnConnectionLost(instance.onConnectionLost)
	return instance, nil
}

// onConnectionLost 连接丢失时的回调函数
//
// 触发场景：
//   - 网络中断
//   - 服务器主动关闭连接
//   - 空闲超时
//
// 处理逻辑：
//   1. 调用 Disconnect 清理本地连接状态
//   2. 记录警告日志（包含服务 URL 和错误信息）
//
// 参数：
//   err - 连接丢失的原因
//
// 注意事项：
//   - 该回调由 mcp-go 库触发
//   - 不会自动重连，只清理状态

// onConnectionLost callback when the connection is lost
func (c *mcpGoClient) onConnectionLost(err error) {
	_ = c.Disconnect()
	logger.Warnf(context.Background(), "MCP server connection has been lost, URL:%s, error:%v", *c.service.URL, err)
}

// checkErrorAndDisconnectIfNeeded 检查错误并根据需要断开连接
//
// 功能说明：
//   处理 SSE 传输类型的已知问题：连接丢失后会话 ID 失效
//   mcp-go 库在 SSE 传输下不会主动触发 onConnectionLost
//
// 问题场景：
//   - SSE 连接丢失后，底层连接可能已断开
//   - 客户端仍然认为已连接
//   - 后续请求会收到 "Invalid session ID" 错误
//
// 解决方案：
//   检测到特定错误时主动调用 Disconnect 清理状态
//
// 参数：
//   err - 操作返回的错误
//
// 注意事项：
//   - 仅对 SSE 传输类型生效
//   - 检测 "Invalid session ID" 错误字符串
//   - 这是对 mcp-go 库 bug 的临时修复

// checkErrorAndDisconnectIfNeeded Check for errors and call Disconnect when reconnection is required
func (c *mcpGoClient) checkErrorAndDisconnectIfNeeded(err error) {
	var transportErr *transport.Error
	// In SSE transport type, connection loss does not always actively trigger onConnectionLost (a go-mcp issue).
	// Once the connection is lost, the session becomes invalid.
	// Without reconnecting, it will continuously cause "Invalid session ID" errors.
	if c.service.TransportType == types.MCPTransportSSE &&
		errors.As(err, &transportErr) &&
		transportErr.Err != nil &&
		strings.Contains(transportErr.Err.Error(), "Invalid session ID") {
		_ = c.Disconnect()
	}
}

// Connect 建立与 MCP 服务的连接
//
// 功能说明：
//   1. 检查是否已连接，避免重复连接
//   2. 启动底层客户端
//   3. 更新连接状态

// Connect establishes connection to the MCP service
func (c *mcpGoClient) Connect(ctx context.Context) error {
	if c.connected {
		return ErrAlreadyConnected
	}

	// Start the client
	if err := c.client.Start(ctx); err != nil {
		return fmt.Errorf("failed to start client: %w", err)
	}
	c.connected = true
	if c.service.TransportType == types.MCPTransportStdio {
		logger.GetLogger(ctx).Infof("MCP stdio client connected: %s %v", c.service.StdioConfig.Command, c.service.StdioConfig.Args)
	} else {
		logger.GetLogger(ctx).Infof("MCP client connected to %s", *c.service.URL)
	}
	return nil
}

// Disconnect 关闭与 MCP 服务的连接
//
// 功能说明：
//   1. 检查连接状态
//   2. 关闭底层客户端
//   3. 重置连接和初始化状态

// Disconnect closes the connection
func (c *mcpGoClient) Disconnect() error {
	if !c.connected {
		return nil
	}

	// Close the client
	if c.client != nil {
		c.client.Close()
	}
	c.connected = false
	c.initialized = false
	return nil
}

// Initialize 执行 MCP 协议初始化握手
//
// 功能说明：
//   1. 检查客户端是否已连接
//   2. 发送 Initialize 请求，协商协议版本、交换客户端和服务器信息
//   3. 更新初始化状态
//
// 请求参数：
//   - ProtocolVersion: 使用最新 MCP 协议版本
//   - ClientInfo: 客户端标识（名称: WeKnora, 版本: 1.0.0）
//
// 返回值：
//   *InitializeResult - 初始化结果（包含服务器信息、协议版本等）
//   error             - 初始化失败时的错误信息

// Initialize performs the MCP initialize handshake
func (c *mcpGoClient) Initialize(ctx context.Context) (*InitializeResult, error) {
	if !c.connected {
		return nil, ErrNotConnected
	}

	// Initialize the client
	req := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			Capabilities:    mcp.ClientCapabilities{},
			ClientInfo: mcp.Implementation{
				Name:    "WeKnora",
				Version: "1.0.0",
			},
		},
	}

	result, err := c.client.Initialize(ctx, req)
	if err != nil {
		c.checkErrorAndDisconnectIfNeeded(err)
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}

	c.initialized = true

	return &InitializeResult{
		ProtocolVersion: result.ProtocolVersion,
		ServerInfo: ServerInfo{
			Name:    result.ServerInfo.Name,
			Version: result.ServerInfo.Version,
		},
	}, nil
}

// ListTools 获取 MCP 服务提供的工具列表
//
// 功能说明：
//   1. 检查客户端是否已完成初始化
//   2. 发送 ListTools 请求
//   3. 响应数据的格式转换
//
// 返回值：
//   []*types.MCPTool - 工具列表（包含名称、描述、参数 Schema）
//   error            - 获取失败时的错误信息
//
// 注意：
//   - 须在 Initialize 成功后调用
//   - 工具的输入参数 Schema 是 JSON 格式
//
// 示例：
//   tools, err := client.ListTools(ctx)
//   for _, tool := range tools {
//       fmt.Printf("工具: %s\n描述: %s\n", tool.Name, tool.Description)
//   }

// ListTools retrieves the list of available tools
func (c *mcpGoClient) ListTools(ctx context.Context) ([]*types.MCPTool, error) {
	if !c.initialized {
		return nil, ErrNotConnected
	}

	req := mcp.ListToolsRequest{}
	result, err := c.client.ListTools(ctx, req)
	if err != nil {
		c.checkErrorAndDisconnectIfNeeded(err)
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	// Convert to our types
	tools := make([]*types.MCPTool, len(result.Tools))
	for i, tool := range result.Tools {
		data, _ := json.Marshal(tool.InputSchema)
		tools[i] = &types.MCPTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: data,
		}
	}

	return tools, nil
}

// ListResources 获取 MCP 服务提供的资源列表
//
// 功能说明：
//   1. 检查客户端是否已初始化
//   2. 发送 ListResources 请求
//   3. 响应数据的格式转换
//
// 返回值：
//   []*types.MCPResource - 资源列表（包含 URI、名称、描述、MIME 类型）
//   error                - 获取失败时的错误信息
//
// 注意事项：
//   - 资源是通过 URI 访问的数据（文件、数据库记录等）
//   - 资源有 MIME 类型，客户端可以根据类型处理

// ListResources retrieves the list of available resources
func (c *mcpGoClient) ListResources(ctx context.Context) ([]*types.MCPResource, error) {
	if !c.initialized {
		return nil, ErrNotConnected
	}

	req := mcp.ListResourcesRequest{}
	result, err := c.client.ListResources(ctx, req)
	if err != nil {
		c.checkErrorAndDisconnectIfNeeded(err)
		return nil, fmt.Errorf("failed to list resources: %w", err)
	}

	// Convert to our types
	resources := make([]*types.MCPResource, len(result.Resources))
	for i, resource := range result.Resources {
		resources[i] = &types.MCPResource{
			URI:         resource.URI,
			Name:        resource.Name,
			Description: resource.Description,
			MimeType:    resource.MIMEType,
		}
	}

	return resources, nil
}

// CallTool 调用 MCP 服务上的工具
//
// 功能说明：
//   1. 检查客户端是否已初始化
//   2. 构建 CallTool 请求（工具名 + 参数）
//   3. 发送请求并等待响应
//   4. 转换响应内容（支持文本和图片）
//
// 参数：
//   ctx  - 上下文（支持超时和取消）
//   name - 工具名称（必须与 ListTools 返回的名称匹配）
//   args - 工具参数（map 格式，需符合工具的 JSON Schema 定义）
//
// 返回值：
//   *CallToolResult - 调用结果（包含内容列表和错误标志）
//   error           - 调用失败时的错误信息
//
// 响应内容类型：
//   - text: 文本内容
//   - image: 图片内容（Base64 编码）
//
// 示例：
//   result, err := client.CallTool(ctx, "get_weather", map[string]interface{}{
//       "city": "Beijing",
//       "units": "celsius",
//   })
//   if result.IsError {
//       fmt.Println("工具执行失败:", result.Content[0].Text)
//   } else {
//       fmt.Println("结果:", result.Content[0].Text)
//   }

// CallTool calls a tool on the MCP service
func (c *mcpGoClient) CallTool(ctx context.Context, name string, args map[string]interface{}) (*CallToolResult, error) {
	if !c.initialized {
		return nil, ErrNotConnected
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}

	result, err := c.client.CallTool(ctx, req)
	if err != nil {
		c.checkErrorAndDisconnectIfNeeded(err)
		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	// Convert to our types
	content := make([]ContentItem, 0, len(result.Content))
	for _, item := range result.Content {
		if textContent, ok := mcp.AsTextContent(item); ok {
			content = append(content, ContentItem{
				Type: "text",
				Text: textContent.Text,
			})
		} else if imageContent, ok := mcp.AsImageContent(item); ok {
			content = append(content, ContentItem{
				Type:     "image",
				Data:     imageContent.Data,
				MimeType: imageContent.MIMEType,
			})
		}
	}

	return &CallToolResult{
		IsError: result.IsError,
		Content: content,
	}, nil
}

// ReadResource 读取 MCP 服务上的资源
//
// 功能说明：
//   1. 检查客户端是否已初始化
//   2. 发送 ReadResource 请求（指定资源 URI）
//   3. 转换响应内容（支持文本和二进制）
//
// 参数：
//   ctx - 上下文（支持超时和取消）
//   uri - 资源 URI（必须与 ListResources 返回的 URI 匹配）
//
// 返回值：
//   *ReadResourceResult - 资源内容（包含文本或二进制数据）
//   error               - 读取失败时的错误信息
//
// 资源内容类型：
//   - Text: 文本内容（纯文本、JSON、XML 等）
//   - Blob: 二进制内容（图片、PDF 等）
//
// 示例：
//   result, err := client.ReadResource(ctx, "file:///docs/readme.md")
//   if err != nil {
//       return err
//   }
//   fmt.Println(result.Contents[0].Text)

// ReadResource reads a resource from the MCP service
func (c *mcpGoClient) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	if !c.initialized {
		return nil, ErrNotConnected
	}

	req := mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{
			URI: uri,
		},
	}

	result, err := c.client.ReadResource(ctx, req)
	if err != nil {
		c.checkErrorAndDisconnectIfNeeded(err)
		return nil, fmt.Errorf("failed to read resource: %w", err)
	}

	// Convert to our types
	contents := make([]ResourceContent, 0, len(result.Contents))
	for _, item := range result.Contents {
		if textContent, ok := mcp.AsTextResourceContents(item); ok {
			contents = append(contents, ResourceContent{
				URI:      textContent.URI,
				MimeType: textContent.MIMEType,
				Text:     textContent.Text,
			})
		} else if blobContent, ok := mcp.AsBlobResourceContents(item); ok {
			contents = append(contents, ResourceContent{
				URI:      blobContent.URI,
				MimeType: blobContent.MIMEType,
				Blob:     blobContent.Blob,
			})
		}
	}

	return &ReadResourceResult{
		Contents: contents,
	}, nil
}

// IsConnected returns true if the client is connected
func (c *mcpGoClient) IsConnected() bool {
	return c.connected
}

// GetServiceID returns the service ID
func (c *mcpGoClient) GetServiceID() string {
	return c.service.ID
}
