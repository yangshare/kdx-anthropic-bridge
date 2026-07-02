package search

import (
	"testing"
)

// ai_stock 单测里的 HTML 样本(已验证 ai_stock parser 能解析出 1 条)
const sampleHTML = `<html><body>
<div class="N54PNb BToiNc">
  <div class="kb0PBd A9Y9g jGGQ5e">
    <div class="yuRUbf"><a href="https://example.com/news"><h3>Test Result</h3></a></div>
  </div>
  <div class="kb0PBd A9Y9g">这是搜索结果摘要,足够长来通过长度检查的测试数据</div>
</div>
</body></html>`

func TestParseGoogleHTML_basic(t *testing.T) {
	items := ParseGoogleHTML(sampleHTML, 5)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it := items[0]
	if it.Title != "Test Result" {
		t.Errorf("title = %q, want Test Result", it.Title)
	}
	if it.URL != "https://example.com/news" {
		t.Errorf("url = %q, want https://example.com/news", it.URL)
	}
	if it.Content != "这是搜索结果摘要,足够长来通过长度检查的测试数据" {
		t.Errorf("content = %q", it.Content)
	}
}

func TestParseGoogleHTML_googleUrlUnwrap(t *testing.T) {
	// Google 重定向包装:/url?q=xxx&sa=U
	html := `<html><body>
<div class="N54PNb">
  <div class="kb0PBd A9Y9g jGGQ5e">
    <a href="/url?q=https://real.example.com/page&sa=U&ved=123"><h3>Wrapped</h3></a>
  </div>
  <div class="kb0PBd A9Y9g">这是足够长的摘要内容用于测试解析逻辑是否正常工作</div>
</div>
</body></html>`
	items := ParseGoogleHTML(html, 5)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].URL != "https://real.example.com/page" {
		t.Errorf("url not unwrapped: %q", items[0].URL)
	}
}

func TestParseGoogleHTML_dedup(t *testing.T) {
	// 相同 URL 去重
	html := `<html><body>
<a href="https://dup.com"><h3>First</h3></a>
<a href="https://dup.com"><h3>Second</h3></a>
</body></html>`
	items := ParseGoogleHTML(html, 5)
	if len(items) != 1 {
		t.Errorf("dup not removed, got %d", len(items))
	}
}

func TestParseGoogleHTML_limit(t *testing.T) {
	// limit 截断
	html := `<html><body>
<a href="https://a.com"><h3>A</h3></a>
<a href="https://b.com"><h3>B</h3></a>
<a href="https://c.com"><h3>C</h3></a>
</body></html>`
	items := ParseGoogleHTML(html, 2)
	if len(items) != 2 {
		t.Errorf("limit not applied, got %d", len(items))
	}
}

func TestParseGoogleHTML_empty(t *testing.T) {
	if items := ParseGoogleHTML("", 5); items != nil {
		t.Errorf("empty html should return nil, got %v", items)
	}
	if items := ParseGoogleHTML("   ", 5); items != nil {
		t.Errorf("whitespace html should return nil, got %v", items)
	}
}

func TestParseGoogleHTML_noH3(t *testing.T) {
	html := `<html><body><div>no results here</div></body></html>`
	items := ParseGoogleHTML(html, 5)
	if len(items) != 0 {
		t.Errorf("no h3 should return empty, got %d", len(items))
	}
}

func TestParseGoogleHTML_skipAnchorHref(t *testing.T) {
	// href 以 # 开头的跳过
	html := `<html><body><a href="#"><h3>Anchor</h3></a></body></html>`
	items := ParseGoogleHTML(html, 5)
	if len(items) != 0 {
		t.Errorf("anchor href should be skipped, got %d", len(items))
	}
}
