package server

import (
	"net/http"
	"runtime/debug"
	"strings"
)

// apiPrefix 是全部 JSON 接口的统一前缀（§9）。静态资源挂在根 "/"，
// 两者不重叠：前缀判断而不是正则匹配，代价是一次 HasPrefix。
const apiPrefix = "/api/v1"

// securityPolicy 是 CSP。用 'self' 而不是内联脚本白名单：前端是零构建的
// 外链 app.js，没有一处内联 <script>，所以这里可以关得很死；style 允许
// 'unsafe-inline' 是因为页面里有几处 style 属性（进度/布局），不放开会白屏。
const securityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'"

// routes 注册全部路由（Go 1.22 的"方法 + 路径模式"语法，无需第三方路由库）。
func (s *Server) routes(mux *http.ServeMux) {
	// 公开
	mux.HandleFunc("GET "+apiPrefix+"/health", s.handleHealth)
	mux.HandleFunc("GET "+apiPrefix+"/auth/session", s.handleAuthSession)
	mux.HandleFunc("POST "+apiPrefix+"/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST "+apiPrefix+"/auth/login", s.handleAuthLogin)

	// 需要登录
	mux.HandleFunc("POST "+apiPrefix+"/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET "+apiPrefix+"/config", s.handleConfigGet)
	mux.HandleFunc("PUT "+apiPrefix+"/config", s.handleConfigPut)
	mux.HandleFunc("POST "+apiPrefix+"/config/import-yaml", s.handleConfigImportYAML)
	mux.HandleFunc("GET "+apiPrefix+"/config/export", s.handleConfigExport)
	mux.HandleFunc("GET "+apiPrefix+"/status", s.handleStatus)
	mux.HandleFunc("GET "+apiPrefix+"/runs", s.handleRuns)
	mux.HandleFunc("POST "+apiPrefix+"/runs", s.handleRunStart)
	mux.HandleFunc("GET "+apiPrefix+"/runs/{id}", s.handleRunByID)
	mux.HandleFunc("POST "+apiPrefix+"/notify/test", s.handleNotifyTest)
	mux.HandleFunc("GET "+apiPrefix+"/logs", s.handleLogs)
	mux.HandleFunc("GET "+apiPrefix+"/logs/stream", s.handleLogStream)

	// 未知 API 路径统一回 JSON（ServeMux 默认的纯文本 404 前端解析不了）
	mux.HandleFunc(apiPrefix+"/", s.handleAPINotFound)
	// 其余全部当静态资源
	mux.HandleFunc("/", s.handleStatic)
}

// handleAPINotFound 回 JSON 404，而不是 ServeMux 默认的 "404 page not found"。
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, newError(http.StatusNotFound, codeNotFound,
		"没有这个接口："+r.Method+" "+r.URL.Path))
}

// ---------- 中间件 ----------

// recoverMW 兜住任何 handler 里的 panic。
//
// 没有它，一次 nil map 写就能把整个进程带走——而进程一挂，当晚就不会签到。
// 栈必须进日志：panic 的现场只有这一次。
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logf("error", "", "请求处理 panic %s %s: %v\n%s",
					r.Method, r.URL.Path, rec, debug.Stack())
				// 可能已经写过响应头（SSE/流式），此时再写错误体会污染流，
				// 但也不能什么都不做——返回 500 让客户端至少能重连。
				writeError(w, newError(http.StatusInternalServerError, codeInternal,
					"服务内部错误（已记录日志）"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMW 给所有响应加基础安全头（§B）。
func (s *Server) securityHeadersMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// nosniff：不许浏览器把 JSON 猜成 HTML（错误信封里常回显用户输入）。
		h.Set("X-Content-Type-Options", "nosniff")
		// 管理界面绝不允许被 iframe 嵌套（点击劫持的经典入口）。
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// CSP 只加在**我们自己托管的页面**上：把 web/static 丢给 nginx、后端只跑 API
		// 的部署里，前端与 API 不同源，若给 API 响应也塞 connect-src 'self'，
		// 反过来会把自己的跨源承诺打死（浏览器只按文档的 CSP 判定，不看接口头）。
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			h.Set("Content-Security-Policy", securityPolicy)
		} else {
			// 配置/历史里带学号、精确坐标、运行结论：任何中间缓存都不该留存。
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// corsMW 按 server.allow_origins 白名单回显（§8）。
//
// 默认同源：白名单为空时不写任何 Access-Control-* 头，浏览器自然拒绝跨源。
// 只有"把前端丢到 nginx、后端只跑 API"这种部署才需要填它。
func (s *Server) corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			if s.originAllowed(origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				// 带 Cookie 的跨源请求必须显式允许凭据，且不能用 "*"。
				h.Set("Access-Control-Allow-Credentials", "true")
				// 回显了 Origin 就必须 Vary：否则中间缓存会把 A 源的响应喂给 B 源。
				h.Add("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			if s.originAllowed(strings.TrimSpace(r.Header.Get("Origin"))) {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, "+csrfHeaderName)
				h.Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed 判断某个 Origin 是否在白名单里（每次读配置：白名单可热改）。
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.store.Config().Server.AllowOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
}

// requestLogMW 只在 debug 级别记录每个请求。
//
// info 级别不记：一次仪表盘刷新就有 2 个请求，常驻几天的日志会被这行噪音淹没，
// 真正的签到结论反而找不到。
func (s *Server) requestLogMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.levelMu.RLock()
		min := s.minLevel
		s.levelMu.RUnlock()
		if !min.allows(levelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := s.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logf("debug", "", "%s %s -> %d %s",
			r.Method, r.URL.RequestURI(), rec.status, s.now().Sub(start).Truncate(1000))
	})
}

// guardMW 做 CSRF 与登录校验（§8）。
func (s *Server) guardMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源不设防：登录页必须能被未登录用户取到。
		if !strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		if !isSafeMethod(r.Method) {
			// 双重保险的第一层（第二层是 Cookie 的 SameSite=Strict）：
			// 自定义头无法由跨站表单伪造，浏览器会先发预检。
			if strings.TrimSpace(r.Header.Get(csrfHeaderName)) != csrfHeaderValue {
				writeError(w, newError(http.StatusForbidden, codeCSRF,
					"缺少 "+csrfHeaderName+": "+csrfHeaderValue+" 请求头，CSRF 校验未通过；请刷新页面后重试"))
				return
			}
		}
		if apiIsPublic(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// 未初始化时用 401 + not_initialized：状态码按 §B/K.1 的"否则 401"，
		// 码则让前端能分辨"还没设过密码"。真正把用户送进设置页的分支在前端是
		// 403 + /init|setup/ —— 那条路径由 POST /auth/login 覆盖（未初始化时
		// 登录回 403 not_initialized），而启动时的 refreshSession 本身也能判出来。
		if s.store.PasswordHash() == "" {
			writeError(w, newError(http.StatusUnauthorized, codeNotInitialized,
				"服务尚未初始化：请先设置管理密码"))
			return
		}
		if !s.authed(r) {
			writeError(w, newError(http.StatusUnauthorized, codeUnauthorized,
				"未登录或登录已过期，请重新登录"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// apiIsPublic 列出无需登录的接口（§B：health / session / setup / login）。
func apiIsPublic(method, reqPath string) bool {
	switch {
	case (method == http.MethodGet || method == http.MethodHead) && reqPath == apiPrefix+"/health":
		// HEAD 也要放行：不少负载均衡/监控探针默认用 HEAD，401 会被判成"服务挂了"。
		return true
	case method == http.MethodGet && reqPath == apiPrefix+"/auth/session":
		return true
	case method == http.MethodPost && reqPath == apiPrefix+"/auth/setup":
		return true
	case method == http.MethodPost && reqPath == apiPrefix+"/auth/login":
		return true
	}
	return false
}

// statusRecorder 记录响应状态码，供 debug 日志使用。
//
// 必须实现 Unwrap：http.NewResponseController（SSE 用来清写超时）靠 Unwrap 链
// 找到真正的 ResponseWriter，包一层却不 Unwrap 会让 SSE 静默退回 30s 超时。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush 透传，否则包一层就把 SSE 的刷新能力弄丢了。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿透到这层包装内。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
