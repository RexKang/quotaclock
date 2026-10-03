package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RexKang/quotaclock/internal/config"
)

// countingReader 记录被读走的字节数（断言 handler 是否读干请求体）。
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestRejectPathsDrainBody：所有「不读 body 就拒绝」的路径必须先读干 body。
//
// 原因（实测）：handler 未消费 body 就写响应时 net/http 会关闭连接，客户端若仍在发
// body 就可能收到 TCP RST、**丢掉这个响应**。修复前用真人客户端实测「带 body 的 PUT
// 未登录 → 401」300 次丢 9 次（3%）；读干 body 后 600 次 0 丢。
// 本用例锁住机制本身（不依赖 TCP 竞态的偶发性）：拒绝响应发出前，body 必须已被读完。
func TestRejectPathsDrainBody(t *testing.T) {
	const payload = `{"version":3}`

	t.Run("鉴权 401（requireWrite）", func(t *testing.T) {
		env := newEnv(t, nil) // 默认 admin 模式 + 无 cookie → 401
		cr := &countingReader{r: strings.NewReader(payload)}
		req := httptest.NewRequest("PUT", "/api/config", cr)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		reached := false
		env.srv.requireWrite(func(w http.ResponseWriter, r *http.Request) { reached = true })(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("应 401，实际 %d", rec.Code)
		}
		if reached {
			t.Fatal("未登录不应进入 handler")
		}
		if cr.n != len(payload) {
			t.Fatalf("401 前未读干 body（读了 %d/%d 字节）：连接会被强关，客户端可能丢失该响应", cr.n, len(payload))
		}
	})

	t.Run("登录 400（Content-Type 非 JSON）", func(t *testing.T) {
		env := newEnv(t, nil)
		cr := &countingReader{r: strings.NewReader(payload)}
		req := httptest.NewRequest("POST", "/api/login", cr)
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		env.srv.handleLogin(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_CONTENT_TYPE") {
			t.Fatalf("应 400 INVALID_CONTENT_TYPE，实际 %d %s", rec.Code, rec.Body.String())
		}
		if cr.n != len(payload) {
			t.Fatalf("400 前未读干 body（读了 %d/%d 字节）", cr.n, len(payload))
		}
	})

	t.Run("登录 429（限速锁定）", func(t *testing.T) {
		env := newEnv(t, nil)
		for i := 0; i < 5; i++ { // 打满失败计数
			_, _, _ = env.doJSON("POST", "/api/login", map[string]string{"password": "wrong"}, nil)
		}
		cr := &countingReader{r: strings.NewReader(payload)}
		req := httptest.NewRequest("POST", "/api/login", cr)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		env.srv.handleLogin(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("应 429，实际 %d", rec.Code)
		}
		if cr.n != len(payload) {
			t.Fatalf("429 前未读干 body（读了 %d/%d 字节）", cr.n, len(payload))
		}
	})

	// 反例守门：正常放行的请求不应被本机制影响（none 模式全开放）
	t.Run("none 模式放行不受影响", func(t *testing.T) {
		env := newEnv(t, func(key []byte, f *config.File) { f.Auth.Mode = config.AuthModeNone })
		env.srv.cfg.Store(config.BuildRuntime(env.srv.file.Load(), env.key))
		cr := &countingReader{r: strings.NewReader(payload)}
		req := httptest.NewRequest("PUT", "/api/config", cr)
		rec := httptest.NewRecorder()
		reached := false
		env.srv.requireWrite(func(w http.ResponseWriter, r *http.Request) { reached = true })(rec, req)
		if !reached {
			t.Fatal("none 模式应放行到 handler")
		}
	})
}
