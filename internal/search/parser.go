// Package search 实现代理内置的网页搜索能力。
//
// 当前实现:用 chromedp 模拟浏览器直连 Google,goquery 解析结果 HTML。
// 反爬关键:--disable-blink-features=AutomationControlled(已验证有效)。
// 解析逻辑复刻自 ai_stock 项目的 playwright_google_parser.py。
package search

import (
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Item 单条搜索结果。
type Item struct {
	Title   string // 标题
	URL     string // 链接
	Content string // 摘要
}

// h3 往上找到结果容器需要的层数(与 ai_stock parser 一致)
const h3ToContainerDepth = 6

// ParseGoogleHTML 从 Google 搜索结果页 HTML 提取结构化结果。
//
// 纯函数,不依赖浏览器。输入渲染后的 HTML,输出 Item 列表。
// 解析失败返回空列表,不抛异常(与 ai_stock 行为一致)。
//
// 解析策略(2026 年 Google DOM):
//   - 以 h3 为锚点
//   - 往上找 a[href] 拿 URL
//   - 往上 6 层找容器,容器内找不含 jGGQ5e 的 kb0PBd div 做摘要
//   - 去 /url?q= 重定向包装,去重
func ParseGoogleHTML(pageHTML string, limit int) []Item {
	if strings.TrimSpace(pageHTML) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}

	var items []Item
	seen := make(map[string]bool)

	doc.Find("h3").EachWithBreak(func(_ int, h3 *goquery.Selection) bool {
		if len(items) >= limit {
			return false
		}

		title := strings.TrimSpace(h3.Text())
		if title == "" {
			return true
		}

		href := closestHref(h3)
		if href == "" {
			return true
		}

		// 去掉 Google 跳转包装:/url?q=xxx&sa=U → xxx
		if strings.HasPrefix(href, "/url?q=") {
			href = unwrapGoogleURL(href)
		}

		if seen[href] {
			return true
		}
		seen[href] = true

		snippet := extractSnippet(h3)

		items = append(items, Item{
			Title:   title,
			URL:     href,
			Content: snippet,
		})
		return true
	})

	return items
}

// closestHref 从 h3 往上最多 5 层找第一个带 href 的 a 标签。
func closestHref(h3 *goquery.Selection) string {
	current := h3
	for i := 0; i < 5; i++ {
		if current.Length() == 0 {
			break
		}
		if goquery.NodeName(current) == "a" {
			if href, ok := current.Attr("href"); ok && href != "" && !strings.HasPrefix(href, "#") {
				return href
			}
		}
		current = current.Parent()
	}
	return ""
}

// extractSnippet 从 h3 往上到结果容器,在容器内找不含 jGGQ5e 的 kb0PBd div 做摘要。
func extractSnippet(h3 *goquery.Selection) string {
	container := h3
	for i := 0; i < h3ToContainerDepth; i++ {
		if container.Length() == 0 {
			break
		}
		container = container.Parent()
	}
	if container.Length() == 0 {
		return ""
	}

	snippet := ""
	container.Find("div").EachWithBreak(func(_ int, div *goquery.Selection) bool {
		class, _ := div.Attr("class")
		if !strings.Contains(class, "kb0PBd") {
			return true
		}
		// 排除标题行(含 jGGQ5e)
		if strings.Contains(class, "jGGQ5e") {
			return true
		}
		text := strings.TrimSpace(div.Text())
		if len(text) > 20 {
			snippet = text
			return false
		}
		return true
	})
	return snippet
}

// unwrapGoogleURL 去掉 Google 结果 URL 的重定向包装。
// /url?q=https://example.com&sa=U&ved=xxx → https://example.com
func unwrapGoogleURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query().Get("q")
	if q != "" {
		return q
	}
	return rawURL
}
