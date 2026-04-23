// Station renderer — one machine on the factory floor, populated from event
// catalog metadata + the live predicate schema fetched from the API.
//
// Anatomy:
//
//   ┌───────────────────────────────────────────┐
//   │ ●  PR Opened                 [ github ]   │  header band
//   │ ┌───────────────────────────────────────┐ │
//   │ │ ┏━┓                                ┏━┓ │ │
//   │●│                  ✦                   │●│  core chamber — glyph + HUD brackets
//   │ │ ┗━┛                                ┗━┛ │ │  port nubs protrude from the frame
//   │ └───────────────────────────────────────┘ │
//   │  [self]  [author]  [repo]  +2             │  predicate chips
//   └───────────────────────────────────────────┘
//
// The frame is glass; the core is a recessed chamber where tasks (coming
// soon) will dwell and event-fire ripples will animate. Port nubs on the
// left/right edges are the visible docking points the belts snap to.
//
// Presentation data (label, category, lifecycle, glyph) comes from
// factory/events.ts. The filterable-field schema comes from the API.

import { Container, Graphics, Text } from 'pixi.js'
import type { FieldSchema } from '../types'
import type { FactoryEvent } from './events'
import { drawGlyph } from './glyphs'

const W = 260
const H = 180
const R = 18
const CORE_R = 10
const HEADER_H = 40
const CHIPS_H = 32
const CORE_PAD_X = 12
const CORE_PAD_TOP = 4
const CORE_PAD_BOT = 6

/** Conveyor belt width — exported so scene.ts can draw the belt flush with
 * the port stubs. */
export const BELT_WIDTH = 28

/** How far each port stub protrudes outward from the station frame edge.
 * Belts connect at the stub's outer end, not the station edge, so the
 * conveyor material is visually continuous. */
export const PORT_STUB_LEN = 24

// Port offsets in station-local coords. Port y is forced to 0 (the
// station's vertical center, which sits on the grid row line) so belts
// between stations and other nodes — whose ports are all at center.y —
// are perfectly horizontal. The core chamber's midline is ~3px below
// this axis; that offset is small enough to read as intentional
// asymmetry between header and chip strip rather than a bent belt.
const CORE_Y = -H / 2 + HEADER_H + CORE_PAD_TOP
const CORE_H = H - HEADER_H - CHIPS_H - CORE_PAD_TOP - CORE_PAD_BOT
const PORT_LOCAL_Y = 0

const CATEGORY_COLOR: Record<string, number> = {
  pr_flow: 0xc47a5a,
  pr_review: 0x7a9aad,
  pr_ci: 0x6ea87a,
  pr_signals: 0x9a7aad,
  jira_flow: 0xb8943a,
  jira_signals: 0x8a8480,
}

const TEXT_PRIMARY = 0x1a1a1a
const TEXT_TERTIARY = 0xa09a94
const STATE_ENABLED = 0x5a8c6a
const RIM_HIGHLIGHT = 0xffffff

// Abbreviations for predicate field names so chips stay readable. Anything
// missing falls back to the raw field name (lowercased, truncated if huge).
const FIELD_ABBREV: Record<string, string> = {
  author_is_self: 'self',
  is_draft: 'draft',
  has_label: 'label',
  label_name: 'label',
  reviewer_is_self: 'reviewer',
  commenter_is_self: 'commenter',
  assignee_is_self: 'assignee',
  reporter_is_self: 'reporter',
  check_name: 'check',
  review_type: 'review',
  new_status: 'to',
  old_status: 'from',
  new_priority: 'to',
  old_priority: 'from',
  issue_type: 'type',
}

export interface StationOptions {
  event: FactoryEvent
  fields: FieldSchema[]
  /** Whether any prompt is currently wired to this event. Dims the station when false. */
  enabled?: boolean
  center: { x: number; y: number }
}

/** A belt dock point. `dir` is the outward unit vector the port faces —
 * belts exit along this direction and arrive against it, which lets the
 * belt renderer build smooth S-curves that tangent-match the port on both
 * ends. Left ports face west (-1, 0); right ports face east (1, 0). */
export interface Port {
  x: number
  y: number
  dir: { x: number; y: number }
}

export interface StationHandle {
  kind: 'station'
  container: Container
  center: { x: number; y: number }
  eventType: string
  leftPort: Port
  rightPort: Port
  /** Stations don't expose top/bottom ports — those live on splitter/merger
   * nodes only. Declared for shape-compatibility with the GraphNode union
   * consumers use in the routing layer. */
  topPort?: undefined
  bottomPort?: undefined
  /** Station world-space size — used by the detail overlay to compute a
   * screen-space anchor rect without re-deriving from hard-coded constants. */
  worldSize: { w: number; h: number; coreY: number; coreH: number }
  /** dt is seconds since last frame; scale is the current viewport scale
   * (1 = neutral, <1 = zoomed out, >1 = zoomed in). The station toggles
   * LOD sub-groups based on scale — at near zoom the predicate chips and
   * glyph hide so an HTML overlay can take over the interior. */
  update(dt: number, scale: number): void
  /** Set the count of entities currently parked at this station. At far
   * zoom a small badge renders this near the glyph in place of the
   * individual item pills, which get too dense to read when the whole
   * factory fits on screen. Zero hides the badge. */
  setItemCount(n: number): void
  /** Set the list of entities currently parked at this station. At mid
   * zoom, the predicate chip row is replaced by these pills so the
   * station self-describes "these PRs are waiting here" rather than
   * showing static schema hints. Empty list falls back to predicate
   * chips. At near/far zoom the entity pills hide (overlay / badge
   * take over). */
  setEntities(entities: Array<{ label: string; mine: boolean }>): void
}

/** Viewport scale at or above which the station enters "near" LOD: chips
 * and glyph hide so the HTML detail overlay can render active runs and
 * throughput in the freed space. */
export const NEAR_ZOOM_THRESHOLD = 1.5

/** Viewport scale below which the station enters "far" LOD: the dense
 * header + core + chips visuals all collapse to a single oversized label
 * so the station stays legible when the whole factory fits on screen.
 * 14px Pixi text at scale 0.5 renders at ~7 CSS px — unreadable — so we
 * swap in a 36px label that holds up when the viewport is zoomed out. */
export const FAR_ZOOM_THRESHOLD = 0.6

export function buildStation(parent: Container, opts: StationOptions): StationHandle {
  const { event, fields, enabled = true, center } = opts
  const color = event.tint ?? CATEGORY_COLOR[event.category] ?? CATEGORY_COLOR.pr_flow

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  const fx = -W / 2
  const fy = -H / 2

  // Drop shadow — soft warm lift, drawn as a slightly-larger filled shape so
  // we don't pay for a blur filter per-station. Reads fine at this scale on
  // the light surface.
  const shadow = new Graphics()
  shadow.roundRect(fx - 5, fy + 8, W + 10, H + 10, R + 5)
  shadow.fill({ color: 0x000000, alpha: 0.06 })
  root.addChild(shadow)

  // Port stubs — short belt extensions protruding from the left and right
  // edges of the frame. Drawn BEFORE the frame body so the body overlaps
  // the stub's inner end, making the stub appear to emerge from inside the
  // station. Belts attach at the stub's outer end.
  drawPortStub(root, fx, PORT_LOCAL_Y, -1, color)
  drawPortStub(root, fx + W, PORT_LOCAL_Y, 1, color)

  // Frame body — translucent white over the warm surface.
  const body = new Graphics()
  body.roundRect(fx, fy, W, H, R)
  body.fill({ color: 0xffffff, alpha: 0.78 })
  root.addChild(body)

  // Category wash on the frame — barely-there warmth hinting at the row.
  const tint = new Graphics()
  tint.roundRect(fx, fy, W, H, R)
  tint.fill({ color, alpha: 0.04 })
  root.addChild(tint)

  // Top sheen — the "light catches the top" liquid-glass cue.
  const sheen = new Graphics()
  sheen.roundRect(fx + 4, fy + 4, W - 8, H / 3, R - 6)
  sheen.fill({ color: 0xffffff, alpha: 0.3 })
  root.addChild(sheen)

  // Outer hairline + inner rim highlight.
  const outerRim = new Graphics()
  outerRim.roundRect(fx, fy, W, H, R)
  outerRim.stroke({ width: 1, color: 0x000000, alpha: 0.08, alignment: 1 })
  root.addChild(outerRim)

  const innerRim = new Graphics()
  innerRim.roundRect(fx + 1, fy + 1, W - 2, H - 2, R - 1)
  innerRim.stroke({ width: 1, color: RIM_HIGHLIGHT, alpha: 0.9, alignment: 0 })
  root.addChild(innerRim)

  // ── Detail layer ──────────────────────────────────────────────────────────
  // Wraps everything that belongs to the "mid+near" LOD: header visuals,
  // core chamber, HUD brackets. At far zoom this whole layer hides and the
  // farLayer below (a single big label) takes over — 14px Pixi text shrinks
  // to unreadable at scale 0.4, so we need a distinct simplified view.
  const detailLayer = new Container()
  root.addChild(detailLayer)

  // ── Header ────────────────────────────────────────────────────────────────
  const headerBottomY = fy + HEADER_H
  const headerDivider = new Graphics()
  headerDivider.moveTo(fx + 14, headerBottomY)
  headerDivider.lineTo(fx + W - 14, headerBottomY)
  headerDivider.stroke({ width: 1, color: 0x000000, alpha: 0.05 })
  detailLayer.addChild(headerDivider)

  const pip = new Graphics()
  pip.circle(fx + 16, fy + 20, 4)
  pip.fill({ color: enabled ? STATE_ENABLED : TEXT_TERTIARY, alpha: enabled ? 0.9 : 0.5 })
  detailLayer.addChild(pip)

  const title = new Text({
    text: event.label,
    // resolution: 3 lets the raster stay sharp up to 3× zoom (NEAR). Pixi
    // rasters Text once at creation-time DPI and then transforms the
    // texture at draw — without this bump, text gets fuzzy at near zoom.
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 14,
      fontWeight: '600',
      fill: TEXT_PRIMARY,
      letterSpacing: 0.1,
    },
  })
  title.anchor.set(0, 0.5)
  title.x = fx + 28
  title.y = fy + 20
  detailLayer.addChild(title)

  // Source badge — rounded pill, right side of header.
  const badgeText = new Text({
    text: event.source,
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 9,
      fontWeight: '700',
      fill: color,
      letterSpacing: 0.8,
    },
  })
  const badgeW = Math.ceil(badgeText.width) + 14
  const badgeH = 16
  const badgeX = fx + W - 14 - badgeW
  const badgeY = fy + 12
  const badge = new Graphics()
  badge.roundRect(badgeX, badgeY, badgeW, badgeH, badgeH / 2)
  badge.fill({ color, alpha: 0.1 })
  badge.stroke({ width: 0.75, color, alpha: 0.35 })
  detailLayer.addChild(badge)
  badgeText.anchor.set(0.5, 0.5)
  badgeText.x = badgeX + badgeW / 2
  badgeText.y = badgeY + badgeH / 2 + 0.5
  detailLayer.addChild(badgeText)

  // ── Core chamber ──────────────────────────────────────────────────────────
  const coreX = fx + CORE_PAD_X
  const coreW = W - CORE_PAD_X * 2

  // Recession hint: draw a slightly-offset dark rect, then a warm-tinted fill
  // on top. The 1px mismatch between the two creates a subtle top-edge shadow
  // that reads as "this is set into the frame."
  const coreRecess = new Graphics()
  coreRecess.roundRect(coreX, CORE_Y, coreW, CORE_H, CORE_R)
  coreRecess.fill({ color: 0x000000, alpha: 0.04 })
  detailLayer.addChild(coreRecess)

  const coreFill = new Graphics()
  coreFill.roundRect(coreX + 1, CORE_Y + 1, coreW - 2, CORE_H - 2, CORE_R - 1)
  coreFill.fill({ color: 0xf7f5f2, alpha: 0.9 })
  detailLayer.addChild(coreFill)

  const coreTint = new Graphics()
  coreTint.roundRect(coreX + 1, CORE_Y + 1, coreW - 2, CORE_H - 2, CORE_R - 1)
  coreTint.fill({ color, alpha: 0.06 })
  detailLayer.addChild(coreTint)

  // HUD corner brackets — four L-shapes inset inside the core. Very thin,
  // accent-tinted. Adds "tech object" texture without being busy.
  const BRACKET = 10
  const BM = 8 // margin from core edge
  const bX1 = coreX + BM
  const bY1 = CORE_Y + BM
  const bX2 = coreX + coreW - BM
  const bY2 = CORE_Y + CORE_H - BM
  const brackets = new Graphics()
  brackets.moveTo(bX1, bY1 + BRACKET)
  brackets.lineTo(bX1, bY1)
  brackets.lineTo(bX1 + BRACKET, bY1)
  brackets.moveTo(bX2 - BRACKET, bY1)
  brackets.lineTo(bX2, bY1)
  brackets.lineTo(bX2, bY1 + BRACKET)
  brackets.moveTo(bX1, bY2 - BRACKET)
  brackets.lineTo(bX1, bY2)
  brackets.lineTo(bX1 + BRACKET, bY2)
  brackets.moveTo(bX2 - BRACKET, bY2)
  brackets.lineTo(bX2, bY2)
  brackets.lineTo(bX2, bY2 - BRACKET)
  brackets.stroke({ width: 1, color, alpha: 0.4 })
  detailLayer.addChild(brackets)

  // ── Far layer ─────────────────────────────────────────────────────────────
  // Visible only at far zoom (scale < FAR_ZOOM_THRESHOLD). Three pieces,
  // all in the category color so the station reads as a single semantic
  // unit when the whole factory fits on screen:
  //   - procedural glyph centered above the label (same glyph shown in the
  //     core chamber at mid zoom — gives instant identity: check vs.
  //     merge vs. cross etc.)
  //   - big 32px label below, wraps on multi-line event names
  //   - four HUD-style corner brackets framing the card, echoing the mid-
  //     zoom brackets inside the core chamber
  const farLayer = new Container()
  root.addChild(farLayer)

  const farGlyph = new Container()
  farGlyph.x = 0
  farGlyph.y = -28
  farLayer.addChild(farGlyph)
  drawGlyph(farGlyph, event.glyph, color)
  // drawGlyph emits at its natural procedural size (~32px tall). Scale it
  // up a touch so it reads comfortably against the large label.
  farGlyph.scale.set(1.4)

  const farTitle = new Text({
    text: event.label,
    resolution: 2,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 32,
      fontWeight: '700',
      fill: TEXT_PRIMARY,
      letterSpacing: 0.2,
      align: 'center',
      wordWrap: true,
      wordWrapWidth: W - 40,
    },
  })
  farTitle.anchor.set(0.5, 0.5)
  farTitle.x = 0
  farTitle.y = 28
  farLayer.addChild(farTitle)

  // Halo-style corner brackets on the card itself. Bigger and more inset
  // than the core brackets (10 long / 8 margin) so they read as part of
  // the card silhouette, not competition with the smaller core brackets
  // they replace at this zoom.
  const CARD_BRACKET = 18
  const CARD_BM = 14
  const cx1 = fx + CARD_BM
  const cy1 = fy + CARD_BM
  const cx2 = fx + W - CARD_BM
  const cy2 = fy + H - CARD_BM
  const farBrackets = new Graphics()
  farBrackets.moveTo(cx1, cy1 + CARD_BRACKET)
  farBrackets.lineTo(cx1, cy1)
  farBrackets.lineTo(cx1 + CARD_BRACKET, cy1)
  farBrackets.moveTo(cx2 - CARD_BRACKET, cy1)
  farBrackets.lineTo(cx2, cy1)
  farBrackets.lineTo(cx2, cy1 + CARD_BRACKET)
  farBrackets.moveTo(cx1, cy2 - CARD_BRACKET)
  farBrackets.lineTo(cx1, cy2)
  farBrackets.lineTo(cx1 + CARD_BRACKET, cy2)
  farBrackets.moveTo(cx2 - CARD_BRACKET, cy2)
  farBrackets.lineTo(cx2, cy2)
  farBrackets.lineTo(cx2, cy2 - CARD_BRACKET)
  farBrackets.stroke({ width: 2, color, alpha: 0.6 })
  farLayer.addChild(farBrackets)

  // Count badge rendered next to the far-view glyph. At far zoom
  // individual item pills hide (too dense to read), so the count is the
  // only signal of how many entities are parked here. Positioned to the
  // upper-right of the glyph center so it doesn't occlude the title.
  const farCountBadge = new Container()
  farCountBadge.x = 24
  farCountBadge.y = -38
  farCountBadge.visible = false
  farLayer.addChild(farCountBadge)

  const farCountBg = new Graphics()
  farCountBg.circle(0, 0, 11)
  farCountBg.fill({ color, alpha: 0.9 })
  farCountBg.stroke({ width: 1, color: 0xffffff, alpha: 0.8 })
  farCountBadge.addChild(farCountBg)

  const farCountText = new Text({
    text: '',
    resolution: 2,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 11,
      fontWeight: '700',
      fill: 0xffffff,
    },
  })
  farCountText.anchor.set(0.5, 0.5)
  farCountBadge.addChild(farCountText)

  // Procedural glyph centered in the core. Wrapped in its own container so
  // the near-zoom LOD can hide it, yielding the core's interior to the HTML
  // detail overlay.
  const glyphLayer = new Container()
  glyphLayer.x = coreX + coreW / 2
  glyphLayer.y = CORE_Y + CORE_H / 2
  root.addChild(glyphLayer)
  drawGlyph(glyphLayer, event.glyph, color)

  // ── Predicate chips ───────────────────────────────────────────────────────
  // Wrapped in their own container so near-zoom LOD can hide the entire row
  // — the overlay puts live throughput in this strip instead. At mid zoom
  // this layer only renders when there are NO parked entities to display;
  // if entities exist, the entitiesLayer below takes over this strip.
  const chipsLayer = new Container()
  root.addChild(chipsLayer)
  const chipsY = fy + H - CHIPS_H + 6
  const chipsStartX = fx + 14
  const chipsEndX = fx + W - 14
  const available = chipsEndX - chipsStartX
  // Greedy fit — keep adding chips while there's room; overflow counter for
  // anything that didn't fit.
  let cursor = chipsStartX
  const chipGap = 4
  let shown = 0
  for (let i = 0; i < fields.length; i++) {
    const abbrev = FIELD_ABBREV[fields[i].name] ?? fields[i].name
    const estW = estimateChipWidth(abbrev)
    // Reserve ~26px for a possible +N overflow on the last fitting iteration.
    const needOverflow = i < fields.length - 1
    const reserve = needOverflow ? 30 : 0
    if (cursor + estW + reserve > chipsStartX + available) break
    drawChip(chipsLayer, cursor, chipsY, abbrev, color, false)
    cursor += estW + chipGap
    shown++
  }
  const overflow = fields.length - shown
  if (overflow > 0) {
    drawChip(chipsLayer, cursor, chipsY, `+${overflow}`, color, true)
  }

  // ── Entity pills (mid-zoom strip) ─────────────────────────────────────────
  // When entities are parked at this station, replace the static predicate
  // chips with live entity pills. Same strip, same greedy-fit rules, but
  // each pill carries an ownership dot + the entity's label (PR # / source
  // id). Repopulated by scene.ts's reconciler via setEntities on every
  // restack — stable order matches the stacking sort.
  const entitiesLayer = new Container()
  entitiesLayer.visible = false
  root.addChild(entitiesLayer)
  let hasEntities = false
  const rebuildEntityPills = (entities: Array<{ label: string; mine: boolean }>) => {
    entitiesLayer.removeChildren().forEach((c) => c.destroy({ children: true }))
    hasEntities = entities.length > 0
    if (!hasEntities) return
    let cur = chipsStartX
    const gap = 4
    let fit = 0
    for (let i = 0; i < entities.length; i++) {
      const ent = entities[i]
      const estW = estimateEntityPillWidth(ent.label)
      const needOverflow = i < entities.length - 1
      const reserve = needOverflow ? 30 : 0
      if (cur + estW + reserve > chipsStartX + available) break
      drawEntityPill(entitiesLayer, cur, chipsY, ent.label, ent.mine, color)
      cur += estW + gap
      fit++
    }
    const hidden = entities.length - fit
    if (hidden > 0) {
      drawChip(entitiesLayer, cur, chipsY, `+${hidden}`, color, true)
    }
  }

  // Station-wide dim when disabled.
  if (!enabled) {
    root.alpha = 0.6
  }

  // ── Ambient animation ─────────────────────────────────────────────────────
  // Subtle glyph alpha breathing — reads as "standby, powered on" rather
  // than the static card feeling we had before. No scale pulse (distracting
  // at multi-station scale).
  let t = 0
  const baseAlpha = glyphLayer.alpha
  return {
    kind: 'station',
    container: root,
    center,
    eventType: event.eventType,
    worldSize: { w: W, h: H, coreY: CORE_Y, coreH: CORE_H },
    leftPort: {
      x: center.x - W / 2 - PORT_STUB_LEN,
      y: center.y + PORT_LOCAL_Y,
      dir: { x: -1, y: 0 },
    },
    rightPort: {
      x: center.x + W / 2 + PORT_STUB_LEN,
      y: center.y + PORT_LOCAL_Y,
      dir: { x: 1, y: 0 },
    },
    update(dt: number, scale: number) {
      t += dt
      const breathe = 0.78 + 0.22 * (0.5 + 0.5 * Math.sin(t * 1.5))
      glyphLayer.alpha = baseAlpha * breathe

      // Three LOD tiers, gated on the viewport scale:
      //   far  (scale < 0.6): show only the big label + pip in farLayer;
      //                       hide the dense header / core / chips / glyph
      //   mid  (0.6..1.5):    full detail — header, core, glyph, chips
      //   near (scale >= 1.5): detail stays, but chips + glyph hide so the
      //                       HTML overlay can own the interior
      const far = scale < FAR_ZOOM_THRESHOLD
      const near = scale >= NEAR_ZOOM_THRESHOLD
      const mid = !far && !near
      farLayer.visible = far
      detailLayer.visible = !far
      glyphLayer.visible = mid
      // At mid zoom, the bottom strip shows either the entity pills (when
      // entities are parked) or the predicate chips (resting state). At
      // near/far both hide — the HTML overlay / count badge owns signal.
      chipsLayer.visible = mid && !hasEntities
      entitiesLayer.visible = mid && hasEntities
    },
    setItemCount(n: number) {
      if (n <= 0) {
        farCountBadge.visible = false
        return
      }
      farCountText.text = String(n)
      farCountBadge.visible = true
    },
    setEntities(entities: Array<{ label: string; mine: boolean }>) {
      rebuildEntityPills(entities)
    },
  }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function drawPortStub(
  parent: Container,
  attachX: number,
  attachY: number,
  direction: 1 | -1,
  color: number,
) {
  // A short flat belt-like stub extending outward from the station frame
  // edge. Same fill and edge treatment as the main belt so the conveyor
  // material reads as one continuous piece. The inner end starts at the
  // station's outer edge (station body overlaps it); the outer end is
  // where the belt docks.
  const outerX = attachX + direction * PORT_STUB_LEN
  const topY = attachY - BELT_WIDTH / 2
  const botY = attachY + BELT_WIDTH / 2
  const leftX = Math.min(attachX, outerX)
  const rightX = Math.max(attachX, outerX)

  // Belt material fill.
  const body = new Graphics()
  body.rect(leftX, topY, rightX - leftX, BELT_WIDTH)
  body.fill({ color: 0xffffff, alpha: 0.82 })
  parent.addChild(body)

  // Warm tint so the stub inherits the category accent.
  const tint = new Graphics()
  tint.rect(leftX, topY, rightX - leftX, BELT_WIDTH)
  tint.fill({ color, alpha: 0.1 })
  parent.addChild(tint)

  // Top edge highlight (reads as "light hitting the near rail").
  const top = new Graphics()
  top.moveTo(leftX, topY)
  top.lineTo(rightX, topY)
  top.stroke({ width: 1.25, color: 0xffffff, alpha: 0.95 })
  parent.addChild(top)

  // Bottom edge shadow (far rail, in shadow).
  const bot = new Graphics()
  bot.moveTo(leftX, botY)
  bot.lineTo(rightX, botY)
  bot.stroke({ width: 1.25, color: 0x000000, alpha: 0.18 })
  parent.addChild(bot)

  // Outer end-cap — a short accent-colored band marking the dock point.
  const capX = direction === 1 ? outerX - 3 : outerX
  const cap = new Graphics()
  cap.rect(capX, topY + 2, 3, BELT_WIDTH - 4)
  cap.fill({ color, alpha: 0.45 })
  parent.addChild(cap)
}

function estimateEntityPillWidth(label: string): number {
  // Entity pill = tint dot (4px + 3px gap) + label, padded. Over-
  // estimates slightly (same tradeoff as estimateChipWidth) so greedy
  // layout doesn't overshoot the right edge.
  return Math.ceil(label.length * 5.8) + 20
}

const TINT_MINE_HEX = 0xc47a5a
const TINT_OTHER_HEX = 0x7a9aad

function drawEntityPill(
  parent: Container,
  cx: number,
  cy: number,
  label: string,
  mine: boolean,
  color: number,
) {
  const tint = mine ? TINT_MINE_HEX : TINT_OTHER_HEX
  const text = new Text({
    text: label,
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 9,
      fontWeight: '600',
      fill: tint,
      letterSpacing: 0.3,
    },
  })
  const padX = 6
  const padY = 3
  const dotR = 2
  const dotGap = 4
  const w = Math.ceil(text.width) + padX * 2 + dotR * 2 + dotGap
  const h = Math.ceil(text.height) + padY * 2

  const bg = new Graphics()
  bg.roundRect(cx, cy, w, h, h / 2)
  bg.fill({ color: 0xffffff, alpha: 0.92 })
  bg.stroke({ width: 0.75, color, alpha: 0.25 })
  parent.addChild(bg)

  const dot = new Graphics()
  dot.circle(cx + padX + dotR, cy + h / 2, dotR)
  dot.fill({ color: tint, alpha: 1 })
  parent.addChild(dot)

  text.anchor.set(0, 0)
  text.x = cx + padX + dotR * 2 + dotGap
  text.y = cy + padY - 0.5
  parent.addChild(text)

  return w
}

function estimateChipWidth(label: string): number {
  // Rough text-width estimate in Inter 9px bold — measuring for real would
  // require mounting the Text first, which is fine but more allocations.
  // A flat per-char width is close enough at this size, slightly over-
  // estimating to be safe.
  return Math.ceil(label.length * 5.6) + 12
}

function drawChip(
  parent: Container,
  cx: number,
  cy: number,
  label: string,
  color: number,
  muted: boolean,
) {
  const text = new Text({
    text: label,
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 9,
      fontWeight: '600',
      fill: muted ? TEXT_TERTIARY : color,
      letterSpacing: 0.5,
    },
  })
  const padX = 6
  const padY = 3
  const w = Math.ceil(text.width) + padX * 2
  const h = Math.ceil(text.height) + padY * 2

  const bg = new Graphics()
  bg.roundRect(cx, cy, w, h, h / 2)
  bg.fill({ color: muted ? 0x000000 : color, alpha: muted ? 0.04 : 0.09 })
  bg.stroke({ width: 0.75, color: muted ? 0x000000 : color, alpha: muted ? 0.1 : 0.3 })
  parent.addChild(bg)

  text.anchor.set(0, 0)
  text.x = cx + padX
  text.y = cy + padY - 0.5
  parent.addChild(text)

  return w
}
