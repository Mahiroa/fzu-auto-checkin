package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRequestBody 是请求体上限（§8：1 MB）。
// 这个接口面里最大的合法请求体是"导入一份旧 config.yaml"，几十 KB 到头；
// 限死它，一个跑飞的客户端没法把常驻进程的内存吃掉。
const maxRequestBody = 1 << 20

// 错误码。前端至少依赖 not_initialized 与 csrf_failed 两个分支（跳设置页 / 提示刷新），
// 其余码是给人和日志看的：把码与文案分开，前端才能按码分支而不用匹配中文。
const (
	codeBadRequest          = "bad_request"
	codeUnauthorized        = "unauthorized"
	codeCSRF                = "csrf_failed"
	codeNotInitialized      = "not_initialized"
	codeAlreadyInitialized  = "already_initialized"
	codeWeakPassword        = "weak_password"
	codeBadPassword         = "bad_password"
	codeTooManyAttempts     = "too_many_attempts"
	codeConfigInvalid       = "config_invalid"
	codeYAMLInvalid         = "yaml_invalid"
	codeSaveFailed          = "save_failed"
	codeRunInProgress       = "run_in_progress"
	codeRunNotFound         = "run_not_found"
	codeNotifyNotConfigured = "notify_not_configured"
	codeNotifyFailed        = "notify_failed"
	codeSSELimit            = "sse_limit"
	codeNotFound            = "not_found"
	codeMethodNotAllowed    = "method_not_allowed"
	codePayloadTooLarge     = "payload_too_large"
	codeInternal            = "internal_error"
)

// apiError 是唯一的错误形态。整包只允许经由它写错误响应，
// 这样"某个 handler 偷偷回了裸文本"这种前端解析不了的情况在代码层面就不成立。
type apiError struct {
	Status  int
	Code    string
	Message string
	Details []string
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// newError 造一个错误；details 为空时也保留空切片（JSON 里是 []，前端不必判 nil）。
func newError(status int, code, message string, details ...string) *apiError {
	if details == nil {
		details = []string{}
	}
	return &apiError{Status: status, Code: code, Message: message, Details: details}
}

// errorEnvelope 是 §9 的错误信封：{"error":{"code","message","details":[]}}。
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details"`
}

// writeJSON 先序列化再写头。
//
// 顺序不能反：序列化失败时才可能"已经写了 200 头却没法写 body"，那时只能给一个
// 半截响应。先 marshal 就能在写头之前把失败降级成一个合法的 500。
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":{"code":"internal_error","message":"响应序列化失败","details":[]}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// writeError 输出错误信封。
func writeError(w http.ResponseWriter, e *apiError) {
	if e == nil {
		e = newError(http.StatusInternalServerError, codeInternal, "未知错误")
	}
	if e.Status == 0 {
		e.Status = http.StatusInternalServerError
	}
	writeJSON(w, e.Status, errorEnvelope{Error: errorBody{
		Code:    e.Code,
		Message: e.Message,
		Details: e.Details,
	}})
}

// writeOK 是 {"ok":true} 的常用形态。
func writeOK(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]any{"ok": true})
}

// decodeJSON 读取并反序列化请求体，把"太大/不是 JSON/空体"翻译成统一错误。
//
// 空体视为"没带参数"而不是错误：POST /api/v1/runs 不带 body 等价于默认参数，
// 让前端少一个必须构造的字段。未知字段一律容忍（前向兼容），但类型不对会报错。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return newError(http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("请求体超过 %d 字节上限", maxRequestBody))
		}
		return newError(http.StatusBadRequest, codeBadRequest, "请求体不是合法 JSON："+err.Error())
	}
	return nil
}

// detailsOf 把多行错误拆成逐条细节（§6.2 的校验文案就是多行写的）。
// config.Validate 返回的是 strings.Join(problems, "\n")，前端要逐条列出，
// 所以这里按行切开并丢掉空行——把整段塞进 details[0] 会让前端的长列表退化成一段文字。
func detailsOf(err error) []string {
	if err == nil {
		return []string{}
	}
	return splitLines(err.Error())
}

// splitLines 按行切分并去掉空行与行尾空白。
func splitLines(text string) []string {
	out := make([]string, 0, 8)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
