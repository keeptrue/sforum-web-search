package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Tavily 适配器。
//
// 已核对官方文档
// https://docs.tavily.com/documentation/api-reference/endpoint/search
// （2026-10-19）后按此实现：
//   - POST https://api.tavily.com/search，Authorization: Bearer <key>；
//   - body：query / max_results / topic(general|news) / time_range(day|week|month|year)
//     / include_domains / search_depth / include_published_date / safe_search；
//   - 响应：results[]{title,url,content,score,published_date,...}，没有独立摘要字段。
//
// 与抽象契约的对应关系：query→query、max_results→max_results、topic→topic、
// days→time_range（当前文档没有 days 参数，时间窗口用 time_range + filter_by_published_date
// 表达，语义等价于「只返回该范围内发布的结果」）、include_domains→include_domains。
const tavilyDefaultBase = "https://api.tavily.com"

type tavilyProvider struct {
	key     string
	baseURL string
	out     *outbound
}

func newTavilyProvider(config Config, out *outbound) *tavilyProvider {
	return &tavilyProvider{key: strings.TrimSpace(config.TavilyKey), baseURL: tavilyDefaultBase, out: out}
}

func (p *tavilyProvider) Name() string { return providerTavily }

func (p *tavilyProvider) Topics() []string { return []string{topicGeneral, topicNews} }

type tavilyResponse struct {
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date"`
	} `json:"results"`
	ResponseTime any `json:"response_time"`
}

func (p *tavilyProvider) Search(ctx context.Context, req Request) ([]Result, error) {
	topic := topicGeneral
	if req.Topic == topicNews {
		topic = topicNews
	}
	payload := map[string]any{
		"query":       req.Query,
		"max_results": req.Count,
		"topic":       topic,
		// basic 是「1 个额度 + 均衡延迟」的档位；advanced 会翻倍计费且更慢。
		"search_depth":           "basic",
		"include_answer":         false,
		"include_raw_content":    false,
		"include_published_date": true,
		"safe_search":            req.SafeSearch != safeSearchOff,
	}
	if site := normalizeSite(req.Site); site != "" {
		payload["include_domains"] = []string{site}
	}
	if window := tavilyTimeRange(req.Freshness); window != "" {
		payload["time_range"] = window
		// 默认只按时间窗口「加权」，带 filter_by_published_date 才是真正的过滤。
		payload["filter_by_published_date"] = true
	}
	if language := strings.TrimSpace(req.Language); language != "" {
		payload["language"] = language
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &searchFailure{Provider: p.Name(), Reason: "无法编码请求", Retryable: false}
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+p.key)
	header.Set("Content-Type", "application/json")

	endpoint := strings.TrimRight(p.baseURL, "/") + "/search"
	status, body, err := p.out.do(ctx, http.MethodPost, endpoint, header, encoded)
	if err != nil {
		return nil, transportFailure(p.Name(), err)
	}
	if status < 200 || status > 299 {
		return nil, statusFailure(p.Name(), status, providerErrorText(body))
	}
	var response tavilyResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, decodeFailure(p.Name())
	}
	results := make([]Result, 0, len(response.Results))
	for _, item := range response.Results {
		results = append(results, Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Content,
			Date:    normalizeDate(item.PublishedDate),
			Site:    hostOf(item.URL),
		})
	}
	return results, nil
}

// tavilyTimeRange 把抽象时间范围映射到文档里的 time_range 取值。
func tavilyTimeRange(freshness string) string {
	switch freshness {
	case "day", "week", "month", "year":
		return freshness
	default:
		return ""
	}
}

// normalizeDate 把各家不同的日期写法收敛成 YYYY-MM-DD。
// Tavily 用 RFC 1123（"Tue, 11 Mar 2025 17:00:00 GMT"），DeepSeek 用 RFC 3339，
// Brave 用 RFC 3339（page_age）或人类可读的 "2 days ago"。
// 解析不出来时保留原文的短形态，而不是丢掉日期。
func normalizeDate(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	layouts := []string{
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC().Format("2006-01-02")
		}
	}
	if len(trimmed) > 32 {
		return clampBytes(trimmed, 32)
	}
	return trimmed
}
