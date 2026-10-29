package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/validators"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/downloader"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

// WechatPay owns only provider transport; TopUp remains the wallet authority.
type WechatPay struct {
	AppID, MchID, NotifyURL string
	api                     native.NativeApiService
	handler                 *notify.Handler
	responseValidator       auth.Validator
}

var wechatPay atomic.Pointer[WechatPay]
var ErrWechatUnavailable = errors.New("wechat payment unavailable")

func GetWechatPay() *WechatPay { return wechatPay.Load() }

// InitWechatPay uses the official SDK's automatically refreshed platform
// certificates, with response verification enabled for every API request.
func InitWechatPay() error {
	if GetWechatPay() != nil {
		return nil
	}
	if os.Getenv("WECHAT_PAY_ENABLED") != "true" {
		return nil
	}
	appID, mchID, serial := strings.TrimSpace(os.Getenv("WECHAT_PAY_APP_ID")), strings.TrimSpace(os.Getenv("WECHAT_PAY_MCH_ID")), strings.TrimSpace(os.Getenv("WECHAT_PAY_CERT_SERIAL_NO"))
	key := strings.TrimSpace(os.Getenv("WECHAT_PAY_API_V3_KEY"))
	if path := os.Getenv("WECHAT_PAY_API_V3_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return ErrWechatUnavailable
		}
		key = strings.TrimSpace(string(data))
	}
	notifyURL := strings.TrimSpace(os.Getenv("WECHAT_PAY_NOTIFY_URL"))
	parsed, err := url.Parse(notifyURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || appID == "" || mchID == "" || serial == "" || len(key) != 32 {
		return ErrWechatUnavailable
	}
	privateKey, err := utils.LoadPrivateKeyWithPath(os.Getenv("WECHAT_PAY_PRIVATE_KEY_FILE"))
	if err != nil {
		return ErrWechatUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The convenience AutoAuth option uses context.Background for its first
	// download; register explicitly so startup observes our timeout.
	manager := downloader.MgrInstance()
	if !manager.HasDownloader(ctx, mchID) {
		if err := manager.RegisterDownloaderWithPrivateKey(ctx, privateKey, serial, mchID, key); err != nil {
			return ErrWechatUnavailable
		}
	}
	client, err := core.NewClient(ctx, option.WithHTTPClient(&http.Client{Timeout: 15 * time.Second}), option.WithWechatPayAutoAuthCipherUsingDownloaderMgr(mchID, serial, privateKey, manager))
	if err != nil {
		return ErrWechatUnavailable
	}
	verifier := verifiers.NewSHA256WithRSAVerifier(downloader.MgrInstance().GetCertificateVisitor(mchID))
	handler, err := notify.NewRSANotifyHandler(key, verifier)
	if err != nil {
		return ErrWechatUnavailable
	}
	wechatPay.Store(&WechatPay{AppID: appID, MchID: mchID, NotifyURL: notifyURL, api: native.NativeApiService{Client: client}, handler: handler, responseValidator: validators.NewWechatPayResponseValidator(verifier)})
	go runWechatRecovery()
	return nil
}

// RetryWechatPayInitialization is started only after the initial startup
// attempt failed. It exits after success, which starts the recovery loop once.
func RetryWechatPayInitialization() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if err := InitWechatPay(); err == nil {
			common.SysLog(common.LogText("WeChat payment initialization recovered"))
			return
		}
	}
}

func (w *WechatPay) Create(ctx context.Context, order *model.WechatTopUp) (string, error) {
	expires := time.Unix(order.ExpiresAt, 0)
	response, _, err := w.api.Prepay(ctx, native.PrepayRequest{Appid: core.String(order.AppID), Mchid: core.String(order.MchID), Description: core.String("Account recharge"), OutTradeNo: core.String(order.TradeNo), TimeExpire: &expires, NotifyUrl: core.String(w.NotifyURL), Amount: &native.Amount{Total: core.Int64(order.AmountCents), Currency: core.String("CNY")}})
	if err != nil {
		var apiErr *core.APIError
		if errors.As(err, &apiErr) && regexp.MustCompile(`^[A-Z_]{1,64}$`).MatchString(apiErr.Code) {
			common.SysError(common.LogText("WeChat prepay failed trade_no=%s status=%d code=%s", order.TradeNo, apiErr.StatusCode, apiErr.Code))
		} else {
			common.SysError(common.LogText("WeChat prepay transport or verification failed trade_no=%s", order.TradeNo))
		}
		return "", ErrWechatUnavailable
	}
	if response == nil || response.CodeUrl == nil || !strings.HasPrefix(*response.CodeUrl, "weixin://wxpay/") {
		return "", ErrWechatUnavailable
	}
	return *response.CodeUrl, nil
}

func CompleteWechatTransaction(transaction *payments.Transaction, callerIP string) error {
	if transaction == nil || transaction.TradeState == nil || *transaction.TradeState != "SUCCESS" || transaction.TradeType == nil || *transaction.TradeType != "NATIVE" || transaction.OutTradeNo == nil || transaction.TransactionId == nil || transaction.Appid == nil || transaction.Mchid == nil || transaction.Amount == nil || transaction.Amount.Total == nil || transaction.Amount.Currency == nil {
		return model.ErrWechatPaymentMismatch
	}
	return model.CompleteWechatTopUp(model.WechatSettlement{TradeNo: *transaction.OutTradeNo, TransactionID: *transaction.TransactionId, AppID: *transaction.Appid, MchID: *transaction.Mchid, AmountCents: *transaction.Amount.Total, Currency: *transaction.Amount.Currency}, callerIP)
}

func (w *WechatPay) Notify(ctx context.Context, request *http.Request, callerIP string) error {
	var transaction payments.Transaction
	envelope, err := w.handler.ParseNotifyRequest(ctx, request, &transaction)
	if err != nil || envelope == nil || envelope.EventType != "TRANSACTION.SUCCESS" {
		return model.ErrWechatPaymentMismatch
	}
	return CompleteWechatTransaction(&transaction, callerIP)
}

// Refresh queries before closing to resolve the payment /
// expiration race. The next signed query confirms closure; callbacks can still
// settle any late success. A close failure never marks the local order terminal.
func (w *WechatPay) Refresh(ctx context.Context, order *model.WechatTopUp, callerIP string) error {
	transaction, result, err := w.api.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{OutTradeNo: core.String(order.TradeNo), Mchid: core.String(order.MchID)})
	if err != nil {
		// The SDK validates successful responses but returns non-2xx API errors
		// before signature verification. Verify absence explicitly before stopping
		// recovery; an unsigned error can never discard a possibly paid order.
		absent := core.IsAPIError(err, "ORDER_NOT_EXIST") || core.IsAPIError(err, "ORDERNOTEXIST")
		if absent && time.Now().Unix() > order.ExpiresAt && result != nil && result.Response != nil && w.responseValidator != nil && w.responseValidator.Validate(ctx, result.Response) == nil {
			if closeErr := model.MarkWechatClosed(order.TradeNo); closeErr != nil {
				return closeErr
			}
			return model.StopWechatRecovery(order.TradeNo)
		}
		// Retrying creation with the same immutable out_trade_no is safe even if
		// a previous request was accepted but its response was lost.
		if (core.IsAPIError(err, "ORDER_NOT_EXIST") || core.IsAPIError(err, "ORDERNOTEXIST")) && order.CodeURL == "" && time.Now().Unix() < order.ExpiresAt {
			code, createErr := w.Create(ctx, order)
			if createErr != nil {
				return createErr
			}
			return model.UpdateWechatCodeURL(order.TradeNo, code)
		}
		return ErrWechatUnavailable
	}
	if transaction == nil || transaction.OutTradeNo == nil || *transaction.OutTradeNo != order.TradeNo || transaction.Appid == nil || *transaction.Appid != order.AppID || transaction.Mchid == nil || *transaction.Mchid != order.MchID || transaction.TradeState == nil {
		return model.ErrWechatPaymentMismatch
	}
	switch *transaction.TradeState {
	case "SUCCESS":
		if err := CompleteWechatTransaction(transaction, callerIP); err != nil {
			return err
		}
		return model.StopWechatRecovery(order.TradeNo)
	case "CLOSED", "REVOKED", "PAYERROR":
		if err := model.MarkWechatClosed(order.TradeNo); err != nil {
			return err
		}
		return model.StopWechatRecovery(order.TradeNo)
	case "NOTPAY":
		if time.Now().Unix() <= order.ExpiresAt {
			if order.CodeURL == "" {
				code, err := w.Create(ctx, order)
				if err != nil {
					return err
				}
				return model.UpdateWechatCodeURL(order.TradeNo, code)
			}
			return nil
		}
		_, err = w.api.CloseOrder(ctx, native.CloseOrderRequest{OutTradeNo: core.String(order.TradeNo), Mchid: core.String(order.MchID)})
		if err != nil {
			return ErrWechatUnavailable
		}
		if err = model.MarkWechatClosed(order.TradeNo); err != nil {
			return err
		}
		// A signed CLOSED response on the next recovery stops the durable polling.
		return nil
	default:
		return nil
	}
}

func runWechatRecovery() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		orders, err := model.ClaimWechatRecovery(time.Now().Unix())
		if err != nil {
			common.SysError(common.LogText("WeChat payment recovery database error"))
			continue
		}
		for i := range orders {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = GetWechatPay().Refresh(ctx, &orders[i], "")
			cancel()
			if err != nil {
				common.SysError(common.LogText("WeChat payment recovery deferred trade_no=%s", orders[i].TradeNo))
			}
		}
	}
}
