package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

func TestCanRetryAfterCooldownWait(t *testing.T) {
	tests := []struct {
		name   string
		result *proxyResult
		want   bool
	}{
		{
			name:   "nil 结果不重试",
			result: nil,
			want:   false,
		},
		{
			name:   "客户端取消不重试",
			result: &proxyResult{isClientCanceled: true, nextAction: cooldown.ActionRetryChannel},
			want:   false,
		},
		{
			name:   "响应已成功提交不重试",
			result: &proxyResult{succeeded: true, nextAction: cooldown.ActionReturnClient},
			want:   false,
		},
		{
			name:   "客户端语义错误不重试",
			result: &proxyResult{status: 400, nextAction: cooldown.ActionReturnClient},
			want:   false,
		},
		{
			name:   "渠道级失败可重试",
			result: &proxyResult{status: 429, nextAction: cooldown.ActionRetryChannel},
			want:   true,
		},
		{
			name:   "Key级失败可重试",
			result: &proxyResult{status: 429, nextAction: cooldown.ActionRetryKey},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canRetryAfterCooldownWait(tt.result); got != tt.want {
				t.Errorf("canRetryAfterCooldownWait() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newCooldownWaitTestServer 建一个只带 store 的 Server，够用于冷却查询。
func newCooldownWaitTestServer(t *testing.T) (*Server, context.Context) {
	t.Helper()

	store, cleanup := setupTestStore(t)
	t.Cleanup(cleanup)

	return &Server{store: store}, context.Background()
}

func createCooldownWaitChannel(t *testing.T, srv *Server, ctx context.Context, name string) *model.Config {
	t.Helper()

	cfg, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         name,
		URL:          "https://api.example.com",
		Priority:     100,
		Enabled:      true,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	return cfg
}

func TestEarliestReadyAtWhenAllCooled(t *testing.T) {
	t.Run("存在可用渠道时不等待", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		cooled := createCooldownWaitChannel(t, srv, ctx, "cooled")
		available := createCooldownWaitChannel(t, srv, ctx, "available")

		if err := srv.store.SetChannelCooldown(ctx, cooled.ID, time.Now().Add(30*time.Second)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		if _, ok := srv.earliestReadyAtWhenAllCooled(ctx, []*model.Config{cooled, available}); ok {
			t.Error("仍有可用渠道时应返回 ok=false（等待无意义）")
		}
	})

	t.Run("全部冷却时返回最早恢复时刻", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		soon := createCooldownWaitChannel(t, srv, ctx, "soon")
		later := createCooldownWaitChannel(t, srv, ctx, "later")

		soonAt := time.Now().Add(5 * time.Second)
		if err := srv.store.SetChannelCooldown(ctx, soon.ID, soonAt); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}
		if err := srv.store.SetChannelCooldown(ctx, later.ID, time.Now().Add(120*time.Second)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		readyAt, ok := srv.earliestReadyAtWhenAllCooled(ctx, []*model.Config{later, soon})
		if !ok {
			t.Fatal("全部冷却时应返回 ok=true")
		}
		if diff := readyAt.Sub(soonAt); diff > 2*time.Second || diff < -2*time.Second {
			t.Errorf("readyAt=%v, want 约 %v（最早恢复的渠道）", readyAt, soonAt)
		}
	})

	t.Run("空候选返回 false", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		if _, ok := srv.earliestReadyAtWhenAllCooled(ctx, nil); ok {
			t.Error("空候选应返回 ok=false")
		}
	})
}

func TestWaitForCooledCandidates(t *testing.T) {
	retryable := &proxyResult{status: 429, nextAction: cooldown.ActionRetryChannel}

	t.Run("预算为0不等待", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		cfg := createCooldownWaitChannel(t, srv, ctx, "cooled")
		if err := srv.store.SetChannelCooldown(ctx, cfg.ID, time.Now().Add(time.Second)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		if srv.waitForCooledCandidates(ctx, []*model.Config{cfg}, retryable, 0) {
			t.Error("预算为0时不应等待")
		}
	})

	t.Run("恢复时刻超出预算不等待", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		cfg := createCooldownWaitChannel(t, srv, ctx, "cooled")
		if err := srv.store.SetChannelCooldown(ctx, cfg.ID, time.Now().Add(10*time.Minute)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		start := time.Now()
		if srv.waitForCooledCandidates(ctx, []*model.Config{cfg}, retryable, 30*time.Second) {
			t.Error("恢复时刻超出预算时不应等待")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("判定应立即返回，实际耗时 %v", elapsed)
		}
	})

	t.Run("恢复时刻在预算内则等到该时刻", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		cfg := createCooldownWaitChannel(t, srv, ctx, "cooled")
		// 冷却时刻按 unix 秒存储，取 2s 以免被截断成“已过期”。
		readyAt := time.Now().Add(2 * time.Second)
		if err := srv.store.SetChannelCooldown(ctx, cfg.ID, readyAt); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		start := time.Now()
		if !srv.waitForCooledCandidates(ctx, []*model.Config{cfg}, retryable, 30*time.Second) {
			t.Fatal("恢复时刻在预算内时应等待并返回 true")
		}
		if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
			t.Errorf("应等到冷却结束，实际只等了 %v", elapsed)
		}
	})

	t.Run("客户端取消时立即放弃等待", func(t *testing.T) {
		srv, baseCtx := newCooldownWaitTestServer(t)
		cfg := createCooldownWaitChannel(t, srv, baseCtx, "cooled")
		if err := srv.store.SetChannelCooldown(baseCtx, cfg.ID, time.Now().Add(20*time.Second)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		ctx, cancel := context.WithCancel(baseCtx)
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		defer cancel()

		start := time.Now()
		if srv.waitForCooledCandidates(ctx, []*model.Config{cfg}, retryable, 30*time.Second) {
			t.Error("context 取消后不应继续重试")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("取消后应立即返回，实际耗时 %v", elapsed)
		}
	})

	t.Run("不可重试的失败结果不等待", func(t *testing.T) {
		srv, ctx := newCooldownWaitTestServer(t)
		cfg := createCooldownWaitChannel(t, srv, ctx, "cooled")
		if err := srv.store.SetChannelCooldown(ctx, cfg.ID, time.Now().Add(time.Second)); err != nil {
			t.Fatalf("SetChannelCooldown 失败: %v", err)
		}

		clientErr := &proxyResult{status: 400, nextAction: cooldown.ActionReturnClient}
		if srv.waitForCooledCandidates(ctx, []*model.Config{cfg}, clientErr, 30*time.Second) {
			t.Error("客户端语义错误不应等待重试")
		}
	})
}
