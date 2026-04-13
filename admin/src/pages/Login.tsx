import { useState, useEffect } from "react"
import { useLazyQuery, useMutation } from "@apollo/client/react"
import { motion, AnimatePresence } from "framer-motion"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { ShimmerButton } from "@/components/ui/shimmer-button"
import { LookupWorkspaceDocument, LookupWorkspacesByEmailDocument, InitiateAuthDocument } from "@/generated/graphql"
import type { LookupWorkspacesByEmailQuery } from "@/generated/graphql"
import {
  Shield,
  Fingerprint,
  Globe,
  Mail,
  Terminal,
  CheckCircle2,
  Building2,
  ArrowRight,
} from "lucide-react"

function BackgroundPattern() {
  return (
    <div className="absolute inset-0 overflow-hidden pointer-events-none">
      <div 
        className="absolute inset-0 opacity-[0.015]"
        style={{
          backgroundImage: `
            linear-gradient(oklch(0.55 0.18 250) 1px, transparent 1px),
            linear-gradient(90deg, oklch(0.55 0.18 250) 1px, transparent 1px)
          `,
          backgroundSize: '40px 40px',
        }}
      />
      <div 
        className="absolute -top-[30%] -left-[10%] w-[60%] h-[60%] rounded-full blur-[120px]"
        style={{ background: 'radial-gradient(circle, oklch(0.55 0.18 250 / 0.08) 0%, transparent 70%)' }}
      />
      <div 
        className="absolute -bottom-[20%] -right-[10%] w-[50%] h-[50%] rounded-full blur-[100px]"
        style={{ background: 'radial-gradient(circle, oklch(0.55 0.18 250 / 0.06) 0%, transparent 70%)' }}
      />
    </div>
  )
}

function StatusBar() {
  const [time, setTime] = useState(new Date())

  useEffect(() => {
    const t = setInterval(() => setTime(new Date()), 1000)
    return () => clearInterval(t)
  }, [])

  return (
    <div className="fixed top-0 left-0 right-0 z-50 flex items-center justify-between px-6 py-3 bg-white/80 backdrop-blur-md border-b border-border/50">
      <div className="flex items-center gap-2.5">
        <div className="relative flex items-center justify-center h-2 w-2">
          <div className="absolute h-3.5 w-3.5 rounded-full bg-secure/20 animate-pulse" />
          <div className="absolute h-2 w-2 rounded-full bg-secure" />
        </div>
        <span className="font-mono text-[11px] tracking-[0.2em] text-muted-foreground uppercase">
          System Secure
        </span>
      </div>
      <div className="flex items-center gap-3">
        <Globe className="h-3 w-3 text-muted-foreground/70" />
        <span className="font-mono text-[11px] text-muted-foreground/70 tabular-nums tracking-wider">
          {time.toLocaleTimeString("en-US", { hour12: false })}
        </span>
      </div>
    </div>
  )
}

type AuthMode = "endpoint" | "email"

export default function Login() {
  const [mode, setMode] = useState<AuthMode>("endpoint")
  const [slug, setSlug] = useState("")
  const [email, setEmail] = useState("")
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [foundWorkspaces, setFoundWorkspaces] = useState<
    LookupWorkspacesByEmailQuery["lookupWorkspacesByEmail"]["workspaces"]
  >([])
  const [showWorkspaces, setShowWorkspaces] = useState(false)

  const [lookupWorkspace] = useLazyQuery(LookupWorkspaceDocument)
  const [lookupByEmail] = useLazyQuery(LookupWorkspacesByEmailDocument)
  const [initiateAuth] = useMutation(InitiateAuthDocument)

  function handleSlugChange(val: string) {
    setSlug(val.toLowerCase().replace(/[^a-z0-9-]/g, ""))
    setError(null)
  }

  async function handleEndpointSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!slug.trim()) return
    setLoading(true)
    setError(null)
    try {
      // Step 1: Verify the workspace exists
      const result = await lookupWorkspace({ variables: { slug } })
      const ws = result.data?.lookupWorkspace
      if (!ws?.found || !ws.workspace) {
        setError("Network not found — verify endpoint and retry")
        return
      }

      // Step 2: Store slug for post-auth use
      sessionStorage.setItem("ztna_workspace_slug", slug)

      // Step 3: Trigger OAuth flow with the workspace name
      const authResult = await initiateAuth({
        variables: { provider: "google", workspaceName: ws.workspace.name },
      })
      const { redirectUrl, state } = authResult.data!.initiateAuth
      sessionStorage.setItem("ztna_oauth_state", state)

      // Step 4: Redirect to Google OAuth
      window.location.href = redirectUrl
    } catch {
      setError("Connection failed")
    } finally {
      setLoading(false)
    }
  }

  async function handleEmailSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!email.trim() || !email.includes("@")) return
    setLoading(true)
    setError(null)
    setShowWorkspaces(false)
    try {
      const result = await lookupByEmail({ variables: { email: email.trim() } })
      const ws = result.data?.lookupWorkspacesByEmail.workspaces ?? []
      if (ws.length === 0) {
        setError("No workspaces found for this email")
        return
      }
      // Always show workspace list — user must click to proceed
      setFoundWorkspaces(ws)
      setShowWorkspaces(true)
    } catch {
      setError("Lookup failed — check your connection")
    } finally {
      setLoading(false)
    }
  }

  async function startOAuth(workspaceName: string, workspaceSlug: string) {
    setLoading(true)
    setError(null)
    try {
      sessionStorage.setItem("ztna_workspace_slug", workspaceSlug)
      const authResult = await initiateAuth({
        variables: { provider: "google", workspaceName },
      })
      const { redirectUrl, state } = authResult.data!.initiateAuth
      sessionStorage.setItem("ztna_oauth_state", state)
      window.location.href = redirectUrl
    } catch {
      setError("Authentication failed")
      setLoading(false)
    }
  }

  const fadeUp = {
    initial: { opacity: 0, y: 16 },
    animate: { opacity: 1, y: 0 },
  }

  return (
    <div className="relative min-h-screen flex items-center justify-center overflow-hidden bg-background">
      <BackgroundPattern />
      <StatusBar />

      <motion.div
        className="relative z-10 w-full max-w-md mx-4"
        initial={{ opacity: 0, y: 24 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.6, ease: [0.22, 1, 0.36, 1] }}
      >
        <div 
          className="relative rounded-2xl border border-border bg-white shadow-[0_2px_40px_rgba(0,0,0,0.06),0_0_0_1px_rgba(0,0,0,0.04)] overflow-hidden"
        >
          <div 
            className="absolute top-0 left-0 right-0 h-px"
            style={{
              background: 'linear-gradient(90deg, transparent 5%, oklch(0.55 0.18 250 / 0.3) 30%, oklch(0.55 0.18 250 / 0.5) 50%, oklch(0.55 0.18 250 / 0.3) 70%, transparent 95%)',
            }}
          />

          <div className="relative p-8">
            <motion.div
              className="flex justify-center mb-6"
              {...fadeUp}
              transition={{ delay: 0.1, duration: 0.5 }}
            >
              <div className="relative">
                <motion.div
                  className="absolute inset-0 rounded-full"
                  animate={{ 
                    boxShadow: ['0 0 0 0 rgba(99, 102, 241, 0)', '0 0 0 12px rgba(99, 102, 241, 0.1)', '0 0 0 0 rgba(99, 102, 241, 0)']
                  }}
                  transition={{ duration: 2, repeat: Infinity }}
                />
                <div className="relative flex items-center justify-center w-14 h-14 rounded-2xl bg-primary/10 border border-primary/20">
                  <Shield className="h-7 w-7 text-primary" strokeWidth={1.5} />
                </div>
                <motion.div
                  className="absolute -top-1 -right-1"
                  initial={{ scale: 0 }}
                  animate={{ scale: 1 }}
                  transition={{ delay: 0.3, type: "spring" }}
                >
                  <CheckCircle2 className="h-4 w-4 text-secure" />
                </motion.div>
              </div>
            </motion.div>

            <motion.div
              className="text-center mb-7"
              {...fadeUp}
              transition={{ delay: 0.15, duration: 0.5 }}
            >
              <h1 className="font-display text-2xl font-semibold tracking-tight text-foreground">
                ZECURITY
              </h1>
              <p className="mt-1.5 text-sm text-muted-foreground">
                Zero Trust Network Access
              </p>
            </motion.div>

            <motion.div
              className="flex mb-6 rounded-xl bg-muted p-1"
              {...fadeUp}
              transition={{ delay: 0.2, duration: 0.5 }}
            >
              <button
                type="button"
                onClick={() => {
                  setMode("endpoint")
                  setError(null)
                }}
                className={`flex-1 flex items-center justify-center gap-2 rounded-lg px-3 py-2.5 text-sm font-medium transition-all duration-200 ${
                  mode === "endpoint"
                    ? "bg-white shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Shield className="h-4 w-4" />
                Endpoint
              </button>
              <button
                type="button"
                onClick={() => {
                  setMode("email")
                  setError(null)
                }}
                className={`flex-1 flex items-center justify-center gap-2 rounded-lg px-3 py-2.5 text-sm font-medium transition-all duration-200 ${
                  mode === "email"
                    ? "bg-white shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Mail className="h-4 w-4" />
                Identity
              </button>
            </motion.div>

            <motion.div
              {...fadeUp}
              transition={{ delay: 0.25, duration: 0.5 }}
            >
              <AnimatePresence mode="wait">
                {mode === "endpoint" ? (
                  <motion.form
                    key="endpoint"
                    onSubmit={handleEndpointSubmit}
                    initial={{ opacity: 0, x: -20 }}
                    animate={{ opacity: 1, x: 0 }}
                    exit={{ opacity: 0, x: 20 }}
                    transition={{ duration: 0.25 }}
                  >
                    <div className="space-y-5">
                      <div className="space-y-2">
                        <Label className="text-xs font-medium text-muted-foreground">
                          Network Endpoint
                        </Label>
                        <div className="relative flex items-center group">
                          <Input
                            value={slug}
                            onChange={(e) => handleSlugChange(e.target.value)}
                            placeholder="your-network"
                            className="pr-20 bg-muted/50 border-border h-11 transition-all focus-visible:ring-primary/30"
                            autoFocus
                          />
                          <div className="absolute right-0 top-0 bottom-0 flex items-center pr-3 pointer-events-none">
                            <span className="text-xs text-muted-foreground">.zecurity.in</span>
                          </div>
                        </div>
                      </div>

                      <ShimmerButton
                        type="submit"
                        disabled={loading || !slug.trim()}
                        className="w-full h-11 text-sm"
                      >
                        {loading ? (
                          <span className="flex items-center gap-2">
                            <span className="h-4 w-4 animate-spin rounded-full border-2 border-primary border-t-transparent" />
                            Authenticating...
                          </span>
                        ) : (
                          <>
                            <Fingerprint className="h-4 w-4" />
                            Authenticate
                          </>
                        )}
                      </ShimmerButton>
                    </div>
                  </motion.form>
                ) : (
                  <motion.form
                    key="email"
                    onSubmit={handleEmailSubmit}
                    initial={{ opacity: 0, x: 20 }}
                    animate={{ opacity: 1, x: 0 }}
                    exit={{ opacity: 0, x: -20 }}
                    transition={{ duration: 0.25 }}
                  >
                    <div className="space-y-5">
                      {!showWorkspaces ? (
                        <>
                          <div className="space-y-2">
                            <Label className="text-xs font-medium text-muted-foreground">
                              Identity Lookup
                            </Label>
                            <Input
                              type="email"
                              value={email}
                              onChange={(e) => {
                                setEmail(e.target.value)
                                setError(null)
                              }}
                              placeholder="you@company.com"
                              className="bg-muted/50 border-border h-11 transition-all focus-visible:ring-primary/30"
                              autoFocus
                            />
                          </div>
                          <ShimmerButton
                            type="submit"
                            disabled={loading || !email.trim() || !email.includes("@")}
                            className="w-full h-11 text-sm"
                          >
                            {loading ? (
                              <span className="flex items-center gap-2">
                                <span className="h-4 w-4 animate-spin rounded-full border-2 border-primary border-t-transparent" />
                                Looking up...
                              </span>
                            ) : (
                              <>
                                <Mail className="h-4 w-4" />
                                Find my workspaces
                              </>
                            )}
                          </ShimmerButton>
                        </>
                      ) : (
                        <div className="space-y-3">
                          <Label className="text-xs font-medium text-muted-foreground">
                            Select workspace for {email}
                          </Label>
                          <div className="space-y-2">
                            {foundWorkspaces.map((ws) => (
                              <button
                                key={ws.id}
                                type="button"
                                onClick={() => startOAuth(ws.name, ws.slug)}
                                disabled={loading}
                                className="w-full flex items-center gap-3 rounded-xl border border-border bg-muted/30 px-4 py-3 text-left transition-all hover:border-primary/30 hover:bg-primary/5 disabled:opacity-50"
                              >
                                <div className="flex items-center justify-center h-9 w-9 rounded-lg bg-primary/10 border border-primary/20 shrink-0">
                                  <Building2 className="h-4 w-4 text-primary" />
                                </div>
                                <div className="flex-1 min-w-0">
                                  <div className="text-sm font-medium truncate">{ws.name}</div>
                                  <div className="text-xs text-muted-foreground font-mono">{ws.slug}.zecurity.in</div>
                                </div>
                                <ArrowRight className="h-4 w-4 text-muted-foreground shrink-0" />
                              </button>
                            ))}
                          </div>
                          <button
                            type="button"
                            onClick={() => { setShowWorkspaces(false); setFoundWorkspaces([]) }}
                            className="text-xs text-muted-foreground hover:text-foreground transition-colors"
                          >
                            ← Try a different email
                          </button>
                        </div>
                      )}
                    </div>
                  </motion.form>
                )}
              </AnimatePresence>
            </motion.div>

            <AnimatePresence>
              {error && (
                <motion.div
                  className="mt-5 flex items-start gap-2.5 rounded-xl border border-destructive/20 bg-destructive/5 p-4"
                  initial={{ opacity: 0, height: 0 }}
                  animate={{ opacity: 1, height: "auto" }}
                  exit={{ opacity: 0, height: 0 }}
                >
                  <Terminal className="h-4 w-4 text-destructive mt-0.5 shrink-0" />
                  <span className="text-sm text-destructive">{error}</span>
                </motion.div>
              )}
            </AnimatePresence>

            <motion.div
              className="mt-7 pt-5 border-t border-border relative"
              {...fadeUp}
              transition={{ delay: 0.35, duration: 0.5 }}
            >
              <div
                className="absolute top-0 left-1/4 right-1/4 h-px"
                style={{
                  background: 'linear-gradient(90deg, transparent, oklch(0.55 0.18 250 / 0.1), transparent)',
                }}
              />
              <p className="text-center text-sm text-muted-foreground">
                Don't have a network?{" "}
                <a
                  href="/signup"
                  className="text-primary hover:text-primary/80 font-medium transition-colors"
                >
                  Deploy one
                </a>
              </p>
            </motion.div>
          </div>
        </div>
      </motion.div>
    </div>
  )
}