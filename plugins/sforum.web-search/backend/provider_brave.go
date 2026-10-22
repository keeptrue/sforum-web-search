package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Brave 适配器。
//
// 已核对官方文档（2026-10-19）后按此实现：
//   - Web Search：GET https://api.search.brave.com/res/v1/web/search
//     参数 q / count(1-20) / freshness(pd|pw|pm|py) / safesearch(off|moderate|strict)
//     / search_lang；站点限定走 q 里的 site: 算子；
//     响应 web.results[]{title,url,description,page_age,meta_url.hostname}。
//   - News Search：GET https://api.search.brave.com/res/v1/news/search
//     参数同上（count 上限 50）；响应顶层 results[]，字段同上（另有 age/breaking）。
//   - 鉴权头是 X-Subscription-Token（不是 Authorization: Bearer）。
//
// 文档来源：
//
//	https://api-dashboard.search.brave.com/app/documentation/web-search/query
//	https://api-dashboard.search.brave.com/api-reference/web/search/get
//	https://api-dashboard.search.brave.com/app/documentation/news-search/query
//	https://api-dashboard.search.brave.com/api-reference/news/news_search/get
const braveDefaultBase = "https://api.search.brave.com"

type braveProvider struct {
	key     string
	baseURL string
	out     *outbound
}

func newBraveProvider(config Config, out *outbound) *braveProvider {
	return &braveProvider{key: strings.TrimSpace(config.BraveKey), baseURL: braveDefaultBase, out: out}
}

func (p *braveProvider) Name() string { return providerBrave }

func (p *braveProvider) Topics() []string { return []string{topicGeneral, topicNews} }

// braveResult 同时覆盖 web 与 news 两种结果项：两者共享 title/url/description/
// page_age/meta_url，差异字段（age、breaking）本插件不使用。
type braveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	PageAge     string `json:"page_age"`
	Age         string `json:"age"`
	MetaURL     struct {
		Hostname string `json:"hostname"`
	} `json:"meta_url"`
}

type braveWebResponse struct {
	Web struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
}

type braveNewsResponse struct {
	Results []braveResult `json:"results"`
}

func (p *braveProvider) Search(ctx context.Context, req Request) ([]Result, error) {
	path := "/res/v1/web/search"
	if req.Topic == topicNews {
		path = "/res/v1/news/search"
	}
	query := strings.TrimSpace(req.Query)
	if site := normalizeSite(req.Site); site != "" {
		query += " site:" + site
	}
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", strconv.Itoa(req.Count))
	params.Set("safesearch", braveSafeSearch(req.SafeSearch))
	if freshness := braveFreshness(req.Freshness); freshness != "" {
		params.Set("freshness", freshness)
	}
	if language := strings.TrimSpace(req.Language); language != "" {
		params.Set("search_lang", language)
	}

	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("X-Subscription-Token", p.key)

	endpoint := strings.TrimRight(p.baseURL, "/") + path + "?" + params.Encode()
	status, body, err := p.out.do(ctx, http.MethodGet, endpoint, header, nil)
	if err != nil {
		return nil, transportFailure(p.Name(), err)
	}
	if status < 200 || status > 299 {
		return nil, statusFailure(p.Name(), status, providerErrorText(body))
	}

	items := make([]braveResult, 0)
	if req.Topic == topicNews {
		var response braveNewsResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, decodeFailure(p.Name())
		}
		items = response.Results
	} else {
		var response braveWebResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, decodeFailure(p.Name())
		}
		items = response.Web.Results
	}

	results := make([]Result, 0, len(items))
	for _, item := range items {
		site := item.MetaURL.Hostname
		if site == "" {
			site = hostOf(item.URL)
		}
		results = append(results, Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Description,
			Date:    normalizeDate(item.PageAge),
			Site:    site,
		})
	}
	return results, nil
}

// braveFreshness 把抽象时间范围映射到 Brave 的 freshness 取值。
func braveFreshness(freshness string) string {
	switch freshness {
	case "day":
		return "pd"
	case "week":
		return "pw"
	case "month":
		return "pm"
	case "year":
		return "py"
	default:
		return ""
	}
}

// braveSafeSearch 直接透传三档取值（Brave 支持 off/moderate/strict 全部三档）。
func braveSafeSearch(safeSearch string) string {
	switch safeSearch {
	case safeSearchOff, safeSearchModerate, safeSearchStrict:
		return safeSearch
	default:
		return safeSearchModerate
	}
}
