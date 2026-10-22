package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DeepSeek 适配器。
//
// 已核对官方文档 https://api-docs.deepseek.com/guides/search/ 后按此实现：
// POST {base}/v1/tools/search，body 用 query / count / need_content。
// 站点、时间范围与语言不是独立参数，而是 query 里的检索算子：
// site:<domain>、after:YYYY-MM-DD、lang:<code>。
const deepSeekDefaultBase = "https://api.deepseek.com"

type deepSeekProvider struct {
	key     string
	baseURL string
	out     *outbound
	now     func() time.Time
}

func newDeepSeekProvider(config Config, out *outbound) *deepSeekProvider {
	return &deepSeekProvider{
		key:     strings.TrimSpace(config.DeepSeekKey),
		baseURL: deepSeekDefaultBase,
		out:     out,
		now:     time.Now,
	}
}

func (p *deepSeekProvider) Name() string { return providerDeepSeek }

// Topics 声明 general 与 news：DeepSeek 没有独立的 news 端点，
// 新闻语义由 after: 算子与检索词共同表达。
func (p *deepSeekProvider) Topics() []string { return []string{topicGeneral, topicNews} }

type deepSeekResponse struct {
	Results []struct {
		Title     string  `json:"title"`
		URL       string  `json:"url"`
		Snippet   string  `json:"snippet"`
		Summary   string  `json:"summary"`
		Content   string  `json:"content"`
		SiteName  string  `json:"site_name"`
		Date      string  `json:"date"`
		Authority int     `json:"authority"`
		Rerank    float64 `json:"rerank_score"`
	} `json:"results"`
}

func (p *deepSeekProvider) Search(ctx context.Context, req Request) ([]Result, error) {
	payload := map[string]any{
		"query":        buildDeepSeekQuery(req, p.now()),
		"count":        req.Count,
		"need_content": false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &searchFailure{Provider: p.Name(), Reason: "无法编码请求", Retryable: false}
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+p.key)
	header.Set("Content-Type", "application/json")

	endpoint := strings.TrimRight(p.baseURL, "/") + "/v1/tools/search"
	status, body, err := p.out.do(ctx, http.MethodPost, endpoint, header, encoded)
	if err != nil {
		return nil, transportFailure(p.Name(), err)
	}
	if status < 200 || status > 299 {
		return nil, statusFailure(p.Name(), status, providerErrorText(body))
	}
	var response deepSeekResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, decodeFailure(p.Name())
	}
	results := make([]Result, 0, len(response.Results))
	for _, item := range response.Results {
		results = append(results, Result{
			Title: item.Title,
			URL:   item.URL,
			// snippet 是给界面看的短摘要；summary 是官方说明「给模型读」的长摘要。
			Snippet: item.Snippet,
			Summary: item.Summary,
			Date:    normalizeDate(item.Date),
			Site:    item.SiteName,
		})
	}
	return results, nil
}

// buildDeepSeekQuery 把抽象参数翻译成检索算子。
func buildDeepSeekQuery(req Request, now time.Time) string {
	parts := []string{req.Query}
	if site := normalizeSite(req.Site); site != "" {
		parts = append(parts, "site:"+site)
	}
	if after := deepSeekAfter(req.Freshness, now); after != "" {
		parts = append(parts, "after:"+after)
	}
	if language := strings.TrimSpace(req.Language); language != "" {
		parts = append(parts, "lang:"+language)
	}
	return strings.Join(parts, " ")
}

// deepSeekAfter 把时间范围换算成「今天往前 N 天」的日期。
// day=1 / week=7 / month=30 / year=365，与设置页说明一致。
func deepSeekAfter(freshness string, now time.Time) string {
	days := 0
	switch freshness {
	case "day":
		days = 1
	case "week":
		days = 7
	case "month":
		days = 30
	case "year":
		days = 365
	default:
		return ""
	}
	return now.UTC().AddDate(0, 0, -days).Format("2006-01-02")
}
