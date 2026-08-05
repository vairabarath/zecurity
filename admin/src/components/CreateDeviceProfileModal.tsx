import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { CreateDeviceProfileDocument, GetDeviceProfilesDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertTriangle, Loader2, X, ShieldCheck } from 'lucide-react'
import { toast } from 'sonner'

interface CreateDeviceProfileModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSuccess?: () => void
}

export function CreateDeviceProfileModal({
  open,
  onOpenChange,
  onSuccess,
}: CreateDeviceProfileModalProps) {
  const [name, setName] = useState('')
  const [error, setError] = useState<string | null>(null)

  const [createDeviceProfile, { loading: creating }] = useMutation(CreateDeviceProfileDocument, {
    onCompleted: (data) => {
      toast.success(`Device profile "${data.createDeviceProfile.name}" created`)
      setName('')
      onSuccess?.()
      handleClose(false)
    },
    onError: (err) => setError(err.message),
    refetchQueries: [{ query: GetDeviceProfilesDocument }],
  })

  const handleClose = (isOpen: boolean) => {
    if (!isOpen) {
      setName('')
      setError(null)
    }
    onOpenChange(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim()) {
      setError('Name is required')
      return
    }

    await createDeviceProfile({ variables: { name: name.trim() } })
  }

  const isValid = name.trim().length > 0

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => handleClose(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <ShieldCheck className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Create Device Profile</h2>
              <p className="text-sm text-muted-foreground">
                Define a posture profile. Add requirements and bind resources from its detail page.
              </p>
            </div>
            <button
              onClick={() => handleClose(false)}
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto p-5">
            <form onSubmit={handleSubmit} className="space-y-5">
              <div className="space-y-2">
                <Label className="text-sm font-semibold">
                  Name <span className="text-destructive">*</span>
                </Label>
                <Input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. Corporate Laptops"
                  className="h-11 font-medium"
                  autoFocus
                />
              </div>

              {error && (
                <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                  <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                  <span>{error}</span>
                </div>
              )}
            </form>
          </div>

          <div className="flex items-center justify-between gap-3 border-t border-border p-5">
            <Button
              type="button"
              variant="outline"
              onClick={() => handleClose(false)}
              className="h-11 flex-1"
            >
              Cancel
            </Button>
            <Button
              onClick={handleSubmit}
              disabled={!isValid || creating}
              className="h-11 flex-1 gap-2"
            >
              {creating ? (
                <span className="flex items-center gap-2">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  Creating...
                </span>
              ) : (
                <span className="flex items-center gap-2">
                  <ShieldCheck className="h-4 w-4" />
                  Create Device Profile
                </span>
              )}
            </Button>
          </div>
        </div>
      </div>

      <style>{`
        @keyframes slide-in {
          from {
            transform: translateX(100%);
          }
          to {
            transform: translateX(0);
          }
        }
        .animate-slide-in {
          animation: slide-in 0.3s ease-out;
        }
      `}</style>
    </div>
  )
}
