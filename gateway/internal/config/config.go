package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

type Config struct {
	Port           int
	APIKey         string
	Model          string
	BaseURL        string
	RequestTimeout time.Duration
	MaxConcurrent  int
	MaxRetries     int
	TraceDir       string
	PriceIn        float64
	PriceOut       float64
	PriceCached    float64
}

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
	maxRetries, err := intEnv("GATEWAY_MAX_RETRIES", 2)
	if err != nil {
		return Config{}, err
	}
	if maxRetries < 0 {
		return Config{}, fmt.Errorf("GATEWAY_MAX_RETRIES 不能小于 0")
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

	return Config{
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
	}, nil
}

func (c Config) Address() string {
	return fmt.Sprintf(":%d", c.Port)
}

func loadRootEnv() error {
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
	if err != nil || parsed < 0 {
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

func stringEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
