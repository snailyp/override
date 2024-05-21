package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http2"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const InstructModel = "deepseek-coder"

// 导入的包在这里不做注释

type config struct {
	// config结构体用于存储配置信息
	Bind                 string            `json:"bind"`                   // 监听地址
	ProxyUrl             string            `json:"proxy_url"`              // 代理URL
	Timeout              int               `json:"timeout"`                // 请求超时时间
	CodexApiBase         string            `json:"codex_api_base"`         // Codex API的基础URL
	CodexApiKey          string            `json:"codex_api_key"`          // Codex API的密钥
	CodexApiOrganization string            `json:"codex_api_organization"` // Codex API的组织
	CodexApiProject      string            `json:"codex_api_project"`      // Codex API的项目
	ChatApiBase          string            `json:"chat_api_base"`          // Chat API的基础URL
	ChatApiKey           string            `json:"chat_api_key"`           // Chat API的密钥
	ChatApiOrganization  string            `json:"chat_api_organization"`  // Chat API的组织
	ChatApiProject       string            `json:"chat_api_project"`       // Chat API的项目
	ChatModelDefault     string            `json:"chat_model_default"`     // 默认的Chat模型
	ChatModelMap         map[string]string `json:"chat_model_map"`         // Chat模型映射
	ChatMaxTokens        int               `json:"chat_max_tokens"`
	ChatLocale           string            `json:"chat_locale"`
}

// readConfig用于读取配置文件并返回config结构体实例
func readConfig() *config {
	// 读取配置文件
	content, err := os.ReadFile("config.json")
	if nil != err {
		log.Fatal(err)
	}

	_cfg := &config{}
	// 解析配置文件内容到config结构体
	err = json.Unmarshal(content, &_cfg)
	if nil != err {
		log.Fatal(err)
	}

	v := reflect.ValueOf(_cfg).Elem()
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}

		value, exists := os.LookupEnv("OVERRIDE_" + strings.ToUpper(tag))
		if !exists {
			continue
		}

		switch field.Kind() {
		case reflect.String:
			field.SetString(value)
		case reflect.Bool:
			if boolValue, err := strconv.ParseBool(value); err == nil {
				field.SetBool(boolValue)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
				field.SetInt(intValue)
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if uintValue, err := strconv.ParseUint(value, 10, 64); err == nil {
				field.SetUint(uintValue)
			}
		case reflect.Float32, reflect.Float64:
			if floatValue, err := strconv.ParseFloat(value, field.Type().Bits()); err == nil {
				field.SetFloat(floatValue)
			}
		}
	}

	return _cfg
}

// getClient用于根据配置创建并返回一个HTTP客户端实例
func getClient(cfg *config) (*http.Client, error) {
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		DisableKeepAlives: false,
	}

	// 配置HTTP/2
	err := http2.ConfigureTransport(transport)
	if nil != err {
		return nil, err
	}

	// 如果配置了代理URL，则设置代理
	if "" != cfg.ProxyUrl {
		proxyUrl, err := url.Parse(cfg.ProxyUrl)
		if nil != err {
			return nil, err
		}

		transport.Proxy = http.ProxyURL(proxyUrl)
	}

	// 创建HTTP客户端实例
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.Timeout) * time.Second,
	}

	return client, nil
}

// abortCodex用于中断Codex的请求处理
func abortCodex(c *gin.Context, status int) {
	// 设置响应类型为text/event-stream
	c.Header("Content-Type", "text/event-stream")

	// 发送DONE信号并中断处理
	c.String(status, "data: [DONE]\n")
	c.Abort()
}

// closeIO用于关闭io.Closer类型的实例
func closeIO(c io.Closer) {
	// 关闭资源并记录错误
	err := c.Close()
	if nil != err {
		log.Println(err)
	}
}

// ProxyService定义了代理服务的相关方法和属性
type ProxyService struct {
	cfg    *config      // 配置信息
	client *http.Client // HTTP客户端实例
}

// NewProxyService用于创建一个新的ProxyService实例
func NewProxyService(cfg *config) (*ProxyService, error) {
	client, err := getClient(cfg)
	if nil != err {
		return nil, err
	}

	return &ProxyService{
		cfg:    cfg,
		client: client,
	}, nil
}

// InitRoutes用于初始化ProxyService的路由
func (s *ProxyService) InitRoutes(e *gin.Engine) {
	// 绑定POST请求处理函数
	e.POST("/v1/chat/completions", s.completions)
	e.POST("/v1/engines/copilot-codex/completions", s.codeCompletions)
}

// completions处理聊天模型的完成请求
func (s *ProxyService) completions(c *gin.Context) {
	ctx := c.Request.Context()

	// 读取请求体
	body, err := io.ReadAll(c.Request.Body)
	if nil != err {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// 处理模型映射
	model := gjson.GetBytes(body, "model").String()
	if mapped, ok := s.cfg.ChatModelMap[model]; ok {
		model = mapped
	} else {
		model = s.cfg.ChatModelDefault
	}
	// 更新请求体中的模型字段
	body, _ = sjson.SetBytes(body, "model", model)
	// 删除请求体中的intent字段

	if !gjson.GetBytes(body, "function_call").Exists() {
		messages := gjson.GetBytes(body, "messages").Array()
		lastIndex := len(messages) - 1
		if !strings.Contains(messages[lastIndex].Get("content").String(), "Respond in the following locale") {
			locale := s.cfg.ChatLocale
			if locale == "" {
				locale = "zh_CN"
			}
			body, _ = sjson.SetBytes(body, "messages."+strconv.Itoa(lastIndex)+".content", messages[lastIndex].Get("content").String()+"Respond in the following locale: "+locale+".")
		}
	}

	body, _ = sjson.DeleteBytes(body, "intent")
	body, _ = sjson.DeleteBytes(body, "intent_threshold")
	body, _ = sjson.DeleteBytes(body, "intent_content")

	if int(gjson.GetBytes(body, "max_tokens").Int()) > s.cfg.ChatMaxTokens {
		body, _ = sjson.SetBytes(body, "max_tokens", s.cfg.ChatMaxTokens)
	}

	// 构建转发请求
	proxyUrl := s.cfg.ChatApiBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyUrl, io.NopCloser(bytes.NewBuffer(body)))
	if nil != err {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.ChatApiKey)
	if "" != s.cfg.ChatApiOrganization {
		req.Header.Set("OpenAI-Organization", s.cfg.ChatApiOrganization)
	}
	if "" != s.cfg.ChatApiProject {
		req.Header.Set("OpenAI-Project", s.cfg.ChatApiProject)
	}

	// 发送请求并处理响应
	resp, err := s.client.Do(req)
	if nil != err {
		if errors.Is(err, context.Canceled) {
			c.AbortWithStatus(http.StatusRequestTimeout)
			return
		}

		log.Println("request conversation failed:", err.Error())
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer closeIO(resp.Body)

	if resp.StatusCode != http.StatusOK { // 记录失败的请求
		body, _ := io.ReadAll(resp.Body)
		log.Println("request completions failed:", string(body))

		resp.Body = io.NopCloser(bytes.NewBuffer(body))
	}

	// 返回响应状态码和头信息
	c.Status(resp.StatusCode)
	contentType := resp.Header.Get("Content-Type")
	if "" != contentType {
		c.Header("Content-Type", contentType)
	}

	// 返回响应体
	_, _ = io.Copy(c.Writer, resp.Body)
}

// codeCompletions处理代码补全请求
func (s *ProxyService) codeCompletions(c *gin.Context) {
	ctx := c.Request.Context()

	// 模拟处理耗时操作
	time.Sleep(100 * time.Millisecond)
	// 检查上下文是否被取消
	if ctx.Err() != nil {
		abortCodex(c, http.StatusRequestTimeout)
		return
	}

	// 读取请求体
	body, err := io.ReadAll(c.Request.Body)
	if nil != err {
		abortCodex(c, http.StatusBadRequest)
		return
	}

	// 处理请求体字段
	body, _ = sjson.DeleteBytes(body, "extra")
	body, _ = sjson.DeleteBytes(body, "nwo")
	body, _ = sjson.SetBytes(body, "model", InstructModel)

	// 构建转发请求
	proxyUrl := s.cfg.CodexApiBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyUrl, io.NopCloser(bytes.NewBuffer(body)))
	if nil != err {
		abortCodex(c, http.StatusInternalServerError)
		return
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.CodexApiKey)
	if "" != s.cfg.CodexApiOrganization {
		req.Header.Set("OpenAI-Organization", s.cfg.CodexApiOrganization)
	}
	if "" != s.cfg.CodexApiProject {
		req.Header.Set("OpenAI-Project", s.cfg.CodexApiProject)
	}

	// 发送请求并处理响应
	resp, err := s.client.Do(req)
	if nil != err {
		if errors.Is(err, context.Canceled) {
			abortCodex(c, http.StatusRequestTimeout)
			return
		}

		log.Println("request completions failed:", err.Error())
		abortCodex(c, http.StatusInternalServerError)
		return
	}
	defer closeIO(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Println("request completions failed:", string(body))

		abortCodex(c, resp.StatusCode)
		return
	}

	// 返回响应状态码和头信息
	c.Status(resp.StatusCode)
	contentType := resp.Header.Get("Content-Type")
	if "" != contentType {
		c.Header("Content-Type", contentType)
	}

	// 返回响应体
	_, _ = io.Copy(c.Writer, resp.Body)
}

// main函数负责服务的初始化和启动
func main() {
	cfg := readConfig()

	// 设置Gin运行模式为Release
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	proxyService, err := NewProxyService(cfg)
	if nil != err {
		log.Fatal(err)
		return
	}

	// 初始化路由
	proxyService.InitRoutes(r)

	// 启动服务
	err = r.Run(cfg.Bind)
	if nil != err {
		log.Fatal(err)
		return
	}
}
