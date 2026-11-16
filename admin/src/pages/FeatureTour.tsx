import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowRight, ChevronDown, Compass, Info, PlayCircle, RotateCcw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { toast } from 'sonner'
import { featureTours, resetPageTours } from '@/lib/guidedTours'
import { useGuidedTour } from '@/store/guidedTour'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { tourFeatures, type TourFeature } from '@/lib/featureTour'
import { cn } from '@/lib/utils'

// Feature Tour: one card per console feature, with how to use it and where to
// find it. Content lives in lib/featureTour.ts.

function renderStep(text: string) {
  // `code` spans for commands.
  return text.split(/(`[^`]+`)/g).map((part, i) =>
    part.startsWith('`') && part.endsWith('`') ? (
      <code key={i} className="rounded bg-secondary px-1 py-0.5 font-mono text-[12px]">
        {part.slice(1, -1)}
      </code>
    ) : (
      <span key={i}>{part}</span>
    ),
  )
}

function FeatureCard({ feature }: { feature: TourFeature }) {
  const [open, setOpen] = useState(true)
  const startTour = useGuidedTour((s) => s.start)
  const Icon = feature.icon
  const guided = feature.guided ? featureTours[feature.guided] : undefined
  const bodyId = `tour-${feature.id}`
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          className="flex w-full items-start gap-3 text-left"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          aria-controls={bodyId}
        >
          <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[10px] bg-primary/15 text-primary">
            <Icon className="h-5 w-5" />
          </div>
          <div className="min-w-0 flex-1 space-y-1">
            <CardTitle className="text-base">{feature.title}</CardTitle>
            <p className="text-sm text-muted-foreground">{feature.summary}</p>
          </div>
          <ChevronDown
            className={cn('mt-1 h-4 w-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')}
          />
        </button>
      </CardHeader>
      {open && (
        <CardContent id={bodyId} className="space-y-5 pt-5 text-sm">
          {guided && (
            <Button size="sm" className="gap-1.5" onClick={() => startTour(guided)}>
              <PlayCircle className="h-4 w-4" />
              Take the guided tour
            </Button>
          )}
          <ol className="grid gap-4 md:grid-cols-2">
            {feature.sections.map((section, i) => (
              <li key={section.title} className="rounded-xl border border-border bg-secondary/40 p-4">
                <div className="mb-2 flex items-center gap-2 font-medium">
                  <span className="flex h-5 w-5 items-center justify-center rounded-full bg-primary/20 text-[11px] font-semibold text-primary">
                    {i + 1}
                  </span>
                  {section.title}
                </div>
                <ul className="list-disc space-y-1.5 pl-5 text-muted-foreground">
                  {section.steps.map((step) => (
                    <li key={step}>{renderStep(step)}</li>
                  ))}
                </ul>
                {section.link && (
                  <Link
                    to={section.link.to}
                    className="mt-3 inline-flex items-center gap-1 text-[13px] font-medium text-primary hover:underline"
                  >
                    {section.link.label}
                    <ArrowRight className="h-3.5 w-3.5" />
                  </Link>
                )}
              </li>
            ))}
          </ol>
          {feature.notes && feature.notes.length > 0 && (
            <div className="rounded-xl border border-border p-4">
              <div className="mb-2 flex items-center gap-2 font-medium">
                <Info className="h-4 w-4 text-muted-foreground" />
                Good to know
              </div>
              <ul className="list-disc space-y-1.5 pl-5 text-muted-foreground">
                {feature.notes.map((note) => (
                  <li key={note}>{note}</li>
                ))}
              </ul>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  )
}

export default function FeatureTour() {
  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title flex items-center gap-2">
            <Compass className="h-5 w-5" />
            Feature Tour
          </h2>
          <p className="page-subtitle">What this console can do, how to use each feature, and where to find it.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5"
            onClick={() => {
              resetPageTours()
              toast.success('Page tours reset. Each page shows its tour again on your next visit.')
            }}
          >
            <RotateCcw className="h-3.5 w-3.5" />
            Show page tours again
          </Button>
          <span className="metric-chip">
            <strong>{tourFeatures.length}</strong> {tourFeatures.length === 1 ? 'feature' : 'features'}
          </span>
        </div>
      </div>
      <div className="space-y-4">
        {tourFeatures.map((feature) => (
          <FeatureCard key={feature.id} feature={feature} />
        ))}
      </div>
    </div>
  )
}
