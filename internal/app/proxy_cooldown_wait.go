package app

import (
	"context"
	"log"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

// waitForCooledCandidates 在「一轮候选全部失败」之后，判断是否值得等待渠道冷却结束再打一轮。
//
// 触发条件（全部满足才等）：
//   - 配置了等待预算（cooldown_all_cooled_wait_seconds > 0）且预算还有剩余
//   - 本轮失败结果允许重试（非客户端取消、非 ActionReturnClient）
//   - 候选池里**没有任何**渠道当前可用（否则应该由正常选路继续尝试，而不是干等）
//   - 最早恢复的渠道落在剩余预算之内
//
// 设计取舍：只在"全部候选都在冷却"时等待。只要还有可用渠道，等待就是纯浪费——
// 上一轮已经把它们都试过了，失败原因不是冷却，等下去不会变好。
//
// 返回 true 表示已等到目标时刻，调用方应重新选路并再打一轮。
func (s *Server) waitForCooledCandidates(
	ctx context.Context,
	cands []*model.Config,
	lastResult *proxyResult,
	budgetLeft time.Duration,
) bool {
	if budgetLeft <= 0 || len(cands) == 0 {
		return false
	}
	if !canRetryAfterCooldownWait(lastResult) {
		return false
	}

	readyAt, ok := s.earliestReadyAtWhenAllCooled(ctx, cands)
	if !ok {
		return false
	}

	waitFor := time.Until(readyAt)
	if waitFor <= 0 {
		// 冷却刚好在查询与计算之间到期：无需等待，直接重试。
		return true
	}
	if waitFor > budgetLeft {
		log.Printf("[INFO] 所有候选渠道冷却中，最早 %.1fs 后恢复，超出等待预算 %.1fs，放弃等待",
			waitFor.Seconds(), budgetLeft.Seconds())
		return false
	}

	log.Printf("[INFO] 所有候选渠道冷却中，等待 %.1fs 待渠道恢复后重试", waitFor.Seconds())

	timer := time.NewTimer(waitFor)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		// 客户端断开或请求超时：不再重试。
		return false
	}
}

// canRetryAfterCooldownWait 判断本轮失败结果是否允许「等一会儿再试」。
func canRetryAfterCooldownWait(lastResult *proxyResult) bool {
	if lastResult == nil {
		return false
	}
	if lastResult.isClientCanceled {
		return false
	}
	// 响应已提交给客户端后重试在 HTTP 协议层面不可能。
	if lastResult.succeeded {
		return false
	}
	// 客户端语义错误（400/406/413 等）重试多少次都是同样结果。
	return lastResult.nextAction != cooldown.ActionReturnClient
}

// earliestReadyAtWhenAllCooled 返回候选池中最早的恢复时刻。
// 只要存在任何一个当前可用（未冷却）的候选，就返回 ok=false —— 那种情况不该等。
func (s *Server) earliestReadyAtWhenAllCooled(ctx context.Context, cands []*model.Config) (time.Time, bool) {
	// 冷却状态读取失败时不猜：宁可不等，也不要凭空睡一觉。
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 等待判定：获取渠道冷却状态失败，跳过等待: %v", err)
		return time.Time{}, false
	}
	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 等待判定：获取 Key 冷却状态失败，跳过等待: %v", err)
		return time.Time{}, false
	}

	now := time.Now()
	var earliest time.Time
	for _, cfg := range cands {
		if cfg == nil {
			continue
		}
		readyAt := channelReadyAt(cfg, channelCooldowns, keyCooldowns, now)
		if !readyAt.After(now) {
			// 存在当前可用的候选：等待无意义。
			return time.Time{}, false
		}
		if earliest.IsZero() || readyAt.Before(earliest) {
			earliest = readyAt
		}
	}

	if earliest.IsZero() {
		return time.Time{}, false
	}
	return earliest, true
}
