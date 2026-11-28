import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { ShieldAlert } from 'lucide-react'
import { ApiError } from '@/api/client'
import { acknowledgeBreakGlass, listBreakGlass, type BreakGlassEvent } from '@/api/breakGlass'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { formatDateTime } from '@/lib/format'
import { reasonValid } from '@/lib/pool-format'

// The break-glass banner (Sprint 27 O2 with G1; D-75): super-admins see every
// recover-admin run until one of them acknowledges it with a note. Shown on
// every page (Layout); the alert and the email come from the controller.

export function BreakGlassBanner() {
  const [events, setEvents] = useState<BreakGlassEvent[]>([])
  const [acking, setAcking] = useState<BreakGlassEvent | null>(null)

  const load = useCallback(async () => {
    try {
      setEvents((await listBreakGlass()).items)
    } catch (err) {
      // A read error keeps what was shown; a session end is handled by the guards.
      if (err instanceof ApiError && err.sessionEnded) return
    }
  }, [])

  useEffect(() => {
    // Initial load; load() only sets state once the request settles.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void load()
  }, [load])

  if (events.length === 0) return null
  return (
    <div className="mb-5 flex flex-col gap-2" role="region" aria-label="Break-glass recoveries">
      {events.map((e) => (
        <Alert key={e.id} variant="destructive" data-break-glass={e.id}>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span className="flex items-start gap-2">
              <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
              <span>
                Break-glass recovery used for <strong>{e.email}</strong> at {formatDateTime(e.created_at)}: {e.reason}
                <span className="block text-xs opacity-80">On {e.host}. Every active super-admin was alerted and emailed.</span>
              </span>
            </span>
            <Button variant="outline" size="sm" onClick={() => setAcking(e)} aria-label={`Acknowledge break-glass for ${e.email}`}>
              Acknowledge
            </Button>
          </AlertDescription>
        </Alert>
      ))}
      {acking && (
        <AckDialog
          event={acking}
          onCancel={() => setAcking(null)}
          onDone={() => {
            setAcking(null)
            void load()
          }}
        />
      )}
    </div>
  )
}

function AckDialog({ event, onCancel, onDone }: { event: BreakGlassEvent; onCancel: () => void; onDone: () => void }) {
  const [note, setNote] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!reasonValid(note) || pending) return
    setPending(true)
    setError(null)
    try {
      await acknowledgeBreakGlass(event.id, note.trim())
      onDone()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'already_acknowledged') {
        onDone() // another super-admin was first
        return
      }
      setError(err instanceof ApiError && err.status === 403 ? 'Only super-admins can acknowledge break-glass recoveries.' : 'Couldn’t acknowledge it. Try again.')
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Acknowledge break-glass recovery</DialogTitle>
            <DialogDescription>
              Confirms that the recovery of <strong className="text-foreground">{event.email}</strong> was checked. The banner goes away for every
              super-admin and the alert resolves. Your note is recorded in the provider audit log.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <ReasonField id="break-glass-note" value={note} onChange={setNote} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!reasonValid(note) || pending}>
              {pending ? 'Saving…' : 'Acknowledge'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
