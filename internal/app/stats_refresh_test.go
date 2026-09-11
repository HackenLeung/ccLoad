package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

type refreshStatsStore struct {
	storage.Store
	stats  []model.StatsEntry
	mu     sync.Mutex
	ranges [][2]time.Time
}

func (s *refreshStatsStore) GetStats(context.Context, time.Time, time.Time, *model.LogFilter, bool) ([]model.StatsEntry, error) {
	return s.stats, nil
}

func (s *refreshStatsStore) GetRPMStats(context.Context, time.Time, time.Time, *model.LogFilter, bool) (*model.RPMStats, error) {
	return &model.RPMStats{}, nil
}

func (s *refreshStatsStore) GetHealthTimeline(context.Context, model.HealthTimelineParams) ([]model.HealthTimelineRow, error) {
	return nil, nil
}

func (s *refreshStatsStore) GetStatsLite(_ context.Context, start, end time.Time, _ *model.LogFilter) ([]model.StatsEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranges = append(s.ranges, [2]time.Time{start, end})
	return []model.StatsEntry{{Total: len(s.ranges)}}, nil
}

func TestHandleStats_ConcurrentResponsesDoNotMutateCache(t *testing.T) {
	channelID := 1
	store := &refreshStatsStore{stats: []model.StatsEntry{{ChannelID: &channelID, Model: "m"}}}
	cache := NewStatsCache(store)
	defer cache.Close()
	s := &Server{store: store, statsCache: cache}
	request := func() {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/stats?range=yesterday", nil)
		s.HandleStats(c)
		if r.Code != http.StatusOK {
			t.Errorf("stats status=%d: %s", r.Code, r.Body.String())
		}
	}
	request() // 填充缓存，使并发请求共享同一缓存结果。
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(request)
	}
	wg.Wait()
	if store.stats[0].HealthTimeline != nil {
		t.Fatal("request health timeline leaked into cached statistics")
	}
}

func TestStatsCache_RecentMinuteReusesWindowWithoutChangingCustomRanges(t *testing.T) {
	store := &refreshStatsStore{}
	cache := NewStatsCache(store)
	defer cache.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute).Add(5 * time.Second)
	for _, at := range []time.Time{now, now.Add(10 * time.Second), now.Add(30 * time.Second)} {
		if _, err := cache.GetRecentMinuteStats(ctx, at, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.ranges) != 2 {
		t.Fatalf("expected two time buckets, got %d queries", len(store.ranges))
	}
	if got := store.ranges[0]; !got[0].Equal(now.Add(-time.Minute)) || !got[1].Equal(now) {
		t.Fatalf("query must include the complete recent minute: %v", got)
	}
	for _, start := range []time.Time{now.Add(-time.Hour), now.Add(-time.Hour + time.Second)} {
		if _, err := cache.GetStatsLite(ctx, start, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.ranges) != 4 {
		t.Fatal("distinct custom ranges must not share a cache entry")
	}
}

func TestStatsCache_CumulativeTimestampAndExplicitRefresh(t *testing.T) {
	store := &refreshStatsStore{}
	cache := NewStatsCache(store)
	defer cache.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour).Add(time.Minute)
	first, updatedAt, err := cache.GetStatsLiteWithTTL(ctx, time.Unix(0, 0), now, nil, time.Hour, false)
	if err != nil || updatedAt.IsZero() {
		t.Fatalf("first query: updatedAt=%v err=%v", updatedAt, err)
	}
	cached, cachedAt, err := cache.GetStatsLiteWithTTL(ctx, time.Unix(0, 0), now.Add(time.Minute), nil, time.Hour, false)
	if err != nil || !cachedAt.Equal(updatedAt) || cached[0].Total != first[0].Total || len(store.ranges) != 1 {
		t.Fatalf("cache hit must preserve timestamp and skip database: %v", err)
	}
	fresh, refreshedAt, err := cache.GetStatsLiteWithTTL(ctx, time.Unix(0, 0), now, nil, time.Hour, true)
	if err != nil || refreshedAt.Before(updatedAt) || fresh[0].Total != 2 {
		t.Fatalf("explicit refresh must query again: %v", err)
	}
}

func TestStatsCache_HardCapacityUnderConcurrentWrites(t *testing.T) {
	cache := NewStatsCache(nil)
	defer cache.Close()
	var wg sync.WaitGroup
	for i := range maxCacheEntries * 2 {
		wg.Go(func() {
			cache.storeCache(fmt.Sprint(i), &cachedStats{expiry: time.Now().Add(time.Hour)})
		})
	}
	wg.Wait()
	count := 0
	var existingKey string
	cache.cache.Range(func(key, _ any) bool {
		count++
		existingKey = key.(string)
		return true
	})
	if count != maxCacheEntries || cache.entryCount.Load() != int64(count) {
		t.Fatalf("capacity/count mismatch: actual=%d tracked=%d", count, cache.entryCount.Load())
	}
	// 满额仍能更新；清理与新增操作保持计数一致。
	cache.storeCache(existingKey, &cachedStats{expiry: time.Now().Add(-time.Second)})
	cache.cleanupExpired()
	cache.storeCache("after-cleanup", &cachedStats{expiry: time.Now().Add(time.Hour)})
	if _, ok := cache.cache.Load("after-cleanup"); !ok || cache.entryCount.Load() != maxCacheEntries {
		t.Fatal("expired entry did not release capacity")
	}
}
