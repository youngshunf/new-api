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
import { api } from '@/lib/api'
import type {
  AdminToken,
  AdminTokenUpdate,
  ApiResponse,
  GetAdminTokensParams,
  PaginatedAdminTokens,
  SearchAdminTokensParams,
} from './types'

export async function getAdminTokens(
  params: GetAdminTokensParams = {}
): Promise<ApiResponse<PaginatedAdminTokens>> {
  const { p = 1, size = 20 } = params
  const res = await api.get(
    `/api/admin_token/?p=${p}&page_size=${size}`
  )
  return res.data
}

export async function searchAdminTokens(
  params: SearchAdminTokensParams
): Promise<ApiResponse<PaginatedAdminTokens>> {
  const { keyword = '', token = '', username = '', p = 1, size = 20 } = params
  const queryParams = new URLSearchParams()
  if (keyword) queryParams.set('keyword', keyword)
  if (token) queryParams.set('token', token)
  if (username) queryParams.set('username', username)
  queryParams.set('p', String(p))
  queryParams.set('page_size', String(size))
  const res = await api.get(
    `/api/admin_token/search?${queryParams.toString()}`
  )
  return res.data
}

export async function updateAdminToken(
  data: AdminTokenUpdate
): Promise<ApiResponse<AdminToken>> {
  const res = await api.put('/api/admin_token/', data)
  return res.data
}

export async function deleteAdminToken(id: number): Promise<ApiResponse> {
  const res = await api.delete(`/api/admin_token/${id}`)
  return res.data
}

export async function fetchAdminTokenKey(
  id: number
): Promise<ApiResponse<{ key: string }>> {
  const res = await api.post(`/api/admin_token/${id}/key`)
  return res.data
}
