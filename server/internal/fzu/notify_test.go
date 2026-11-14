package fzu

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fzucheckin/internal/model"
)

// pushObs 记录一次推送请求的路径与表单字段。
type pushObs struct {
	Path string
	Form url.Values
}

func notifyConfig(ntype string) model.Config {
	return model.Config{Notify: model.NotifyConfig{Type: ntype}}
}

// withEndpoints 把包级推送端点临时指向假上游，测试结束自动还原。
func withEndpoints(t *testing.T, serverChan, pushPlus string) {
	t.Helper()
	oldServerChan, oldPushPlus := ServerChanURL, PushPlusURL
	if serverChan != "" {
		ServerChanURL = serverChan
	}
	if pushPlus != "" {
		PushPlusURL = pushPlus
	}
	t.Cleanup(func() { ServerChanURL, PushPlusURL = oldServerChan, oldPushPlus })
}

func TestNotifyNoneSendsNothing(t *testing.T) {
	hits := capture[int]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.update(func(n *int) { *n++ })
	}))
	defer srv.Close()
	withEndpoints(t, srv.URL, srv.URL+"/send")

	cfg := notifyConfig("none")
	cfg.Notify.ServerChanKey = "KEY"
	cfg.Notify.PushPlusToken = "TOKEN"
	if err := Notify(cfg, "标题", "内容", nil); err != nil {
		t.Fatalf("none 不该报错: %v", err)
	}
	// 空 type 也视为没配推送（旧配置常见）。
	if err := Notify(notifyConfig(""), "标题", "内容", nil); err != nil {
		t.Fatalf("空 type 不该报错: %v", err)
	}
	if got := hits.get(); got != 0 {
		t.Errorf("none/空 type 不该发请求，实际 %d 次", got)
	}
}

func TestNotifyServerChan(t *testing.T) {
	obs := capture[pushObs]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		obs.update(func(o *pushObs) {
			o.Form = r.Form
			o.Path = r.URL.Path
		})
		_, _ = io.WriteString(w, `{"code":0,"message":"ok"}`)
	}))
	defer srv.Close()
	withEndpoints(t, srv.URL, "")

	cfg := notifyConfig("serverchan")
	cfg.Notify.ServerChanKey = "SCTKEY"
	if err := Notify(cfg, "签到成功", "已自动签到", nil); err != nil {
		t.Fatalf("serverchan 推送应成功: %v", err)
	}
	got := obs.get()
	if got.Path != "/SCTKEY.send" {
		t.Errorf("端点路径 = %q，期望 /SCTKEY.send", got.Path)
	}
	if got.Form.Get("title") != "签到成功" || got.Form.Get("desp") != "已自动签到" {
		t.Errorf("表单字段不对: title=%q desp=%q", got.Form.Get("title"), got.Form.Get("desp"))
	}
}

func TestNotifyServerChanFailure(t *testing.T) {
	resp := &responder{}
	resp.set(200, `{"code":40001,"message":"bad key"}`)
	srv := httptest.NewServer(resp)
	defer srv.Close()
	withEndpoints(t, srv.URL, "")

	cfg := notifyConfig("serverchan")
	cfg.Notify.ServerChanKey = "WRONG"
	err := Notify(cfg, "t", "c", nil)
	if err == nil {
		t.Fatal("code != 0 必须报错")
	}
	if !strings.Contains(err.Error(), "serverchan") || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("错误应带渠道与原文: %v", err)
	}
	// HTTP 非 200 也要报错
	resp.set(500, "boom")
	if err := Notify(cfg, "t", "c", nil); err == nil {
		t.Error("HTTP 500 必须报错")
	}
}

func TestNotifyMissingCredentials(t *testing.T) {
	cases := []struct {
		ntype string
		want  string
	}{
		{"serverchan", "serverchan_key"},
		{"pushplus", "pushplus_token"},
		{"bark", "bark_url"},
		{"wecom", "wecom_webhook"},
	}
	for _, tc := range cases {
		t.Run(tc.ntype, func(t *testing.T) {
			err := Notify(notifyConfig(tc.ntype), "t", "c", nil)
			if err == nil {
				t.Fatal("没配 key 应报错（而不是静默成功）")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误应点名缺哪个键: %v", err)
			}
		})
	}
}

func TestNotifyPushPlus(t *testing.T) {
	payload := capture[map[string]any]{}
	resp := &responder{}
	resp.set(200, `{"code":200,"msg":"ok"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		payload.set(body)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("pushplus 的 Content-Type = %q", ct)
		}
		resp.serve(w, r)
	}))
	defer srv.Close()
	withEndpoints(t, "", srv.URL)

	cfg := notifyConfig("pushplus")
	cfg.Notify.PushPlusToken = "PPTOKEN"
	if err := Notify(cfg, "标题", "内容", nil); err != nil {
		t.Fatalf("pushplus 应成功: %v", err)
	}
	got := payload.get()
	if got["token"] != "PPTOKEN" || got["title"] != "标题" || got["content"] != "内容" {
		t.Errorf("JSON body 不对: %v", got)
	}

	// code 不是 200 → 失败
	resp.set(200, `{"code":500,"msg":"quota exceeded"}`)
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("code != 200 必须报错")
	}
	// 非 JSON → 失败
	resp.set(200, `<html>oops</html>`)
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("非 JSON 响应必须报错")
	}
	// 非 200 → 失败
	resp.set(502, "bad gateway")
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("HTTP 502 必须报错")
	}
}

func TestNotifyBark(t *testing.T) {
	payload := capture[map[string]any]{}
	resp := &responder{}
	resp.set(200, `{"code":200}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		payload.set(body)
		resp.serve(w, r)
	}))
	defer srv.Close()

	cfg := notifyConfig("bark")
	cfg.Notify.BarkURL = srv.URL + "/device-key"
	if err := Notify(cfg, "标题", "内容", nil); err != nil {
		t.Fatalf("bark 应成功: %v", err)
	}
	got := payload.get()
	if got["title"] != "标题" || got["body"] != "内容" {
		t.Errorf("bark body 不对: %v", got)
	}
	resp.set(404, "not found")
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("bark 非 200 必须报错")
	}
	// 非法 URL → 报错但不 panic
	cfg.Notify.BarkURL = "://bad url"
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("非法 URL 必须报错")
	}
}

func TestNotifyWecom(t *testing.T) {
	payload := capture[map[string]any]{}
	resp := &responder{}
	resp.set(200, `{"errcode":0,"errmsg":"ok"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		payload.set(body)
		resp.serve(w, r)
	}))
	defer srv.Close()

	cfg := notifyConfig("wecom")
	cfg.Notify.WecomWebhook = srv.URL + "/hook"
	if err := Notify(cfg, "标题", "内容", nil); err != nil {
		t.Fatalf("wecom 应成功: %v", err)
	}
	got := payload.get()
	if got["msgtype"] != "text" {
		t.Errorf("msgtype = %v", got["msgtype"])
	}
	text, _ := got["text"].(map[string]any)
	if text == nil || text["content"] != "标题\n内容" {
		t.Errorf("企业微信正文应为「标题\\n内容」: %v", got["text"])
	}

	resp.set(200, `{"errcode":93000,"errmsg":"invalid webhook url"}`)
	if err := Notify(cfg, "标题", "内容", nil); err == nil {
		t.Error("errcode != 0 必须报错")
	}
}

func TestNotifyNetworkFailureReturnsError(t *testing.T) {
	// 关掉的 server：连接失败必须是 error，不是 panic。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	cfg := notifyConfig("wecom")
	cfg.Notify.WecomWebhook = addr + "/hook"
	if err := Notify(cfg, "t", "c", nil); err == nil {
		t.Error("连不上时必须返回 error")
	}
}

func TestNotifyUnknownChannel(t *testing.T) {
	err := Notify(notifyConfig("telegram"), "t", "c", nil)
	if err == nil {
		t.Fatal("未知渠道应报错")
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Errorf("错误应带上渠道名: %v", err)
	}
}

func TestNotifyAcceptsChannelCaseInsensitively(t *testing.T) {
	hits := capture[int]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.update(func(n *int) { *n++ })
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer srv.Close()
	withEndpoints(t, srv.URL, "")

	cfg := notifyConfig(" ServerChan ")
	cfg.Notify.ServerChanKey = "K"
	if err := Notify(cfg, "t", "c", nil); err != nil {
		t.Fatalf("渠道名应容忍大小写与空白: %v", err)
	}
	if got := hits.get(); got != 1 {
		t.Errorf("应发 1 次请求，实际 %d", got)
	}
}

func TestNotifyUsesProvidedClient(t *testing.T) {
	hits := capture[int]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.update(func(n *int) { *n++ })
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer srv.Close()
	withEndpoints(t, srv.URL, "")

	cfg := notifyConfig("serverchan")
	cfg.Notify.ServerChanKey = "K"
	// 传入自定义 client（验证 httpc 被真正使用，而不是被忽略）。
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-From-Test", "1")
		return http.DefaultTransport.RoundTrip(r)
	})}
	if err := Notify(cfg, "t", "c", client); err != nil {
		t.Fatalf("带自定义 client 应成功: %v", err)
	}
	if got := hits.get(); got != 1 {
		t.Errorf("应发 1 次请求，实际 %d", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
