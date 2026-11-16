package fzu

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLooksLikeHTML 保证"是不是 HTML"的判定不会把 JSON 响应误判成网页：
// 误判的代价是把一次正常的 init 当成"token 被拒绝"，进而误报失败。
func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"<html><head><title>信息提示</title></head></html>", true},
		{"<!DOCTYPE html><html></html>", true},
		{"  \n<html>", true},
		{"\ufeff<html>", true},
		{"<?xml version=\"1.0\"?><root/>", true},
		{`{"success":true,"code":1,"data":{}}`, false},
		{"", false},
		{"  ", false},
		{`{"msg":"坐标 < 0 非法"}`, false},
	}
	for _, tc := range cases {
		if got := looksLikeHTML([]byte(tc.body)); got != tc.want {
			t.Fatalf("looksLikeHTML(%q) = %t，期望 %t", tc.body, got, tc.want)
		}
	}
}

// TestInitHTMLPageMeansTokenRejected：上游回 HTML 错误页时要能被 ErrTokenRejected
// 认出来——上层靠这个哨兵把"token 不被接受"和"今日无计划"分开。
func TestInitHTMLPageMeansTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html><head><title>信息提示</title></head><body>请重新登录</body></html>`))
	}))
	defer srv.Close()

	c := NewClient("SYNTHETIC-access-token-0001")
	c.BaseURL = srv.URL
	_, err := c.Init()
	if err == nil {
		t.Fatal("HTML 错误页必须返回 error")
	}
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("应能被 ErrTokenRejected 命中，实际: %v", err)
	}
}

// TestInitJSON500StaysGenericError：纯文本/JSON 的 500 不能被当成 token 问题，
// 否则上游每次抖动都会变成一条"你的 token 错了"的误导。
func TestInitJSON500StaysGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient("tok")
	c.BaseURL = srv.URL
	_, err := c.Init()
	if err == nil {
		t.Fatal("500 必须返回 error")
	}
	if errors.Is(err, ErrTokenRejected) {
		t.Fatalf("纯文本 500 不该被判成 token 被拒绝: %v", err)
	}
}
