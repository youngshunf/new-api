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
import { describe, expect, test } from 'vitest'

import { ERROR_MESSAGES } from '../../constants'
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  buildSettingJSON,
  channelFormSchema,
  transformChannelToFormDefaults,
} from '../channel-form'

function channelWithSetting(setting: Record<string, unknown>) {
  return channelSchema.parse({
    id: 9,
    name: 'Search',
    type: 1,
    key: '',
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    models: 'web-search',
    group: 'default',
    setting: JSON.stringify(setting),
  })
}

function budgetForm(period: 'none' | 'day' | 'month', limit?: number) {
  return {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'Search',
    key: 'test-key',
    models: 'web-search',
    request_budget_period: period,
    request_budget_limit: limit,
  }
}

describe('channel request budget setting', () => {
  test.each([
    ['day', 30],
    ['month', 1950],
  ] as const)(
    'round-trips a %s budget through the setting JSON',
    (period, limit) => {
      const values = transformChannelToFormDefaults(
        channelWithSetting({ proxy: '', request_budget: { period, limit } })
      )
      expect(values.request_budget_period).toBe(period)
      expect(values.request_budget_limit).toBe(limit)
      expect(JSON.parse(buildSettingJSON(values)).request_budget).toEqual({
        period,
        limit,
      })
    }
  )

  test('loads a channel without budget as no limit and saves no budget key', () => {
    const values = transformChannelToFormDefaults(
      channelWithSetting({ proxy: '' })
    )
    expect(values.request_budget_period).toBe('none')
    expect(JSON.parse(buildSettingJSON(values))).not.toHaveProperty(
      'request_budget'
    )
  })

  test('switching an existing budget to no limit removes the key', () => {
    const values = transformChannelToFormDefaults(
      channelWithSetting({ request_budget: { period: 'month', limit: 1950 } })
    )
    expect(
      JSON.parse(buildSettingJSON({ ...values, request_budget_period: 'none' }))
    ).not.toHaveProperty('request_budget')
  })

  test.each([
    ['missing limit', undefined],
    ['zero limit', 0],
    ['fractional limit', 1.5],
    ['limit above bound', 100000001],
  ])('rejects a budget with %s', (_name, limit) => {
    const result = channelFormSchema.safeParse(budgetForm('month', limit))
    expect(result.success).toBe(false)
    expect(
      result.error?.issues.find(
        (issue) => issue.path[0] === 'request_budget_limit'
      )?.message
    ).toBe(ERROR_MESSAGES.INVALID_REQUEST_BUDGET_LIMIT)
  })

  test('accepts no limit without a count and a budget at the upper bound', () => {
    expect(channelFormSchema.safeParse(budgetForm('none')).success).toBe(true)
    expect(
      channelFormSchema.safeParse(budgetForm('day', 100000000)).success
    ).toBe(true)
  })
})
