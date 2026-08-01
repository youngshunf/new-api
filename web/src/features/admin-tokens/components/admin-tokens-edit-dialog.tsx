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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { updateAdminToken } from '../api'
import { useAdminTokens } from './admin-tokens-provider'

type FormState = {
  name: string
  remain_quota: number
  expired_time: number
  unlimited_quota: boolean
  group: string
  cross_group_retry: boolean
  model_limits_enabled: boolean
  model_limits: string
  allow_ips: string
  status: number
}

const initialFormState: FormState = {
  name: '',
  remain_quota: 0,
  expired_time: -1,
  unlimited_quota: false,
  group: '',
  cross_group_retry: false,
  model_limits_enabled: false,
  model_limits: '',
  allow_ips: '',
  status: 1,
}

function toLocalDateTimeInput(epochSec: number): string {
  if (epochSec <= 0) return ''
  const d = new Date(epochSec * 1000)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function fromLocalDateTimeInput(value: string): number {
  if (!value) return -1
  const ms = new Date(value).getTime()
  if (Number.isNaN(ms)) return -1
  return Math.floor(ms / 1000)
}

export function AdminTokensEditDialog() {
  const { t } = useTranslation()
  const { open, setOpen, currentRow, triggerRefresh } = useAdminTokens()
  const [form, setForm] = useState<FormState>(initialFormState)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (open !== 'edit' || !currentRow) return
    setForm({
      name: currentRow.name,
      remain_quota: currentRow.remain_quota,
      expired_time: currentRow.expired_time,
      unlimited_quota: currentRow.unlimited_quota,
      group: currentRow.group ?? '',
      cross_group_retry: currentRow.cross_group_retry,
      model_limits_enabled: currentRow.model_limits_enabled,
      model_limits: currentRow.model_limits ?? '',
      allow_ips: currentRow.allow_ips ?? '',
      status: currentRow.status,
    })
  }, [open, currentRow])

  const handleSubmit = async () => {
    if (!currentRow) return
    setSubmitting(true)
    try {
      const result = await updateAdminToken({
        id: currentRow.id,
        ...form,
      })
      if (result.success) {
        toast.success(t('Token updated'))
        setOpen(null)
        triggerRefresh()
      } else {
        toast.error(result.message || t('Failed to update token'))
      }
    } catch {
      toast.error(t('An unexpected error occurred'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open === 'edit'} onOpenChange={(o) => !o && setOpen(null)}>
      <DialogContent className='sm:max-w-lg'>
        <DialogHeader>
          <DialogTitle>{t('Edit Token')}</DialogTitle>
          <DialogDescription>
            {currentRow?.username
              ? `${t('Owner:')} ${currentRow.username}`
              : ''}
          </DialogDescription>
        </DialogHeader>

        <div className='space-y-4'>
          <div className='space-y-1.5'>
            <Label htmlFor='admin-token-name'>{t('Name')}</Label>
            <Input
              id='admin-token-name'
              value={form.name}
              onChange={(e) =>
                setForm((f) => ({ ...f, name: e.target.value }))
              }
            />
          </div>

          <div className='flex items-center justify-between gap-2'>
            <Label htmlFor='admin-token-unlimited'>
              {t('Unlimited Quota')}
            </Label>
            <Switch
              id='admin-token-unlimited'
              checked={form.unlimited_quota}
              onCheckedChange={(v) =>
                setForm((f) => ({ ...f, unlimited_quota: v }))
              }
            />
          </div>

          {!form.unlimited_quota && (
            <div className='space-y-1.5'>
              <Label htmlFor='admin-token-quota'>
                {t('Remaining Quota (raw units)')}
              </Label>
              <Input
                id='admin-token-quota'
                type='number'
                value={form.remain_quota}
                onChange={(e) =>
                  setForm((f) => ({
                    ...f,
                    remain_quota: Number(e.target.value),
                  }))
                }
              />
            </div>
          )}

          <div className='space-y-1.5'>
            <Label htmlFor='admin-token-expires'>
              {t('Expires At (leave empty = never)')}
            </Label>
            <Input
              id='admin-token-expires'
              type='datetime-local'
              value={toLocalDateTimeInput(form.expired_time)}
              onChange={(e) =>
                setForm((f) => ({
                  ...f,
                  expired_time: fromLocalDateTimeInput(e.target.value),
                }))
              }
            />
          </div>

          <div className='space-y-1.5'>
            <Label htmlFor='admin-token-group'>{t('Group')}</Label>
            <Input
              id='admin-token-group'
              value={form.group}
              onChange={(e) =>
                setForm((f) => ({ ...f, group: e.target.value }))
              }
              placeholder={t('Leave empty to use user default')}
            />
          </div>

          <div className='space-y-1.5'>
            <Label htmlFor='admin-token-allow-ips'>
              {t('Allowed IPs (newline separated, optional)')}
            </Label>
            <Input
              id='admin-token-allow-ips'
              value={form.allow_ips}
              onChange={(e) =>
                setForm((f) => ({ ...f, allow_ips: e.target.value }))
              }
            />
          </div>

          <div className='flex items-center justify-between gap-2'>
            <Label htmlFor='admin-token-status'>{t('Enabled')}</Label>
            <Switch
              id='admin-token-status'
              checked={form.status === 1}
              onCheckedChange={(v) =>
                setForm((f) => ({ ...f, status: v ? 1 : 2 }))
              }
            />
          </div>
        </div>

        <DialogFooter>
          <Button
            variant='outline'
            onClick={() => setOpen(null)}
            disabled={submitting}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={handleSubmit} disabled={submitting || !form.name}>
            {submitting ? t('Saving...') : t('Save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
