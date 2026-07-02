package search

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// GoogleSearcher 用 chromedp 模拟浏览器直连 Google 搜索。
//
// 反爬关键:--disable-blink-features=AutomationControlled(已验证有效)。
// 复刻自 ai_stock 的 PlaywrightSearchService。
type GoogleSearcher struct {
	// Proxy 代理地址(http://host:port),谷歌直连会超时
	Proxy string
	// Timeout 单次搜索超时
	Timeout time.Duration
}

// Search 执行谷歌搜索,返回结果 items。
//
// 流程:启动无头 chromium(带反爬 arg + 代理 + UA)→ 导航到 Google 搜索页 →
// 注入 navigator.webdriver=undefined → 等渲染 → 取 HTML → ParseGoogleHTML。
func (g *GoogleSearcher) Search(ctx context.Context, query string, limit int) ([]Item, error) {
	if query == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	if limit <= 0 {
		limit = 10
	}
	if g.Timeout == 0 {
		g.Timeout = 15 * time.Second
	}

	searchURL := fmt.Sprintf("https://www.google.com/search?q=%s&hl=zh-CN", url.QueryEscape(query))

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		// 反爬关键:隐藏自动化标记
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
	)
	if g.Proxy != "" {
		opts = append(opts, chromedp.Flag("proxy-server", g.Proxy))
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	taskCtx, cancel = context.WithTimeout(taskCtx, g.Timeout)
	defer cancel()

	var html string
	err := chromedp.Run(taskCtx,
		// 注入 stealth:隐藏 webdriver
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(
				`Object.defineProperty(navigator,'webdriver',{get:()=>undefined})`,
			).Do(ctx)
			return err
		}),
		chromedp.Navigate(searchURL),
		chromedp.Sleep(3*time.Second),
		chromedp.OuterHTML("html", &html),
	)
	if err != nil {
		return nil, fmt.Errorf("search: chromedp run: %w", err)
	}

	items := ParseGoogleHTML(html, limit)
	if len(items) == 0 {
		return nil, fmt.Errorf("search: no results parsed (html len=%d, maybe blocked)", len(html))
	}
	return items, nil
}
