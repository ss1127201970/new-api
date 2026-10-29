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
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import { api } from '@/lib/api'

import { Wallet } from '../index'

const i18n = createInstance()
let client: QueryClient

beforeEach(async () => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en },
    nsSeparator: false,
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/topup/info') {
      return {
        data: {
          success: true,
          data: {
            enable_online_topup: true,
            min_topup: 10,
            pay_methods: [{ name: 'Alipay', type: 'alipay' }],
            amount_options: [],
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
    if (url === '/api/user/aff') return { data: { success: true, data: 'qa' } }
    return { data: { success: true, data: [] } }
  })
})

afterEach(() => client.clear())

async function renderWallet() {
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <Wallet />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const input = await screen.findByRole('spinbutton', { name: 'Custom Amount' })
  await waitFor(() => expect(input).toHaveValue(10))
  await waitFor(() =>
    expect(screen.queryByText('Balance credit: CNY 10.00')).toBeVisible()
  )
  return input
}

it('shows an automatic quote failure under the amount without a toast and clears it when typing continues', async () => {
  vi.spyOn(api, 'post').mockImplementation(async (_url, body) => ({
    data:
      (body as { amount: number }).amount < 10
        ? { message: 'error', data: 'Minimum topup amount: 10' }
        : { success: true, data: '10.00', credited_cny: '10.00' },
  }))
  const error = vi.spyOn(toast, 'error')
  const input = await renderWallet()
  fireEvent.change(input, { target: { value: '2' } })
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Minimum topup amount: 10'
  )
  expect(input).toHaveAttribute('aria-invalid', 'true')
  expect(input).toHaveAccessibleDescription('Minimum topup amount: 10')
  expect(error).not.toHaveBeenCalled()
  fireEvent.change(input, { target: { value: '20' } })
  await waitFor(() =>
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  )
  expect(input).toHaveAttribute('aria-invalid', 'false')
  expect(screen.getByRole('button', { name: 'Alipay' })).toBeEnabled()
})

it('toasts a quote failure after the user selects a payment method and keeps confirmation closed', async () => {
  vi.spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: true, data: '10.00', credited_cny: '10.00' },
    })
    .mockResolvedValue({
      data: { message: 'error', data: 'Quote provider unavailable' },
    })
  const error = vi.spyOn(toast, 'error')
  await renderWallet()
  fireEvent.click(screen.getByRole('button', { name: 'Alipay' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Quote provider unavailable'
  )
  expect(error).toHaveBeenCalledWith('Quote provider unavailable')
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
})

it('keeps the newest quote visible when an older automatic quote fails', async () => {
  let rejectOlder!: (error: Error) => void
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: '10.00', credited_cny: '10.00' },
  })
  const error = vi.spyOn(toast, 'error')
  const input = await renderWallet()
  post
    .mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          rejectOlder = reject
        })
    )
    .mockResolvedValue({
      data: { success: true, data: '20.00', credited_cny: '20.00' },
    })
  fireEvent.change(input, { target: { value: '2' } })
  fireEvent.change(input, { target: { value: '20' } })
  expect(await screen.findByText('Balance credit: CNY 20.00')).toBeVisible()
  await act(async () => {
    rejectOlder(new Error('Outdated failure'))
  })
  expect(screen.getByText('Balance credit: CNY 20.00')).toBeVisible()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  expect(input).toHaveAttribute('aria-invalid', 'false')
  expect(error).not.toHaveBeenCalled()
})
