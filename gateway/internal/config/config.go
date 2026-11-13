// 本文件读取并校验网关及模型服务配置，支持进程环境变量和根目录 .env。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

const (
	DefaultDailyBudgetYuan       = 100.0
	DefaultRequestMaxCostYuan    = 0.50
	DefaultBudgetMaxOutputTokens = 4096
)

// Config 保存启动网关及调用 Provider 所需的配置快照。
type Config struct {
	Provider               string
	Port                   int
	APIKey                 string
	Model                  string
	BaseURL                string
	RequestTimeout         time.Duration
	MaxConcurrent          int
	MaxRetries             int
	TraceDir               string
	PriceIn                float64
	PriceOut               float64
	PriceCached            float64
	RouteSimpleModel       string
	RouteComplexModel      string
	RouteContextThreshold  int
	SemanticCacheThreshold float64
	DailyBudgetYuan        float64
	RequestMaxCostYuan     float64
	BudgetMaxOutputTokens  int
	BudgetStateFile        string
	ToolWorkspace          string
	ToolsAllowWrites       bool
	ToolApprovalToken      string
	ProviderBackup         string
	FailoverThreshold      int
	AdminToken             string
}

// Load 读取 .env 与进程环境变量，校验参数后返回配置。
func Load() (Config, error) {
	if err := loadRootEnv(); err != nil {
		return Config{}, err
	}

	port, err := intEnv("GATEWAY_PORT", 8080)
	if err != nil {
		return Config{}, err
	}
	if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("GATEWAY_PORT 必须在 1 到 65535 之间")
	}
	maxConcurrent, err := intEnv("GATEWAY_MAX_CONCURRENCY", 200)
	if err != nil {
		return Config{}, err
	}
	if maxConcurrent < 1 {
		return Config{}, fmt.Errorf("GATEWAY_MAX_CONCURRENCY 必须大于 0")
	}
	maxRetries, err := intEnv("GATEWAY_MAX_RETRIES", 3)
	if err != nil {
		return Config{}, err
	}
	if maxRetries < 0 || maxRetries > 3 {
		return Config{}, fmt.Errorf("GATEWAY_MAX_RETRIES 必须在 0 到 3 之间")
	}
	timeout, err := timeEnv("GATEWAY_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	if timeout <= 0 {
		return Config{}, fmt.Errorf("GATEWAY_TIMEOUT 必须大于 0")
	}
	priceIn, err := floatEnv("ARK_PRICE_IN", 0.8)
	if err != nil {
		return Config{}, err
	}
	priceOut, err := floatEnv("ARK_PRICE_OUT", 3.2)
	if err != nil {
		return Config{}, err
	}
	priceCached, err := floatEnv("ARK_PRICE_CACHED", 0.016)
	if err != nil {
		return Config{}, err
	}
	routeContextThreshold, err := intEnv("GATEWAY_ROUTE_CONTEXT_THRESHOLD", 8192)
	if err != nil {
		return Config{}, err
	}
	if routeContextThreshold < 1 {
		return Config{}, fmt.Errorf("GATEWAY_ROUTE_CONTEXT_THRESHOLD 必须大于 0")
	}
	cacheThreshold, err := floatEnv("GATEWAY_SEMANTIC_CACHE_THRESHOLD", 0.95)
	if err != nil {
		return Config{}, err
	}
	if !(cacheThreshold > 0 && cacheThreshold <= 1) {
		return Config{}, fmt.Errorf("GATEWAY_SEMANTIC_CACHE_THRESHOLD 必须大于 0 且不超过 1")
	}
	dailyBudget, err := floatEnv("GATEWAY_DAILY_BUDGET_YUAN", DefaultDailyBudgetYuan)
	if err != nil {
		return Config{}, err
	}
	if dailyBudget <= 0 {
		return Config{}, fmt.Errorf("GATEWAY_DAILY_BUDGET_YUAN 必须大于 0")
	}
	requestMaxCost, err := floatEnv("GATEWAY_REQUEST_MAX_COST_YUAN", DefaultRequestMaxCostYuan)
	if err != nil {
		return Config{}, err
	}
	if requestMaxCost <= 0 {
		return Config{}, fmt.Errorf("GATEWAY_REQUEST_MAX_COST_YUAN 必须大于 0")
	}
	budgetMaxOutputTokens, err := intEnv("GATEWAY_BUDGET_MAX_OUTPUT_TOKENS", DefaultBudgetMaxOutputTokens)
	if err != nil {
		return Config{}, err
	}
	if budgetMaxOutputTokens < 1 || budgetMaxOutputTokens > 1_000_000 {
		return Config{}, fmt.Errorf("GATEWAY_BUDGET_MAX_OUTPUT_TOKENS 必须在 1 到 1000000 之间")
	}
	toolsAllowWrites, err := boolEnv("GATEWAY_TOOLS_ALLOW_WRITES", false)
	if err != nil {
		return Config{}, err
	}
	toolApprovalToken := stringEnv("GATEWAY_TOOL_APPROVAL_TOKEN", "")
	if toolsAllowWrites && len(toolApprovalToken) < 32 {
		return Config{}, fmt.Errorf("启用 GATEWAY_TOOLS_ALLOW_WRITES 时 GATEWAY_TOOL_APPROVAL_TOKEN 至少需要 32 个字符")
	}
	providerBackup := stringEnv("GATEWAY_PROVIDER_BACKUP", "")
	failoverThreshold, err := intEnv("GATEWAY_FAILOVER_THRESHOLD", 3)
	if err != nil {
		return Config{}, err
	}
	if failoverThreshold < 1 || failoverThreshold > 10 {
		return Config{}, fmt.Errorf("GATEWAY_FAILOVER_THRESHOLD 必须在 1 到 10 之间")
	}
	adminToken := stringEnv("GATEWAY_ADMIN_TOKEN", "")
	if len(adminToken) > 0 && len(adminToken) < 32 {
		return Config{}, fmt.Errorf("设置 GATEWAY_ADMIN_TOKEN 时至少需要 32 个字符")
	}

	return Config{
		Provider:       stringEnv("GATEWAY_PROVIDER", "doubao"),
		Port:           port,
		APIKey:         os.Getenv("ARK_API_KEY"),
		Model:          os.Getenv("ARK_MODEL"),
		BaseURL:        stringEnv("ARK_BASE_URL", defaultBaseURL),
		RequestTimeout: timeout,
		MaxConcurrent:  maxConcurrent,
		MaxRetries:     maxRetries,
		TraceDir:       stringEnv("GATEWAY_TRACE_DIR", "traces"),
		PriceIn:        priceIn,
		PriceOut:       priceOut,
		PriceCached:    priceCached,
		RouteSimpleModel: stringEnv(
			"GATEWAY_ROUTE_SIMPLE_MODEL",
			stringEnv("ARK_MODEL_MINI", os.Getenv("ARK_MODEL")),
		),
		RouteComplexModel:      stringEnv("GATEWAY_ROUTE_COMPLEX_MODEL", os.Getenv("ARK_MODEL")),
		RouteContextThreshold:  routeContextThreshold,
		SemanticCacheThreshold: cacheThreshold,
		DailyBudgetYuan:        dailyBudget,
		RequestMaxCostYuan:     requestMaxCost,
		BudgetMaxOutputTokens:  budgetMaxOutputTokens,
		BudgetStateFile:        stringEnv("GATEWAY_BUDGET_STATE_FILE", "traces/budget.json"),
		ToolWorkspace:          stringEnv("GATEWAY_TOOL_WORKSPACE", "tool-workspace"),
		ToolsAllowWrites:       toolsAllowWrites,
		ToolApprovalToken:      toolApprovalToken,
		ProviderBackup:         providerBackup,
		FailoverThreshold:      failoverThreshold,
		AdminToken:             adminToken,
	}, nil
}

// Address 返回供 HTTP server 监听的 host-port 地址。
func (c Config) Address() string {
	return fmt.Sprintf(":%d", c.Port)
}

func loadRootEnv() error {
	// 从 gateway/ 或仓库根目录启动都能找到根 .env；已存在的进程环境变量优先。
	for _, path := range []string{".env", filepath.Join("..", ".env")} {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取 %s 失败：%w", path, err)
		}
		parseErr := parseEnv(file)
		closeErr := file.Close()
		if parseErr != nil {
			return fmt.Errorf("解析 %s 失败：%w", path, parseErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 %s 失败：%w", path, closeErr)
		}
		return nil
	}
	return nil
}

// parseEnv 解析简单 .env 语法，且不覆盖已由进程设置的变量。
func parseEnv(file *os.File) error {
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("第 %d 行缺少等号", lineNo)
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("第 %d 行变量名为空", lineNo)
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("设置 %s 失败：%w", key, err)
			}
		}
	}
	return scanner.Err()
}

func intEnv(key string, fallback int) (int, error) {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数：%w", key, err)
	}
	return parsed, nil
}

func floatEnv(key string, fallback float64) (float64, error) {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s 必须是非负数", key)
	}
	return parsed, nil
}

func timeEnv(key string, fallback time.Duration) (time.Duration, error) {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是有效时长（例如 30s）：%w", key, err)
	}
	return parsed, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s 必须是 true 或 false：%w", key, err)
	}
	return parsed, nil
}

func stringEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
