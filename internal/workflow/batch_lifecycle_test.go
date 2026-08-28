package workflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type batchLifecycleStore struct {
	storage.Store

	batches      []domain.Batch
	listErr      error
	cancelErr    map[int64]error
	recoverErr   map[int64]error
	listFilters  []storage.BatchFilter
	recoveredIDs []int64
	cancelledIDs []int64
}

type retryCancelStore struct {
	*batchLifecycleStore
	errors []error
	calls  int
}

func (s *retryCancelStore) CancelBatch(_ context.Context, id int64) (domain.Batch, error) {
	s.calls++
	if len(s.errors) > 0 {
		err := s.errors[0]
		s.errors = s.errors[1:]
		if err != nil {
			return domain.Batch{}, err
		}
	}
	return domain.Batch{ID: id, Status: domain.BatchStatusCancelled}, nil
}

func (s *batchLifecycleStore) RecoverBatchPurchaseOnStartup(_ context.Context, id int64) error {
	s.recoveredIDs = append(s.recoveredIDs, id)
	return s.recoverErr[id]
}

func (s *batchLifecycleStore) ListBatches(_ context.Context, filter storage.BatchFilter) ([]domain.Batch, error) {
	s.listFilters = append(s.listFilters, filter)
	if s.listErr != nil {
		return nil, s.listErr
	}
	if filter.Page.Offset >= len(s.batches) {
		return nil, nil
	}
	end := filter.Page.Offset + filter.Page.Limit
	if end > len(s.batches) {
		end = len(s.batches)
	}
	return append([]domain.Batch(nil), s.batches[filter.Page.Offset:end]...), nil
}

func (s *batchLifecycleStore) CancelBatch(_ context.Context, id int64) (domain.Batch, error) {
	s.cancelledIDs = append(s.cancelledIDs, id)
	if err := s.cancelErr[id]; err != nil {
		return domain.Batch{}, err
	}
	return domain.Batch{ID: id, Status: domain.BatchStatusCancelled}, nil
}

func newBatchLifecycleManager(store storage.Store) *Manager {
	return New(
		store,
		nil,
		nil,
		Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestRecoverStartupBatchesPreservesAndRecoversEveryUnfinishedBatchOnce(t *testing.T) {
	store := &batchLifecycleStore{batches: []domain.Batch{
		{ID: 11, Status: domain.BatchStatusPending},
		{ID: 12, Status: domain.BatchStatusRunning},
		{ID: 13, Status: domain.BatchStatusCompleted},
		{ID: 14, Status: domain.BatchStatusCancelled},
		// A duplicated row must not result in a duplicated recovery request.
		{ID: 12, Status: domain.BatchStatusRunning},
	}}
	manager := newBatchLifecycleManager(store)

	if err := manager.recoverStartupBatches(context.Background()); err != nil {
		t.Fatalf("recoverStartupBatches() 返回错误：%v", err)
	}
	if len(store.cancelledIDs) != 0 {
		t.Fatalf("启动恢复不应取消批次，实际取消 = %v", store.cancelledIDs)
	}
	if want := []int64{11, 12}; !reflect.DeepEqual(store.recoveredIDs, want) {
		t.Fatalf("恢复购买状态的批次 = %v，期望 %v", store.recoveredIDs, want)
	}
	if len(store.listFilters) != 1 {
		t.Fatalf("ListBatches 调用次数 = %d，期望 1", len(store.listFilters))
	}
	filter := store.listFilters[0]
	if want := []domain.BatchStatus{domain.BatchStatusPending, domain.BatchStatusRunning}; !reflect.DeepEqual(filter.Statuses, want) {
		t.Fatalf("启动清理状态过滤 = %v，期望 %v", filter.Statuses, want)
	}
	if filter.Page.Limit != startupBatchPageSize || filter.Page.Offset != 0 {
		t.Fatalf("启动清理分页 = %+v，期望 limit=%d offset=0", filter.Page, startupBatchPageSize)
	}
}

func TestRunDoesNotStartSchedulerWhenStartupCleanupFails(t *testing.T) {
	listFailure := errors.New("list startup batches failed")
	recoverFailureOne := errors.New("recover startup purchase one failed")
	recoverFailureTwo := errors.New("recover startup purchase two failed")

	for _, test := range []struct {
		name          string
		store         *batchLifecycleStore
		wantErrors    []error
		wantRecovered []int64
	}{
		{
			name:          "读取遗留批次失败",
			store:         &batchLifecycleStore{listErr: listFailure},
			wantErrors:    []error{listFailure},
			wantRecovered: nil,
		},
		{
			name: "汇总每个批次的遗留购号恢复错误",
			store: &batchLifecycleStore{
				batches: []domain.Batch{
					{ID: 20, Status: domain.BatchStatusRunning},
					{ID: 21, Status: domain.BatchStatusPending},
				},
				recoverErr: map[int64]error{20: recoverFailureOne, 21: recoverFailureTwo},
			},
			wantErrors:    []error{recoverFailureOne, recoverFailureTwo},
			wantRecovered: []int64{20, 21},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newBatchLifecycleManager(test.store)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := manager.Run(ctx)
			for _, wantErr := range test.wantErrors {
				if !errors.Is(err, wantErr) {
					t.Fatalf("Run() 错误 = %v，期望包含 %v", err, wantErr)
				}
			}
			if len(test.store.cancelledIDs) != 0 {
				t.Fatalf("启动恢复不应取消批次，实际取消 = %v", test.store.cancelledIDs)
			}
			if !reflect.DeepEqual(test.store.recoveredIDs, test.wantRecovered) {
				t.Fatalf("恢复购买状态的批次 = %v，期望 %v", test.store.recoveredIDs, test.wantRecovered)
			}

			waitDone := make(chan struct{})
			go func() {
				manager.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
				// 启动清理失败时没有调度协程需要等待。
			case <-time.After(200 * time.Millisecond):
				t.Fatal("启动清理失败后调度器仍在运行")
			}
		})
	}
}

func TestStopBatchKeepsWorkersRunningWhenPersistenceFails(t *testing.T) {
	cancelFailure := errors.New("persist batch cancellation failed")
	store := &batchLifecycleStore{cancelErr: map[int64]error{41: cancelFailure}}
	manager := newBatchLifecycleManager(store)
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	manager.workers[41] = map[int64]activationWorker{
		201: {leaseVersion: 1, cancel: cancelWorker},
	}

	_, err := manager.StopBatch(context.Background(), 41)
	if !errors.Is(err, cancelFailure) {
		t.Fatalf("StopBatch() 错误 = %v，期望包含 %v", err, cancelFailure)
	}
	select {
	case <-workerCtx.Done():
		t.Fatal("持久化取消失败时不应中断仍由旧状态保护的 worker")
	default:
	}
	if _, exists := manager.workers[41]; !exists {
		t.Fatal("持久化取消失败时不应移除 worker 注册")
	}
}

func TestStopBatchResolvesUnknownCancellationCommitBeforeStoppingWorkers(t *testing.T) {
	store := &retryCancelStore{
		batchLifecycleStore: &batchLifecycleStore{},
		errors:              []error{storage.ErrCommitUnknown, nil},
	}
	manager := newBatchLifecycleManager(store)
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	manager.workers[42] = map[int64]activationWorker{
		202: {leaseVersion: 1, cancel: cancelWorker},
	}

	batch, err := manager.StopBatch(context.Background(), 42)
	if err != nil {
		t.Fatalf("StopBatch() 返回错误：%v", err)
	}
	if store.calls != 2 || batch.Status != domain.BatchStatusCancelled {
		t.Fatalf("停止提交复核 calls=%d batch=%+v，期望重试后取消", store.calls, batch)
	}
	select {
	case <-workerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("确认持久化停止后本机 worker 未被中断")
	}
}

func TestStopBatchPersistsCancellationAndInterruptsOnlyItsWorkers(t *testing.T) {
	store := &batchLifecycleStore{}
	manager := newBatchLifecycleManager(store)
	batchWorkerCtx, cancelBatchWorker := context.WithCancel(context.Background())
	otherWorkerCtx, cancelOtherWorker := context.WithCancel(context.Background())
	defer cancelOtherWorker()
	manager.workers[31] = map[int64]activationWorker{
		101: {leaseVersion: 1, cancel: cancelBatchWorker},
	}
	manager.workers[32] = map[int64]activationWorker{
		102: {leaseVersion: 1, cancel: cancelOtherWorker},
	}

	batch, err := manager.StopBatch(context.Background(), 31)
	if err != nil {
		t.Fatalf("StopBatch() 返回错误：%v", err)
	}
	if batch.ID != 31 || batch.Status != domain.BatchStatusCancelled {
		t.Fatalf("StopBatch() = %+v，期望批次 31 已取消", batch)
	}
	if want := []int64{31}; !reflect.DeepEqual(store.cancelledIDs, want) {
		t.Fatalf("持久化取消批次 = %v，期望 %v", store.cancelledIDs, want)
	}
	select {
	case <-batchWorkerCtx.Done():
	default:
		t.Fatal("被停止批次的处理 worker 未被中断")
	}
	select {
	case <-otherWorkerCtx.Done():
		t.Fatal("停止批次时误中断了其他批次的 worker")
	default:
	}
	if _, exists := manager.workers[31]; exists {
		t.Fatal("停止后仍保留该批次的 worker 注册")
	}
	if _, exists := manager.workers[32]; !exists {
		t.Fatal("停止后丢失了其他批次的 worker 注册")
	}
}
