/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QRCodeSVG } from 'qrcode.react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { formatTimestamp } from '../../lib/billing'
import { formatCnyAmount } from '../../lib/format'
import type { WeChatOrder } from '../../types'

interface WeChatPaymentDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  order?: WeChatOrder
  loading: boolean
  error: Error | null
  onRefresh: () => void
}

export function WeChatPaymentDialog(props: WeChatPaymentDialogProps) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (!props.open || props.order?.status !== 'pending') return
    setNow(Date.now())
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [props.open, props.order?.status])
  const expired =
    !!props.order?.expires_at && now >= props.order.expires_at * 1000
  const pending = props.order?.status === 'pending'
  const showQr =
    pending && !expired && props.order?.code_url.startsWith('weixin://wxpay/')

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('WeChat Pay')}
      description={t(
        'Scan this QR code with WeChat to pay. Your balance updates after payment is confirmed.'
      )}
      contentClassName='max-sm:w-[calc(100vw-1.5rem)] sm:max-w-md'
      bodyClassName='space-y-4 text-center'
      footer={
        <Button variant='outline' onClick={() => props.onOpenChange(false)}>
          {t('Close')}
        </Button>
      }
    >
      {props.loading && <Skeleton className='mx-auto h-56 w-56' />}
      {props.order && (
        <>
          <p className='text-2xl font-semibold'>
            {formatCnyAmount(props.order.amount_cents / 100, locale)}
          </p>
          {props.order.credited_cny && (
            <p className='text-muted-foreground text-sm'>
              {t('Balance credit: {{amount}}', {
                amount: formatCnyAmount(
                  Number(props.order.credited_cny),
                  locale
                ),
              })}
            </p>
          )}
          {showQr && (
            <div className='mx-auto w-fit rounded-lg bg-white p-4'>
              <QRCodeSVG
                value={props.order.code_url}
                size={224}
                marginSize={4}
                title={t('WeChat payment QR code')}
              />
            </div>
          )}
          <p role='status' className='text-sm'>
            {props.order.status === 'success' && t('Payment confirmed.')}
            {(props.order.status === 'closed' || (pending && expired)) &&
              t(
                'This payment code has expired or the order is closed. Check your order history before creating another payment.'
              )}
            {pending && !expired && t('Waiting for payment confirmation...')}
          </p>
          {pending && !expired && !showQr && (
            <p className='text-muted-foreground text-sm'>
              {t(
                'The payment code is unavailable. Check the order status before creating another payment.'
              )}
            </p>
          )}
          {pending && !expired && props.order.expires_at > 0 && (
            <p className='text-muted-foreground text-xs'>
              {t('Valid until {{time}}', {
                time: formatTimestamp(props.order.expires_at),
              })}
            </p>
          )}
          <p className='text-muted-foreground text-xs break-all'>
            {props.order.trade_no}
          </p>
          {pending && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'You can close this window and reopen the payment from Order History.'
              )}
            </p>
          )}
        </>
      )}
      {props.error && (
        <div role='alert' className='space-y-2 text-sm'>
          <p>{getServerErrorMessage(props.error)}</p>
          <Button variant='outline' onClick={props.onRefresh}>
            {t('Check payment status')}
          </Button>
        </div>
      )}
    </Dialog>
  )
}
