package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RexKang/quotaclock/internal/config"
)

// classifyTable C-col-01：分类器表逐一归入 §7.2 对应分类。
func classifyTable() []struct {
	name  string
	make  func(t *testing.T) Classified
	class ErrClass
} {
	return []struct {
		name  string
		make  func(t *testing.T) Classified
		class ErrClass
	}{
		{"网络错", func(t *testing.T) Classified {
			return ClassifyTransport(context.DeadlineExceeded)
		}, ClassNetwork},
		{"超时", func(t *testing.T) Classified {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
			defer srv.Close()
			c := &Client{HTTP: &http.Client{Timeout: 50 * time.Millisecond}, Version: "t"}
			return c.FetchOnce(context.Background(), &config.RuntimeProvider{ID: "x", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer}, "/")
		}, ClassNetwork},
		{"401", func(t *testing.T) Classified {
			return fetch(t, 401, nil, `{}`)
		}, ClassTokenInvalid},
		{"403", func(t *testing.T) Classified {
			return fetch(t, 403, nil, `{}`)
		}, ClassTokenInvalid},
		{"429 有头", func(t *testing.T) Classified {
			h := http.Header{}
			h.Set("Retry-After", "120")
			return fetch(t, 429, h, `{}`)
		}, ClassRateLimited},
		{"429 无头", func(t *testing.T) Classified {
			return fetch(t, 429, nil, `{}`)
		}, ClassRateLimited},
		{"400", func(t *testing.T) Classified {
			return fetch(t, 400, nil, `{}`)
		}, ClassClientErr},
		{"500", func(t *testing.T) Classified {
			return fetch(t, 500, nil, `{}`)
		}, ClassServerErr},
		{"200+success=false", func(t *testing.T) Classified {
			return fetch(t, 200, nil, `{"success":false,"msg":"额度查询失败"}`)
		}, ClassBizError},
		{"200+success字符串", func(t *testing.T) Classified {
			return fetch(t, 200, nil, `{"success":"true"}`)
		}, ClassBizError},
		{"200+error键", func(t *testing.T) Classified {
			return fetch(t, 200, nil, `{"error":{"message":"bad"}}`)
		}, ClassBizError},
		{"200 成功", func(t *testing.T) Classified {
			return fetch(t, 200, nil, `{"success":true,"data":{"limit":1}}`)
		}, ClassOK},
		{"200 非JSON透传", func(t *testing.T) Classified {
			return fetch(t, 200, nil, `;0x1;((self.$R=[],...))`)
		}, ClassOK},
	}
}

func TestClassifyTable(t *testing.T) { // C-col-01
	for _, tc := range classifyTable() {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.make(t)
			if got.Class != tc.class {
				t.Fatalf("分类 = %s, want %s（msg=%q）", got.Class, tc.class, got.Message)
			}
		})
	}
}

func TestBizMessageOrder(t *testing.T) { // §7.5 文案提取顺序：msg → error.message → HTTP <status>
	if got := ClassifyHTTP(200, nil, []byte(`{"success":false,"msg":"余额不足","error":{"message":"E"}}`)).Message; got != "余额不足" {
		t.Fatalf("msg 优先: %q", got)
	}
	if got := ClassifyHTTP(200, nil, []byte(`{"success":false,"error":{"message":"深层错误"}}`)).Message; got != "深层错误" {
		t.Fatalf("error.message: %q", got)
	}
	if got := ClassifyHTTP(200, nil, []byte(`{"success":false}`)).Message; got != "HTTP 200" {
		t.Fatalf("兜底: %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) { // C-col-03 前置
	now := time.Unix(1000000, 0)
	if got := ParseRetryAfter("120", now); got == nil || *got != 120 {
		t.Fatalf("整数秒: %v", got)
	}
	if got := ParseRetryAfter("-5", now); got == nil || *got != 0 {
		t.Fatalf("负数取 0: %v", got)
	}
	future := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfter(future, now); got == nil || *got < 89 || *got > 90 {
		t.Fatalf("HTTP 日期: %v", got)
	}
	if got := ParseRetryAfter("garbage", now); got != nil {
		t.Fatalf("垃圾值: %v", got)
	}
	if got := ParseRetryAfter("", now); got != nil {
		t.Fatal("空头: nil")
	}
}

// fetch 本地起 httptest 上游按给定状态/头/body 响应并走完整 FetchOnce 分类。
func fetch(t *testing.T, status int, header http.Header, body string) Classified {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Version: "t"}
	return c.FetchOnce(context.Background(), &config.RuntimeProvider{ID: "x", BaseURL: srv.URL, Paths: []string{"/"}, AuthStyle: config.AuthStyleBearer}, "/")
}

func TestOutboundHeaders(t *testing.T) { // C-col-06
	var gotAuth, gotCookie, gotAccept, gotUA, gotExtra, gotExtra2 string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		gotExtra = r.Header.Get("x-server-id")
		gotExtra2 = r.Header.Get("Accept-Encoding")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	// bearer（唯一鉴权方式，v0.2.5 起 cookie 分支已移除）
	c := &Client{HTTP: srv.Client(), Version: "9.9.9"}
	p := &config.RuntimeProvider{ID: "p", BaseURL: srv.URL, Paths: []string{"/a"}, AuthStyle: config.AuthStyleBearer, Token: "sk-tok"}
	c.FetchOnce(context.Background(), p, "/a")
	if gotAuth != "Bearer sk-tok" || gotCookie != "" || gotAccept != "application/json" || gotUA != "QuotaClock/9.9.9" {
		t.Fatalf("bearer 头错误: auth=%q cookie=%q accept=%q ua=%q", gotAuth, gotCookie, gotAccept, gotUA)
	}

	// 无 token（未配置凭据）：不发 Authorization，也不发 Cookie（凭据缺失由分类器判 401）
	pNoTok := &config.RuntimeProvider{ID: "p", BaseURL: srv.URL, Paths: []string{"/a"}, AuthStyle: config.AuthStyleBearer}
	c.FetchOnce(context.Background(), pNoTok, "/a")
	if gotAuth != "" || gotCookie != "" || gotAccept != "application/json" {
		t.Fatalf("无 token 时头错误: auth=%q cookie=%q accept=%q", gotAuth, gotCookie, gotAccept)
	}

	// S2 extra_headers 生效且可覆盖默认
	p3 := &config.RuntimeProvider{ID: "p", BaseURL: srv.URL, Paths: []string{"/a"}, AuthStyle: config.AuthStyleBearer, Token: "t",
		ExtraHeaders: map[string]string{"x-server-id": "abc123", "Accept-Encoding": "identity"}}
	c.FetchOnce(context.Background(), p3, "/a")
	if gotExtra != "abc123" || gotExtra2 != "identity" {
		t.Fatalf("extra_headers: id=%q ae=%q", gotExtra, gotExtra2)
	}
}

func TestURLJoin(t *testing.T) { // C-col-08：不丢前缀；query 原样保留
	cases := []struct{ base, path, want string }{
		{"https://api.example.com/coding/v1", "/usages", "https://api.example.com/coding/v1/usages"},
		{"https://api.example.com/", "usages", "https://api.example.com/usages"},
		{"https://opencode.ai", "/_server?id=abc&args=%7B%7D", "https://opencode.ai/_server?id=abc&args=%7B%7D"},
		{"https://x.com", "/", "https://x.com/"},
	}
	for _, tc := range cases {
		if got := BuildURL(tc.base, tc.path); got != tc.want {
			t.Fatalf("BuildURL(%q,%q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestNonJSONPassthrough(t *testing.T) { // C-col-09
	cl := fetch(t, 200, nil, `;0x1;((self.$R=[]),(self.$R["server-fn:5"]=[],({usagePercent:12})))`)
	if cl.Class != ClassOK {
		t.Fatalf("class = %s", cl.Class)
	}
	s := string(cl.Data)
	if !strings.HasPrefix(s, `"`) || !strings.Contains(s, "usagePercent") {
		t.Fatalf("非 JSON 应为带引号字符串透传: %q", s)
	}
	// JSON 数组也原样
	cl2 := fetch(t, 200, nil, `[1,2,3]`)
	if string(cl2.Data) != "[1,2,3]" {
		t.Fatalf("数组透传: %s", cl2.Data)
	}
}

func TestTimeoutClassifiedNetwork(t *testing.T) { // C-col-07（短超时注入验证路径；生产 25s 为常量）
	if OutboundTimeout != 25*time.Second {
		t.Fatalf("OutboundTimeout = %v", OutboundTimeout)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer srv.Close()
	c := &Client{HTTP: &http.Client{Timeout: 60 * time.Millisecond}, Version: "t"}
	cl := c.FetchOnce(context.Background(), &config.RuntimeProvider{ID: "x", BaseURL: srv.URL, Paths: []string{"/"}}, "/")
	if cl.Class != ClassNetwork || cl.Status != 0 {
		t.Fatalf("超时应 NETWORK: %+v", cl)
	}
	// ctx 取消同样归 NETWORK
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl2 := c.FetchOnce(ctx, &config.RuntimeProvider{ID: "x", BaseURL: srv.URL}, "/")
	if cl2.Class != ClassNetwork {
		t.Fatalf("ctx 取消应 NETWORK: %+v", cl2)
	}
}
