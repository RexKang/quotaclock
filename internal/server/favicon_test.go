package server

import (
	"bytes"
	"io"
	"testing"

	"github.com/RexKang/quotaclock/web"
)

func TestFaviconAndIcons(t *testing.T) {
	env := newEnv(t, nil)

	// GET /favicon.ico：默认探测路径 → 200 + image/x-icon + 内容一致
	resp := env.do("GET", "/favicon.ico", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/x-icon" {
		t.Fatalf("favicon.ico: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	want, _ := web.Icon("favicon.ico")
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(b, want) {
		t.Fatal("favicon.ico 内容与内嵌资源不一致")
	}

	// /icons/{file}：png / svg 各自的 Content-Type + 缓存头
	resp2 := env.do("GET", "/icons/favicon-32.png", nil, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 || resp2.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("favicon-32.png: %d %s", resp2.StatusCode, resp2.Header.Get("Content-Type"))
	}
	if cc := resp2.Header.Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Fatalf("图标缓存头 = %q", cc)
	}
	resp3 := env.do("GET", "/icons/favicon.svg", nil, nil)
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 || resp3.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon.svg: %d %s", resp3.StatusCode, resp3.Header.Get("Content-Type"))
	}

	// 白名单外 / 不存在 → 404（含预览稿 favicon-sheet.png，不外发）
	for _, p := range []string{"/icons/favicon-sheet.png", "/icons/nope.png", "/icons/../server.go"} {
		resp4 := env.do("GET", p, nil, nil)
		resp4.Body.Close()
		if resp4.StatusCode != 404 {
			t.Fatalf("%s 应 404，got %d", p, resp4.StatusCode)
		}
	}

	// 页面 head 应引用图标（校验内嵌源文件；测试环境的 Static 是占位页）
	page, err := web.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`rel="icon" type="image/svg+xml" href="/icons/favicon.svg"`,
		`rel="shortcut icon" href="/favicon.ico"`,
		`href="/icons/favicon-32.png"`,
		`href="/icons/favicon-16.png"`,
		`rel="apple-touch-icon" sizes="128x128" href="/icons/favicon-128.png"`,
	} {
		if !bytes.Contains(page, []byte(want)) {
			t.Fatalf("head 缺少 %q", want)
		}
	}
}
