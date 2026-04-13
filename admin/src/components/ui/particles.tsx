"use client"

import { useEffect, useRef, useState, useCallback } from "react"
import { cn } from "@/lib/utils"

interface ParticlesProps {
  className?: string
  quantity?: number
  color?: string
  size?: number
  speed?: number
  connectionDistance?: number
  vx?: number
  vy?: number
}

interface Particle {
  x: number
  y: number
  vx: number
  vy: number
  size: number
  opacity: number
  targetOpacity: number
}

function Particles({
  className,
  quantity = 60,
  color = "oklch(0.68 0.19 250)",
  size = 1.5,
  speed = 0.3,
  connectionDistance = 120,
}: ParticlesProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const particlesRef = useRef<Particle[]>([])
  const animationRef = useRef<number>(0)
  const [, setDimensions] = useState({ w: 0, h: 0 })

  const initParticles = useCallback(
    (w: number, h: number) => {
      const particles: Particle[] = []
      for (let i = 0; i < quantity; i++) {
        particles.push({
          x: Math.random() * w,
          y: Math.random() * h,
          vx: (Math.random() - 0.5) * speed,
          vy: (Math.random() - 0.5) * speed,
          size: Math.random() * size + 0.5,
          opacity: 0,
          targetOpacity: Math.random() * 0.6 + 0.2,
        })
      }
      return particles
    },
    [quantity, speed, size]
  )

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return

    const ctx = canvas.getContext("2d")
    if (!ctx) return

    const resizeObserver = new ResizeObserver((entries) => {
      const entry = entries[0]
      if (entry) {
        const { width, height } = entry.contentRect
        canvas.width = width
        canvas.height = height
        setDimensions({ w: width, h: height })
        particlesRef.current = initParticles(width, height)
      }
    })

    resizeObserver.observe(canvas.parentElement || canvas)

    // Parse oklch color to rgb for canvas
    const tempEl = document.createElement("div")
    tempEl.style.color = color
    document.body.appendChild(tempEl)
    const computed = getComputedStyle(tempEl).color
    document.body.removeChild(tempEl)

    // Extract rgb values
    const rgbMatch = computed.match(/(\d+)/g)
    const r = rgbMatch?.[0] ?? "136"
    const g = rgbMatch?.[1] ?? "170"
    const b = rgbMatch?.[2] ?? "255"

    const animate = () => {
      if (!ctx || !canvas) return
      ctx.clearRect(0, 0, canvas.width, canvas.height)

      const particles = particlesRef.current

      // Update and draw particles
      for (let i = 0; i < particles.length; i++) {
        const p = particles[i]

        // Fade in
        p.opacity += (p.targetOpacity - p.opacity) * 0.02

        // Move
        p.x += p.vx
        p.y += p.vy

        // Wrap around
        if (p.x < 0) p.x = canvas.width
        if (p.x > canvas.width) p.x = 0
        if (p.y < 0) p.y = canvas.height
        if (p.y > canvas.height) p.y = 0

        // Draw particle
        ctx.beginPath()
        ctx.arc(p.x, p.y, p.size, 0, Math.PI * 2)
        ctx.fillStyle = `rgba(${r}, ${g}, ${b}, ${p.opacity})`
        ctx.fill()

        // Draw connections
        for (let j = i + 1; j < particles.length; j++) {
          const p2 = particles[j]
          const dx = p.x - p2.x
          const dy = p.y - p2.y
          const dist = Math.sqrt(dx * dx + dy * dy)
          if (dist < connectionDistance) {
            const lineOpacity =
              (1 - dist / connectionDistance) *
              0.15 *
              Math.min(p.opacity, p2.opacity)
            ctx.beginPath()
            ctx.moveTo(p.x, p.y)
            ctx.lineTo(p2.x, p2.y)
            ctx.strokeStyle = `rgba(${r}, ${g}, ${b}, ${lineOpacity})`
            ctx.lineWidth = 0.5
            ctx.stroke()
          }
        }
      }

      animationRef.current = requestAnimationFrame(animate)
    }

    animate()

    return () => {
      resizeObserver.disconnect()
      cancelAnimationFrame(animationRef.current)
    }
  }, [color, connectionDistance, initParticles])

  return (
    <canvas
      ref={canvasRef}
      className={cn("pointer-events-none absolute inset-0", className)}
      style={{ width: "100%", height: "100%" }}
    />
  )
}

export { Particles }
