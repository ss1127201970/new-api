package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"gorm.io/gorm"
)

const PaymentProviderWechat = "wechat"

// WechatTopUp stores the immutable quote and merchant identity alongside the
// authoritative TopUp. A separate table avoids changing legacy payment semantics.
type WechatTopUp struct {
	ID            int    `gorm:"primaryKey"`
	TradeNo       string `gorm:"type:varchar(32);uniqueIndex"`
	UserID        int    `gorm:"index"`
	AmountCents   int64
	CreditedQuota int
	Currency      string  `gorm:"type:varchar(3)"`
	AppID         string  `gorm:"type:varchar(64)"`
	MchID         string  `gorm:"type:varchar(32)"`
	TransactionID *string `gorm:"type:varchar(64);uniqueIndex"`
	CodeURL       string  `gorm:"type:text"`
	ExpiresAt     int64
	NextCheckAt   int64 `gorm:"index"`
}

type WechatSettlement struct {
	TradeNo, TransactionID, Currency, AppID, MchID string
	AmountCents                                    int64
}

var ErrWechatPaymentMismatch = errors.New("payment details do not match order")

func CreateWechatTopUp(topUp *TopUp, order *WechatTopUp) error {
	if topUp.PaymentProvider != PaymentProviderWechat || topUp.PaymentMethod != PaymentProviderWechat || topUp.UserId != order.UserID || topUp.TradeNo != order.TradeNo || order.Currency != "CNY" || order.AmountCents <= 0 || order.CreditedQuota <= 0 || order.CreditedQuota > common.MaxWalletQuota {
		return ErrWechatPaymentMismatch
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(topUp).Error; err != nil {
			return err
		}
		return tx.Create(order).Error
	})
}

func GetWechatTopUp(tradeNo string, userID int) (*WechatTopUp, *TopUp, error) {
	var order WechatTopUp
	if userID <= 0 {
		return nil, nil, ErrTopUpNotFound
	}
	query := DB.Where("trade_no = ? AND user_id = ?", tradeNo, userID)
	if err := query.First(&order).Error; err != nil {
		return nil, nil, err
	}
	var topUp TopUp
	if err := DB.Where("trade_no = ? AND user_id = ? AND payment_provider = ? AND payment_method = ?", tradeNo, order.UserID, PaymentProviderWechat, PaymentProviderWechat).First(&topUp).Error; err != nil {
		return nil, nil, err
	}
	return &order, &topUp, nil
}

// CompleteWechatTopUp is shared by signed callbacks and authenticated provider
// queries. Merchant/amount checks precede even the duplicate-success fast path.
func CompleteWechatTopUp(payment WechatSettlement, callerIP string) error {
	var order WechatTopUp
	var credited bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var topUp TopUp
		if err := lockForUpdate(tx).Where("trade_no = ?", payment.TradeNo).First(&topUp).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("trade_no = ?", payment.TradeNo).First(&order).Error; err != nil {
			return err
		}
		if topUp.PaymentProvider != PaymentProviderWechat || topUp.PaymentMethod != PaymentProviderWechat || topUp.UserId != order.UserID || payment.TransactionID == "" || len(payment.TransactionID) > 64 || payment.AmountCents != order.AmountCents || payment.Currency != "CNY" || payment.Currency != order.Currency || payment.AppID != order.AppID || payment.MchID != order.MchID {
			return ErrWechatPaymentMismatch
		}
		if topUp.Status == common.TopUpStatusSuccess {
			if order.TransactionID == nil || *order.TransactionID != payment.TransactionID {
				return ErrWechatPaymentMismatch
			}
			return nil
		}
		// Closed/expired orders remain eligible: a verified late success must not be lost.
		if topUp.Status != common.TopUpStatusPending && topUp.Status != common.TopUpStatusExpired && topUp.Status != "closed" {
			return ErrTopUpStatusInvalid
		}
		result := tx.Model(&TopUp{}).Where("id = ? AND status = ?", topUp.Id, topUp.Status).Updates(map[string]any{"status": common.TopUpStatusSuccess, "complete_time": time.Now().Unix()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTopUpStatusInvalid
		}
		if err := tx.Model(&WechatTopUp{}).Where("id = ?", order.ID).Update("transaction_id", payment.TransactionID).Error; err != nil {
			return err
		}
		if err := creditTopUpQuota(tx, order.UserID, order.CreditedQuota, nil); err != nil {
			return err
		}
		credited = true
		return nil
	})
	if err != nil {
		return err
	}
	if credited {
		syncCreditUserQuotaCache(order.UserID, order.CreditedQuota, common.LogText("wechat topup"))
		RecordTopupLog(order.UserID, common.NewMessage("Online top-up succeeded, amount added: {{quota}}, payment amount: {{amount}}", map[string]any{"quota": logger.FormatQuota(order.CreditedQuota), "amount": fmt.Sprintf("%.2f", float64(order.AmountCents)/100)}), callerIP, PaymentProviderWechat, PaymentProviderWechat)
	}
	return nil
}

func UpdateWechatCodeURL(tradeNo, codeURL string) error {
	return DB.Model(&WechatTopUp{}).Where("trade_no = ?", tradeNo).Update("code_url", codeURL).Error
}
func MarkWechatClosed(tradeNo string) error {
	return DB.Model(&TopUp{}).Where("trade_no = ? AND payment_provider = ? AND status = ?", tradeNo, PaymentProviderWechat, common.TopUpStatusPending).Update("status", "closed").Error
}

// ClaimWechatRecovery distributes bounded recovery across replicas. The next
// attempt is durable, so one failing remote order cannot starve later orders.
func ClaimWechatRecovery(now int64) ([]WechatTopUp, error) {
	var candidates []WechatTopUp
	err := DB.Where("next_check_at <= ?", now).Order("next_check_at asc").Limit(20).Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	claimed := make([]WechatTopUp, 0, len(candidates))
	for _, order := range candidates {
		nextCheck := now + 120
		if now > order.ExpiresAt+24*60*60 {
			nextCheck = now + 24*60*60
		}
		result := DB.Model(&WechatTopUp{}).Where("id = ? AND next_check_at = ?", order.ID, order.NextCheckAt).Update("next_check_at", nextCheck)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			claimed = append(claimed, order)
		}
	}
	return claimed, nil
}
func StopWechatRecovery(tradeNo string) error {
	return DB.Model(&WechatTopUp{}).Where("trade_no = ?", tradeNo).Update("next_check_at", int64(9223372036854775807)).Error
}

// WechatTopUpHistory enriches the existing history response without changing the
// legacy TopUp amount column or reconstructing frozen credits from cash paid.
type WechatTopUpHistory struct {
	*TopUp
	CreditedQuota *int `json:"credited_quota,omitempty"`
}

func WithWechatTopUpCredits(topUps []*TopUp) ([]WechatTopUpHistory, error) {
	records := make([]WechatTopUpHistory, len(topUps))
	var tradeNos []string
	for i, topUp := range topUps {
		records[i].TopUp = topUp
		if topUp.PaymentProvider == PaymentProviderWechat {
			tradeNos = append(tradeNos, topUp.TradeNo)
		}
	}
	if len(tradeNos) == 0 {
		return records, nil
	}
	var orders []WechatTopUp
	if err := DB.Select("trade_no", "credited_quota").Where("trade_no IN ?", tradeNos).Find(&orders).Error; err != nil {
		return nil, err
	}
	credits := make(map[string]int, len(orders))
	for _, order := range orders {
		credits[order.TradeNo] = order.CreditedQuota
	}
	for i, topUp := range topUps {
		if topUp.PaymentProvider == PaymentProviderWechat {
			if quota, ok := credits[topUp.TradeNo]; ok {
				records[i].CreditedQuota = &quota
			}
		}
	}
	return records, nil
}
