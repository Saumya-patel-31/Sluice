package policy

import (
	"container/list"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// Cache memoises policy results.
//
// Policy evaluation is the one part of the request path whose cost grows with
// operator input: a document with fifty constraints against twelve backends is
// six hundred expression evaluations per request. Real traffic is extremely
// repetitive at this granularity — the same service calling the same path with
// the same identity — so a small cache removes almost all of that work.
//
// Correctness rests on the key covering every input the engine can observe.
// Everything a policy can read is either in the key or bounded by the TTL:
// subject, request and candidate set are hashed exactly; wall-clock attributes
// are covered by keeping entries short-lived.
type Cache struct {
	ttl    time.Duration
	shards [cacheShards]*cacheShard
	hits   atomic.Uint64
	misses atomic.Uint64
	now    func() time.Time
}

const cacheShards = 16

type cacheShard struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[uint64]*list.Element
}

type cacheEntry struct {
	key     uint64
	res     Result
	expires time.Time
}

// NewCache returns a cache holding roughly capacity entries in total.
func NewCache(capacity int, ttl time.Duration) *Cache {
	if capacity < cacheShards {
		capacity = cacheShards
	}
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	c := &Cache{ttl: ttl, now: time.Now}
	per := capacity / cacheShards
	for i := range c.shards {
		c.shards[i] = &cacheShard{
			capacity: per,
			ll:       list.New(),
			items:    make(map[uint64]*list.Element, per),
		}
	}
	return c
}

// Get returns a cached result.
func (c *Cache) Get(key uint64) (Result, bool) {
	sh := c.shards[key%cacheShards]
	sh.mu.Lock()
	el, ok := sh.items[key]
	if !ok {
		sh.mu.Unlock()
		c.misses.Add(1)
		return Result{}, false
	}
	ent := el.Value.(*cacheEntry)
	if c.now().After(ent.expires) {
		sh.ll.Remove(el)
		delete(sh.items, key)
		sh.mu.Unlock()
		c.misses.Add(1)
		return Result{}, false
	}
	sh.ll.MoveToFront(el)
	res := ent.res
	sh.mu.Unlock()

	c.hits.Add(1)
	return res.clone(), true
}

// Put stores a result.
func (c *Cache) Put(key uint64, res Result) {
	sh := c.shards[key%cacheShards]
	stored := res.clone()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	if el, ok := sh.items[key]; ok {
		ent := el.Value.(*cacheEntry)
		ent.res = stored
		ent.expires = c.now().Add(c.ttl)
		sh.ll.MoveToFront(el)
		return
	}
	el := sh.ll.PushFront(&cacheEntry{key: key, res: stored, expires: c.now().Add(c.ttl)})
	sh.items[key] = el

	for sh.ll.Len() > sh.capacity {
		oldest := sh.ll.Back()
		if oldest == nil {
			break
		}
		sh.ll.Remove(oldest)
		delete(sh.items, oldest.Value.(*cacheEntry).key)
	}
}

// Purge empties the cache. Called on every policy reload, since a new document
// invalidates every memoised verdict.
func (c *Cache) Purge() {
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.ll.Init()
		sh.items = make(map[uint64]*list.Element, sh.capacity)
		sh.mu.Unlock()
	}
}

// Stats returns lifetime hit and miss counts.
func (c *Cache) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// HitRate returns the lifetime cache hit rate in [0,1].
func (c *Cache) HitRate() float64 {
	h, m := c.Stats()
	if h+m == 0 {
		return 0
	}
	return float64(h) / float64(h+m)
}

// clone deep-copies the mutable parts of a Result so a cached entry cannot be
// mutated by whoever received a previous copy.
func (r Result) clone() Result {
	out := r
	out.Eligible = append([]string(nil), r.Eligible...)
	out.Trace = append([]model.PolicyHit(nil), r.Trace...)
	out.Pruned = make(map[string]string, len(r.Pruned))
	for k, v := range r.Pruned {
		out.Pruned[k] = v
	}
	return out
}

// CacheKey derives a cache key covering every attribute a policy can read,
// plus the identity of the policy document itself.
func CacheKey(setHash string, in Input) uint64 {
	h := fnv.New64a()
	ws := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	wb := func(b bool) {
		if b {
			ws("1")
		} else {
			ws("0")
		}
	}

	ws(setHash)

	if s := in.Subject; s != nil {
		ws(s.ID)
		ws(s.TrustDomain)
		ws(s.Namespace)
		ws(s.Service)
		ws(s.Issuer)
		wb(s.MTLS)
		wb(s.Authenticated)
		writeSortedMap(ws, s.Claims)
	} else {
		ws("<nil-subject>")
	}

	if r := in.Request; r != nil {
		ws(r.Method)
		ws(r.Path)
		ws(r.Host)
		ws(r.SourceIP)
		ws(r.SourceGeo)
		ws(string(r.DataClass))
		writeSortedMap(ws, r.Headers)
	} else {
		ws("<nil-request>")
	}

	// The candidate set is part of the key because constraints prune from it;
	// the same request against a different pool is a different question.
	ids := make([]string, 0, len(in.Candidates))
	for _, b := range in.Candidates {
		suffix := "-"
		if b.Enabled {
			suffix = "+"
		}
		ids = append(ids, b.ID+suffix)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ws(id)
	}

	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		ws(strconv.FormatFloat(in.BaseObjectives[d], 'f', 4, 64))
	}

	return h.Sum64()
}

func writeSortedMap(ws func(string), m map[string]string) {
	if len(m) == 0 {
		ws("{}")
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ws(k)
		ws(m[k])
	}
}
