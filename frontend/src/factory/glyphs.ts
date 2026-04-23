// Procedural glyphs drawn with Pixi Graphics — one per GlyphKind. Each draws
// centered at (0,0); callers position the container. Kept procedural (no SVG,
// no raster) so they scale at any zoom and inherit the category color as a
// uniform.

import { Container, Graphics } from 'pixi.js'
import type { GlyphKind } from './events'

export function drawGlyph(parent: Container, kind: GlyphKind, color: number): Graphics {
  const g = new Graphics()
  switch (kind) {
    case 'spark':
      drawSpark(g, color)
      break
    case 'unlock':
      drawUnlock(g, color)
      break
    case 'pulse':
      drawPulse(g, color)
      break
    case 'check':
      drawCheck(g, color)
      break
    case 'cross':
      drawCross(g, color)
      break
    case 'tag':
      drawTag(g, color)
      break
    case 'bubble':
      drawBubble(g, color)
      break
    case 'merge':
      drawMerge(g, color)
      break
    case 'close':
      drawClose(g, color)
      break
  }
  parent.addChild(g)
  return g
}

function drawSpark(g: Graphics, color: number) {
  // 4 long cardinal rays + 4 short diagonal rays + center dot.
  for (const a of [0, 90, 180, 270]) {
    const r = (a * Math.PI) / 180
    g.moveTo(Math.cos(r) * 6, Math.sin(r) * 6)
    g.lineTo(Math.cos(r) * 16, Math.sin(r) * 16)
  }
  for (const a of [45, 135, 225, 315]) {
    const r = (a * Math.PI) / 180
    g.moveTo(Math.cos(r) * 5, Math.sin(r) * 5)
    g.lineTo(Math.cos(r) * 10, Math.sin(r) * 10)
  }
  g.stroke({ width: 1.5, color, alpha: 0.85 })
  g.circle(0, 0, 2.5)
  g.fill({ color, alpha: 0.95 })
}

function drawUnlock(g: Graphics, color: number) {
  // Two vertical "gate posts" with a rightward chevron passing through.
  g.moveTo(-14, -11)
  g.lineTo(-14, 11)
  g.moveTo(14, -11)
  g.lineTo(14, 11)
  g.stroke({ width: 1.25, color, alpha: 0.5 })

  g.moveTo(-5, -6)
  g.lineTo(4, 0)
  g.lineTo(-5, 6)
  g.stroke({ width: 1.8, color, alpha: 0.95 })
}

function drawPulse(g: Graphics, color: number) {
  // Waveform-like three chevrons advancing right.
  for (let i = -1; i <= 1; i++) {
    const x = i * 8
    g.moveTo(x - 3, -4)
    g.lineTo(x + 1, 0)
    g.lineTo(x - 3, 4)
  }
  g.stroke({ width: 1.5, color, alpha: 0.85 })
}

function drawCheck(g: Graphics, color: number) {
  g.moveTo(-8, 0)
  g.lineTo(-2, 6)
  g.lineTo(10, -6)
  g.stroke({ width: 2, color, alpha: 0.95 })
}

function drawCross(g: Graphics, color: number) {
  g.moveTo(-8, -8)
  g.lineTo(8, 8)
  g.moveTo(8, -8)
  g.lineTo(-8, 8)
  g.stroke({ width: 2, color, alpha: 0.95 })
}

function drawTag(g: Graphics, color: number) {
  // Diamond label with a hole, like a luggage tag.
  g.moveTo(-12, -2)
  g.lineTo(-4, -10)
  g.lineTo(12, -10)
  g.lineTo(12, 10)
  g.lineTo(-4, 10)
  g.closePath()
  g.stroke({ width: 1.4, color, alpha: 0.9 })
  g.circle(-2, 0, 1.8)
  g.fill({ color, alpha: 0.85 })
}

function drawBubble(g: Graphics, color: number) {
  g.roundRect(-12, -8, 24, 14, 6)
  g.stroke({ width: 1.4, color, alpha: 0.9 })
  g.moveTo(-6, 6)
  g.lineTo(-8, 10)
  g.lineTo(-2, 6)
  g.stroke({ width: 1.4, color, alpha: 0.9 })
}

function drawMerge(g: Graphics, color: number) {
  // Two lines converging into one (Y shape rotated).
  g.moveTo(-10, -8)
  g.quadraticCurveTo(-2, -8, 2, 0)
  g.moveTo(-10, 8)
  g.quadraticCurveTo(-2, 8, 2, 0)
  g.moveTo(2, 0)
  g.lineTo(12, 0)
  g.stroke({ width: 1.6, color, alpha: 0.9 })
}

function drawClose(g: Graphics, color: number) {
  g.circle(0, 0, 9)
  g.stroke({ width: 1.25, color, alpha: 0.7 })
  g.moveTo(-4, -4)
  g.lineTo(4, 4)
  g.moveTo(4, -4)
  g.lineTo(-4, 4)
  g.stroke({ width: 1.6, color, alpha: 0.9 })
}
