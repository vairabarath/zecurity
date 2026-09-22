import { useState } from "react";
import { useMutation } from "@apollo/client/react";
import { AlertTriangle, Layers, Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  CreateResourcePolicyDocument,
  GetResourcePoliciesDocument,
} from "@/generated/graphql";

interface CreateResourcePolicyModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSuccess?: () => void;
}

/**
 * A policy is created with no device profiles, which is a valid state meaning
 * "Any Device" — profiles are attached afterwards from the edit panel. The copy
 * says so, because an empty requirement list reads like an unfinished object
 * otherwise.
 */
export function CreateResourcePolicyModal({
  open,
  onOpenChange,
  onSuccess,
}: CreateResourcePolicyModalProps) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);

  const [createResourcePolicy, { loading }] = useMutation(
    CreateResourcePolicyDocument,
    {
      onCompleted: (data) => {
        toast.success(`Resource policy "${data.createResourcePolicy.name}" created`);
        onSuccess?.();
        handleClose(false);
      },
      onError: (err) => setError(err.message),
      refetchQueries: [{ query: GetResourcePoliciesDocument }],
    },
  );

  function handleClose(next: boolean) {
    if (!next) {
      setName("");
      setError(null);
    }
    onOpenChange(next);
  }

  function handleSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    createResourcePolicy({ variables: { name: name.trim() } });
  }

  const isValid = name.trim().length > 0;

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => handleClose(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <form onSubmit={handleSubmit} className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <Layers className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Create Resource Policy</h2>
              <p className="text-sm text-muted-foreground">
                The access requirement a resource applies to devices.
              </p>
            </div>
            <button
              type="button"
              onClick={() => handleClose(false)}
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 space-y-5 overflow-y-auto p-5">
            <div className="space-y-2">
              <Label className="text-sm font-semibold">
                Policy Name <span className="text-destructive">*</span>
              </Label>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Policy Name"
                autoFocus
              />
            </div>

            <div className="rounded-2xl border border-border p-4 text-sm text-muted-foreground">
              <p className="font-semibold text-foreground">
                Starts as “Any Device”
              </p>
              <p className="mt-1">
                A new policy has no device profiles, so it places no posture
                requirement on devices. Attach profiles afterwards to require
                them — a device satisfying any one of them meets the policy.
              </p>
            </div>

            {error && (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                <span>{error}</span>
              </div>
            )}
          </div>

          <div className="flex items-center justify-end gap-2 border-t border-border p-5">
            <Button
              type="button"
              variant="outline"
              onClick={() => handleClose(false)}
              disabled={loading}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!isValid || loading}>
              {loading && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Create Resource Policy
            </Button>
          </div>
        </form>
      </div>

      {/*
        These keyframes are defined here as well as in CreateDeviceProfileModal
        because each slide-over only animates while a component carrying the
        definition is mounted.
      */}
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
  );
}
