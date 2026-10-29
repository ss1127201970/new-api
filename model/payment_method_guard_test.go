package model

import (
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertUserForPaymentGuardTest(t *testing.T, id int, quota int) *User {
	t.Helper()
	user := &User{
		Id:       id,
		Username: "payment_guard_user",
		Status:   common.UserStatusEnabled,
		Quota:    quota,
	}
	require.NoError(t, DB.Create(user).Error)
	return user
}

func insertSubscriptionPlanForPaymentGuardTest(t *testing.T, id int) *SubscriptionPlan {
	t.Helper()
	plan := &SubscriptionPlan{
		Id:            id,
		Title:         "Guard Plan",
		PriceAmount:   9.99,
		Currency:      "USD",
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		Enabled:       true,
		TotalAmount:   1000,
	}
	require.NoError(t, DB.Create(plan).Error)
	return plan
}

func insertSubscriptionOrderForPaymentGuardTest(t *testing.T, tradeNo string, userID int, planID int, paymentProvider string) {
	t.Helper()
	order := &SubscriptionOrder{
		UserId:          userID,
		PlanId:          planID,
		Money:           9.99,
		TradeNo:         tradeNo,
		PaymentMethod:   paymentProvider,
		PaymentProvider: paymentProvider,
		Status:          common.TopUpStatusPending,
		CreateTime:      time.Now().Unix(),
	}
	require.NoError(t, order.Insert())
}

func insertTopUpForPaymentGuardTest(t *testing.T, tradeNo string, userID int, paymentProvider string) {
	t.Helper()
	topUp := &TopUp{
		UserId:          userID,
		Amount:          2,
		Money:           9.99,
		TradeNo:         tradeNo,
		PaymentMethod:   paymentProvider,
		PaymentProvider: paymentProvider,
		Status:          common.TopUpStatusPending,
		CreateTime:      time.Now().Unix(),
	}
	require.NoError(t, topUp.Insert())
}

func getTopUpStatusForPaymentGuardTest(t *testing.T, tradeNo string) string {
	t.Helper()
	topUp := GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, topUp)
	return topUp.Status
}

func countUserSubscriptionsForPaymentGuardTest(t *testing.T, userID int) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", userID).Count(&count).Error)
	return count
}

func getUserQuotaForPaymentGuardTest(t *testing.T, userID int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").Where("id = ?", userID).First(&user).Error)
	return user.Quota
}

func TestRechargeWaffoPancake_RejectsMismatchedPaymentMethod(t *testing.T) {
	truncateTables(t)

	insertUserForPaymentGuardTest(t, 101, 0)
	insertTopUpForPaymentGuardTest(t, "waffo-pancake-guard", 101, PaymentProviderStripe)

	err := RechargeWaffoPancake("waffo-pancake-guard")
	require.Error(t, err)

	topUp := GetTopUpByTradeNo("waffo-pancake-guard")
	require.NotNil(t, topUp)
	assert.Equal(t, common.TopUpStatusPending, topUp.Status)
	assert.Equal(t, 0, getUserQuotaForPaymentGuardTest(t, 101))
}

func TestUpdatePendingTopUpStatus_RejectsMismatchedPaymentProvider(t *testing.T) {
	testCases := []struct {
		name                    string
		tradeNo                 string
		storedPaymentProvider   string
		expectedPaymentProvider string
		targetStatus            string
	}{
		{
			name:                    "stripe expire",
			tradeNo:                 "stripe-expire-guard",
			storedPaymentProvider:   PaymentProviderCreem,
			expectedPaymentProvider: PaymentProviderStripe,
			targetStatus:            common.TopUpStatusExpired,
		},
		{
			name:                    "waffo failed",
			tradeNo:                 "waffo-failed-guard",
			storedPaymentProvider:   PaymentProviderStripe,
			expectedPaymentProvider: PaymentProviderWaffo,
			targetStatus:            common.TopUpStatusFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			insertUserForPaymentGuardTest(t, 150, 0)
			insertTopUpForPaymentGuardTest(t, tc.tradeNo, 150, tc.storedPaymentProvider)

			err := UpdatePendingTopUpStatus(tc.tradeNo, tc.expectedPaymentProvider, tc.targetStatus)
			require.ErrorIs(t, err, ErrPaymentMethodMismatch)
			assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, tc.tradeNo))
		})
	}
}

func TestCompleteSubscriptionOrder_RejectsMismatchedPaymentProvider(t *testing.T) {
	truncateTables(t)

	insertUserForPaymentGuardTest(t, 202, 0)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 301)
	insertSubscriptionOrderForPaymentGuardTest(t, "sub-guard-order", 202, plan.Id, PaymentProviderStripe)

	err := CompleteSubscriptionOrder("sub-guard-order", `{"provider":"epay"}`, PaymentProviderEpay, "alipay")
	require.ErrorIs(t, err, ErrPaymentMethodMismatch)

	order := GetSubscriptionOrderByTradeNo("sub-guard-order")
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
	assert.Zero(t, countUserSubscriptionsForPaymentGuardTest(t, 202))

	topUp := GetTopUpByTradeNo("sub-guard-order")
	assert.Nil(t, topUp)
}

func TestExpireSubscriptionOrder_RejectsMismatchedPaymentProvider(t *testing.T) {
	truncateTables(t)

	insertUserForPaymentGuardTest(t, 303, 0)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 401)
	insertSubscriptionOrderForPaymentGuardTest(t, "sub-expire-guard", 303, plan.Id, PaymentProviderStripe)

	err := ExpireSubscriptionOrder("sub-expire-guard", PaymentProviderCreem)
	require.ErrorIs(t, err, ErrPaymentMethodMismatch)

	order := GetSubscriptionOrderByTradeNo("sub-expire-guard")
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
}

func createEpayTestOrder(t *testing.T, userId int, tradeNo string, provider string, status string) TopUp {
	t.Helper()
	topUp := TopUp{
		UserId:          userId,
		Amount:          2,
		Money:           10.0,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: provider,
		CreateTime:      common.GetTimestamp(),
		Status:          status,
	}
	require.NoError(t, DB.Create(&topUp).Error)
	return topUp
}

func TestRechargeEpayCreditsQuotaExactlyOnce(t *testing.T) {
	truncateTables(t)

	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	user := insertUserForPaymentGuardTest(t, 501, 0)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTONCE", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)
	assert.Equal(t, 2*500000, getUserQuotaForPaymentGuardTest(t, user.Id))

	reloaded := GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, common.TopUpStatusSuccess, reloaded.Status)
	assert.NotZero(t, reloaded.CompleteTime)

	alreadyDone, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, alreadyDone)
	assert.Equal(t, 2*500000, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestRechargeEpayKeepsRedisAndDatabaseCreditInSync(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)

	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 5
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	user := insertUserForPaymentGuardTest(t, 502, 7)
	require.NoError(t, populateUserCache(*user))
	order := createEpayTestOrder(t, user.Id, "EPAYTESTREDISSYNC", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)
	assert.Equal(t, 17, getUserQuotaForPaymentGuardTest(t, user.Id))
	cached, err := cacheGetUserBase(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 17, cached.Quota)

	alreadyDone, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, alreadyDone)
	cached, err = cacheGetUserBase(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 17, cached.Quota)
}

func TestRechargeEpayUpdatesPaymentMethodToActual(t *testing.T) {
	truncateTables(t)

	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	user := insertUserForPaymentGuardTest(t, 503, 0)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTMETHOD", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "wxpay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)

	reloaded := GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, "wxpay", reloaded.PaymentMethod)
	assert.Equal(t, 2*500000, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestRechargeEpayRejectsForeignAndNonPendingOrders(t *testing.T) {
	truncateTables(t)

	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	user := insertUserForPaymentGuardTest(t, 504, 7)

	t.Run("order from another payment provider", func(t *testing.T) {
		order := createEpayTestOrder(t, user.Id, "EPAYTESTSTRIPE", PaymentProviderStripe, common.TopUpStatusPending)
		_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrPaymentMethodMismatch)
		assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
	})

	t.Run("order that is not pending", func(t *testing.T) {
		order := createEpayTestOrder(t, user.Id, "EPAYTESTEXPIRED", PaymentProviderEpay, common.TopUpStatusExpired)
		_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrTopUpStatusInvalid)
		assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
	})

	t.Run("missing order", func(t *testing.T) {
		_, err := RechargeEpay("EPAYTESTMISSING", "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrTopUpNotFound)
	})
}

func TestRechargeEpayRejectsQuotaOverflowBeforeCompletingOrder(t *testing.T) {
	truncateTables(t)

	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = float64(common.MaxWalletQuota + 1)
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	user := insertUserForPaymentGuardTest(t, 505, 3)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTOVERFLOW", PaymentProviderEpay, common.TopUpStatusPending)

	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.Error(t, err)
	assert.Equal(t, 3, getUserQuotaForPaymentGuardTest(t, user.Id))
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
}

func TestRechargeEpayEnforcesFinalWalletQuotaLimit(t *testing.T) {
	oldQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldQuotaPerUnit })

	testCases := []struct {
		name         string
		currentQuota int
		wantErr      bool
		wantQuota    int
		wantStatus   string
	}{
		{
			name:         "allows exact highest representable wallet balance",
			currentQuota: common.MaxWalletQuota - 1_000_000,
			wantQuota:    common.MaxWalletQuota,
			wantStatus:   common.TopUpStatusSuccess,
		},
		{
			name:         "rejects balance above wallet quota domain",
			currentQuota: common.MaxWalletQuota - 999_999,
			wantErr:      true,
			wantQuota:    common.MaxWalletQuota - 999_999,
			wantStatus:   common.TopUpStatusPending,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			user := insertUserForPaymentGuardTest(t, 506, tc.currentQuota)
			order := createEpayTestOrder(t, user.Id, "EPAYTESTWALLETLIMIT", PaymentProviderEpay, common.TopUpStatusPending)

			_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
			if tc.wantErr {
				require.ErrorIs(t, err, ErrTopUpQuotaLimitExceeded)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantQuota, getUserQuotaForPaymentGuardTest(t, user.Id))
			assert.Equal(t, tc.wantStatus, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
		})
	}
}

// Optional external DSNs MUST name disposable, empty test databases.
func TestWechatSettlementAndMigration(t *testing.T) {
	tests := []struct {
		name, dsn string
		dialector gorm.Dialector
		dbType    common.DatabaseType
	}{
		{name: "sqlite", dialector: sqlite.Open(":memory:"), dbType: common.DatabaseTypeSQLite},
	}
	if dsn := os.Getenv("WECHAT_TEST_MYSQL_DSN"); dsn != "" {
		tests = append(tests, struct {
			name, dsn string
			dialector gorm.Dialector
			dbType    common.DatabaseType
		}{name: "mysql", dsn: dsn, dialector: mysql.Open(dsn), dbType: common.DatabaseTypeMySQL})
	}
	if dsn := os.Getenv("WECHAT_TEST_POSTGRES_DSN"); dsn != "" {
		tests = append(tests, struct {
			name, dsn string
			dialector gorm.Dialector
			dbType    common.DatabaseType
		}{name: "postgres", dsn: dsn, dialector: postgres.Open(dsn), dbType: common.DatabaseTypePostgreSQL})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previousDB, previousLogDB := DB, LOG_DB
			db, err := gorm.Open(tc.dialector, &gorm.Config{})
			require.NoError(t, err)
			DB, LOG_DB = db, db
			var engineVersion string
			if tc.name == "sqlite" {
				require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&engineVersion).Error)
			} else {
				require.NoError(t, db.Raw("SELECT VERSION()").Scan(&engineVersion).Error)
			}
			t.Logf("database version: %s", engineVersion)
			common.SetDatabaseTypes(tc.dbType, tc.dbType)
			initCol()
			connection, err := db.DB()
			require.NoError(t, err)
			if tc.name == "sqlite" {
				connection.SetMaxOpenConns(1)
			}
			t.Cleanup(func() {
				_ = connection.Close()
				DB, LOG_DB = previousDB, previousLogDB
				common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
				initCol()
			})
			// External DSNs explicitly designate disposable test databases.
			require.NoError(t, db.Migrator().DropTable(&WechatTopUp{}, &TopUp{}, &Log{}, &User{}))
			// Simulate rc47 schema and data first, then migrate the payment extension twice.
			require.NoError(t, db.AutoMigrate(&User{}, &TopUp{}, &Log{}))
			user := insertUserForPaymentGuardTest(t, 99001, 17)
			legacy := createEpayTestOrder(t, user.Id, "WXLEGACY", PaymentProviderEpay, common.TopUpStatusPending)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&WechatTopUp{}, &TopUp{}))
			}
			require.Equal(t, legacy.TradeNo, GetTopUpByTradeNo(legacy.TradeNo).TradeNo)
			makeOrder := func(tradeNo string) *WechatTopUp {
				order := &WechatTopUp{TradeNo: tradeNo, UserID: user.Id, AmountCents: 1234, CreditedQuota: 650001, Currency: "CNY", AppID: "wx-app", MchID: "merchant", ExpiresAt: time.Now().Unix() + 1800}
				topUp := &TopUp{TradeNo: tradeNo, UserId: user.Id, Amount: 999, Money: 12.34, PaymentProvider: PaymentProviderWechat, PaymentMethod: PaymentProviderWechat, Status: common.TopUpStatusPending}
				require.NoError(t, CreateWechatTopUp(topUp, order))
				return order
			}
			makeOrder("WXVALID")
			payment := WechatSettlement{TradeNo: "WXVALID", TransactionID: "transaction-1", AmountCents: 1234, Currency: "CNY", AppID: "wx-app", MchID: "merchant"}
			for _, field := range []string{"amount", "currency", "appid", "mchid", "transaction"} {
				t.Run(field, func(t *testing.T) {
					invalid := payment
					switch field {
					case "amount":
						invalid.AmountCents++
					case "currency":
						invalid.Currency = "USD"
					case "appid":
						invalid.AppID = "other"
					case "mchid":
						invalid.MchID = "other"
					case "transaction":
						invalid.TransactionID = ""
					}
					require.Error(t, CompleteWechatTopUp(invalid, ""))
					assert.Equal(t, 17, getUserQuotaForPaymentGuardTest(t, user.Id))
					assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(payment.TradeNo).Status)
				})
			}
			_, _, err = GetWechatTopUp("WXVALID", user.Id+1)
			require.Error(t, err)
			require.Error(t, ManualCompleteTopUp("WXVALID", ""))
			// Two real database transactions race; repeat delivery then remains a no-op.
			var wg sync.WaitGroup
			errors := make(chan error, 2)
			for range 2 {
				wg.Go(func() { errors <- CompleteWechatTopUp(payment, "") })
			}
			wg.Wait()
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			require.NoError(t, CompleteWechatTopUp(payment, ""))
			assert.Equal(t, 650018, getUserQuotaForPaymentGuardTest(t, user.Id))
			payment.AmountCents++
			require.Error(t, CompleteWechatTopUp(payment, ""))
			payment.AmountCents--
			makeOrder("WXDUPTRANSACTION")
			payment.TradeNo = "WXDUPTRANSACTION"
			require.Error(t, CompleteWechatTopUp(payment, ""))
			assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(payment.TradeNo).Status)
			assert.Equal(t, 650018, getUserQuotaForPaymentGuardTest(t, user.Id))
			// A provider success arriving after local close must still be credited.
			payment.TransactionID = "late-transaction"
			require.NoError(t, MarkWechatClosed(payment.TradeNo))
			require.NoError(t, CompleteWechatTopUp(payment, ""))
			assert.Equal(t, 1300019, getUserQuotaForPaymentGuardTest(t, user.Id))
			makeOrder("WXPROVIDER")
			require.NoError(t, db.Model(&TopUp{}).Where("trade_no = ?", "WXPROVIDER").Update("payment_provider", PaymentProviderEpay).Error)
			payment.TradeNo = "WXPROVIDER"
			payment.TransactionID = "foreign"
			require.Error(t, CompleteWechatTopUp(payment, ""))
			// Capacity is rechecked atomically at settlement; failure rolls back both
			// the status transition and unique transaction assignment.
			makeOrder("WXCAPACITY")
			require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("quota", common.MaxWalletQuota).Error)
			payment.TradeNo = "WXCAPACITY"
			payment.TransactionID = "over-capacity"
			require.ErrorIs(t, CompleteWechatTopUp(payment, ""), ErrTopUpQuotaLimitExceeded)
			assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(payment.TradeNo).Status)
			var unchanged WechatTopUp
			require.NoError(t, db.Where("trade_no = ?", payment.TradeNo).First(&unchanged).Error)
			assert.Nil(t, unchanged.TransactionID)
			// Fresh schema creation and repeated migration, including unique transaction IDs.
			require.NoError(t, db.Migrator().DropTable(&WechatTopUp{}))
			for range 2 {
				require.NoError(t, db.AutoMigrate(&WechatTopUp{}))
			}
		})
	}
}
