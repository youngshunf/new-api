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
import { type ColumnDef } from '@tanstack/react-table'
import { useTranslation } from 'react-i18next'
import { formatQuota, formatTimestampToDate } from '@/lib/format'
import { cn } from '@/lib/utils'
import { DataTableColumnHeader } from '@/components/data-table'
import { GroupBadge } from '@/components/group-badge'
import { StatusBadge } from '@/components/status-badge'
import { type AdminToken } from '../types'
import { AdminTokensRowActions } from './admin-tokens-row-actions'

const STATUS_LABELS: Record<
  number,
  {
    label: string
    variant: 'success' | 'danger' | 'warning' | 'neutral'
  }
> = {
  1: { label: 'Enabled', variant: 'success' },
  2: { label: 'Disabled', variant: 'danger' },
  3: { label: 'Expired', variant: 'warning' },
  4: { label: 'Exhausted', variant: 'warning' },
}

export function useAdminTokensColumns(): ColumnDef<AdminToken>[] {
  const { t } = useTranslation()
  return [
    {
      accessorKey: 'id',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('ID')} />
      ),
      cell: ({ row }) => (
        <span className='text-muted-foreground font-mono text-xs tabular-nums'>
          {row.getValue('id') as number}
        </span>
      ),
      meta: { label: t('ID') },
    },
    {
      accessorKey: 'username',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Username')} />
      ),
      cell: ({ row }) => (
        <div className='max-w-[160px] truncate text-sm'>
          {(row.getValue('username') as string) || '-'}
        </div>
      ),
      meta: { label: t('Username') },
    },
    {
      accessorKey: 'name',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Token Name')} />
      ),
      cell: ({ row }) => (
        <div className='max-w-[200px] truncate font-medium'>
          {row.getValue('name') as string}
        </div>
      ),
      meta: { label: t('Token Name'), mobileTitle: true },
    },
    {
      accessorKey: 'status',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Status')} />
      ),
      cell: ({ row }) => {
        const status = row.getValue('status') as number
        const cfg = STATUS_LABELS[status]
        if (!cfg) return null
        return (
          <StatusBadge
            label={t(cfg.label)}
            variant={cfg.variant}
            copyable={false}
          />
        )
      },
      meta: { label: t('Status'), mobileBadge: true },
    },
    {
      id: 'quota',
      accessorKey: 'remain_quota',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Quota')} />
      ),
      cell: ({ row }) => {
        const tk = row.original
        if (tk.unlimited_quota) {
          return (
            <StatusBadge
              label={t('Unlimited')}
              variant='neutral'
              copyable={false}
            />
          )
        }
        return (
          <span className='font-mono text-xs tabular-nums'>
            {formatQuota(tk.remain_quota)}
            <span className='text-muted-foreground'>
              {' / '}
              {formatQuota(tk.remain_quota + tk.used_quota)}
            </span>
          </span>
        )
      },
      meta: { label: t('Quota') },
    },
    {
      accessorKey: 'group',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Group')} />
      ),
      cell: ({ row }) => {
        const group = row.getValue('group') as string
        if (!group) return <span className='text-muted-foreground'>-</span>
        return <GroupBadge group={group} />
      },
      meta: { label: t('Group'), mobileHidden: true },
    },
    {
      accessorKey: 'created_time',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Created')} />
      ),
      cell: ({ row }) => (
        <span className='text-muted-foreground font-mono text-xs tabular-nums'>
          {formatTimestampToDate(row.getValue('created_time') as number)}
        </span>
      ),
      meta: { label: t('Created'), mobileHidden: true },
    },
    {
      accessorKey: 'expired_time',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Expires')} />
      ),
      cell: ({ row }) => {
        const expiredTime = row.getValue('expired_time') as number
        if (expiredTime === -1) {
          return (
            <StatusBadge
              label={t('Never')}
              variant='neutral'
              copyable={false}
            />
          )
        }
        const isExpired = expiredTime * 1000 < Date.now()
        return (
          <span
            className={cn(
              'font-mono text-xs tabular-nums',
              isExpired ? 'text-destructive' : 'text-muted-foreground'
            )}
          >
            {formatTimestampToDate(expiredTime)}
          </span>
        )
      },
      meta: { label: t('Expires'), mobileHidden: true },
    },
    {
      id: 'actions',
      cell: ({ row }) => <AdminTokensRowActions row={row} />,
      meta: { label: t('Actions') },
      size: 88,
    },
  ]
}
