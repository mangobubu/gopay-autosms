package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type pinStateTransition struct {
	id           int64
	expected     []domain.ActivationStatus
	next         domain.ActivationStatus
	reason       string
	owner        string
	leaseVersion int64
}

type pinStateTouch struct {
	id        int64
	owner     string
	polledAt  time.Time
	nextRunAt time.Time
}

type pinStateStore struct {
	storage.Store
	calls []string

	fulfilledID           int64
	fulfilledOwner        string
	fulfilledLeaseVersion int64
	transition            pinStateTransition
	touch                 pinStateTouch
}

func (s *pinStateStore) MarkActivationFulfilledOwned(_ context.Context, id int64, owner string, leaseVersion int64) (bool, error) {
	s.calls = append(s.calls, "mark_fulfilled")
	s.fulfilledID = id
	s.fulfilledOwner = owner
	s.fulfilledLeaseVersion = leaseVersion
	return true, nil
}

func (s *pinStateStore) TouchActivationPoll(_ context.Context, id int64, owner string, polledAt, nextRunAt time.Time) error {
	s.calls = append(s.calls, "touch_poll")
	s.touch = pinStateTouch{id: id, owner: owner, polledAt: polledAt, nextRunAt: nextRunAt}
	return nil
}

func (s *pinStateStore) TransitionActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	reason string,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	s.calls = append(s.calls, "transition")
	s.transition = pinStateTransition{
		id:           id,
		expected:     append([]domain.ActivationStatus(nil), expected...),
		next:         next,
		reason:       reason,
		owner:        owner,
		leaseVersion: leaseVersion,
	}
	return domain.Activation{ID: id, Status: next}, nil
}

func TestFinalizePINSettingPublishesChangedStateAfterScheduling(t *testing.T) {
	store := &pinStateStore{}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}
	activation := domain.Activation{
		ID:           42,
		Status:       domain.ActivationStatusSettingPIN,
		LeaseOwner:   "worker-1",
		LeaseVersion: 7,
	}

	if err := manager.finalizePINSetting(context.Background(), activation); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mark_fulfilled", "touch_poll", "transition"}; !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("call order=%v, want %v", store.calls, want)
	}
	if store.fulfilledID != activation.ID || store.fulfilledOwner != activation.LeaseOwner || store.fulfilledLeaseVersion != activation.LeaseVersion {
		t.Fatalf("fulfilled args=(%d, %q, %d)", store.fulfilledID, store.fulfilledOwner, store.fulfilledLeaseVersion)
	}
	if store.touch.id != activation.ID || store.touch.owner != activation.LeaseOwner {
		t.Fatalf("touch args=%+v", store.touch)
	}
	if delay := store.touch.nextRunAt.Sub(store.touch.polledAt); delay < 4*time.Second {
		t.Fatalf("pin_changed display delay=%s, want at least 4s", delay)
	}
	wantTransition := pinStateTransition{
		id:           activation.ID,
		expected:     []domain.ActivationStatus{domain.ActivationStatusSettingPIN},
		next:         domain.ActivationStatusPINChanged,
		owner:        activation.LeaseOwner,
		leaseVersion: activation.LeaseVersion,
	}
	if !reflect.DeepEqual(store.transition, wantTransition) {
		t.Fatalf("transition=%+v, want %+v", store.transition, wantTransition)
	}
}

func TestPublishPINSettingStateSchedulesVisibleWindow(t *testing.T) {
	store := &pinStateStore{}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}
	activation := domain.Activation{
		ID:           41,
		Status:       domain.ActivationStatusAwaitingPINCode,
		LeaseOwner:   "worker-0",
		LeaseVersion: 6,
	}

	if err := manager.publishPINSettingState(context.Background(), activation); err != nil {
		t.Fatal(err)
	}
	if want := []string{"touch_poll", "transition"}; !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("call order=%v, want %v", store.calls, want)
	}
	if delay := store.touch.nextRunAt.Sub(store.touch.polledAt); delay < 4*time.Second {
		t.Fatalf("setting_pin display delay=%s, want at least 4s", delay)
	}
	wantTransition := pinStateTransition{
		id:           activation.ID,
		expected:     []domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
		next:         domain.ActivationStatusSettingPIN,
		owner:        activation.LeaseOwner,
		leaseVersion: activation.LeaseVersion,
	}
	if !reflect.DeepEqual(store.transition, wantTransition) {
		t.Fatalf("transition=%+v, want %+v", store.transition, wantTransition)
	}
}

func TestTransitionToSubsequentPolling(t *testing.T) {
	store := &pinStateStore{}
	manager := &Manager{store: store}
	activation := domain.Activation{
		ID:           43,
		Status:       domain.ActivationStatusPINChanged,
		LeaseOwner:   "worker-2",
		LeaseVersion: 8,
	}

	if err := manager.transitionToSubsequentPolling(context.Background(), activation); err != nil {
		t.Fatal(err)
	}
	if want := []string{"transition", "touch_poll"}; !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("call order=%v, want %v", store.calls, want)
	}
	wantTransition := pinStateTransition{
		id:           activation.ID,
		expected:     []domain.ActivationStatus{domain.ActivationStatusPINChanged},
		next:         domain.ActivationStatusAwaitingSubsequentCode,
		owner:        activation.LeaseOwner,
		leaseVersion: activation.LeaseVersion,
	}
	if !reflect.DeepEqual(store.transition, wantTransition) {
		t.Fatalf("transition=%+v, want %+v", store.transition, wantTransition)
	}
	if store.touch.id != activation.ID || store.touch.owner != activation.LeaseOwner {
		t.Fatalf("touch args=%+v", store.touch)
	}
	if !store.touch.nextRunAt.Equal(store.touch.polledAt) {
		t.Fatalf("next poll=%s, want immediate %s", store.touch.nextRunAt, store.touch.polledAt)
	}
}

func TestPINStatusDisplayDuration(t *testing.T) {
	for _, test := range []struct {
		name         string
		pollInterval time.Duration
		want         time.Duration
	}{
		{name: "two_seconds_uses_minimum", pollInterval: 2 * time.Second, want: 4 * time.Second},
		{name: "larger_interval_is_not_shortened", pollInterval: 9 * time.Second, want: 9 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pinStatusDisplayDuration(test.pollInterval); got != test.want {
				t.Fatalf("pinStatusDisplayDuration(%s)=%s, want %s", test.pollInterval, got, test.want)
			}
		})
	}
}

func TestProviderActionConcluded(t *testing.T) {
	if !providerActionConcluded(nil) {
		t.Fatal("nil provider action error should be concluded")
	}
	for _, code := range []string{"NO_ACTIVATION", "BAD_STATUS"} {
		err := &smsbower.APIError{Action: "setStatus", Code: code}
		if !providerActionConcluded(err) {
			t.Fatalf("%s should be treated as a concluded provider action", code)
		}
	}
	if providerActionConcluded(errors.New("temporary network failure")) {
		t.Fatal("transient provider failure must remain retryable")
	}
}
