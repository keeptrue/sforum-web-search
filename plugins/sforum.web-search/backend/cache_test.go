package main

import (
	"strings"
	"testing"
	"time"
)

func TestResultCacheHitAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cache := newResultCache(4)
	key := cacheKeyFor(Request{Query: "sforum", Topic: topicGeneral, Count: 5})
	cache.Put(key, cacheEntry{Provider: providerDeepSeek, Results: sampleResults(), Expires: now.Add(time.Minute)})

	entry, ok := cache.Get(key, now)
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if entry.Provider != providerDeepSeek || len(entry.Results) != 1 {
		t.Fatalf("entry = %+v", entry)
	}

	if _, ok := cache.Get(key, now.Add(time.Minute)); ok {
		t.Fatal("entry must expire at its deadline")
	}
	if cache.Len() != 0 {
		t.Errorf("expired entries must be dropped, len = %d", cache.Len())
	}
}

func TestResultCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newResultCache(2)
	now := time.Now()
	cache.Put("a", cacheEntry{Provider: "a", Expires: now.Add(time.Minute)})
	cache.Put("b", cacheEntry{Provider: "b", Expires: now.Add(time.Minute)})
	// 访问 a，让 b 成为最久未使用。
	if _, ok := cache.Get("a", now); !ok {
		t.Fatal("expected a to be present")
	}
	cache.Put("c", cacheEntry{Provider: "c", Expires: now.Add(time.Minute)})

	if cache.Len() != 2 {
		t.Fatalf("len = %d, want capacity 2", cache.Len())
	}
	if _, ok := cache.Get("b", now); ok {
		t.Error("b should have been evicted as least recently used")
	}
	if _, ok := cache.Get("a", now); !ok {
		t.Error("a should still be cached")
	}
	if _, ok := cache.Get("c", now); !ok {
		t.Error("c should be cached")
	}
}

func TestCacheKeyCoversEveryQueryField(t *testing.T) {
	base := Request{Query: "sforum", Topic: topicGeneral, Freshness: "week", Site: "example.com",
		Count: 5, Language: "zh", SafeSearch: safeSearchModerate}
	key := cacheKeyFor(base)
	if key == "" {
		t.Fatal("cache key must not be empty")
	}
	variants := []Request{
		{Query: "sforum2", Topic: base.Topic, Freshness: base.Freshness, Site: base.Site, Count: base.Count, Language: base.Language, SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: topicNews, Freshness: base.Freshness, Site: base.Site, Count: base.Count, Language: base.Language, SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: base.Topic, Freshness: "day", Site: base.Site, Count: base.Count, Language: base.Language, SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: base.Topic, Freshness: base.Freshness, Site: "other.com", Count: base.Count, Language: base.Language, SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: base.Topic, Freshness: base.Freshness, Site: base.Site, Count: 6, Language: base.Language, SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: base.Topic, Freshness: base.Freshness, Site: base.Site, Count: base.Count, Language: "en", SafeSearch: base.SafeSearch},
		{Query: base.Query, Topic: base.Topic, Freshness: base.Freshness, Site: base.Site, Count: base.Count, Language: base.Language, SafeSearch: safeSearchStrict},
	}
	for index, variant := range variants {
		if cacheKeyFor(variant) == key {
			t.Errorf("variant %d produced the same cache key", index)
		}
	}
	// 大小写不同的同一个查询应当命中同一条缓存。
	upper := base
	upper.Query = strings.ToUpper(base.Query)
	if cacheKeyFor(upper) != key {
		t.Error("query casing must not split the cache")
	}
}
