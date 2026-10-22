package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 进程内 LRU + TTL 缓存。
//
// 为什么需要它：一次 AI 回复里模型可能连续问两三个近似的问题，而每一家搜索
// 后端都按请求计费。缓存把「同一份检索」压成一次出站请求。
//
// 为什么 key 不含后端名：缓存描述的是「这个问题问过没有」，不是「这个后端答过
// 没有」。代价是换后端后短时间内仍可能拿到旧后端的结果——README 写明了这一点，
// 缓存值里也因此记住是哪个后端答的，结果头会如实标出引擎名。
type cacheEntry struct {
	Provider string
	Results  []Result
	Expires  time.Time
}

type cacheItem struct {
	key   string
	entry cacheEntry
}

type resultCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	order    *list.List
}

func newResultCache(capacity int) *resultCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &resultCache{
		capacity: capacity,
		items:    make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

// Get 返回未过期的条目，并把它移到队首（最近使用）。
func (c *resultCache) Get(key string, now time.Time) (cacheEntry, bool) {
	if c == nil {
		return cacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return cacheEntry{}, false
	}
	item := element.Value.(*cacheItem)
	if !now.Before(item.entry.Expires) {
		c.order.Remove(element)
		delete(c.items, key)
		return cacheEntry{}, false
	}
	c.order.MoveToFront(element)
	return item.entry, true
}

// Put 写入条目，超容量时淘汰队尾（最久未使用）。
func (c *resultCache) Put(key string, entry cacheEntry) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		element.Value.(*cacheItem).entry = entry
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&cacheItem{key: key, entry: entry})
	c.items[key] = element
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheItem).key)
	}
}

func (c *resultCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// cacheKeyFor 只把「这次检索问的是什么」放进 key。
func cacheKeyFor(req Request) string {
	joined := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(req.Query)),
		req.Topic,
		req.Freshness,
		req.Site,
		strconv.Itoa(req.Count),
		req.Language,
		req.SafeSearch,
	}, "|")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}
