package workflow

import (
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

func TestBatchReadyForPurchaseUsesPurchasedCount(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		batch domain.Batch
		want  bool
	}{
		{
			name: "已购买数量不足时继续购买",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 2,
				NextPurchaseAt: now.Add(-time.Second),
			},
			want: true,
		},
		{
			name: "失败号码释放处理中槽位后仍不补购",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 3,
				FulfilledCount: 0, InflightCount: 0, NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "跨实例已预占购买名额时等待",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 1,
				PurchaseReservedCount: 1, NextPurchaseAt: now.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "旧批次购买数超过上限时停止购买",
			batch: domain.Batch{
				Status: domain.BatchStatusRunning, Quantity: 3, PurchasedCount: 5,
				NextPurchaseAt: now.Add(-time.Second),
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
				NextPurchaseAt: now.Add(time.Second),
			},
			want: false,
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
