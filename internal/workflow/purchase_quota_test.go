package workflow

import (
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

func TestBatchReadyForPurchaseUsesSuccessSlots(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		batch domain.Batch
		want  bool
	}{
		{
			name: "成功与处理中数量未填满目标时继续购买",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 8,
				FulfilledCount: 1, InflightCount: 1,
				NextPurchaseAt: now.Add(-time.Second),
			},
			want: true,
		},
		{
			name: "失败号码释放处理中槽位后继续补购",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 9,
				FulfilledCount: 0, InflightCount: 0, NextPurchaseAt: now.Add(-time.Second),
			},
			want: true,
		},
		{
			name: "成功目标已由成功与处理中号码填满时等待",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 5,
				FulfilledCount: 1, InflightCount: 2, NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "跨实例已预占成功槽位时等待",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 5,
				FulfilledCount: 1, InflightCount: 1, PurchaseReservedCount: 1,
				NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "购买次数超过目标但成功槽仍空缺时继续购买",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 12,
				FulfilledCount: 2, NextPurchaseAt: now.Add(-time.Second),
			},
			want: true,
		},
		{
			name: "三个成功任务完成后停止购买",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 12,
				FulfilledCount: 3, NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "终态批次停止购买",
			batch: domain.Batch{
				Status: domain.BatchStatusCompleted, Quantity: 3, PurchasedCount: 2,
				NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "重试时间未到时等待",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 2,
				FulfilledCount: 1, InflightCount: 1,
				NextPurchaseAt: now.Add(time.Second),
			},
			want: false,
		},
		{
			name: "代理池耗尽时不再购买号码",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 2,
				ProxyTotal: 2, ProxyAvailable: 0, NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "直连模式不需要代理名额",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 2,
				ProxyTotal: 0, ProxyAvailable: 0, NextPurchaseAt: now.Add(-time.Second),
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := batchReadyForPurchase(test.batch, now); got != test.want {
				t.Fatalf("batchReadyForPurchase(%+v)=%t, want %t", test.batch, got, test.want)
			}
		})
	}
}

func TestBatchProxyPoolExhaustedOnlyStopsAfterInflightWorkFinishes(t *testing.T) {
	base := domain.Batch{
		Status:   domain.BatchStatusRunning,
		Quantity: 3, FulfilledCount: 1,
		ProxyTotal: 2, ProxyAvailable: 0,
	}
	if !batchProxyPoolExhausted(base) {
		t.Fatal("代理已耗尽且没有处理中号码时，应停止未达标任务")
	}
	withInflight := base
	withInflight.InflightCount = 1
	if batchProxyPoolExhausted(withInflight) {
		t.Fatal("仍有号码处理中时，应等待其结果后再判断代理池是否导致任务失败")
	}
	direct := base
	direct.ProxyTotal = 0
	if batchProxyPoolExhausted(direct) {
		t.Fatal("直连模式不应被判定为代理池耗尽")
	}
	completed := base
	completed.FulfilledCount = completed.Quantity
	if batchProxyPoolExhausted(completed) {
		t.Fatal("成功数已经达标时不应改写任务结果")
	}
}

func TestQuantityThreeRefillsFailuresUntilThreeTasksSucceed(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	batch := domain.Batch{
		Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 3,
		InflightCount: 3, NextPurchaseAt: now,
	}

	// One of the original three allocations fails and releases its processing
	// slot. The lifetime purchase counter stays at three, but a replacement is
	// immediately eligible.
	batch.InflightCount--
	if !batchReadyForPurchase(batch, now) {
		t.Fatal("失败任务释放槽位后应继续补购")
	}
	batch.PurchasedCount++
	batch.InflightCount++
	if batchReadyForPurchase(batch, now) {
		t.Fatal("补购落库后成功槽已填满，应等待处理结果")
	}

	// Two successes plus another failure open one more replacement even though
	// purchased_count is already greater than quantity.
	batch.FulfilledCount += 2
	batch.InflightCount -= 3
	if !batchReadyForPurchase(batch, now) {
		t.Fatal("购买次数超过计划数时仍应为缺少的成功任务补位")
	}
	batch.PurchasedCount++
	batch.InflightCount++

	// The replacement succeeds: fulfilled reaches the requested target, and the
	// storage release path will atomically mark this batch completed.
	batch.InflightCount--
	batch.FulfilledCount++
	batch.Status = domain.BatchStatusCompleted
	if batch.PurchasedCount != 5 || batch.FulfilledCount != 3 {
		t.Fatalf("最终计数 = purchased:%d fulfilled:%d，期望 5/3", batch.PurchasedCount, batch.FulfilledCount)
	}
	if batchReadyForPurchase(batch, now) {
		t.Fatal("三个成功任务完成后必须停止补购")
	}
}
