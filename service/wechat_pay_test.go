package service

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/validators"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
	"gorm.io/gorm"
)

func TestWechatSignedEncryptedNotification(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.WechatTopUp{}, &model.Log{}))
	user := model.User{Username: "wechat-notify-test", Quota: 5}
	require.NoError(t, db.Create(&user).Error)
	order := model.WechatTopUp{TradeNo: "WXNOTIFY", UserID: user.Id, AmountCents: 100, CreditedQuota: 12345, Currency: "CNY", AppID: "app", MchID: "mch"}
	require.NoError(t, model.CreateWechatTopUp(&model.TopUp{TradeNo: order.TradeNo, UserId: user.Id, PaymentProvider: "wechat", PaymentMethod: "wechat", Status: common.TopUpStatusPending}, &order))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	certificate := &x509.Certificate{PublicKey: &key.PublicKey, NotAfter: time.Now().Add(time.Hour)}
	verifier := verifiers.NewSHA256WithRSAVerifier(core.NewCertificateMap(map[string]*x509.Certificate{"1234": certificate}))
	apiKey := "12345678901234567890123456789012"
	handler, err := notify.NewRSANotifyHandler(apiKey, verifier)
	require.NoError(t, err)
	provider := WechatPay{handler: handler}
	transaction := payments.Transaction{OutTradeNo: core.String(order.TradeNo), TransactionId: core.String("wx-transaction"), TradeType: core.String("NATIVE"), TradeState: core.String("SUCCESS"), Appid: core.String("app"), Mchid: core.String("mch"), Amount: &payments.TransactionAmount{Total: core.Int64(100), Currency: core.String("CNY")}}
	for _, testCase := range []string{"bad signature", "bad ciphertext", "wrong total", "wrong currency", "wrong appid", "wrong merchant", "wrong type", "missing total", "valid", "duplicate"} {
		t.Run(testCase, func(t *testing.T) {
			payload := transaction.Clone()
			switch testCase {
			case "wrong total":
				payload.Amount.Total = core.Int64(99)
			case "wrong currency":
				payload.Amount.Currency = core.String("USD")
			case "wrong appid":
				payload.Appid = core.String("other")
			case "wrong merchant":
				payload.Mchid = core.String("other")
			case "wrong type":
				payload.TradeType = core.String("JSAPI")
			case "missing total":
				payload.Amount.Total = nil
			}
			plaintext, err := common.Marshal(payload)
			require.NoError(t, err)
			block, err := aes.NewCipher([]byte(apiKey))
			require.NoError(t, err)
			gcm, err := cipher.NewGCM(block)
			require.NoError(t, err)
			nonce := "123456789012"
			associated := "transaction"
			encrypted := gcm.Seal(nil, []byte(nonce), plaintext, []byte(associated))
			if testCase == "bad ciphertext" {
				encrypted[0] ^= 1
			}
			body, err := common.Marshal(map[string]any{"id": "event", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource", "resource": map[string]any{"algorithm": "AEAD_AES_256_GCM", "ciphertext": base64.StdEncoding.EncodeToString(encrypted), "nonce": nonce, "associated_data": associated}})
			require.NoError(t, err)
			timestamp := fmt.Sprint(time.Now().Unix())
			signatureNonce := "signed-nonce"
			digest := sha256.Sum256([]byte(timestamp + "\n" + signatureNonce + "\n" + string(body) + "\n"))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			require.NoError(t, err)
			if testCase == "bad signature" {
				signature[0] ^= 1
			}
			request := httptest.NewRequest("POST", "/api/user/wechat/notify", bytes.NewReader(body))
			request.Header.Set("Wechatpay-Timestamp", timestamp)
			request.Header.Set("Wechatpay-Nonce", signatureNonce)
			request.Header.Set("Wechatpay-Serial", "1234")
			request.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
			err = provider.Notify(context.Background(), request, "")
			if testCase == "valid" || testCase == "duplicate" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			var stored model.User
			require.NoError(t, db.First(&stored, user.Id).Error)
			if testCase == "valid" || testCase == "duplicate" {
				assert.Equal(t, 12350, stored.Quota)
			} else {
				assert.Equal(t, 5, stored.Quota)
			}
		})
	}
	t.Run("verified query recovery and close ambiguity", func(t *testing.T) {
		recovery := model.WechatTopUp{TradeNo: "WXRECOVERY", UserID: user.Id, AmountCents: 100, CreditedQuota: 777, Currency: "CNY", AppID: "app", MchID: "mch", ExpiresAt: time.Now().Unix() + 1800}
		require.NoError(t, model.CreateWechatTopUp(&model.TopUp{TradeNo: recovery.TradeNo, UserId: user.Id, PaymentProvider: "wechat", PaymentMethod: "wechat", Status: common.TopUpStatusPending}, &recovery))
		state := "NOTPAY"
		failClose := false
		invalidSignature := false
		absent := false
		createCalls, closeCalls := 0, 0
		transport := wechatTestTransport(func(request *http.Request) (*http.Response, error) {
			assert.NotEmpty(t, request.Header.Get("Authorization"))
			status := http.StatusOK
			body := ""
			switch {
			case request.Method == "GET":
				if absent {
					status = http.StatusNotFound
					body = `{"code":"ORDER_NOT_EXIST","message":"Order not found"}`
				} else {
					payload := transaction.Clone()
					payload.OutTradeNo = core.String(recovery.TradeNo)
					payload.TradeState = core.String(state)
					payload.TransactionId = core.String("recovered-transaction")
					data, marshalErr := common.Marshal(payload)
					require.NoError(t, marshalErr)
					body = string(data)
				}
			case strings.HasSuffix(request.URL.Path, "/close"):
				closeCalls++
				if failClose {
					return nil, errors.New("simulated lost close response")
				}
				status = http.StatusNoContent
				state = "CLOSED"
			default:
				createCalls++
				var prepay native.PrepayRequest
				require.NoError(t, common.DecodeJson(request.Body, &prepay))
				assert.Equal(t, recovery.TradeNo, *prepay.OutTradeNo)
				assert.Equal(t, int64(100), *prepay.Amount.Total)
				assert.Equal(t, "CNY", *prepay.Amount.Currency)
				body = `{"code_url":"weixin://wxpay/test"}`
			}
			timestamp := fmt.Sprint(time.Now().Unix())
			nonce := "response-nonce"
			digest := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + body + "\n"))
			signature, signErr := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			require.NoError(t, signErr)
			if invalidSignature {
				signature[0] ^= 1
			}
			header := http.Header{}
			header.Set("Wechatpay-Timestamp", timestamp)
			header.Set("Wechatpay-Nonce", nonce)
			header.Set("Wechatpay-Serial", "1234")
			header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
			header.Set("Content-Type", "application/json")
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		})
		client, clientErr := core.NewClient(context.Background(), option.WithMerchantCredential("mch", "merchant-serial", key), option.WithVerifier(verifier), option.WithHTTPClient(&http.Client{Transport: transport}))
		require.NoError(t, clientErr)
		recoveredProvider := WechatPay{api: native.NativeApiService{Client: client}, responseValidator: validators.NewWechatPayResponseValidator(verifier)}
		require.NoError(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		assert.Equal(t, 1, createCalls)
		stored, topup, readErr := model.GetWechatTopUp(recovery.TradeNo, user.Id)
		require.NoError(t, readErr)
		assert.Equal(t, "weixin://wxpay/test", stored.CodeURL)
		assert.Equal(t, common.TopUpStatusPending, topup.Status)
		recovery.ExpiresAt = time.Now().Unix() - 60
		failClose = true
		require.Error(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		assert.Equal(t, common.TopUpStatusPending, model.GetTopUpByTradeNo(recovery.TradeNo).Status)
		failClose = false
		require.NoError(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		assert.Equal(t, 2, closeCalls)
		assert.Equal(t, "closed", model.GetTopUpByTradeNo(recovery.TradeNo).Status)
		// A later verified success recovers a late payment through the same ledger.
		state = "SUCCESS"
		invalidSignature = true
		require.Error(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		assert.Equal(t, "closed", model.GetTopUpByTradeNo(recovery.TradeNo).Status)
		invalidSignature = false
		require.NoError(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		require.NoError(t, recoveredProvider.Refresh(context.Background(), &recovery, ""))
		var settled model.User
		require.NoError(t, db.First(&settled, user.Id).Error)
		assert.Equal(t, 13127, settled.Quota)
		// An absent expired order can be terminal only with a valid platform signature.
		absentOrder := recovery
		absentOrder.ID = 0
		absentOrder.TradeNo = "WXABSENT"
		absentOrder.CodeURL = ""
		absentOrder.TransactionID = nil
		require.NoError(t, model.CreateWechatTopUp(&model.TopUp{TradeNo: absentOrder.TradeNo, UserId: user.Id, PaymentProvider: "wechat", PaymentMethod: "wechat", Status: common.TopUpStatusPending}, &absentOrder))
		absent = true
		invalidSignature = true
		require.Error(t, recoveredProvider.Refresh(context.Background(), &absentOrder, ""))
		assert.Equal(t, common.TopUpStatusPending, model.GetTopUpByTradeNo(absentOrder.TradeNo).Status)
		invalidSignature = false
		require.NoError(t, recoveredProvider.Refresh(context.Background(), &absentOrder, ""))
		assert.Equal(t, "closed", model.GetTopUpByTradeNo(absentOrder.TradeNo).Status)
	})

}

type wechatTestTransport func(*http.Request) (*http.Response, error)

func (f wechatTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
