// Package standalone implements MCP Server for Nginx Migration Tools in standalone mode.
package standalone

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"nginx-migration-mcp/internal/rag"
	"nginx-migration-mcp/tools"
)

// NewMCPServer creates a new MCP server instance
func NewMCPServer(config *ServerConfig) *MCPServer {
	// 初始化 RAG 管理器
	// 获取可执行文件所在目录
	execPath, err := os.Executable()
	if err != nil {
		log.Printf("WARNING: Failed to get executable path: %v", err)
		execPath = "."
	}
	execDir := filepath.Dir(execPath)

	// 尝试多个可能的配置文件路径（相对于可执行文件）
	ragConfigPaths := []string{
		filepath.Join(execDir, "config", "rag.json"),       // 同级 config 目录
		filepath.Join(execDir, "..", "config", "rag.json"), // 上级 config 目录
		"config/rag.json", // 当前工作目录
	}

	var ragConfig *rag.RAGConfig
	var configErr error

	for _, path := range ragConfigPaths {
		ragConfig, configErr = rag.LoadRAGConfig(path)
		if configErr == nil {
			log.Printf("Loaded RAG config from: %s", path)
			break
		}
	}

	if configErr != nil {
		log.Printf("WARNING: Failed to load RAG config: %v, RAG will be disabled", configErr)
		ragConfig = &rag.RAGConfig{Enabled: false}
	}

	ragManager := rag.NewRAGManager(ragConfig)

	if ragManager.IsEnabled() {
		log.Printf("RAG Manager initialized and enabled")
	} else {
		log.Printf("RAG Manager disabled, using rule-based approach")
	}

	return &MCPServer{
		config:     config,
		ragManager: ragManager,
	}
}

// HandleMessage processes an incoming MCP message
func (s *MCPServer) HandleMessage(msg MCPMessage) MCPMessage {
	switch msg.Method {
	case "initialize":
		return s.handleInitialize(msg)
	case "tools/list":
		return s.handleToolsList(msg)
	case "tools/call":
		return s.handleToolsCall(msg)
	default:
		return s.errorResponse(msg.ID, -32601, "Method not found")
	}
}

func (s *MCPServer) handleInitialize(msg MCPMessage) MCPMessage {
	// Get requested protocol version from client
	requestedVersion := "2024-11-05" // default
	if params, ok := msg.Params.(map[string]interface{}); ok {
		if pv, ok := params["protocolVersion"].(string); ok {
			requestedVersion = pv
		}
	}

	// Supported protocol versions (newest first)
	supportedVersions := []string{"2025-06-18", "2025-03-26", "2024-11-05"}

	// Find the best matching version (use requested if supported, otherwise latest supported)
	selectedVersion := supportedVersions[0] // default to latest
	for _, v := range supportedVersions {
		if v == requestedVersion {
			selectedVersion = requestedVersion
			break
		}
	}

	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"protocolVersion": selectedVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{
					"listChanged": true,
				},
			},
			"serverInfo": map[string]interface{}{
				"name":    s.config.Server.Name,
				"version": s.config.Server.Version,
			},
		},
	}
}

func (s *MCPServer) handleToolsList(msg MCPMessage) MCPMessage {
	toolsList := tools.GetMCPTools()

	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"tools": toolsList,
		},
	}
}

func (s *MCPServer) handleToolsCall(msg MCPMessage) MCPMessage {
	var params CallToolParams
	paramsBytes, _ := json.Marshal(msg.Params)
	json.Unmarshal(paramsBytes, &params)

	handlers := tools.GetToolHandlers(s)
	handler, exists := handlers[params.Name]

	if !exists {
		return s.errorResponse(msg.ID, -32601, fmt.Sprintf("Unknown tool: %s", params.Name))
	}

	result := handler(params.Arguments)

	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

func (s *MCPServer) errorResponse(id interface{}, code int, message string) MCPMessage {
	return MCPMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    code,
			Message: message,
		},
	}
}

// Tool implementations

func (s *MCPServer) ParseNginxConfig(args map[string]interface{}) tools.ToolResult {
	configContent, ok := args["config_content"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing config_content"}}}
	}

	serverCount := strings.Count(configContent, "server {")
	locationCount := strings.Count(configContent, "location")
	hasSSL := strings.Contains(configContent, "ssl")
	hasProxy := strings.Contains(configContent, "proxy_pass")
	hasRewrite := strings.Contains(configContent, "rewrite")

	complexity := "Simple"
	if serverCount > 1 || (hasRewrite && hasSSL) {
		complexity = "Complex"
	} else if hasRewrite || hasSSL {
		complexity = "Medium"
	}

	analysis := fmt.Sprintf(`Nginx配置分析结果

基础信息:
- Server块: %d个
- Location块: %d个  
- SSL配置: %t
- 反向代理: %t
- URL重写: %t

复杂度: %s

迁移建议:`, serverCount, locationCount, hasSSL, hasProxy, hasRewrite, complexity)

	if hasProxy {
		analysis += "\n- 反向代理将转换为Ingress backend配置"
	}
	if hasRewrite {
		analysis += "\n- URL重写将使用Higress注解 (higress.io/rewrite-target)"
	}
	if hasSSL {
		analysis += "\n- SSL配置将转换为Ingress TLS配置"
	}

	return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: analysis}}}
}

func (s *MCPServer) ConvertToHigress(args map[string]interface{}) tools.ToolResult {
	configContent, ok := args["config_content"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing config_content"}}}
	}

	namespace := s.config.Defaults.Namespace
	if ns, ok := args["namespace"].(string); ok {
		namespace = ns
	}

	// 检查是否使用 Gateway API
	useGatewayAPI := false
	if val, ok := args["use_gateway_api"].(bool); ok {
		useGatewayAPI = val
	}

	// ===  使用增强的解析器解析 Nginx 配置 ===
	nginxConfig, err := tools.ParseNginxConfig(configContent)
	if err != nil {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: fmt.Sprintf("Error parsing Nginx config: %v", err)}}}
	}

	// 分析配置
	analysis := tools.AnalyzeNginxConfig(nginxConfig)

	// === RAG 增强：查询转换示例和最佳实践 ===
	var ragContext string
	if s.ragManager != nil && s.ragManager.IsEnabled() {
		// 构建查询关键词
		queryBuilder := []string{"Nginx 配置转换到 Higress"}

		if useGatewayAPI {
			queryBuilder = append(queryBuilder, "Gateway API HTTPRoute")
		} else {
			queryBuilder = append(queryBuilder, "Kubernetes Ingress")
		}

		// 根据特性添加查询关键词
		if analysis.Features["ssl"] {
			queryBuilder = append(queryBuilder, "SSL TLS 证书配置")
		}
		if analysis.Features["rewrite"] {
			queryBuilder = append(queryBuilder, "URL 重写 rewrite 规则")
		}
		if analysis.Features["redirect"] {
			queryBuilder = append(queryBuilder, "重定向 redirect")
		}
		if analysis.Features["header_manipulation"] {
			queryBuilder = append(queryBuilder, "请求头 响应头处理")
		}
		if len(nginxConfig.Upstreams) > 0 {
			queryBuilder = append(queryBuilder, "负载均衡 upstream")
		}

		queryString := strings.Join(queryBuilder, " ")
		log.Printf("RAG Query: %s", queryString)

		ragResult, err := s.ragManager.QueryForTool(
			"convert_to_higress",
			queryString,
			"nginx_to_higress",
		)

		if err == nil && ragResult.Enabled && len(ragResult.Documents) > 0 {
			log.Printf("RAG: Found %d documents for conversion", len(ragResult.Documents))
			ragContext = "\n\n## 参考文档（来自知识库）\n\n" + ragResult.FormatContextForAI()
		} else {
			if err != nil {
				log.Printf("WARNING: RAG query failed: %v", err)
			}
		}
	}

	// === 将配置数据转换为 JSON 供 AI 使用 ===
	configJSON, _ := json.MarshalIndent(nginxConfig, "", "  ")
	analysisJSON, _ := json.MarshalIndent(analysis, "", "  ")

	// === 构建返回消息 ===
	userMessage := fmt.Sprintf(`📋 Nginx 配置解析完成

## 配置概览
- Server 块: %d
- Location 块: %d
- 域名: %d 个
- 复杂度: %s
- 目标格式: %s
- 命名空间: %s

## 检测到的特性
%s

## 迁移建议
%s
%s

---

## Nginx 配置结构

`+"```json"+`
%s
`+"```"+`

## 分析结果

`+"```json"+`
%s
`+"```"+`
%s
`,
		analysis.ServerCount,
		analysis.LocationCount,
		analysis.DomainCount,
		analysis.Complexity,
		func() string {
			if useGatewayAPI {
				return "Gateway API (HTTPRoute)"
			}
			return "Kubernetes Ingress"
		}(),
		namespace,
		formatFeatures(analysis.Features),
		formatSuggestions(analysis.Suggestions),
		func() string {
			if ragContext != "" {
				return "\n\n已加载知识库参考文档"
			}
			return ""
		}(),
		string(configJSON),
		string(analysisJSON),
		ragContext,
	)

	return tools.FormatToolResultWithAIContext(userMessage, "", map[string]interface{}{
		"nginx_config":    nginxConfig,
		"analysis":        analysis,
		"namespace":       namespace,
		"use_gateway_api": useGatewayAPI,
	})
}

func (s *MCPServer) AnalyzeLuaPlugin(args map[string]interface{}) tools.ToolResult {
	luaCode, ok := args["lua_code"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing lua_code"}}}
	}

	// 使用新的 AI 友好分析
	analysis := tools.AnalyzeLuaPluginForAI(luaCode)

	// === RAG 增强：查询知识库获取转换建议 ===
	var ragContext string
	if s.ragManager != nil && s.ragManager.IsEnabled() && len(analysis.APICalls) > 0 {
		query := fmt.Sprintf("Nginx Lua API %s 在 Higress WASM 中的转换方法和最佳实践", strings.Join(analysis.APICalls, ", "))
		log.Printf("🔍 RAG Query: %s", query)

		ragResult, err := s.ragManager.QueryForTool("analyze_lua_plugin", query, "lua_migration")
		if err == nil && ragResult.Enabled && len(ragResult.Documents) > 0 {
			log.Printf("RAG: Found %d documents for Lua analysis", len(ragResult.Documents))
			ragContext = "\n\n##  知识库参考资料\n\n" + ragResult.FormatContextForAI()
		} else if err != nil {
			log.Printf(" RAG query failed: %v", err)
		}
	}

	// 生成用户友好的消息
	features := []string{}
	for feature := range analysis.Features {
		features = append(features, fmt.Sprintf("- %s", feature))
	}

	userMessage := fmt.Sprintf(`Lua 插件分析完成

## 检测到的特性
%s

## 基本信息
- **复杂度**: %s
- **兼容性**: %s

## 兼容性警告
%s
%s

## 后续操作
- 调用 generate_conversion_hints 获取转换提示
- 或直接使用 convert_lua_to_wasm 一键转换

## 分析结果

`+"```json"+`
%s
`+"```"+`
`,
		strings.Join(features, "\n"),
		analysis.Complexity,
		analysis.Compatibility,
		func() string {
			if len(analysis.Warnings) > 0 {
				return "- " + strings.Join(analysis.Warnings, "\n- ")
			}
			return "无"
		}(),
		ragContext,
		string(mustMarshalJSON(analysis)),
	)

	return tools.FormatToolResultWithAIContext(userMessage, "", analysis)
}

func mustMarshalJSON(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}

func (s *MCPServer) ConvertLuaToWasm(args map[string]interface{}) tools.ToolResult {
	luaCode, ok := args["lua_code"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing lua_code"}}}
	}

	pluginName, ok := args["plugin_name"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing plugin_name"}}}
	}

	analyzer := tools.AnalyzeLuaScript(luaCode)
	result, err := tools.ConvertLuaToWasm(analyzer, pluginName)
	if err != nil {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}}}
	}

	response := fmt.Sprintf(`Lua脚本转换完成

转换分析:
- 复杂度: %s
- 检测特性: %d个
- 兼容性警告: %d个

注意事项:
%s

生成的文件:

==== main.go ====
%s

==== WasmPlugin配置 ====
%s

部署步骤:
1. 创建插件目录: mkdir -p extensions/%s
2. 保存Go代码到: extensions/%s/main.go  
3. 构建插件: PLUGIN_NAME=%s make build
4. 应用配置: kubectl apply -f wasmplugin.yaml

提示:
- 请根据实际需求调整配置
- 测试插件功能后再部署到生产环境
- 如有共享状态需求，请配置Redis等外部存储
`,
		analyzer.Complexity,
		len(analyzer.Features),
		len(analyzer.Warnings),
		strings.Join(analyzer.Warnings, "\n- "),
		result.GoCode,
		result.WasmPluginYAML,
		pluginName, pluginName, pluginName)

	return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: response}}}
}

// GenerateConversionHints 生成详细的代码转换提示
func (s *MCPServer) GenerateConversionHints(args map[string]interface{}) tools.ToolResult {
	analysisResultStr, ok := args["analysis_result"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing analysis_result"}}}
	}

	pluginName, ok := args["plugin_name"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing plugin_name"}}}
	}

	// 解析分析结果
	var analysis tools.AnalysisResultForAI
	if err := json.Unmarshal([]byte(analysisResultStr), &analysis); err != nil {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: fmt.Sprintf("Error parsing analysis_result: %v", err)}}}
	}

	// 生成转换提示
	hints := tools.GenerateConversionHints(analysis, pluginName)

	// === RAG 增强：查询 Nginx API 转换文档 ===
	var ragDocs string

	// 构建更精确的查询语句
	queryBuilder := []string{}
	if len(analysis.APICalls) > 0 {
		queryBuilder = append(queryBuilder, "Nginx Lua API 转换到 Higress WASM")

		// 针对不同的 API 类型使用不同的查询关键词
		hasHeaderOps := analysis.Features["header_manipulation"] || analysis.Features["request_headers"] || analysis.Features["response_headers"]
		hasBodyOps := analysis.Features["request_body"] || analysis.Features["response_body"]
		hasResponseControl := analysis.Features["response_control"]

		if hasHeaderOps {
			queryBuilder = append(queryBuilder, "请求头和响应头处理")
		}
		if hasBodyOps {
			queryBuilder = append(queryBuilder, "请求体和响应体处理")
		}
		if hasResponseControl {
			queryBuilder = append(queryBuilder, "响应控制和状态码设置")
		}

		// 添加具体的 API 调用
		if len(analysis.APICalls) > 0 && len(analysis.APICalls) <= 5 {
			queryBuilder = append(queryBuilder, fmt.Sprintf("涉及 API: %s", strings.Join(analysis.APICalls, ", ")))
		}
	} else {
		queryBuilder = append(queryBuilder, "Higress WASM 插件开发 基础示例 Go SDK 使用")
	}

	// 添加复杂度相关的查询
	if analysis.Complexity == "high" {
		queryBuilder = append(queryBuilder, "复杂插件实现 高级功能")
	}

	queryString := strings.Join(queryBuilder, " ")

	// 只有当 RAG 启用时才查询
	if s.ragManager != nil && s.ragManager.IsEnabled() {
		log.Printf(" RAG Query: %s", queryString)

		ragContext, err := s.ragManager.QueryForTool(
			"generate_conversion_hints",
			queryString,
			"lua_migration",
		)

		if err == nil && ragContext.Enabled && len(ragContext.Documents) > 0 {
			log.Printf("RAG: Found %d documents for conversion hints", len(ragContext.Documents))
			ragDocs = "\n\n##  参考文档（来自知识库）\n\n" + ragContext.FormatContextForAI()
		} else {
			if err != nil {
				log.Printf("  RAG query failed: %v", err)
			}
			ragDocs = ""
		}
	} else {
		ragDocs = ""
	}

	// 格式化输出
	userMessage := fmt.Sprintf(` 代码转换提示

**插件名称**: %s
**复杂度**: %s
**兼容性**: %s
%s

## 代码模板

%s
%s
`,
		pluginName,
		analysis.Complexity,
		analysis.Compatibility,
		func() string {
			if len(hints.Warnings) > 0 {
				return "\n**警告**: " + formatWarningsListForUser(hints.Warnings)
			}
			return ""
		}(),
		hints.CodeTemplate,
		ragDocs,
	)

	return tools.FormatToolResultWithAIContext(userMessage, "", hints)
}

// ValidateWasmCode 验证生成的 Go WASM 代码
func (s *MCPServer) ValidateWasmCode(args map[string]interface{}) tools.ToolResult {
	goCode, ok := args["go_code"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing go_code"}}}
	}

	pluginName, ok := args["plugin_name"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing plugin_name"}}}
	}

	// 执行验证
	report := tools.ValidateWasmCode(goCode, pluginName)

	// 统计各类问题数量
	requiredCount := 0
	recommendedCount := 0
	optionalCount := 0
	bestPracticeCount := 0

	for _, issue := range report.Issues {
		switch issue.Category {
		case "required":
			requiredCount++
		case "recommended":
			recommendedCount++
		case "optional":
			optionalCount++
		case "best_practice":
			bestPracticeCount++
		}
	}

	// 构建用户消息
	userMessage := fmt.Sprintf(`##  代码验证报告

%s

### 发现的回调函数 (%d 个)
%s

### 配置结构
%s

### 问题分类

####  必须修复 (%d 个)
%s

####  建议修复 (%d 个)
%s

####  可选优化 (%d 个)
%s

####  最佳实践 (%d 个)
%s

### 缺失的导入包 (%d 个)
%s

---

`,
		report.Summary,
		len(report.FoundCallbacks),
		formatCallbacksList(report.FoundCallbacks),
		formatConfigStatus(report.HasConfig),
		requiredCount,
		formatIssuesByCategory(report.Issues, "required"),
		recommendedCount,
		formatIssuesByCategory(report.Issues, "recommended"),
		optionalCount,
		formatIssuesByCategory(report.Issues, "optional"),
		bestPracticeCount,
		formatIssuesByCategory(report.Issues, "best_practice"),
		len(report.MissingImports),
		formatList(report.MissingImports),
	)

	// === RAG 增强：查询最佳实践和代码规范 ===
	var ragBestPractices string

	// 根据验证结果构建更针对性的查询
	queryBuilder := []string{"Higress WASM 插件"}

	// 根据发现的问题类型添加关键词
	if requiredCount > 0 || recommendedCount > 0 {
		queryBuilder = append(queryBuilder, "常见错误")

		// 检查具体问题类型
		for _, issue := range report.Issues {
			switch issue.Type {
			case "error_handling":
				queryBuilder = append(queryBuilder, "错误处理")
			case "api_usage":
				queryBuilder = append(queryBuilder, "API 使用规范")
			case "config":
				queryBuilder = append(queryBuilder, "配置解析")
			case "logging":
				queryBuilder = append(queryBuilder, "日志记录")
			}
		}
	} else {
		// 代码已通过基础验证，查询优化建议
		queryBuilder = append(queryBuilder, "性能优化 最佳实践")
	}

	// 根据回调函数类型添加特定查询
	for _, callback := range report.FoundCallbacks {
		if strings.Contains(callback, "RequestHeaders") {
			queryBuilder = append(queryBuilder, "请求头处理")
		}
		if strings.Contains(callback, "RequestBody") {
			queryBuilder = append(queryBuilder, "请求体处理")
		}
		if strings.Contains(callback, "ResponseHeaders") {
			queryBuilder = append(queryBuilder, "响应头处理")
		}
	}

	// 如果有缺失的导入，查询包管理相关信息
	if len(report.MissingImports) > 0 {
		queryBuilder = append(queryBuilder, "依赖包导入")
	}

	queryString := strings.Join(queryBuilder, " ")

	// 只有当 RAG 启用时才查询
	if s.ragManager != nil && s.ragManager.IsEnabled() {
		log.Printf("RAG Query: %s", queryString)

		ragContext, err := s.ragManager.QueryForTool(
			"validate_wasm_code",
			queryString,
			"best_practice",
		)

		if err == nil && ragContext.Enabled && len(ragContext.Documents) > 0 {
			log.Printf("RAG: Found %d best practice documents", len(ragContext.Documents))
			ragBestPractices = "\n\n###  最佳实践建议（来自知识库）\n\n" + ragContext.FormatContextForAI()
			userMessage += ragBestPractices
		} else {
			if err != nil {
				log.Printf("  RAG query failed for validation: %v", err)
			}
		}
	}

	// 根据问题级别给出建议
	hasRequired := requiredCount > 0
	if hasRequired {
		userMessage += "\n **请优先修复 \"必须修复\" 的问题**\n\n"
	} else if recommendedCount > 0 {
		userMessage += "\n **代码基本结构正确**，建议修复 \"建议修复\" 的问题\n\n"
	} else {
		userMessage += "\n **代码验证通过！** 可以调用 `generate_deployment_config` 生成部署配置\n\n"
	}

	return tools.FormatToolResultWithAIContext(userMessage, "", report)
}

// GenerateDeploymentConfig 生成部署配置
func (s *MCPServer) GenerateDeploymentConfig(args map[string]interface{}) tools.ToolResult {
	pluginName, ok := args["plugin_name"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing plugin_name"}}}
	}

	goCode, ok := args["go_code"].(string)
	if !ok {
		return tools.ToolResult{Content: []tools.Content{{Type: "text", Text: "Error: Missing go_code"}}}
	}

	namespace := "higress-system"
	if ns, ok := args["namespace"].(string); ok && ns != "" {
		namespace = ns
	}

	configSchema := ""
	if cs, ok := args["config_schema"].(string); ok {
		configSchema = cs
	}

	// 生成部署包
	pkg := tools.GenerateDeploymentPackage(pluginName, goCode, configSchema, namespace)

	// 格式化输出
	userMessage := fmt.Sprintf(`🎉 部署配置生成完成！

插件 **%s** 的部署配置已生成（命名空间: %s）

## 生成的文件

1. **wasmplugin.yaml** - WasmPlugin 配置
2. **Makefile** - 构建和部署脚本
3. **Dockerfile** - 容器化打包
4. **README.md** - 使用文档
5. **test.sh** - 测试脚本

## 快速部署

`+"```bash"+`
# 构建插件
make build

# 构建并推送镜像
make docker-build docker-push

# 部署
make deploy

# 验证
kubectl get wasmplugin -n %s
`+"```"+`

## 配置文件

### wasmplugin.yaml
`+"```yaml"+`
%s
`+"```"+`

### Makefile
`+"```makefile"+`
%s
`+"```"+`

### Dockerfile
`+"```dockerfile"+`
%s
`+"```"+`

### README.md
`+"```markdown"+`
%s
`+"```"+`

### test.sh
`+"```bash"+`
%s
`+"```"+`
`,
		pluginName,
		namespace,
		namespace,
		pkg.WasmPluginYAML,
		pkg.Makefile,
		pkg.Dockerfile,
		pkg.README,
		pkg.TestScript,
	)

	return tools.FormatToolResultWithAIContext(userMessage, "", pkg)
}

// 辅助格式化函数

func formatWarningsListForUser(warnings []string) string {
	if len(warnings) == 0 {
		return "无"
	}
	return strings.Join(warnings, "\n- ")
}

func formatCallbacksList(callbacks []string) string {
	if len(callbacks) == 0 {
		return "无"
	}
	return "- " + strings.Join(callbacks, "\n- ")
}

func formatConfigStatus(hasConfig bool) string {
	if hasConfig {
		return " 已定义配置结构体"
	}
	return "- 未定义配置结构体（如不需要配置可忽略）"
}

func formatIssuesByCategory(issues []tools.ValidationIssue, category string) string {
	var filtered []string
	for _, issue := range issues {
		if issue.Category == category {
			filtered = append(filtered, fmt.Sprintf("- **[%s]** %s\n  💡 建议: %s\n  📌 影响: %s",
				issue.Type, issue.Message, issue.Suggestion, issue.Impact))
		}
	}
	if len(filtered) == 0 {
		return "无"
	}
	return strings.Join(filtered, "\n\n")
}

func formatList(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return "- " + strings.Join(items, "\n- ")
}

// formatFeatures 格式化特性列表
func formatFeatures(features map[string]bool) string {
	featureNames := map[string]string{
		"ssl":                 "SSL/TLS 加密",
		"proxy":               "反向代理",
		"rewrite":             "URL 重写",
		"redirect":            "重定向",
		"return":              "返回指令",
		"complex_routing":     "复杂路由匹配",
		"header_manipulation": "请求头操作",
		"response_headers":    "响应头操作",
	}

	var result []string
	for key, enabled := range features {
		if enabled {
			if name, ok := featureNames[key]; ok {
				result = append(result, fmt.Sprintf("- %s", name))
			} else {
				result = append(result, fmt.Sprintf("- %s", key))
			}
		}
	}

	if len(result) == 0 {
		return "- 基础配置（无特殊特性）"
	}
	return strings.Join(result, "\n")
}

// formatSuggestions 格式化建议列表
func formatSuggestions(suggestions []string) string {
	if len(suggestions) == 0 {
		return "- 无特殊建议"
	}
	var result []string
	for _, s := range suggestions {
		result = append(result, fmt.Sprintf("- 💡 %s", s))
	}
	return strings.Join(result, "\n")
}
