package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestTopUpQuotaValidation(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	oldDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	common.QuotaPerUnit = 500000
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQuotaPerUnit
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplayType
	})

	testCases := []struct {
		name        string
		displayType string
		amount      int64
		wantQuota   int
		wantErr     bool
	}{
		{
			name:        "currency amount below limit",
			displayType: operation_setting.QuotaDisplayTypeUSD,
			amount:      4294,
			wantQuota:   2_147_000_000,
		},
		{
			name:        "currency amount above limit",
			displayType: operation_setting.QuotaDisplayTypeUSD,
			amount:      4295,
			wantQuota:   2_147_500_000,
		},
		{
			name:        "token amount preserves settlement truncation",
			displayType: operation_setting.QuotaDisplayTypeTokens,
			amount:      2_147_500_000,
			wantQuota:   2_147_500_000,
		},
		{
			name:        "token amount above legacy int32 range",
			displayType: operation_setting.QuotaDisplayTypeTokens,
			amount:      4_294_500_000,
			wantQuota:   4_294_500_000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().QuotaDisplayType = tc.displayType
			quota, err := getTopUpQuota(tc.amount)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, quota)
		})
	}
}

func TestValidateTopUpQuotaReturnsMaximumAmount(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	oldDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	common.QuotaPerUnit = 500000
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQuotaPerUnit
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplayType
	})

	maxAmount := decimal.NewFromInt(common.MaxWalletQuota).
		Div(decimal.NewFromFloat(common.QuotaPerUnit)).
		Floor().IntPart()

	_, err := validateTopUpQuota(maxAmount)
	require.NoError(t, err)
	_, err = validateTopUpQuota(maxAmount + 1)
	require.EqualError(t, err, fmt.Sprintf("Top-up amount cannot exceed %d", maxAmount))
}

func TestRequestAmountRejectsTopUpThatCannotBeSettled(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	oldDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	common.QuotaPerUnit = 500000
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQuotaPerUnit
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplayType
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	maxAmount := decimal.NewFromInt(common.MaxWalletQuota).
		Div(decimal.NewFromFloat(common.QuotaPerUnit)).
		Floor().IntPart()
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/user/amount",
		strings.NewReader(fmt.Sprintf(`{"amount":%d}`, maxAmount+1)),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	RequestAmount(ctx)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, fmt.Sprintf(`{"message":"error","data":"Top-up amount cannot exceed %d","message_key":"Top-up amount cannot exceed {{max}}","message_params":{"max":%d}}`, maxAmount, maxAmount), recorder.Body.String())
}

func TestRequestAmountRejectsTopUpThatWouldOverflowWallet(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	oldDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	oldDB := model.DB
	common.QuotaPerUnit = 500000
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	model.DB = db
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQuotaPerUnit
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplayType
		model.DB = oldDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	require.NoError(t, model.DB.Create(&model.User{
		Id:       42,
		Username: "topup_capacity_user",
		Quota:    common.MaxWalletQuota - 100_000,
		Status:   common.UserStatusEnabled,
	}).Error)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 42)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/user/amount",
		strings.NewReader(`{"amount":1}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	RequestAmount(ctx)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"message":"error","data":"Top-up quota limit exceeded","message_key":"Top-up quota limit exceeded"}`, recorder.Body.String())
}

func TestValidateCreditedQuotaRejectsOverflow(t *testing.T) {
	_, err := validateCreditedQuota(decimal.NewFromInt(int64(common.MaxWalletQuota / 2)))
	require.NoError(t, err)
	_, err = validateCreditedQuota(decimal.Zero)
	require.EqualError(t, err, "Top-up quota must be greater than 0")
	_, err = validateCreditedQuota(decimal.NewFromInt(common.MaxWalletQuota + 1))
	require.EqualError(
		t,
		err,
		"Top-up quota exceeds the range the system can represent",
	)
}

func TestStripeCreditedQuotaIncludesGroupRatio(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	oldTopupGroupRatio := common.TopupGroupRatio2JSONString()
	common.QuotaPerUnit = 500000
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"vip":2}`))
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQuotaPerUnit
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(oldTopupGroupRatio))
	})

	_, err := validateCreditedQuota(getStripeCreditedQuota(2147, "vip"))
	require.NoError(t, err)
	_, err = validateCreditedQuota(getStripeCreditedQuota(2148, "vip"))
	require.NoError(t, err)
	_, err = validateCreditedQuota(getStripeCreditedQuota(int64(common.MaxWalletQuota), "vip"))
	require.Error(t, err)

	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"free":0}`))
	assert.True(t, decimal.NewFromInt(500000).Equal(getStripeCreditedQuota(1, "free")))
}

func TestWechatCNYQuoteAndHistory(t *testing.T) {
	oldRedis, oldRedisEnabled := common.RDB, common.RedisEnabled
	redisServer := miniredis.RunT(t)
	common.RDB = redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RedisEnabled = true
	t.Cleanup(func() { _ = common.RDB.Close(); common.RDB, common.RedisEnabled = oldRedis, oldRedisEnabled })
	oldQuota, oldPrice, oldMin, oldRate := common.QuotaPerUnit, operation_setting.Price, operation_setting.MinTopUp, operation_setting.USDExchangeRate
	oldDisplay := operation_setting.GetGeneralSetting().QuotaDisplayType
	oldDiscount := operation_setting.GetPaymentSetting().AmountDiscount
	oldRatios := common.TopupGroupRatio2JSONString()
	t.Cleanup(func() {
		common.QuotaPerUnit, operation_setting.Price, operation_setting.MinTopUp, operation_setting.USDExchangeRate = oldQuota, oldPrice, oldMin, oldRate
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplay
		operation_setting.GetPaymentSetting().AmountDiscount = oldDiscount
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(oldRatios))
	})
	common.QuotaPerUnit, operation_setting.Price, operation_setting.MinTopUp, operation_setting.USDExchangeRate = 500000, 1, 1, 1
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeCNY
	operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{100: 0.9}
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"default":1,"internal":0.001}`))
	for _, tc := range []struct {
		face, cash int64
		group      string
	}{{1, 100, "default"}, {10, 1000, "default"}, {73, 7300, "default"}, {100, 9000, "default"}, {101, 10100, "default"}, {100, 9, "internal"}, {1000, 100, "internal"}, {5, 1, "internal"}} {
		cents, quota, err := quoteWechatRequest(AmountRequest{Amount: tc.face}, tc.group)
		require.NoError(t, err)
		assert.Equal(t, tc.cash, cents)
		assert.Equal(t, int(tc.face)*500000, quota, "discounts change cash, not credit")
	}
	for _, amount := range []int64{-1, 0, 9223372036854775807} {
		_, _, err := quoteWechatRequest(AmountRequest{Amount: amount}, "default")
		require.Error(t, err)
	}
	for _, body := range []string{`{"amount_cents":100}`, `{"amount":1.5}`, `{"amount":"1"}`, `{"amount":null}`, `{}`} {
		var request AmountRequest
		err := common.UnmarshalJsonStr(body, &request)
		if err == nil {
			_, _, err = quoteWechatRequest(request, "default")
		}
		require.Error(t, err, body)
	}
	_, _, err := quoteWechatRequest(AmountRequest{Amount: 1}, "internal")
	require.Error(t, err, "discounted cash rounding to zero must be rejected")
	operation_setting.MinTopUp = 10
	_, _, err = quoteWechatRequest(AmountRequest{Amount: 1}, "default")
	require.Error(t, err, "shared minimum must apply")
	operation_setting.MinTopUp = 1
	for _, display := range []string{operation_setting.QuotaDisplayTypeUSD, operation_setting.QuotaDisplayTypeTokens, operation_setting.QuotaDisplayTypeCustom} {
		operation_setting.GetGeneralSetting().QuotaDisplayType = display
		_, _, err := quoteWechatRequest(AmountRequest{Amount: 1}, "default")
		require.ErrorContains(t, err, "WeChat Pay requires CNY display")
	}
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeCNY
	for _, value := range []float64{0, 7.3} {
		operation_setting.Price = value
		_, _, err := quoteWechatRequest(AmountRequest{Amount: 1}, "default")
		require.ErrorContains(t, err, "WeChat Pay requires CNY display")
		operation_setting.Price = 1
		operation_setting.USDExchangeRate = value
		_, _, err = quoteWechatRequest(AmountRequest{Amount: 1}, "default")
		require.ErrorContains(t, err, "WeChat Pay requires CNY display")
		operation_setting.USDExchangeRate = 1
	}
	for _, discount := range []float64{0, -1, 1.01} {
		operation_setting.GetPaymentSetting().AmountDiscount[100] = discount
		_, _, err := quoteWechatRequest(AmountRequest{Amount: 100}, "default")
		require.Error(t, err)
	}
	operation_setting.GetPaymentSetting().AmountDiscount[100] = 0.9
	cash, quota, err := quoteWechatRequest(AmountRequest{Amount: 1}, "default")
	require.NoError(t, err)
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			require.NoError(t, common.RDB.FlushAll(context.Background()).Err())
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("WECHAT_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("WECHAT_TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("WECHAT_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("WECHAT_TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			previousDB, previousLogDB := model.DB, model.LOG_DB
			model.DB, model.LOG_DB = db, db
			t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.WechatTopUp{}, &model.Log{}))
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			user := model.User{Username: "cny" + suffix, Quota: 0, AuthVersion: 1, Group: "default", AffCode: suffix}
			require.NoError(t, db.Create(&user).Error)
			_, err = model.GetUserCache(user.Id)
			require.NoError(t, err)
			topUp := &model.TopUp{TradeNo: "WX" + suffix, UserId: user.Id, PaymentProvider: "wechat", PaymentMethod: "wechat", Amount: 0, Money: float64(cash) / 100, Status: common.TopUpStatusPending}
			order := &model.WechatTopUp{TradeNo: topUp.TradeNo, UserID: user.Id, AmountCents: cash, CreditedQuota: quota, Currency: "CNY", AppID: "app", MchID: "mch", ExpiresAt: time.Now().Unix() + 1800}
			require.NoError(t, model.CreateWechatTopUp(topUp, order))
			stored, storedTopUp, err := model.GetWechatTopUp(order.TradeNo, user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(100), stored.AmountCents)
			assert.Equal(t, 500000, stored.CreditedQuota)
			legacyOrder := &model.WechatTopUp{TradeNo: "WXL" + suffix, UserID: user.Id, AmountCents: 730, CreditedQuota: 500000, Currency: "CNY", AppID: "app", MchID: "mch"}
			legacyTopUp := &model.TopUp{TradeNo: legacyOrder.TradeNo, UserId: user.Id, PaymentProvider: "wechat", PaymentMethod: "wechat", Amount: 1, Money: 7.3, Status: common.TopUpStatusPending}
			require.NoError(t, model.CreateWechatTopUp(legacyTopUp, legacyOrder))
			frozenLegacy, _, err := model.GetWechatTopUp(legacyOrder.TradeNo, user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(730), frozenLegacy.AmountCents)
			assert.Equal(t, 500000, frozenLegacy.CreditedQuota)
			records, err := model.WithWechatTopUpCredits([]*model.TopUp{storedTopUp, {TradeNo: "legacy", Amount: 1, PaymentProvider: "epay"}})
			require.NoError(t, err)
			require.NotNil(t, records[0].CreditedQuota)
			assert.Equal(t, quota, *records[0].CreditedQuota)
			assert.Nil(t, records[1].CreditedQuota)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			wechatOrderResponse(ctx, stored, storedTopUp)
			assert.Contains(t, recorder.Body.String(), `"credited_cny":"1.00"`)
			assert.Contains(t, recorder.Body.String(), `"credited_quota":500000`)
			// Changes to current pricing cannot change an existing order's cash or credit.
			operation_setting.Price = 73
			operation_setting.USDExchangeRate = 10
			payment := model.WechatSettlement{TradeNo: order.TradeNo, TransactionID: "wx" + suffix, AmountCents: 100, Currency: "CNY", AppID: "app", MchID: "mch"}
			require.NoError(t, model.CompleteWechatTopUp(payment, ""))
			require.NoError(t, model.CompleteWechatTopUp(payment, ""))
			var credited model.User
			require.NoError(t, db.First(&credited, user.Id).Error)
			assert.Equal(t, quota, credited.Quota)
			cached, err := model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, quota, cached.Quota)
			legacyPayment := model.WechatSettlement{TradeNo: legacyOrder.TradeNo, TransactionID: "wxl" + suffix, AmountCents: 730, Currency: "CNY", AppID: "app", MchID: "mch"}
			require.NoError(t, model.CompleteWechatTopUp(legacyPayment, ""))
			require.NoError(t, model.CompleteWechatTopUp(legacyPayment, ""))
			require.NoError(t, db.First(&credited, user.Id).Error)
			assert.Equal(t, quota+500000, credited.Quota, "old 730-cent orders retain their frozen quota")
			operation_setting.Price, operation_setting.USDExchangeRate = 1, 1
		})
	}
}
