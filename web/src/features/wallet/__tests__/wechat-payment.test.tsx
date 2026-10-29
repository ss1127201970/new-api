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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  act,
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { createInstance } from 'i18next'
import { useState } from 'react'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

import { BillingHistoryDialog } from '../components/dialogs/billing-history-dialog'
import { WeChatPaymentDialog } from '../components/dialogs/wechat-payment-dialog'
import { RechargeFormCard } from '../components/recharge-form-card'
import { usePayment, requestPaymentAmount } from '../hooks/use-payment'
import { useTopupInfo } from '../hooks/use-topup-info'
import { useWaffoPancakePayment } from '../hooks/use-waffo-pancake-payment'
import { useWaffoPayment } from '../hooks/use-waffo-payment'
import { useWeChatPayment } from '../hooks/use-wechat-payment'
import { Wallet } from '../index'
import { isValidTopupAmount, getMinTopupAmount } from '../lib/payment'

const i18n = createInstance()
const paid = vi.fn(async () => undefined)
const order = {
  trade_no: 'native-1',
  code_url: 'weixin://wxpay/bizpayurl?pr=example',
  status: 'pending',
  amount_cents: 1234,
  expires_at: 2000000000,
}
let client: QueryClient

function PaymentFlow() {
  const payment = useWeChatPayment(paid)
  return (
    <>
      <button
        type='button'
        onClick={() => {
          void payment.processPayment(1)
        }}
      >
        Create payment
      </button>
      <button type='button' onClick={() => payment.resumeOrder('native-1')}>
        Resume payment
      </button>
      <WeChatPaymentDialog
        open={payment.open}
        onOpenChange={payment.setOpen}
        order={payment.order}
        loading={payment.loading}
        error={payment.error}
        onRefresh={() => {
          void payment.refresh()
        }}
      />
    </>
  )
}

function renderFlow(element = <PaymentFlow />) {
  return render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>{element}</QueryClientProvider>
    </I18nextProvider>
  )
}

beforeEach(async () => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  paid.mockClear()
  useAuthStore.getState().auth.setUser(null)
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en },
    nsSeparator: false,
    interpolation: { escapeValue: false },
  })
})
afterEach(() => {
  client.clear()
  useAuthStore.getState().auth.setUser(null)
  vi.useRealTimers()
})

it('quotes native WeChat with its dedicated endpoint while wxpay keeps EPay quoting', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { message: 'success', data: '12.34' } })
  expect(await requestPaymentAmount(1, 'wechat')).toEqual({
    amount: 12.34,
    creditedCny: null,
  })
  expect(post).toHaveBeenLastCalledWith('/api/user/wechat/amount', {
    amount: 1,
  })
  await requestPaymentAmount(100, 'wxpay')
  expect(post.mock.calls.at(-1)?.[0]).toBe('/api/user/amount')
})

it('creates one order on repeated submission and renders the server amount and local QR', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: order } })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: order },
  })
  renderFlow()
  fireEvent.click(screen.getByText('Create payment'))
  fireEvent.click(screen.getByText('Create payment'))
  expect(await screen.findByText('CNY 12.34')).toBeVisible()
  expect(screen.getByTitle('WeChat payment QR code')).toBeInTheDocument()
  expect(post).toHaveBeenCalledTimes(1)
  expect(post).toHaveBeenCalledWith('/api/user/wechat/pay', {
    amount: 1,
  })
})

it('resumes without creating an order and refreshes the wallet once after confirmed payment', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  const post = vi.spyOn(api, 'post')
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce({ data: { success: true, data: order } })
    .mockResolvedValue({
      data: { success: true, data: { ...order, status: 'success' } },
    })
  renderFlow()
  fireEvent.click(screen.getByText('Resume payment'))
  await screen.findByText('CNY 12.34')
  await act(async () => {
    await vi.advanceTimersByTimeAsync(3000)
  })
  expect(await screen.findByText('Payment confirmed.')).toBeVisible()
  await waitFor(() => expect(paid).toHaveBeenCalledTimes(1))
  expect(screen.queryByTitle('WeChat payment QR code')).not.toBeInTheDocument()
  expect(post).not.toHaveBeenCalled()
  await act(async () => {
    await vi.advanceTimersByTimeAsync(9000)
  })
  expect(get).toHaveBeenCalledTimes(2)
})

it('stops pending order polling when the payment dialog is closed', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: order } })
  renderFlow()
  fireEvent.click(screen.getByText('Resume payment'))
  await screen.findByText('CNY 12.34')
  fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0])
  await act(async () => {
    await vi.advanceTimersByTimeAsync(9000)
  })
  expect(get).toHaveBeenCalledTimes(1)
  expect(paid).not.toHaveBeenCalled()
})

it('shows failed status checks with a retry action and never claims payment succeeded', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('Connection lost'))
    .mockResolvedValue({
      data: { success: true, data: { ...order, status: 'closed' } },
    })
  renderFlow()
  fireEvent.click(screen.getByText('Resume payment'))
  expect(await screen.findByRole('alert')).toHaveTextContent('Connection lost')
  fireEvent.click(screen.getByRole('button', { name: 'Check payment status' }))
  expect(await screen.findByText(/This payment code has expired/)).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
  expect(paid).not.toHaveBeenCalled()
})

it('hides an expired payment code even before the next server check', async () => {
  renderFlow(
    <WeChatPaymentDialog
      open
      onOpenChange={vi.fn()}
      order={{ ...order, status: 'pending', expires_at: 1 }}
      loading={false}
      error={null}
      onRefresh={vi.fn()}
    />
  )
  expect(await screen.findByText(/This payment code has expired/)).toBeVisible()
  expect(screen.queryByTitle('WeChat payment QR code')).not.toBeInTheDocument()
})

it('offers recovery for the user pending native order from history', async () => {
  const resume = vi.fn()
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            ...order,
            id: 1,
            payment_method: 'wechat',
            amount: 100,
            money: 12.34,
            create_time: 1,
          },
        ],
      },
    },
  })
  renderFlow(
    <BillingHistoryDialog
      open
      onOpenChange={vi.fn()}
      onResumeWeChatOrder={resume}
    />
  )
  fireEvent.click(
    await screen.findByRole('button', { name: 'View payment code' })
  )
  expect(resume).toHaveBeenCalledWith('native-1')
})

it.each(['', 'https://external.example/qr'])(
  'does not render an unavailable or non-WeChat payment code: %s',
  async (codeUrl) => {
    renderFlow(
      <WeChatPaymentDialog
        open
        onOpenChange={vi.fn()}
        order={{ ...order, status: 'pending', code_url: codeUrl }}
        loading={false}
        error={null}
        onRefresh={vi.fn()}
      />
    )
    expect(
      await screen.findByText(/The payment code is unavailable/)
    ).toBeVisible()
    expect(
      screen.queryByTitle('WeChat payment QR code')
    ).not.toBeInTheDocument()
  }
)

it('makes native WeChat available when other online payment providers are disabled', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        enable_online_topup: false,
        enable_stripe_topup: false,
        enable_wechat_pay: true,
        min_topup: 2,
        pay_methods: [],
        amount_options: [],
        discount: {},
      },
    },
  })
  const hook = renderHook(() => useTopupInfo())
  await waitFor(() => expect(hook.result.current.loading).toBe(false))
  expect(hook.result.current.topupInfo?.pay_methods).toEqual([
    { name: 'WeChat Pay', type: 'wechat', min_topup: 2 },
  ])
  expect(getMinTopupAmount(hook.result.current.topupInfo)).toBe(2)
  expect(hook.result.current.presetAmounts[0].value).toBe(2)
})

it('allows administrators to reopen only their own native order and hides manual completion', async () => {
  const resume = vi.fn()
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 100 })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 2,
        items: [7, 8].map((userId) => ({
          ...order,
          id: userId,
          user_id: userId,
          trade_no: `native-${userId}`,
          payment_method: 'wechat',
          amount: 100,
          money: 12.34,
          create_time: 1,
        })),
      },
    },
  })
  renderFlow(
    <BillingHistoryDialog
      open
      onOpenChange={vi.fn()}
      onResumeWeChatOrder={resume}
    />
  )
  const recovery = await screen.findAllByRole('button', {
    name: 'View payment code',
  })
  expect(recovery).toHaveLength(1)
  fireEvent.click(recovery[0])
  expect(resume).toHaveBeenCalledWith('native-7')
  expect(
    screen.queryByRole('button', { name: 'Complete Order' })
  ).not.toBeInTheDocument()
})

it('uses the shared integer amount for native presets, quote, confirmation, QR and history', async () => {
  const config = useSystemConfigStore.getState().config
  useSystemConfigStore.setState({
    config: {
      ...config,
      currency: {
        ...config.currency,
        quotaDisplayType: 'CNY',
        quotaPerUnit: 500000,
        usdExchangeRate: 1,
        displayInCurrency: true,
      },
    },
  })
  const nativeOrder = {
    ...order,
    amount_cents: 100,
    credited_quota: 500000,
    credited_cny: '1.00',
  }
  const post = vi.spyOn(api, 'post').mockImplementation(async (url) => ({
    data:
      url === '/api/user/wechat/amount'
        ? {
            message: 'success',
            data: '1.00',
            amount_cents: 100,
            credited_quota: 500000,
            credited_cny: '1.00',
          }
        : { success: true, data: nativeOrder },
  }))
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/topup/info') {
      return {
        data: {
          success: true,
          data: {
            enable_wechat_pay: true,
            min_topup: 1,
            enable_online_topup: false,
            pay_methods: [],
            amount_options: [1, 10],
            discount: {},
          },
        },
      }
    }
    if (url === '/api/user/self') {
      return {
        data: { success: true, data: { id: 21, username: 'user', quota: 0 } },
      }
    }
    if (url.startsWith('/api/user/topup/self')) {
      return {
        data: {
          success: true,
          data: {
            total: 1,
            items: [
              {
                ...nativeOrder,
                id: 1,
                amount: 0,
                money: 1,
                payment_method: 'wechat',
                create_time: 1,
              },
            ],
          },
        },
      }
    }
    if (url.includes('/wechat/order/')) {
      return { data: { success: true, data: nativeOrder } }
    }
    if (url === '/api/status') {
      return {
        data: {
          success: true,
          data: {
            price: 1,
            quota_display_type: 'CNY',
            usd_exchange_rate: 1,
            quota_per_unit: 500000,
            display_in_currency: true,
          },
        },
      }
    }
    if (url === '/api/user/aff') return { data: { success: true, data: 'qa' } }
    return { data: { success: true, data: [] } }
  })
  try {
    renderFlow(<Wallet />)
    const input = await screen.findByRole('spinbutton', {
      name: 'Custom Amount',
    })
    await waitFor(() => expect(input).toHaveValue(1))
    expect(await screen.findByText('Balance credit: CNY 1.00')).toBeVisible()
    expect(screen.getByRole('button', { name: /10 Pay/ })).toBeVisible()
    expect(screen.queryByText('50% OFF')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Other payment methods' })
    ).not.toBeInTheDocument()
    fireEvent.change(input, { target: { value: '1.001' } })
    expect(screen.getByRole('button', { name: /WeChat Pay/ })).toBeDisabled()
    fireEvent.change(input, { target: { value: '1' } })
    await screen.findByText('Balance credit: CNY 1.00')
    fireEvent.click(screen.getByRole('button', { name: 'WeChat Pay' }))
    const confirm = await screen.findByRole('alertdialog')
    expect(within(confirm).getAllByText('CNY 1.00')).toHaveLength(2)
    fireEvent.click(
      within(confirm).getByRole('button', { name: 'Confirm Payment' })
    )
    expect(
      await screen.findByTitle('WeChat payment QR code')
    ).toBeInTheDocument()
    expect(post).toHaveBeenCalledWith('/api/user/wechat/pay', {
      amount: 1,
    })
    fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0])
    fireEvent.click(
      await screen.findByRole('button', { name: 'Order History' })
    )
    const history = await screen.findByRole('dialog')
    await waitFor(() => expect(within(history).getByText('¥1')).toBeVisible())
  } finally {
    useSystemConfigStore.setState({ config })
  }
})

it.each([
  [1, true],
  [2, true],
  [1.23, false],
  [0.99, false],
  [1.001, false],
  [Number.MAX_SAFE_INTEGER + 1, false],
])('validates integer yuan %s without silently rounding', (value, expected) => {
  expect(isValidTopupAmount(value)).toBe(expected)
})

it('keeps the frozen amount when reopening an older 730-cent order', async () => {
  renderFlow(
    <WeChatPaymentDialog
      open
      onOpenChange={vi.fn()}
      order={{
        ...order,
        status: 'pending',
        amount_cents: 730,
        credited_cny: '7.30',
      }}
      loading={false}
      error={null}
      onRefresh={vi.fn()}
    />
  )
  expect(await screen.findByText('CNY 7.30')).toBeVisible()
  expect(screen.getByText('Balance credit: CNY 7.30')).toBeVisible()
  expect(screen.queryByText('CNY 1.00')).not.toBeInTheDocument()
})

it('uses the native presets and discounts with the authoritative WeChat quote', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      message: 'success',
      data: '90.00',
      amount_cents: 9000,
      credited_cny: '100.00',
    },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/topup/info') {
      return {
        data: {
          success: true,
          data: {
            enable_wechat_pay: true,
            min_topup: 5,
            enable_online_topup: true,
            pay_methods: [{ name: 'Alipay', type: 'alipay' }],
            amount_options: [5, 100],
            discount: { 100: 0.9 },
          },
        },
      }
    }
    if (url === '/api/user/self') {
      return {
        data: { success: true, data: { id: 21, username: 'user', quota: 0 } },
      }
    }
    if (url === '/api/user/aff') return { data: { success: true, data: 'qa' } }
    return { data: { success: true, data: [] } }
  })
  renderFlow(<Wallet />)
  const preset = await screen.findByRole('button', { name: /100/ })
  expect(
    screen.queryByRole('button', { name: /^1 Pay/ })
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /730/ })).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Alipay' })).toBeVisible()
  fireEvent.click(preset)
  fireEvent.click(screen.getByRole('button', { name: 'WeChat Pay' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith('/api/user/wechat/amount', {
      amount: 100,
    })
  )
  expect(screen.queryByText('Other payment methods')).not.toBeInTheDocument()
  expect(screen.getByText('10% OFF')).toBeVisible()
  expect(await screen.findByText('Balance credit: CNY 100.00')).toBeVisible()
  const dialog = await screen.findByRole('alertdialog')
  expect(within(dialog).getByText('CNY 90.00')).toBeVisible()
  expect(within(dialog).getAllByText('CNY 100.00')[0]).toBeVisible()
})

it.each(['2.5', '-2'])(
  'keeps invalid input %s visible and disables all integer-amount payment methods',
  (value) => {
    const select = vi.fn()
    function Recharge() {
      const [amount, setAmount] = useState(1)
      return (
        <RechargeFormCard
          topupInfo={{
            enable_online_topup: true,
            enable_stripe_topup: true,
            stripe_min_topup: 1,
            amount_options: [],
            discount: {},
            min_topup: 1,
            pay_methods: [
              { name: 'Alipay', type: 'alipay' },
              { name: 'Stripe', type: 'stripe' },
              { name: 'Pancake', type: 'waffo_pancake' },
            ],
          }}
          presetAmounts={[]}
          selectedPreset={null}
          onSelectPreset={vi.fn()}
          topupAmount={amount}
          onTopupAmountChange={setAmount}
          paymentAmount={0}
          calculating={false}
          onPaymentMethodSelect={select}
          paymentLoading={null}
          redemptionCode=''
          onRedemptionCodeChange={vi.fn()}
          onRedeem={vi.fn()}
          redeeming={false}
          enableWaffoTopup
          waffoPayMethods={[{ name: 'Waffo card' }]}
          waffoMinTopup={1}
          onWaffoMethodSelect={select}
        />
      )
    }
    renderFlow(<Recharge />)
    const input = screen.getByRole('spinbutton', { name: 'Custom Amount' })
    fireEvent.change(input, { target: { value } })
    expect(input).toHaveValue(Number(value))
    expect(input).toHaveAttribute('aria-invalid', 'true')
    for (const name of ['Alipay', 'Stripe', 'Pancake', 'Waffo card']) {
      const button = screen.getByRole('button', { name: new RegExp(name) })
      expect(button).toBeDisabled()
      expect(button).toHaveAccessibleName(
        `${name}. Amount must be a whole number`
      )
    }
    fireEvent.change(input, { target: { value: '3' } })
    expect(screen.getByRole('button', { name: 'Alipay' })).toBeEnabled()
  }
)

it.each([
  { message: 'error', data: 'Quote provider unavailable' },
  { success: true, data: 'not-a-number' },
  { success: true, data: '0' },
])(
  'reports a failed or unusable quote instead of silently returning zero: %j',
  async (response) => {
    vi.spyOn(api, 'post').mockResolvedValue({ data: response })
    const error = vi.spyOn(toast, 'error')
    const hook = renderHook(() => usePayment())
    await act(async () => {
      expect(
        await hook.result.current.calculatePaymentAmount(2, 'alipay', 'payment')
      ).toBe(0)
    })
    expect(error).toHaveBeenCalledWith(
      response.message === 'error'
        ? 'Quote provider unavailable'
        : 'Payment request failed'
    )
    expect(hook.result.current.amount).toBe(0)
    expect(hook.result.current.creditedCny).toBeNull()
  }
)

it('does not send a fractional amount or truncate it when requesting payment', async () => {
  const post = vi.spyOn(api, 'post')
  const error = vi.spyOn(toast, 'error')
  const hook = renderHook(() => usePayment())
  await act(async () => {
    expect(await hook.result.current.processPayment(2.5, 'alipay')).toBe(false)
  })
  expect(post).not.toHaveBeenCalled()
  expect(error).toHaveBeenCalledWith('Amount must be a whole number')
})

it('rejects fractional Waffo and Pancake submissions without creating a truncated order', async () => {
  const post = vi.spyOn(api, 'post')
  vi.spyOn(toast, 'error')
  const hook = renderHook(() => ({
    waffo: useWaffoPayment(),
    pancake: useWaffoPancakePayment(),
  }))
  await act(async () => {
    expect(await hook.result.current.waffo.processWaffoPayment(2.5, 0)).toBe(
      false
    )
    expect(
      await hook.result.current.pancake.processWaffoPancakePayment(2.5)
    ).toBe(false)
  })
  expect(post).not.toHaveBeenCalled()
})

it('clears a prior native quote and reports transport failures', async () => {
  vi.spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: true, data: '2.00', credited_cny: '3.00' },
    })
    .mockRejectedValueOnce(new Error('Connection lost'))
  const error = vi.spyOn(toast, 'error')
  const hook = renderHook(() => usePayment())
  await act(async () => {
    await hook.result.current.calculatePaymentAmount(2, 'wechat')
  })
  expect(hook.result.current.creditedCny).toBe(3)
  await act(async () => {
    await hook.result.current.calculatePaymentAmount(3, 'wechat', 'payment')
  })
  expect(hook.result.current.amount).toBe(0)
  expect(hook.result.current.creditedCny).toBeNull()
  expect(error).toHaveBeenCalledWith('Connection lost')
})

it('keeps the latest quote when an older request fails and suppresses the stale error', async () => {
  let rejectOlder!: (error: Error) => void
  vi.spyOn(api, 'post')
    .mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          rejectOlder = reject
        })
    )
    .mockResolvedValueOnce({
      data: { success: true, data: '4.00', credited_cny: '5.00' },
    })
  const error = vi.spyOn(toast, 'error')
  const hook = renderHook(() => usePayment())
  let older!: Promise<number>
  await act(async () => {
    older = hook.result.current.calculatePaymentAmount(1, 'wechat')
    await hook.result.current.calculatePaymentAmount(2, 'wechat')
  })
  await act(async () => {
    rejectOlder(new Error('Outdated failure'))
    await older
  })
  expect(hook.result.current.amount).toBe(4)
  expect(hook.result.current.creditedCny).toBe(5)
  expect(hook.result.current.calculating).toBe(false)
  expect(error).not.toHaveBeenCalled()
})
