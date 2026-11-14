package fzu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fzucheckin/internal/model"
)

// 推送端点做成包级变量：测试用 httptest.Server 顶掉它们，就能在零联网的前提下
// 校验"发了什么、判据是什么"。bark 与企业微信的地址来自用户配置，天然可指向本地。
var (
	// ServerChanURL Server酱 Turbo 的根地址（实际请求 {URL}/{key}.send）。
	ServerChanURL = "https://sctapi.ftqq.com"
	// PushPlusURL PushPlus 的发送端点。
	PushPlusURL = "https://www.pushplus.plus/send"
)

// Notify 按配置渠道推送一条消息。
//
// 约定：type 为 none/空 → nil 且不发任何请求（"没配推送"不是错误）；
// 其余任何失败都返回 error 但绝不 panic —— 推送是签到链路的收尾动作，
// 它失败不该把一次成功的签到翻成失败（由调用方写 notify.ok=false）。
func Notify(cfg model.Config, title, content string, httpc *http.Client) error {
	channel := strings.ToLower(strings.TrimSpace(cfg.Notify.Type))
	switch channel {
	case "", "none":
		return nil
	case "serverchan":
		key := strings.TrimSpace(cfg.Notify.ServerChanKey)
		if key == "" {
			return errors.New("notify.serverchan_key 未配置")
		}
		status, body, err := postForm(httpc, strings.TrimRight(ServerChanURL, "/")+"/"+key+".send",
			url.Values{"title": {title}, "desp": {content}})
		if err != nil {
			return err
		}
		// 判据是"HTTP 200 且 code==0"：Server酱 用 code=0 表示已受理，
		// 有 key 写错时仍回 200，只能靠 body 里的 code 区分。
		if !codeIs(body, "code", 0) {
			return fmt.Errorf("serverchan 推送失败（HTTP %d）: %s", status, truncate(body))
		}
		return nil
	case "pushplus":
		token := strings.TrimSpace(cfg.Notify.PushPlusToken)
		if token == "" {
			return errors.New("notify.pushplus_token 未配置")
		}
		status, body, err := postJSON(httpc, PushPlusURL, map[string]any{
			"token":   token,
			"title":   title,
			"content": content,
		})
		if err != nil {
			return err
		}
		if !codeIs(body, "code", 200) {
			return fmt.Errorf("pushplus 推送失败（HTTP %d）: %s", status, truncate(body))
		}
		return nil
	case "bark":
		endpoint := strings.TrimSpace(cfg.Notify.BarkURL)
		if endpoint == "" {
			return errors.New("notify.bark_url 未配置")
		}
		// Bark 的地址本身就是用户自建的完整 URL（含 device key），只能原样 POST。
		_, _, err := postJSON(httpc, endpoint, map[string]any{"title": title, "body": content})
		return err
	case "wecom":
		webhook := strings.TrimSpace(cfg.Notify.WecomWebhook)
		if webhook == "" {
			return errors.New("notify.wecom_webhook 未配置")
		}
		status, body, err := postJSON(httpc, webhook, map[string]any{
			"msgtype": "text",
			"text":    map[string]any{"content": title + "\n" + content},
		})
		if err != nil {
			return err
		}
		// 企微机器人 key 失效时也回 HTTP 200，真正的结论在 errcode。
		if !codeIs(body, "errcode", 0) {
			return fmt.Errorf("wecom 推送失败（HTTP %d）: %s", status, truncate(body))
		}
		return nil
	default:
		return fmt.Errorf("未知的推送渠道 %q（可选 serverchan|pushplus|bark|wecom|none）", channel)
	}
}

// pushClient 推送用的客户端：调用方没传就现场造一个 15s 超时的。
// 不复用 http.DefaultClient（无超时）也不复用接口客户端（带 token 头，
// 不该发给第三方推送网关）。
func pushClient(httpc *http.Client) *http.Client {
	if httpc != nil {
		return httpc
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// postForm 发 form 表单，返回状态码与响应体。
func postForm(httpc *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("推送请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doPush(pushClient(httpc), req)
}

// postJSON 发 JSON body，返回状态码与响应体。
func postJSON(httpc *http.Client, endpoint string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("推送请求序列化失败: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, fmt.Errorf("推送请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doPush(pushClient(httpc), req)
}

// doPush 统一处理状态码：非 200 直接报错并带上响应体原文（推送网关的
// "key 无效/频率超限"都写在 body 里，只报状态码等于什么都没说）。
func doPush(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("推送请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("推送响应读取失败: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, body, fmt.Errorf("推送失败: HTTP %d: %s", resp.StatusCode, truncate(body))
	}
	return resp.StatusCode, body, nil
}

// codeIs 判断响应体 JSON 里 key 的值是否等于 want。
func codeIs(body []byte, key string, want float64) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	value, ok := payload[key]
	if !ok {
		return false
	}
	number, ok := value.(float64)
	return ok && number == want
}
