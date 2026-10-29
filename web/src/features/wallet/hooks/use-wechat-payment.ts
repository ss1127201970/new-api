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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { handleServerError } from '@/lib/handle-server-error'
import { createServerError } from '@/lib/server-error-message'

import { getWeChatOrder, isApiSuccess, requestWeChatPayment } from '../api'
import { isValidTopupAmount } from '../lib/payment'

export function useWeChatPayment(onPaid: () => Promise<void>) {
  const queryClient = useQueryClient()
  const [tradeNo, setTradeNo] = useState('')
  const [open, setOpen] = useState(false)
  const submitting = useRef(false)
  const refreshedTradeNo = useRef('')
  const creation = useMutation({
    mutationFn: async (amount: number) => {
      if (!isValidTopupAmount(amount)) throw new Error('Invalid top-up amount')
      const response = await requestWeChatPayment({ amount })
      if (!isApiSuccess(response) || !response.data) {
        throw createServerError(response)
      }
      return response.data
    },
    retry: false,
    onError: (error) => handleServerError(error),
    onSuccess: (order) => {
      queryClient.setQueryData(['wallet', 'wechat', order.trade_no], order)
      setTradeNo(order.trade_no)
      setOpen(true)
    },
  })
  const orderQuery = useQuery({
    queryKey: ['wallet', 'wechat', tradeNo],
    queryFn: async () => {
      const response = await getWeChatOrder(tradeNo)
      if (!isApiSuccess(response) || !response.data) {
        throw createServerError(response)
      }
      return response.data
    },
    enabled: open && !!tradeNo,
    refetchInterval: (query) => {
      if (
        query.state.error ||
        (query.state.data && query.state.data.status !== 'pending')
      ) {
        return false
      }
      return 3000
    },
    retry: false,
    meta: { errorToast: false },
  })

  useEffect(() => {
    if (
      orderQuery.data?.status !== 'success' ||
      refreshedTradeNo.current === tradeNo
    ) {
      return
    }
    refreshedTradeNo.current = tradeNo
    void onPaid()
  }, [orderQuery.data?.status, tradeNo, onPaid])

  const processPayment = async (amount: number): Promise<boolean> => {
    if (submitting.current) return false
    submitting.current = true
    try {
      await creation.mutateAsync(amount)
      return true
    } catch {
      return false
    } finally {
      submitting.current = false
    }
  }

  const resumeOrder = (orderTradeNo: string): void => {
    setTradeNo(orderTradeNo)
    setOpen(true)
    void queryClient.invalidateQueries({
      queryKey: ['wallet', 'wechat', orderTradeNo],
    })
  }

  return {
    open,
    setOpen,
    order: orderQuery.data,
    loading: orderQuery.isPending,
    error: orderQuery.error,
    refresh: orderQuery.refetch,
    processing: creation.isPending,
    processPayment,
    resumeOrder,
  }
}
