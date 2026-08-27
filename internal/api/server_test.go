package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type stopBatchStore struct {
	storage.Store
	result  domain.Batch
	err     error
	callIDs []int64
}

func (s *stopBatchStore) CancelBatch(_ context.Context, id int64) (domain.Batch, error) {
	s.callIDs = append(s.callIDs, id)
	return s.result, s.err
}

func TestStopBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		path       string
		storeErr   error
		wantStatus int
		wantCallID int64
	}{
		{
			name:       "成功停止批次",
			path:       "/api/batches/42/stop",
			wantStatus: http.StatusAccepted,
			wantCallID: 42,
		},
		{
			name:       "非法批次ID",
			path:       "/api/batches/not-a-number/stop",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "批次不存在",
			path:       "/api/batches/42/stop",
			storeErr:   storage.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCallID: 42,
		},
		{
			name:       "批次状态冲突",
			path:       "/api/batches/42/stop",
			storeErr:   storage.ErrConflict,
			wantStatus: http.StatusConflict,
			wantCallID: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stopBatchStore{
				result: domain.Batch{ID: 42, Status: domain.BatchStatusCancelled},
				err:    tt.storeErr,
			}
			manager := workflow.New(store, nil, nil, workflow.Config{}, nil)
			router := NewRouter(store, nil, manager, nil)
			request := httptest.NewRequest(http.MethodPost, tt.path, nil)
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d；响应体：%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.wantCallID == 0 {
				if len(store.callIDs) != 0 {
					t.Fatalf("CancelBatch 调用次数 = %d，期望 0", len(store.callIDs))
				}
				return
			}
			if len(store.callIDs) != 1 {
				t.Fatalf("CancelBatch 调用次数 = %d，期望 1", len(store.callIDs))
			}
			if store.callIDs[0] != tt.wantCallID {
				t.Fatalf("CancelBatch ID = %d，期望 %d", store.callIDs[0], tt.wantCallID)
			}
		})
	}
}

type createBatchConflictStore struct {
	storage.Store
	setting     domain.Setting
	createCalls int
}

func (s *createBatchConflictStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *createBatchConflictStore) CreateBatch(context.Context, storage.CreateBatchParams) (domain.Batch, error) {
	s.createCalls++
	return domain.Batch{}, storage.ErrActiveBatchExists
}

func TestCreateBatchReturnsConflictWhenAnotherBatchIsActive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	box, err := secure.New("create-batch-conflict-test")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCiphertext, err := box.Seal([]byte("fixture-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(apiKeyCiphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &createBatchConflictStore{setting: domain.Setting{
		Key: appsettings.SMSBowerKey, Value: settingValue,
	}}
	manager := workflow.New(store, appsettings.New(store, box, "http://sms.test"), box, workflow.Config{}, nil)
	router := NewRouter(store, appsettings.New(store, box, "http://sms.test"), manager, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 %d；响应体：%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if store.createCalls != 1 {
		t.Fatalf("CreateBatch 调用次数 = %d，期望 1", store.createCalls)
	}
}
