package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/dotnode/gatelm/pkg/gatelm"
	"gopkg.in/yaml.v3"
)

// runInteractiveSetup runs an interactive wizard when no config file is found.
// It asks the user a few questions and writes a minimal config.yaml.
func runInteractiveSetup(configPath string) (*gatelm.Config, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("未找到配置文件，进入初始化引导...")
	fmt.Println()

	// 1. Backend URL
	backendURL := promptValidated(reader,
		"? 后端 API 地址",
		"",
		func(s string) error {
			if s == "" {
				return fmt.Errorf("不能为空")
			}
			u, err := url.Parse(s)
			if err != nil {
				return fmt.Errorf("无效 URL: %v", err)
			}
			if u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("请输入完整 URL (如 https://api.openai.com)")
			}
			return nil
		},
	)

	// 2. API Key
	apiKey := promptValidated(reader,
		"? API Key",
		"",
		func(s string) error {
			if s == "" {
				return fmt.Errorf("不能为空")
			}
			return nil
		},
	)

	// 3. Protocol
	fmt.Println()
	fmt.Println("  协议说明:")
	fmt.Println("    openai            - OpenAI Chat Completions API (/v1/chat/completions)")
	fmt.Println("    openai-responses  - OpenAI Responses API (/v1/responses)")
	fmt.Println("    anthropic         - Anthropic Messages API (/v1/messages)")
	protocol := promptValidated(reader,
		"? 后端协议 [openai]",
		"openai",
		func(s string) error {
			switch s {
			case "openai", "openai-responses", "anthropic":
				return nil
			default:
				return fmt.Errorf("请输入 openai / openai-responses / anthropic")
			}
		},
	)

	// 4. Listen port
	listenPort := promptValidated(reader,
		"? 监听端口 [18765]",
		"18765",
		func(s string) error {
			port, err := strconv.Atoi(s)
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("请输入 1-65535 之间的端口号")
			}
			return nil
		},
	)

	// 5. Console
	enableConsole := promptYN(reader, "? 是否开启管理台 (y/N)", false)

	var consolePassword string
	if enableConsole {
		consolePassword = promptDefault(reader, "? 管理台密码 (留空自动生成)", "")
		if consolePassword == "" {
			consolePassword = generatePassword(10)
			fmt.Printf("  -> 已生成密码: %s\n", consolePassword)
		}
	}

	cfg := &gatelm.Config{
		Listen: ":" + listenPort,
		Backends: []gatelm.Backend{
			{
				Name:             "default",
				URL:              backendURL,
				Protocol:         protocol,
				APIKey:           apiKey,
				AnthropicVersion: defaultAnthropicVersion(protocol),
				Default:          true,
				Weight:           1,
				Models:           []gatelm.Model{},
			},
		},
		Console: gatelm.ConsoleConfig{
			Enabled:  enableConsole,
			Password: consolePassword,
		},
	}

	// Write config to disk
	if err := writeConfigFile(configPath, cfg); err != nil {
		return nil, fmt.Errorf("写入配置文件失败: %w", err)
	}

	fmt.Println()
	fmt.Printf("✓ 配置已保存到 %s\n", configPath)

	return cfg, nil
}

// writeConfigFile marshals config to YAML and writes to disk.
// Mode 0600 because the file may contain backend API keys and the console password.
func writeConfigFile(path string, cfg *gatelm.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func defaultAnthropicVersion(protocol string) string {
	if protocol == "anthropic" {
		return "2023-06-01"
	}
	return ""
}

func prompt(reader *bufio.Reader, question string) string {
	fmt.Printf("%s: ", question)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptDefault(reader *bufio.Reader, question, defaultVal string) string {
	val := prompt(reader, question)
	if val == "" {
		return defaultVal
	}
	return val
}

func promptValidated(reader *bufio.Reader, question, defaultVal string, validate func(string) error) string {
	for {
		val := prompt(reader, question)
		if val == "" && defaultVal != "" {
			val = defaultVal
		}
		if err := validate(val); err != nil {
			fmt.Printf("  ✗ %s\n", err)
			continue
		}
		return val
	}
}

func promptYN(reader *bufio.Reader, question string, defaultYes bool) bool {
	val := prompt(reader, question)
	val = strings.ToLower(val)
	if val == "" {
		return defaultYes
	}
	return val == "y" || val == "yes"
}

func generatePassword(length int) string {
	b := make([]byte, length)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:length]
}
