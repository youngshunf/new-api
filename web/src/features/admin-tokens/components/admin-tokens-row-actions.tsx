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
import { type Row } from '@tanstack/react-table'
import {
  Edit,
  Eye,
  MoreHorizontal as DotsHorizontalIcon,
  Trash2,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { adminTokenSchema, type AdminToken } from '../types'
import { useAdminTokens } from './admin-tokens-provider'

type AdminTokensRowActionsProps = {
  row: Row<AdminToken>
}

export function AdminTokensRowActions({ row }: AdminTokensRowActionsProps) {
  const { t } = useTranslation()
  const token = adminTokenSchema.parse(row.original)
  const { setOpen, setCurrentRow } = useAdminTokens()

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant='ghost'
            className='size-8 p-0'
            aria-label={t('Open menu')}
          />
        }
      >
        <DotsHorizontalIcon className='size-4' />
      </DropdownMenuTrigger>
      <DropdownMenuContent align='end' className='w-44'>
        <DropdownMenuItem
          onClick={() => {
            setCurrentRow(token)
            setOpen('view-key')
          }}
        >
          <Eye className='mr-2 size-4' />
          {t('View Key')}
        </DropdownMenuItem>
        <DropdownMenuItem
          onClick={() => {
            setCurrentRow(token)
            setOpen('edit')
          }}
        >
          <Edit className='mr-2 size-4' />
          {t('Edit')}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          variant='destructive'
          onClick={() => {
            setCurrentRow(token)
            setOpen('delete')
          }}
        >
          <Trash2 className='mr-2 size-4' />
          {t('Delete')}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
