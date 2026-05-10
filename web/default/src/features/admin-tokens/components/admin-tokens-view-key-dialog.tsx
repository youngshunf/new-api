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
import { Copy } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { copyToClipboard } from '@/lib/copy-to-clipboard'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { fetchAdminTokenKey } from '../api'
import { useAdminTokens } from './admin-tokens-provider'

export function AdminTokensViewKeyDialog() {
  const { t } = useTranslation()
  const { open, setOpen, currentRow } = useAdminTokens()
  const [fullKey, setFullKey] = useState('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (open !== 'view-key' || !currentRow) {
      setFullKey('')
      return
    }
    let cancelled = false
    setLoading(true)
    fetchAdminTokenKey(currentRow.id)
      .then((res) => {
        if (cancelled) return
        if (res.success && res.data?.key) {
          setFullKey(`sk-${res.data.key}`)
        } else {
          toast.error(res.message || t('Failed to fetch token key'))
          setOpen(null)
        }
      })
      .catch(() => {
        if (cancelled) return
        toast.error(t('An unexpected error occurred'))
        setOpen(null)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [open, currentRow, setOpen, t])

  const handleCopy = async () => {
    if (!fullKey) return
    const ok = await copyToClipboard(fullKey)
    if (ok) toast.success(t('Copied to clipboard'))
  }

  return (
    <Dialog
      open={open === 'view-key'}
      onOpenChange={(o) => !o && setOpen(null)}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t('Token Key')}</DialogTitle>
          <DialogDescription>
            {currentRow?.username
              ? `${t('Owner:')} ${currentRow.username} — `
              : ''}
            {t(
              'Treat this key as a password — anyone with it can call the API as the user.'
            )}
          </DialogDescription>
        </DialogHeader>
        <div className='bg-muted text-foreground rounded-md p-3 font-mono text-xs break-all'>
          {loading ? t('Loading...') : fullKey || '—'}
        </div>
        <DialogFooter>
          <Button variant='outline' onClick={() => setOpen(null)}>
            {t('Close')}
          </Button>
          <Button onClick={handleCopy} disabled={!fullKey}>
            <Copy className='mr-2 size-4' />
            {t('Copy')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
