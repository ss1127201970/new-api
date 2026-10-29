package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

func isWechatTopUpEnabled() bool {
	return isPaymentComplianceConfirmed() && service.GetWechatPay() != nil
}

// WeChat Native is configured for this site's CNY base unit.
func quoteWechatRequest(request AmountRequest, group string) (int64, int, error) {
	if operation_setting.GetQuotaDisplayType() != operation_setting.QuotaDisplayTypeCNY || operation_setting.Price != 1 || operation_setting.USDExchangeRate != 1 {
		return 0, 0, common.NewMessage("WeChat Pay requires CNY display, Price = 1 and USDExchangeRate = 1")
	}
	amount := request.Amount
	if amount <= 0 {
		return 0, 0, common.NewMessage("Invalid top-up amount")
	}
	minimum := max(operation_setting.MinTopUp, 1)
	if amount < int64(minimum) {
		return 0, 0, common.NewMessage("Top-up amount cannot be less than {{min}}", map[string]any{"min": minimum})
	}
	ratio := common.GetTopupGroupRatio(group)
	if ratio == 0 {
		ratio = 1
	}
	for _, value := range []float64{ratio, common.QuotaPerUnit} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, 0, common.NewMessage("Invalid top-up amount")
		}
	}
	base := decimal.NewFromInt(amount)
	quota, err := validateCreditedQuota(base.Mul(decimal.NewFromFloat(common.QuotaPerUnit)))
	if err != nil {
		return 0, 0, err
	}
	discount := 1.0
	if configured, ok := operation_setting.GetPaymentSetting().AmountDiscount[int(amount)]; ok {
		if configured <= 0 || configured > 1 || math.IsNaN(configured) || math.IsInf(configured, 0) {
			return 0, 0, common.NewMessage("Invalid top-up amount")
		}
		discount = configured
	}
	cents := base.Mul(decimal.NewFromFloat(operation_setting.Price)).Mul(decimal.NewFromFloat(ratio)).Mul(decimal.NewFromFloat(discount)).Mul(decimal.NewFromInt(100)).Round(0)
	// The provider amount stays integer cents; cap before conversion to int64.
	if cents.LessThan(decimal.NewFromInt(1)) || cents.GreaterThan(decimal.NewFromInt(10000000000)) {
		return 0, 0, common.NewMessage("Invalid top-up amount")
	}
	return cents.IntPart(), quota, nil
}

func wechatCreditedCNY(quota int) string {
	if common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
		return "0.00"
	}
	return decimal.NewFromInt(int64(quota)).Div(decimal.NewFromFloat(common.QuotaPerUnit)).StringFixed(2)
}

func RequestWechatAmount(c *gin.Context) {
	if !isWechatTopUpEnabled() {
		paymentError(c, common.NewMessage("Payment information has not been configured by administrator"))
		return
	}
	var request AmountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	group, err := model.GetUserGroup(c.GetInt("id"), true)
	if err != nil {
		paymentError(c, common.NewMessage("Failed to get the user group"))
		return
	}
	cents, quota, err := quoteWechatRequest(request, group)
	if err == nil {
		err = model.ValidateTopUpQuotaCapacity(c.GetInt("id"), quota)
	}
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": fmt.Sprintf("%d.%02d", cents/100, cents%100), "amount_cents": cents, "credited_quota": quota, "credited_cny": wechatCreditedCNY(quota)})
}

func wechatOrderResponse(c *gin.Context, order *model.WechatTopUp, topUp *model.TopUp) {
	status := topUp.Status
	if status == common.TopUpStatusExpired {
		status = "closed"
	}
	common.ApiSuccess(c, gin.H{"trade_no": order.TradeNo, "code_url": order.CodeURL, "status": status, "amount_cents": order.AmountCents, "expires_at": order.ExpiresAt, "credited_quota": order.CreditedQuota, "credited_cny": wechatCreditedCNY(order.CreditedQuota)})
}

func RequestWechatPay(c *gin.Context) {
	if !isWechatTopUpEnabled() {
		common.ApiErrorT(c, "Payment information has not been configured by administrator")
		return
	}
	var request AmountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	userID := c.GetInt("id")
	group, err := model.GetUserGroup(userID, true)
	if err != nil {
		common.ApiErrorT(c, "Failed to get the user group")
		return
	}
	cents, quota, err := quoteWechatRequest(request, group)
	if err == nil {
		err = model.ValidateTopUpQuotaCapacity(userID, quota)
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	var random [15]byte
	if _, err = rand.Read(random[:]); err != nil {
		common.ApiErrorT(c, "Failed to create order")
		return
	}
	now := time.Now().Unix()
	provider := service.GetWechatPay()
	order := &model.WechatTopUp{TradeNo: "WX" + hex.EncodeToString(random[:]), UserID: userID, AmountCents: cents, CreditedQuota: quota, Currency: "CNY", AppID: provider.AppID, MchID: provider.MchID, ExpiresAt: now + 30*60, NextCheckAt: now + 60}
	topUp := &model.TopUp{UserId: userID, Amount: request.Amount, Money: float64(cents) / 100, TradeNo: order.TradeNo, PaymentMethod: model.PaymentProviderWechat, PaymentProvider: model.PaymentProviderWechat, CreateTime: now, Status: common.TopUpStatusPending}
	if err = model.CreateWechatTopUp(topUp, order); err != nil {
		common.ApiErrorT(c, "Failed to create order")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	code, err := provider.Create(ctx, order)
	if err == nil {
		if err = model.UpdateWechatCodeURL(order.TradeNo, code); err == nil {
			order.CodeURL = code
		}
	}
	// Returning the durable pending order even on timeout lets the owner poll,
	// and permits recovery if the provider accepted an ambiguous create.
	wechatOrderResponse(c, order, topUp)
}

func QueryWechatOrder(c *gin.Context) {
	order, topUp, err := model.GetWechatTopUp(c.Param("trade_no"), c.GetInt("id"))
	if err != nil {
		common.ApiErrorT(c, "Top-up order does not exist")
		return
	}
	if provider := service.GetWechatPay(); provider != nil && topUp.Status != common.TopUpStatusSuccess {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		_ = provider.Refresh(ctx, order, c.ClientIP())
		cancel()
		order, topUp, err = model.GetWechatTopUp(order.TradeNo, c.GetInt("id"))
		if err != nil {
			common.ApiErrorT(c, "Failed to search top-up records")
			return
		}
	}
	wechatOrderResponse(c, order, topUp)
}

func WechatNotify(c *gin.Context) {
	provider := service.GetWechatPay()
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "FAIL", "message": "Payment unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	// Timestamp freshness limits replay work. Settlement is also idempotent.
	timestamp, err := strconv.ParseInt(c.GetHeader("Wechatpay-Timestamp"), 10, 64)
	now := time.Now().Unix()
	if err != nil || timestamp < now-300 || timestamp > now+300 {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "FAIL", "message": "Invalid notification"})
		return
	}
	if err = provider.Notify(c.Request.Context(), c.Request, c.ClientIP()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "Invalid notification"})
		return
	}
	c.Status(http.StatusNoContent)
}
