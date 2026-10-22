package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// AnySearch 适配器。
//
// 已核对官方客户端源码后按此实现：POST {base}/v1/search，body 用 query /
// max_results / tag，zone 与 language 可选。没有 key 时**不带 Authorization**，
// 走匿名模式（按来源 IP 限流并计入日免费额度，这一点写在设置页 callout 里）。
const (
	anySearchDefaultBase = "https://api.anysearch.com"
	// anySearchTag 是 AnySearch 的检索标签：general.general 表示通用检索。
	anySearchTag = "general.general"
	// anySearchClient 是 AnySearch 用来识别调用方的客户端标识。
	anySearchClient = "sforum-web-search/1.0.0"
)

type anySearchProvider struct {
	key     string
	zone    string
	baseURL string
	out     *outbound
}

func newAnySearchProvider(config Config, out *outbound) *anySearchProvider {
	return &anySearchProvider{
		key:     strings.TrimSpace(config.AnySearchKey),
		zone:    normalizeZone(config.AnySearchZone),
		baseURL: anySearchDefaultBase,
		out:     out,
	}
}

func (p *anySearchProvider) Name() string { return providerAnySearch }

// Topics 只声明 general：AnySearch 的 tag 里没有「只找新闻」这种语义，
// 所以 topic=news 时它不参与候选，而不是让它返回一堆通用结果冒充新闻。
func (p *anySearchProvider) Topics() []string { return []string{topicGeneral} }

type anySearchResponse struct {
	Data struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			// content 是正文片段，本插件只要摘要，因此刻意不读它。
			Content string `json:"content"`
		} `json:"results"`
		Metadata struct {
			TotalResults int `json:"total_results"`
			SearchTimeMS int `json:"search_time_ms"`
		} `json:"metadata"`
	} `json:"data"`
}

func (p *anySearchProvider) Search(ctx context.Context, req Request) ([]Result, error) {
	payload := map[string]any{
		"query":       req.Query,
		"max_results": req.Count,
		"tag":         anySearchTag,
	}
	if p.zone != "" && p.zone != zoneAuto {
		payload["zone"] = p.zone
	}
	if language := anySearchLanguage(req.Language); language != "" {
		payload["language"] = language
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &searchFailure{Provider: p.Name(), Reason: "无法编码请求", Retryable: false}
	}

	header := http.Header{}
	header.Set("accept", "application/json")
	header.Set("content-type", "application/json")
	header.Set("x-anysearch-client", anySearchClient)
	if p.key != "" {
		header.Set("authorization", "Bearer "+p.key)
	}

	endpoint := strings.TrimRight(p.baseURL, "/") + "/v1/search"
	status, body, err := p.out.do(ctx, http.MethodPost, endpoint, header, encoded)
	if err != nil {
		return nil, transportFailure(p.Name(), err)
	}
	if status < 200 || status > 299 {
		return nil, statusFailure(p.Name(), status, providerErrorText(body))
	}
	var response anySearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, decodeFailure(p.Name())
	}
	results := make([]Result, 0, len(response.Data.Results))
	for _, item := range response.Data.Results {
		results = append(results, Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Snippet,
			Site:    hostOf(item.URL),
		})
	}
	return results, nil
}

// anySearchLanguage 只发送 AnySearch 明确接受的 zh / en 两种语言；
// 其它语言交给后端默认（否则可能被当成非法参数整单拒绝）。
func anySearchLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "zh":
		return "zh"
	case "en":
		return "en"
	default:
		return ""
	}
}
