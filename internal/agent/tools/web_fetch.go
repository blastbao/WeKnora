package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// web_fetch 通常作为 web_search（联网搜索）的“增强补丁”，用于获取网页的完整内容并利用 AI 进行精炼总结。

const (
	webFetchTimeout  = 60 * time.Second // timeout for web fetch
	webFetchMaxChars = 100000           // maximum number of characters to fetch
)

var webFetchTool = BaseTool{
	name: ToolWebFetch,
	description: `Fetch detailed web content from previously discovered URLs and analyze it with an LLM.

## Usage
- Receive one or more {url, prompt} combinations
- Fetch web page content and convert to Markdown text
- Use prompt to call small model for analysis and summary (if model is available)
- Return summary result and original content fragment

## When to Use
- **MANDATORY**: After web_search returns results, if content is truncated or incomplete, use web_fetch to get full page content
- When web_search snippet is insufficient for answering the question`,
	schema: utils.GenerateSchema[WebFetchInput](),
}

// WebFetchInput defines the input parameters for web fetch tool
type WebFetchInput struct {
	Items []WebFetchItem `json:"items" jsonschema:"批量抓取任务，每项包含 url 与 prompt"`
}

// WebFetchItem represents a single web fetch task
type WebFetchItem struct {
	URL    string `json:"url" jsonschema:"待抓取的网页 URL，需来自 web_search 结果"`
	Prompt string `json:"prompt" jsonschema:"分析该网页内容时使用的提示词"`
}

// webFetchParams is the parameters for the web fetch tool
type webFetchParams struct {
	URL    string
	Prompt string
}

// validatedParams holds validated input plus DNS-pinned host/IP for SSRF protection.
// PinnedIP is the single IP we resolved at validation time; chromedp and HTTP both use it.
type validatedParams struct {
	URL      string
	Prompt   string
	Host     string
	Port     string
	PinnedIP net.IP
}

// webFetchItemResult is the result for a web fetch item
type webFetchItemResult struct {
	output string
	data   map[string]interface{}
	err    error
}

// WebFetchTool fetches web page content and summarizes it using an LLM
type WebFetchTool struct {
	BaseTool
	client    *http.Client
	chatModel chat.Chat
}

// NewWebFetchTool creates a new web_fetch tool instance
func NewWebFetchTool(chatModel chat.Chat) *WebFetchTool {
	// Use SSRF-safe HTTP client to prevent redirect-based SSRF attacks
	ssrfConfig := utils.DefaultSSRFSafeHTTPClientConfig()
	ssrfConfig.Timeout = webFetchTimeout

	return &WebFetchTool{
		BaseTool:  webFetchTool,
		client:    utils.NewSSRFSafeHTTPClient(ssrfConfig),
		chatModel: chatModel,
	}
}

// Execute 执行 web_fetch 工具
//
// 网页抓取：
//   - 接收一个列表 items，每项包含 {url, prompt}。
//   - 使用 sync.WaitGroup 并行抓取所有 URL。
//   - 在抓取前调用 normalizeGitHubURL 检测是否是 GitHub 文件链接(blob)，自动转换为原始文件链接，确保能直接下载到代码或文本，而不是 HTML 页面。
func (t *WebFetchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][WebFetch] Execute started")

	// Parse args from json.RawMessage
	var input WebFetchInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][WebFetch] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	if len(input.Items) == 0 {
		logger.Errorf(ctx, "[Tool][WebFetch] 参数缺失: items")
		return &types.ToolResult{
			Success: false,
			Error:   "missing required parameter: items",
		}, nil
	}

	results := make([]*webFetchItemResult, len(input.Items))

	var wg sync.WaitGroup
	wg.Add(len(input.Items))

	for idx := range input.Items {
		i := idx
		item := input.Items[i]

		params := webFetchParams{
			URL:    item.URL,
			Prompt: item.Prompt,
		}

		go func(index int, p webFetchParams) {
			defer wg.Done()

			// Normalize URL before validation so we pin the host we actually fetch (e.g. raw.githubusercontent.com)
			finalURL := t.normalizeGitHubURL(p.URL)
			vp, err := t.validateAndResolve(webFetchParams{URL: finalURL, Prompt: p.Prompt})
			if err != nil {
				results[index] = &webFetchItemResult{
					err: err,
					data: map[string]interface{}{
						"url":    p.URL,
						"prompt": p.Prompt,
						"error":  err.Error(),
					},
					output: fmt.Sprintf("URL: %s\n错误: %v\n\n", p.URL, err),
				}
				return
			}

			output, data, err := t.executeFetch(ctx, vp, p.URL)
			results[index] = &webFetchItemResult{
				output: output,
				data:   data,
				err:    err,
			}
		}(i, params)
	}

	wg.Wait()

	var builder strings.Builder
	builder.WriteString("=== Web Fetch Results ===\n\n")

	aggregated := make([]map[string]interface{}, 0, len(results))
	success := true
	var firstErr error

	for idx, res := range results {
		if res == nil {
			success = false
			if firstErr == nil {
				firstErr = fmt.Errorf("fetch item %d returned nil", idx)
			}
			builder.WriteString(fmt.Sprintf("#%d: 无结果（内部错误）\n\n", idx+1))
			continue
		}

		builder.WriteString(fmt.Sprintf("#%d:\n%s", idx+1, res.output))
		if !strings.HasSuffix(res.output, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("\n")

		if res.data != nil {
			aggregated = append(aggregated, res.data)
		}

		if res.err != nil {
			success = false
			if firstErr == nil {
				firstErr = res.err
			}
		}
	}

	// Add guidance for next steps
	builder.WriteString("\n=== Next Steps ===\n")
	if len(aggregated) > 0 {
		builder.WriteString("- ✅ Full page content has been fetched and analyzed.\n")
		builder.WriteString("- Evaluate if the content is sufficient to answer the question completely.\n")
		builder.WriteString("- Synthesize information from all fetched pages for comprehensive answers.\n")
		if !success {
			builder.WriteString("- ⚠️ Some URLs failed to fetch. Use available content or try alternative sources.\n")
		}
	} else {
		builder.WriteString("- ❌ No content was successfully fetched. Consider:\n")
		builder.WriteString("  - Verify URLs are accessible\n")
		builder.WriteString("  - Try alternative sources from web_search results\n")
		builder.WriteString("  - Check if information can be found in knowledge base instead\n")
	}

	data := map[string]interface{}{
		"results":      aggregated,
		"count":        len(aggregated),
		"display_type": "web_fetch_results",
	}

	logger.Infof(ctx, "[Tool][WebFetch] Completed with success=%v, items=%d", success, len(aggregated))

	return &types.ToolResult{
		Success: success,
		Output:  builder.String(),
		Data:    data,
		Error: func() string {
			if firstErr != nil {
				return firstErr.Error()
			}
			return ""
		}(),
	}, nil
}

// parseParams parses the parameters for a web fetch item
func (t *WebFetchTool) parseParams(item interface{}) webFetchParams {
	params := webFetchParams{}
	if m, ok := item.(map[string]interface{}); ok {
		if v, ok := m["url"].(string); ok {
			params.URL = strings.TrimSpace(v)
		}
		if v, ok := m["prompt"].(string); ok {
			params.Prompt = strings.TrimSpace(v)
		}
	}
	return params
}

// validateAndResolve validates parameters and resolves the host to a single public IP (DNS pinning).
// The returned PinnedIP is used for both chromedp (host-resolver-rules) and HTTP to prevent DNS rebinding.
func (t *WebFetchTool) validateAndResolve(p webFetchParams) (*validatedParams, error) {
	if p.URL == "" {
		return nil, fmt.Errorf("url is required")
	}
	if p.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return nil, fmt.Errorf("invalid URL format")
	}

	// SSRF protection: validate URL is safe (scheme, hostname, and that resolved IPs are not restricted)
	if safe, reason := utils.IsSSRFSafeURL(p.URL); !safe {
		return nil, fmt.Errorf("URL rejected for security reasons: %s", reason)
	}

	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	hostname := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	// Resolve and pin to the first public IP (same resolver as IsSSRFSafeURL; we pin so chromedp cannot re-resolve)
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", hostname)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", hostname, err)
	}
	var pinnedIP net.IP
	for _, ip := range ips {
		if utils.IsPublicIP(ip) {
			pinnedIP = ip
			break
		}
	}
	if pinnedIP == nil {
		return nil, fmt.Errorf("no public IP available for host %s", hostname)
	}

	return &validatedParams{
		URL:      p.URL,
		Prompt:   p.Prompt,
		Host:     hostname,
		Port:     port,
		PinnedIP: pinnedIP,
	}, nil
}

// 核心流程
//  抓取 (Fetch)：调用 fetchHTMLContent。
//		- 尝试用 Chromedp 或 HTTP 拿到网页原始 HTML。
//		- 如果失败，立即通过 fmt.Sprintf 构造错误信息并返回，确保流程不中断。
//  提纯 (Convert)：调用 convertHTMLToText。
//		- 将抓到的 htmlContent（可能包含大量标签、脚本）转换为纯净的 Markdown/文本 (textContent)。
//		- 为 LLM 提供高质量的输入，去除噪音，节省 Token。
// 	总结 (Process)：调用 processWithLLM。
//		- 把清洗后的文本送给 AI，根据用户的 Prompt 进行分析。
// 	容错：
//		- 如果 AI 总结失败（summaryErr != nil），不中断整个工具的运行，体现了“保底输出”的原则。
// 	包装 (Build)：调用 buildOutputText，根据是否有 summary 决定展示“摘要”还是“原文预览”。
//		- output: 给用户的最终文本（Markdown格式）。
//		- resultData: 给系统的结构化数据（可能含 summary，也可能不含）。
//		- summaryErr: 返回 LLM 的错误（如果有）。注意这里返回的是 summaryErr 而不是 nil，即使抓取成功了。这允许上层调用者知道“虽然拿到了内容，但总结失败了”，从而决定是否需要重试或提示用户。
//
// 在处理过程中，代码维护了一个 resultData（map[string]interface{}），它包含了这次任务的所有“结构化档案”：
//	- url & prompt：原始任务信息。
//	- raw_content：清洗后的网页全文（供后续可能的需求使用）。
//	- content_length：抓取到的文本长度，可以帮助监控是否抓取到了空页面或异常大的页面。
//	- method：标记是用了 chromedp 还是 http 抓取的，运维可以通过分析这个字段来优化策略（例如：如果发现 90% 的网站用 HTTP 就能搞定，可以减少 Chrome 的资源配额）。
//	- summary：AI 生成的最终精华。
//
// 函数返回三个值：
//	- string (output)：人类可读的文本，直接在聊天界面展示给用户。
//	- map (resultData)：机器可读的结构体，包含 raw_content, method, content_length 等字段，供后续插件、日志系统或数据分析使用。
//	- error：给代码逻辑看的，判断这次任务是否圆满成功。

// executeFetch executes a web fetch item. displayURL is the URL shown to the user (e.g. original); vp.URL is the normalized URL we fetch.
func (t *WebFetchTool) executeFetch(
	ctx context.Context,
	vp *validatedParams,
	displayURL string,
) (string, map[string]interface{}, error) {
	logger.Infof(ctx, "[Tool][WebFetch] Fetching URL: %s", displayURL)

	htmlContent, method, err := t.fetchHTMLContent(ctx, vp)
	if err != nil {
		logger.Errorf(ctx, "[Tool][WebFetch] 获取页面失败 url=%s err=%v", vp.URL, err)
		return fmt.Sprintf("URL: %s\n错误: %v\n", displayURL, err),
			map[string]interface{}{
				"url":    displayURL,
				"prompt": vp.Prompt,
				"error":  err.Error(),
			}, err
	}

	textContent := t.convertHTMLToText(htmlContent)

	resultData := map[string]interface{}{
		"url":            displayURL,
		"prompt":         vp.Prompt,
		"raw_content":    textContent,
		"content_length": len(textContent),
		"method":         method,
	}
	params := webFetchParams{URL: displayURL, Prompt: vp.Prompt}
	var summary string
	var summaryErr error
	summary, summaryErr = t.processWithLLM(ctx, params, textContent)
	if summaryErr != nil {
		logger.Warnf(ctx, "[Tool][WebFetch] LLM 处理失败 url=%s err=%v", displayURL, summaryErr)
	} else if summary != "" {
		resultData["summary"] = summary
	}

	output := t.buildOutputText(params, textContent, summary, summaryErr)

	return output, resultData, summaryErr
}

// normalizeGitHubURL normalizes a GitHub URL
func (t *WebFetchTool) normalizeGitHubURL(source string) string {
	if strings.Contains(source, "github.com") && strings.Contains(source, "/blob/") {
		source = strings.Replace(source, "github.com", "raw.githubusercontent.com", 1)
		source = strings.Replace(source, "/blob/", "/", 1)
	}
	return source
}

// processWithLLM 负责将抓取到的原始网页内容（Raw Content）与用户的特定问题（Prompt）结合，利用大语言模型（LLM）生成精准的摘要或答案。
//
// 1. 角色设定和行为准则
// 2. 结构化输入：用户想要什么(params.Prompt)、“参考材料是什么” (content)
// 3. 模型调用：
//   - 低温度，降低模型的随机性和创造性；
//   - 限制输出最大长度，防止模型啰嗦，节省 Token ，避免超时。

// processWithLLM processes the content with an LLM
func (t *WebFetchTool) processWithLLM(ctx context.Context, params webFetchParams, content string) (string, error) {
	if t.chatModel == nil {
		return "", fmt.Errorf("chat model not available for web_fetch")
	}

	systemMessage := "你是一名擅长阅读网页内容的智能助手，请根据提供的网页文本回答用户需求，严禁编造未在文本中出现的信息。"
	userTemplate := `用户请求:
%s

网页内容:
%s`

	messages := []chat.Message{
		{
			Role:    "system",
			Content: systemMessage,
		},
		{
			Role:    "user",
			Content: fmt.Sprintf(userTemplate, params.Prompt, content),
		},
	}

	response, err := t.chatModel.Chat(ctx, messages, &chat.ChatOptions{
		Temperature: 0.3,
		MaxTokens:   1024,
	})
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(response.Content), nil
}

// buildOutputText builds the output text for a web fetch item
func (t *WebFetchTool) buildOutputText(params webFetchParams, content string, summary string, summaryErr error) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("URL: %s\n", params.URL))
	builder.WriteString(fmt.Sprintf("Prompt: %s\n", params.Prompt))

	if summaryErr == nil && summary != "" {
		builder.WriteString("Summary:\n")
		builder.WriteString(summary)
		builder.WriteString("\n")
	} else {
		builder.WriteString("Content Preview:\n")
		builder.WriteString(content)
		builder.WriteString("\n")
	}

	return builder.String()
}

// 首先尝试启动 无头浏览器 (Chrome)。
// 现代网页（如 React, Vue 编写的单页应用）往往是“空壳”，内容是靠 JavaScript 动态生成的。
// 只有浏览器引擎才能把这些脚本执行完，拿到最终看到的文字。
//
// 如果浏览器抓取失败，函数会调用底层的 HTTP 直接向服务器请求 HTML 源码。
// 这种方法速度极快，消耗资源极低。但无法抓到那些靠 JS 动态加载的数据。
// 但对于新闻、博客、代码等静态内容较多的网页，这种方法非常可靠。

// fetchHTMLContent fetches the HTML content for a web fetch item using pinned IP (DNS pinning).
func (t *WebFetchTool) fetchHTMLContent(ctx context.Context, vp *validatedParams) (string, string, error) {
	html, err := t.fetchWithChromedp(ctx, vp)
	if err == nil && strings.TrimSpace(html) != "" {
		return html, "chromedp", nil
	}

	if err != nil {
		logger.Debugf(ctx, "[Tool][WebFetch] Chromedp 抓取失败 url=%s err=%v，尝试直接请求", vp.URL, err)
	}

	html, httpErr := t.fetchWithHTTP(ctx, vp)
	if httpErr != nil {
		if err != nil {
			return "", "", fmt.Errorf("chromedp error: %v; http error: %w", err, httpErr)
		}
		return "", "", httpErr
	}

	return html, "http", nil
}

// 使用无头浏览器（Chromedp）进行动态网页抓取。它不仅仅是简单的“打开网页”，其核心在于安全防御和对抗反爬。

// 1. DNS Pinning（防 DNS 重绑定攻击）
//
// 背景: 之前的步骤已经解析过域名，确认了 vp.PinnedIP 是一个安全的公网 IP。
// 问题:
//	如果不做处理，Chrome 在发起网络请求时，会再次向 DNS 服务器查询该域名。
//	黑客可以利用“DNS 重绑定”攻击，在这第二次查询时返回一个内网 IP（如 127.0.0.1），从而骗过浏览器访问内网。
// 解决方案:
//	MAP 域名 IP 是 Chrome 的底层网络规则语法。
//	通过 host-resolver-rules 规则强制 Chrome：“当你看到域名 vp.Host 时，不要查 DNS，直接连接到 vp.PinnedIP”。
//	彻底切断了 DNS 重绑定的可能性。无论外部 DNS 怎么变，浏览器只连我们验证过的那个 IP。

// 2. 对抗反爬
// 为了让抓取看起来更像“真人”而非“机器人”，代码配置了多个关键参数：
// 	headless: true									无头模式。不在屏幕上显示窗口，节省服务器资源，适合后台运行。
//	disable-setuid-sandbox: true					禁用沙箱权限检查。在 Docker 中运行 Chrome 通常需要此选项，否则会因为权限问题启动失败。
//	disable-dev-shm-usage: true						防止内存崩溃。Docker 默认共享内存很小，Chrome 容易爆内存。此选项让 Chrome 使用 /tmp 代替，更稳定。
//	disable-gpu: true								禁用 GPU	服务器通常没有显卡，禁用可避免初始化错误。
//	disable-blink-features: AutomationControlled	移除 navigator.webdriver 特征。很多网站会检测这个特征来拦截机器人，移除它能提高抓取成功率。
//	UserAgent: ... Chrome/120...					伪装成最新的 Windows Chrome 浏览器，避免被网站识别为 Linux 爬虫而拒绝服务。

// 3. 性能与资源控制
// 	资源节约：禁用了 GPU 硬件加速 (disable-gpu)、共享内存限制 (disable-dev-shm-usage) 和显示合成器 (VizDisplayCompositor)，这些配置让浏览器在服务器后台运行得更轻量。
//	超时管理：如果网页加载太慢（比如超过 60 秒），程序会自动掐断，防止死锁导致服务器资源耗尽。

// 4. 抓取动作
//  代码执行了一个典型的浏览器指令序列：
//	- Navigate：跳转到目标 URL。
//	- WaitReady("body")：等待 HTML 文档的基础结构（DOM）加载完成，特别是对于那些需要加载一点脚本才能显示内容的网页。
//	- OuterHTML：抓取整个 HTML 源码，包括由 JavaScript 动态生成后的内容。
//
// 其中：
//	WaitReady 是关键，它不是傻等固定时间，而是等到 DOM 树中的 body 出现才继续。这对于抓取由 JavaScript 动态生成内容的网站（如 React/Vue 应用）至关重要。
// 	OuterHTML("html") 会抓取包括 <html> 标签在内的完整源代码，包含所有动态插入的 DOM 节点。

// 总结：
//	安全性 (Security): 通过 host-resolver-rules 实现了应用层的 DNS 锁定，这是防御 SSRF 攻击的黄金标准。
//	兼容性 (Compatibility): 专门针对 Docker 环境优化了内存和权限配置，保证在云原生环境下稳定运行。
//	隐蔽性 (Stealth): 去除了自动化特征，伪装成普通用户，提高了对反爬网站的穿透力。
//	动态支持 (Dynamic Content): 能够执行 JS 并等待渲染，解决了传统 HTTP 爬虫无法抓取“首屏后加载”内容的痛点。

// fetchWithChromedp fetches the HTML content with Chromedp. Uses host-resolver-rules to pin host to vp.PinnedIP (DNS rebinding protection).
func (t *WebFetchTool) fetchWithChromedp(ctx context.Context, vp *validatedParams) (string, error) {
	logger.Debugf(ctx, "[Tool][WebFetch] Chromedp 抓取开始 url=%s", vp.URL)

	// DNS pinning: force Chrome to use the IP we resolved at validation time, not a second resolution.
	hostRule := fmt.Sprintf("MAP %s %s", vp.Host, vp.PinnedIP.String())
	opts := append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("host-resolver-rules", hostRule),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-features", "VizDisplayCompositor"),
		chromedp.UserAgent(
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	ctx, cancel = chromedp.NewContext(allocCtx)
	defer cancel()

	ctx, cancel = context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	var html string
	err := chromedp.Run(ctx,
		chromedp.Navigate(vp.URL),                    // 1. 导航到 URL (受 DNS Pinning 保护)
		chromedp.WaitReady("body", chromedp.ByQuery), // 2. 等待 <body> 标签就绪 (确保页面已初步渲染)
		chromedp.OuterHTML("html", &html),            // 3. 获取整个 <html> 标签的内容
	)
	if err != nil {
		return "", fmt.Errorf("chromedp run failed: %w", err)
	}

	logger.Debugf(ctx, "[Tool][WebFetch] Chromedp 抓取成功 url=%s", vp.URL)
	return html, nil
}

// fetchWithHTTP fetches the HTML content with HTTP using pinned IP (same as chromedp path).
func (t *WebFetchTool) fetchWithHTTP(ctx context.Context, vp *validatedParams) (string, error) {
	resp, err := t.fetchWithTimeout(ctx, vp)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with status %d %s", resp.StatusCode, resp.Status)
	}

	limitedReader := io.LimitReader(resp.Body, webFetchMaxChars*2)
	htmlBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(htmlBytes), nil
}

// fetchWithTimeout fetches the HTML content with a timeout. Uses pinned IP and original Host header (DNS pinning).
func (t *WebFetchTool) fetchWithTimeout(ctx context.Context, vp *validatedParams) (*http.Response, error) {
	// Connect to pinned IP so we do not re-resolve; set Host so the server gets the right virtual host.
	hostPort := net.JoinHostPort(vp.PinnedIP.String(), vp.Port)
	rawURL := vp.URL
	u, _ := url.Parse(rawURL)
	u.Host = hostPort
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	// Preserve original host for TLS SNI and Host header (required for virtual hosting).
	req.Host = net.JoinHostPort(vp.Host, vp.Port)

	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; WebFetchTool/1.0)")
	req.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
	)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")

	return t.client.Do(req)
}

// 将杂乱无章、充满标签的 HTML 源码，转换为干净、结构化且适合 LLM 阅读的 Markdown 文本。
// 如果不做这个处理，直接把 HTML 发给 AI，会面临两个问题：
//	- Token 浪费：一个普通网页 HTML 可能有 10 万字符，其中 90% 都是标签和 JS 脚本。
//	- 注意力分散：AI 可能会被 CSS 样式名或复杂的 DOM 结构干扰，找不到正文。
//
//
// 数据解析：
// 	主路径: 使用 goquery (基于 jQuery 语法的 HTML 解析库) 将 HTML 字符串解析为 DOM 树。这使得后续可以使用类似 CSS 选择器的语法精准操作节点。
// 	兜底策略: 如果 HTML 严重损坏导致 goquery 解析失败，不直接报错，而是降级调用 t.basicTextExtraction(html)（通常是用正则简单提取可见文本）。
// 	总结: 这保证了即使网页代码不规范，工具也能尽可能提取出内容，而不是直接崩溃。
//
// 噪音清洗：
//	通过 doc.Find("").Remove() 直接删除网页中对 AI 毫无用处的部分：
//	- script, style：代码和样式（AI 不需要看这些）。
//	- nav, footer, header：导航栏、页脚、页眉。
//	这些内容在每个网页都一样，而且充满了无关链接，删掉它们能节省大量 Token 空间。
//
// 递归式 Markdown 转换:
//	从 <body> 标签开始，通过 processNode 函数递归地遍历每一个子节点，执行转换：
//		<h1> -> #
//		<p> -> 段落 + 换行
//		<ul>/<li> -> -
//		<a> -> [text](url)
//		<table> -> Markdown 表格
// 	LLM 在 Markdown 格式上的训练数据最多，理解能力最强，更容易识别哪些是标题，哪些是重点。
// 	将 HTML 转为 Markdown 能显著提升总结的准确性。
//
// 后处理与格式化
//	在转换过程中，可能会因为嵌套标签产生连续多个换行符（如 \n\n\n\n）。正则表达式将其统一压缩为标准的 双换行 (\n\n)，这是 Markdown 中段落的標準分隔符。
//  用 strings.TrimSpace 确保输出没有多余的开头/结尾空行，保持整洁。

// convertHTMLToText converts the HTML content to text
func (t *WebFetchTool) convertHTMLToText(html string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return t.basicTextExtraction(html)
	}

	doc.Find("script, style, nav, footer, header").Remove()

	var markdown strings.Builder
	doc.Find("body").Each(func(i int, body *goquery.Selection) {
		t.processNode(body, &markdown)
	})

	result := markdown.String()
	result = regexp.MustCompile(`\n{3,}`).ReplaceAllString(result, "\n\n")
	return strings.TrimSpace(result)
}

// processNode 通过递归遍历 HTML DOM 树，将各种 HTML 标签精准地“翻译”成对应的 Markdown 语法，是实现“从网页到 LLM 可读文本”转换的核心引擎。
// 它没有使用现成的库（如 html2text），而是手写了一套定制化的转换逻辑，这通常是为了获得更精细的控制（例如处理代码块、表格格式或特定的清洗规则）。

// 为什么 Markdown 对 AI 更友好？
// 	语义增强：AI 对 Markdown 的理解远强于对 HTML 的理解。加粗（**）和标题（#）能显著提高 AI 提取重点的准确率。
//	数据极简：剔除了 HTML 标签中的所有属性（如 class, style, id 等），这些对阅读毫无意义的字符占用了大量的 Token 空间。
//	格式对齐：通过将不同风格的网页统一转换成一种 Markdown 格式，你的 processWithLLM 函数（那个调用 AI 总结的步骤）就可以使用统一的 Prompt，不需要为每个网站单独写逻辑。

// processNode processes a node in the HTML content
func (t *WebFetchTool) processNode(s *goquery.Selection, markdown *strings.Builder) {
	s.Contents().Each(func(i int, node *goquery.Selection) {
		nodeName := goquery.NodeName(node)

		switch nodeName {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			headerLevel := int(nodeName[1] - '0')
			markdown.WriteString("\n")
			markdown.WriteString(strings.Repeat("#", headerLevel))
			markdown.WriteString(" ")
			markdown.WriteString(strings.TrimSpace(node.Text()))
			markdown.WriteString("\n\n")
		case "p":
			t.processNode(node, markdown)
			markdown.WriteString("\n\n")
		case "a":
			href, exists := node.Attr("href")
			text := strings.TrimSpace(node.Text())
			if exists && text != "" {
				markdown.WriteString("[")
				markdown.WriteString(text)
				markdown.WriteString("](")
				markdown.WriteString(href)
				markdown.WriteString(")")
			} else if text != "" {
				markdown.WriteString(text)
			}
		case "img":
			src, _ := node.Attr("src")
			alt, _ := node.Attr("alt")
			if src != "" {
				markdown.WriteString("![")
				markdown.WriteString(alt)
				markdown.WriteString("](")
				markdown.WriteString(src)
				markdown.WriteString(")\n\n")
			}
		case "ul", "ol":
			markdown.WriteString("\n")
			isOrdered := nodeName == "ol"
			node.Find("li").Each(func(idx int, li *goquery.Selection) {
				if isOrdered {
					fmt.Fprintf(markdown, "%d. ", idx+1)
				} else {
					markdown.WriteString("- ")
				}
				markdown.WriteString(strings.TrimSpace(li.Text()))
				markdown.WriteString("\n")
			})
			markdown.WriteString("\n")
		case "br":
			markdown.WriteString("\n")
		case "code":
			parent := node.Parent()
			if goquery.NodeName(parent) == "pre" {
				markdown.WriteString("\n```\n")
				markdown.WriteString(node.Text())
				markdown.WriteString("\n```\n\n")
			} else {
				markdown.WriteString("`")
				markdown.WriteString(node.Text())
				markdown.WriteString("`")
			}
		case "blockquote":
			lines := strings.Split(strings.TrimSpace(node.Text()), "\n")
			for _, line := range lines {
				markdown.WriteString("> ")
				markdown.WriteString(strings.TrimSpace(line))
				markdown.WriteString("\n")
			}
			markdown.WriteString("\n")
		case "strong", "b":
			markdown.WriteString("**")
			markdown.WriteString(strings.TrimSpace(node.Text()))
			markdown.WriteString("**")
		case "em", "i":
			markdown.WriteString("*")
			markdown.WriteString(strings.TrimSpace(node.Text()))
			markdown.WriteString("*")
		case "hr":
			markdown.WriteString("\n---\n\n")
		case "table":
			markdown.WriteString("\n")
			node.Find("tr").Each(func(idx int, tr *goquery.Selection) {
				tr.Find("th, td").Each(func(i int, cell *goquery.Selection) {
					markdown.WriteString("| ")
					markdown.WriteString(strings.TrimSpace(cell.Text()))
					markdown.WriteString(" ")
				})
				markdown.WriteString("|\n")
				if idx == 0 {
					tr.Find("th").Each(func(i int, _ *goquery.Selection) {
						markdown.WriteString("|---")
					})
					markdown.WriteString("|\n")
				}
			})
			markdown.WriteString("\n")
		case "#text":
			text := node.Text()
			if strings.TrimSpace(text) != "" {
				markdown.WriteString(text)
			}
		default:
			t.processNode(node, markdown)
		}
	})
}

// basicTextExtraction extracts the text from the HTML content
func (t *WebFetchTool) basicTextExtraction(html string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(html, " ")
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}
