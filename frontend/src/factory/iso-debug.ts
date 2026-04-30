// Debug scene for the 3D rewrite (SKY-196 / SKY-197).
//
// Section ported from the 2.5D factory: the new-commits intake,
// CI fan-out, CI Failed re-queue loopback, and stubs for the
// post-CI-Passed router and the not-yet-wired review-outcome
// splitter (placed for spatial planning).
//
//                                          .─────→ MC               ↑ top
//                                          ↑
//                                          | (pole_above)
//                                          |
//   merger ──→ NC ──→ S1 ──→ S2 ────────→──┼──→ CI Passed ──→ post-CI                  ⊕ review (unwired)
//   ▲                                      |
//   ▲                                      | (pole_below)
//   │                                      ↓
//   │                                      .─────→ CI Failed ──┐
//   │                                                          │
//   │       pole_3 ←────────── pole_2 ←────────── pole_1 ◀─────┘   (loopback)
//   │       │
//   └───────┘
//
// Items spawn at the merger's west input — appearing at its
// outside face — traverse the merger body, exit east into NC,
// and ride the main belt graph through S1/S2 to one of the CI
// stations. Stations the items must transit (NC, CI Passed, CI
// Failed) are wired passthrough (west.next = east.segment) so
// items don't dead-end at every recess. MC stays a true terminal.
//
// The CI Failed loopback is geometric only today: the simulator
// picks next[0] at every fork, so all items currently route to
// CI Passed → post-CI. Other branches (MC, CI Failed loop, post-CI
// outputs, review splitter) sit as static structure ready for the
// routing-decision layer that lands with real entity data.
//
// Path-offset math chains chevrons through stub → belt → stub at
// every junction, anchored at hardcoded station-stub UVs. Where a
// belt straddles two anchors (loopback to merger, CI Failed east
// face), the math accepts a small chevron jump at the upstream
// face — invisible at the splitter/router junctions but a tiny
// phase shift at the CI Failed east wall.

import { DEFAULT_PORT_RECESS_DEPTH } from './iso-port'
import type { Pole } from './iso-pole'
import { IsoScene } from './iso-renderer'
import type { Router } from './iso-router'
import type { Station } from './iso-station'

const FLOOR_CELL = 80
const FLOOR_SIZE = 4800 // 60 cells — extra room for downstream additions
const INITIAL_ZOOM_RADIUS = FLOOR_SIZE / 2

// ─── Cell-grid layout ─────────────────────────────────────────────
//
// Stations are 5×3 cells, splitters/mergers/poles are 1 cell.
// Vertical separation between station mid-rows is 4 cells (3-cell
// belt at 240 wu); horizontal spacing is mostly 1 cell (80 wu).
// The CI Failed loopback wraps below the main belts at row 5.

const STATION_W = 5
const STATION_D = 3
const STATION_H = 64

// Source side: PR Opened (above and slightly left of RFR), then
// Ready-for-Review, intake splitter, merger, then NC. PR Opened
// is a standard west-input/east-output station; four corner poles
// wrap the conveyor east → south → west → south → east, ducking
// under PR Opened along row 23 before dropping into RFR's west
// input at row 19.
const PR_OPENED_COL = 0
const PR_OPENED_ROW = 24 // mid-row 25
const PRO_POLE1_COL = 6 // 1-cell gap east of PR Opened east face
const PRO_POLE1_ROW = 25 // aligned with PR Opened east port mid-row
const PRO_POLE2_COL = 6
const PRO_POLE2_ROW = 22 // bottom-east corner; turns the chain west
const PRO_POLE3_COL = 2
const PRO_POLE3_ROW = 22 // bottom-west corner; turns south again
const PRO_POLE4_COL = 2
const PRO_POLE4_ROW = 19 // final corner; turns east into RFR.west
const READY_FOR_REVIEW_COL = 5
const READY_FOR_REVIEW_ROW = 18 // mid-row 19
const INTAKE_SPLITTER_COL = 11
const INTAKE_SPLITTER_ROW = 19
const MERGER_COL = 13
const MERGER_ROW = 19
const NC_COL = 15
const NC_ROW = 18 // mid-row 19

// Splitters and turn poles run along the source mid-row.
const S1_COL = 21
const S2_COL = 23
const SPLITTER_ROW = 19

// Destinations (5-col stations, west face at col 16). High-row =
// top of screen after the camera's α=+π/2 flip, so MC sits at
// high row to land at the top of the diagram and CI Failed at low
// row to land at the bottom.
const DEST_COL = 26
const MC_ROW = 22 // mid-row 23, top of screen
const CI_PASSED_ROW = 18 // mid-row 19
const CI_FAILED_ROW = 14 // mid-row 15, bottom of screen

// Turn poles between S2 and the top/bottom destinations.
const POLE_ABOVE_ROW = 23
const POLE_BELOW_ROW = 15

// Post-CI splitter sits 1 cell east of CI Passed. Then a 1-cell
// gap, the review-intake merger (loopback target for "changes
// requested" — south input unwired today), 1-cell gap, the
// Review Requested station, 1-cell gap, the review-outcomes
// splitter, and a 3-way fan to Review Commented (east) / Review
// Approved (top) / Changes Requested (bottom) — same shape as
// the CI fan.
const POST_CI_COL = 32
const POST_CI_ROW = 19
const RM_COL = 34 // review-intake merger
const RM_ROW = 19
const RR_COL = 36 // Review Requested station (5-wide)
const RR_ROW = 18 // mid-row 19
const REVIEW_COL = 42 // review-outcomes splitter
const REVIEW_ROW = 19

// Review-fan destination stations (5-col, west face at col 45).
// Mirror of the CI fan: top of screen = high row, bottom = low.
const REVIEW_DEST_COL = 45
const REVIEW_APPROVED_ROW = 22 // mid-row 23, top of screen
const REVIEW_COMMENTED_ROW = 18 // mid-row 19
const CHANGES_REQUESTED_ROW = 14 // mid-row 15, bottom of screen

// Turn poles between review splitter and the top/bottom review
// destinations.
const POLE_REVIEW_ABOVE_ROW = 23
const POLE_REVIEW_BELOW_ROW = 15

// CI Failed loopback corners. Pole 1 has been promoted to a
// merger — it consumes both the CI Failed east output AND the
// post-CI splitter's south output (sometimes people commit even
// when CI is passing, so the post-CI south branch routes back
// into the new-commits loop). The chain then runs straight south
// down col 22, wraps west along row 12, and climbs the merger
// column (col 3) back into the NC merger's south input.
//
// The loopback's horizontal stretch sits at row 12 — 2 rows below
// CI Failed (rows 14–16). Row 11 is freed up for the new
// "ready-for-review intake" chain (RFR_POLE_*) below.
const LOOP_MERGER_COL = 32 // column-aligned with post-CI splitter
const LOOP_MERGER_ROW = 15 // east of CI Failed, merger feeds south output
const LOOP_POLE2_COL = 32
const LOOP_POLE2_ROW = 12 // south-east corner, turns west
const LOOP_POLE3_COL = 13
const LOOP_POLE3_ROW = 12 // south-west corner, turns north

// Ready-for-Review intake corners. The intake splitter's south
// output drops to row 11, runs east along row 11, and climbs into
// the review-intake merger's south input. RM_SOUTH was originally
// reserved for the changes_requested loopback; both will
// eventually merge into it (multiple inputs, single output).
const RFR_POLE1_COL = 11 // = INTAKE_SPLITTER_COL
const RFR_POLE1_ROW = 11
const RFR_POLE2_COL = 34 // = RM_COL
const RFR_POLE2_ROW = 11

// ─── Path-offset math ─────────────────────────────────────────────

const SPACING = 54 // CHEVRON_SPACING_WORLD
const ROUTER_STUB_LEN = 12 // ROUTER_RECESS_DEPTH (14) − STUB_BACK_GAP (2)
// Station east output stub: UV at wall plane = (60 − 2) mod 54 = 4.
const STATION_EAST_UV_AT_WALL = 4
const TURN_ARC_LENGTH = (Math.PI * FLOOR_CELL) / 4

// Belt lengths derive from the cell-grid layout. The RFR → intake
// splitter belt anchors forward off RFR's hardcoded east stub UV,
// so the belt length doesn't enter the chevron math.
const BELT_PRO_TO_POLE1_LEN = (PRO_POLE1_COL - (PR_OPENED_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_POLE1_TO_POLE2_LEN = (PRO_POLE1_ROW - (PRO_POLE2_ROW + 1)) * FLOOR_CELL // 240
const BELT_POLE2_TO_POLE3_LEN = (PRO_POLE2_COL - (PRO_POLE3_COL + 1)) * FLOOR_CELL // 400
const BELT_POLE3_TO_POLE4_LEN = (PRO_POLE3_ROW - (PRO_POLE4_ROW + 1)) * FLOOR_CELL // 240
// pole_4 → RFR.west belt's length doesn't enter the chevron math
// (chain anchors forward from PR Opened.east; jump lands at RFR
// west wall), so the length is implicit in the belt's geometry.
const BELT_INTAKE_SPLITTER_TO_MERGER_LEN = (MERGER_COL - (INTAKE_SPLITTER_COL + 1)) * FLOOR_CELL // 80
const BELT_MERGER_TO_NC_LEN = (NC_COL - (MERGER_COL + 1)) * FLOOR_CELL // 80
const BELT_NC_TO_S1_LEN = (S1_COL - (NC_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_S1_TO_S2_LEN = (S2_COL - (S1_COL + 1)) * FLOOR_CELL // 80
const BELT_S2_TO_CI_PASSED_LEN = (DEST_COL - (S2_COL + 1)) * FLOOR_CELL // 160
const BELT_S2_TO_POLE_LEN = (POLE_ABOVE_ROW - (SPLITTER_ROW + 1)) * FLOOR_CELL // 240
const BELT_POLE_TO_DEST_LEN = (DEST_COL - (S2_COL + 1)) * FLOOR_CELL // 160
const BELT_CI_PASSED_TO_POST_CI_LEN = (POST_CI_COL - (DEST_COL + STATION_W)) * FLOOR_CELL // 80
// Review chain belts. The merger→RR belt's length doesn't enter
// the offset math (chain anchors forward off the merger east face;
// any chevron mismatch lands at RR's hardcoded wall UV), so it's
// derived implicitly by addBelt from port positions.
const BELT_POSTCI_TO_RM_LEN = (RM_COL - (POST_CI_COL + 1)) * FLOOR_CELL // 80
const BELT_RR_TO_REVIEW_LEN = (REVIEW_COL - (RR_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_REVIEW_TO_DEST_LEN = (REVIEW_DEST_COL - (REVIEW_COL + 1)) * FLOOR_CELL // 160
const BELT_REVIEW_TO_REVIEW_POLE_LEN = (POLE_REVIEW_ABOVE_ROW - (REVIEW_ROW + 1)) * FLOOR_CELL // 240
const BELT_REVIEW_POLE_TO_DEST_LEN = BELT_REVIEW_TO_DEST_LEN // 160
// Loopback belts. Loop pole 1 is now a merger so the belt names
// reflect that (CIF → loop_merger.west, post-CI south → loop_merger.north).
const BELT_CIF_TO_LOOP_MERGER_LEN = (LOOP_MERGER_COL - (DEST_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_POSTCI_SOUTH_TO_LOOP_MERGER_LEN = (POST_CI_ROW - (LOOP_MERGER_ROW + 1)) * FLOOR_CELL // 240
const BELT_LOOP_MERGER_TO_LOOP2_LEN = (LOOP_MERGER_ROW - (LOOP_POLE2_ROW + 1)) * FLOOR_CELL // 160
const BELT_LOOP2_TO_LOOP3_LEN = (LOOP_POLE2_COL - (LOOP_POLE3_COL + 1)) * FLOOR_CELL // 1440
const BELT_LOOP3_TO_MERGER_LEN = (MERGER_ROW - (LOOP_POLE3_ROW + 1)) * FLOOR_CELL // 480

// Ready-for-Review intake belts. The chain hangs off the intake
// splitter's south output, so vertical legs are anchored at
// INTAKE_SPLITTER_ROW (= 19) and RFR_POLE2_ROW (= 11).
const BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_POLE1_LEN =
  (INTAKE_SPLITTER_ROW - (RFR_POLE1_ROW + 1)) * FLOOR_CELL // 560
const BELT_RFR_POLE1_TO_RFR_POLE2_LEN = (RFR_POLE2_COL - (RFR_POLE1_COL + 1)) * FLOOR_CELL // 1760
const BELT_RFR_POLE2_TO_RM_LEN = (RM_ROW - (RFR_POLE2_ROW + 1)) * FLOOR_CELL // 560

const mod = (x: number) => ((x % SPACING) + SPACING) % SPACING

// Source: RFR.east → intake_splitter → merger → NC.west. Anchor
// at NC.west wall (UV 0) and walk backward through merger body,
// belt to splitter, splitter body, belt to RFR. The chevron jump
// (if any) lands at RFR.east, where the station's hardcoded UV
// (= STATION_EAST_UV_AT_WALL) meets the upstream belt.
const BELT_MERGER_TO_NC = mod(-BELT_MERGER_TO_NC_LEN)
const MERGER_EAST_OFFSET = mod(BELT_MERGER_TO_NC - ROUTER_STUB_LEN)
const MERGER_WEST_OFFSET = mod(MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // body-continuous
const MERGER_SOUTH_OFFSET = mod(MERGER_EAST_OFFSET - ROUTER_STUB_LEN)
const BELT_INTAKE_SPLITTER_TO_MERGER = mod(MERGER_WEST_OFFSET - BELT_INTAKE_SPLITTER_TO_MERGER_LEN)
const INTAKE_SPLITTER_EAST_OFFSET = mod(BELT_INTAKE_SPLITTER_TO_MERGER - ROUTER_STUB_LEN)
const INTAKE_SPLITTER_WEST_OFFSET = mod(INTAKE_SPLITTER_EAST_OFFSET - ROUTER_STUB_LEN)
// RFR.east → intake_splitter.west: belt anchored forward off the
// RFR east stub (UV 4); chevron jump (if any) lands at the
// splitter's west wall.
const BELT_RFR_TO_INTAKE_SPLITTER = STATION_EAST_UV_AT_WALL

// PR Opened.east → 4 corner poles → RFR.west. Anchored forward
// off PR Opened's east stub (wall UV = STATION_EAST_UV_AT_WALL).
// Each pole adds TURN_ARC_LENGTH to the chevron offset; the small
// residual chevron jump lands at RFR.west.
const BELT_PRO_TO_POLE1 = STATION_EAST_UV_AT_WALL
const PRO_POLE1_OFFSET = mod(BELT_PRO_TO_POLE1 + BELT_PRO_TO_POLE1_LEN)
const BELT_POLE1_TO_POLE2 = mod(PRO_POLE1_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE2_OFFSET = mod(BELT_POLE1_TO_POLE2 + BELT_POLE1_TO_POLE2_LEN)
const BELT_POLE2_TO_POLE3 = mod(PRO_POLE2_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE3_OFFSET = mod(BELT_POLE2_TO_POLE3 + BELT_POLE2_TO_POLE3_LEN)
const BELT_POLE3_TO_POLE4 = mod(PRO_POLE3_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE4_OFFSET = mod(BELT_POLE3_TO_POLE4 + BELT_POLE3_TO_POLE4_LEN)
const BELT_POLE4_TO_RFR = mod(PRO_POLE4_OFFSET + TURN_ARC_LENGTH)

// NC.east → S1 → S2: chain forward continuous from NC east stub.
const BELT_NC_TO_S1 = STATION_EAST_UV_AT_WALL
const S1_WEST_OFFSET = mod(BELT_NC_TO_S1 + BELT_NC_TO_S1_LEN)
const S1_EAST_OFFSET = mod(S1_WEST_OFFSET + ROUTER_STUB_LEN) // body-continuous
const BELT_S1_TO_S2 = mod(S1_EAST_OFFSET + ROUTER_STUB_LEN)
const S2_WEST_OFFSET = mod(BELT_S1_TO_S2 + BELT_S1_TO_S2_LEN)
// S1 south output is unwired; pathOffset is irrelevant.
const S1_SOUTH_OFFSET = 0

// East destination: anchor at CI Passed (UV 0); S2.east adopts.
const BELT_S2_TO_CI_PASSED = mod(-BELT_S2_TO_CI_PASSED_LEN)
const S2_EAST_OFFSET = mod(BELT_S2_TO_CI_PASSED - ROUTER_STUB_LEN)

// Post-CI splitter: belt continuous forward from CI Passed.east
// (UV 4); post-CI's outputs adopt body-continuous values for
// later belt extensions.
const BELT_CI_PASSED_TO_POST_CI = STATION_EAST_UV_AT_WALL
const POST_CI_WEST_OFFSET = mod(BELT_CI_PASSED_TO_POST_CI + BELT_CI_PASSED_TO_POST_CI_LEN)
const POST_CI_OUTPUT_OFFSET = mod(POST_CI_WEST_OFFSET + ROUTER_STUB_LEN)

// Forward chain past post-CI: post-CI.east → review-intake merger
// → Review Requested → review-outcomes splitter. Body-continuous
// from post-CI east (UV at wall = POST_CI_OUTPUT_OFFSET + 12 = 0).
const BELT_POSTCI_TO_RM = mod(POST_CI_OUTPUT_OFFSET + ROUTER_STUB_LEN)
const RM_WEST_OFFSET = mod(BELT_POSTCI_TO_RM + BELT_POSTCI_TO_RM_LEN)
const RM_EAST_OFFSET = mod(RM_WEST_OFFSET + ROUTER_STUB_LEN) // body-continuous
// South input: loopback target, unwired today; body-continuous
// with the rest of the merger.
const RM_SOUTH_OFFSET = RM_WEST_OFFSET
const BELT_RM_TO_RR = mod(RM_EAST_OFFSET + ROUTER_STUB_LEN) // RM east wall UV

// RR east → review splitter west: anchor at RR east stub (UV 4).
const BELT_RR_TO_REVIEW = STATION_EAST_UV_AT_WALL
const REVIEW_WEST_OFFSET = mod(BELT_RR_TO_REVIEW + BELT_RR_TO_REVIEW_LEN)

// Review splitter outputs — each computed backward from its
// destination station's west wall (UV 0), so chevrons land
// continuous at the destination (the user-visible station face)
// at the cost of small phase shifts at the splitter face.
const BELT_REVIEW_TO_REVIEW_COMMENTED = mod(-BELT_REVIEW_TO_DEST_LEN)
const REVIEW_EAST_OFFSET = mod(BELT_REVIEW_TO_REVIEW_COMMENTED - ROUTER_STUB_LEN)

const BELT_POLE_REVIEW_ABOVE_TO_RA = mod(-BELT_REVIEW_POLE_TO_DEST_LEN)
const POLE_REVIEW_ABOVE_OFFSET = mod(BELT_POLE_REVIEW_ABOVE_TO_RA - TURN_ARC_LENGTH)
const BELT_REVIEW_TO_POLE_REVIEW_ABOVE = mod(
  POLE_REVIEW_ABOVE_OFFSET - BELT_REVIEW_TO_REVIEW_POLE_LEN,
)
const REVIEW_NORTH_OFFSET = mod(BELT_REVIEW_TO_POLE_REVIEW_ABOVE - ROUTER_STUB_LEN)

const BELT_POLE_REVIEW_BELOW_TO_CR = mod(-BELT_REVIEW_POLE_TO_DEST_LEN)
const POLE_REVIEW_BELOW_OFFSET = mod(BELT_POLE_REVIEW_BELOW_TO_CR - TURN_ARC_LENGTH)
const BELT_REVIEW_TO_POLE_REVIEW_BELOW = mod(
  POLE_REVIEW_BELOW_OFFSET - BELT_REVIEW_TO_REVIEW_POLE_LEN,
)
const REVIEW_SOUTH_OFFSET = mod(BELT_REVIEW_TO_POLE_REVIEW_BELOW - ROUTER_STUB_LEN)

// Above-S2 chain (toward MC): pole sits at high-row, fed by S2.NORTH.
const BELT_POLE_ABOVE_TO_MC = mod(-BELT_POLE_TO_DEST_LEN)
const POLE_ABOVE_OFFSET = mod(BELT_POLE_ABOVE_TO_MC - TURN_ARC_LENGTH)
const BELT_S2_TO_POLE_ABOVE = mod(POLE_ABOVE_OFFSET - BELT_S2_TO_POLE_LEN)
const S2_NORTH_OFFSET = mod(BELT_S2_TO_POLE_ABOVE - ROUTER_STUB_LEN)

// Below-S2 chain (toward CI Failed): mirror — pole at low-row,
// fed by S2.SOUTH.
const BELT_POLE_BELOW_TO_CI_FAILED = mod(-BELT_POLE_TO_DEST_LEN)
const POLE_BELOW_OFFSET = mod(BELT_POLE_BELOW_TO_CI_FAILED - TURN_ARC_LENGTH)
const BELT_S2_TO_POLE_BELOW = mod(POLE_BELOW_OFFSET - BELT_S2_TO_POLE_LEN)
const S2_SOUTH_OFFSET = mod(BELT_S2_TO_POLE_BELOW - ROUTER_STUB_LEN)

// CI Failed → loopback → NC merger.south: anchor at NC merger's
// south wall and walk backward through three belts, two corner
// poles, and a 3-port loop merger. Chevron jumps land at CI
// Failed's east wall and at post-CI's south wall (the two
// hardcoded upstream stubs we can't tune individually).
const BELT_LOOP3_TO_MERGER = mod(MERGER_SOUTH_OFFSET - BELT_LOOP3_TO_MERGER_LEN)
const LOOP_POLE3_OFFSET = mod(BELT_LOOP3_TO_MERGER - TURN_ARC_LENGTH)
const BELT_LOOP2_TO_LOOP3 = mod(LOOP_POLE3_OFFSET - BELT_LOOP2_TO_LOOP3_LEN)
const LOOP_POLE2_OFFSET = mod(BELT_LOOP2_TO_LOOP3 - TURN_ARC_LENGTH)
const BELT_LOOP_MERGER_TO_LOOP2 = mod(LOOP_POLE2_OFFSET - BELT_LOOP_MERGER_TO_LOOP2_LEN)
// Loop merger: south is the output (chevron-continuous with the
// belt heading downstream); west and north are inputs, both
// body-continuous via the recess back.
const LOOP_MERGER_SOUTH_OFFSET = mod(BELT_LOOP_MERGER_TO_LOOP2 - ROUTER_STUB_LEN)
const LOOP_MERGER_WEST_OFFSET = mod(LOOP_MERGER_SOUTH_OFFSET - ROUTER_STUB_LEN)
const LOOP_MERGER_NORTH_OFFSET = mod(LOOP_MERGER_SOUTH_OFFSET - ROUTER_STUB_LEN)
const BELT_CIF_TO_LOOP_MERGER = mod(LOOP_MERGER_WEST_OFFSET - BELT_CIF_TO_LOOP_MERGER_LEN)
const BELT_POSTCI_SOUTH_TO_LOOP_MERGER = mod(
  LOOP_MERGER_NORTH_OFFSET - BELT_POSTCI_SOUTH_TO_LOOP_MERGER_LEN,
)

// Ready-for-Review intake chain: intake_splitter.south → rfr_pole_1
// → rfr_pole_2 → review-merger.south. Anchor at RM.south wall and
// walk backward; the chevron jump lands at the splitter's south
// face (the only stub UV in the chain we don't compute).
const BELT_RFR_POLE2_TO_RM = mod(RM_SOUTH_OFFSET - BELT_RFR_POLE2_TO_RM_LEN)
const RFR_POLE2_OFFSET = mod(BELT_RFR_POLE2_TO_RM - TURN_ARC_LENGTH)
const BELT_RFR_POLE1_TO_RFR_POLE2 = mod(RFR_POLE2_OFFSET - BELT_RFR_POLE1_TO_RFR_POLE2_LEN)
const RFR_POLE1_OFFSET = mod(BELT_RFR_POLE1_TO_RFR_POLE2 - TURN_ARC_LENGTH)
const BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_POLE1 = mod(
  RFR_POLE1_OFFSET - BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_POLE1_LEN,
)
const INTAKE_SPLITTER_SOUTH_OFFSET = mod(BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_POLE1 - ROUTER_STUB_LEN)

// ─── Station specs ────────────────────────────────────────────────

const stationSpec = (col: number, row: number, queued: number, wip: number): Station => ({
  x: col * FLOOR_CELL,
  y: row * FLOOR_CELL,
  z: 0,
  w: STATION_W * FLOOR_CELL,
  d: STATION_D * FLOOR_CELL,
  h: STATION_H,
  queuedCount: queued,
  wipCount: wip,
  ports: [
    { kind: 'input', direction: 'west', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
    { kind: 'output', direction: 'east', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
  ],
})

const PR_OPENED = stationSpec(PR_OPENED_COL, PR_OPENED_ROW, 1, 0)
const READY_FOR_REVIEW = stationSpec(READY_FOR_REVIEW_COL, READY_FOR_REVIEW_ROW, 2, 1)
const NEW_COMMITS = stationSpec(NC_COL, NC_ROW, 4, 2)
const MERGE_CONFLICTS = stationSpec(DEST_COL, MC_ROW, 1, 0)
const CI_PASSED = stationSpec(DEST_COL, CI_PASSED_ROW, 0, 0)
const CI_FAILED = stationSpec(DEST_COL, CI_FAILED_ROW, 2, 1)
const REVIEW_REQUESTED = stationSpec(RR_COL, RR_ROW, 3, 1)
const REVIEW_APPROVED = stationSpec(REVIEW_DEST_COL, REVIEW_APPROVED_ROW, 0, 0)
const REVIEW_COMMENTED = stationSpec(REVIEW_DEST_COL, REVIEW_COMMENTED_ROW, 1, 0)
const CHANGES_REQUESTED = stationSpec(REVIEW_DEST_COL, CHANGES_REQUESTED_ROW, 2, 0)

// ─── Mergers / Splitters ──────────────────────────────────────────

// Merger: 2 inputs (west = spawner intake, south = CI Failed
// loopback), 1 output (east → NC). Same primitive as Router.
// Intake splitter: 1 input west (spawner appears here), 2 outputs
// — east continues to the NC merger; south drops into the
// ready-for-review intake chain.
const INTAKE_SPLITTER: Router = {
  col: INTAKE_SPLITTER_COL,
  row: INTAKE_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

const MERGER: Router = {
  col: MERGER_COL,
  row: MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

const SPLITTER_1: Router = {
  col: S1_COL,
  row: SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    // South output reserved for the "skip-build" branch (future).
    { direction: 'south', kind: 'output' },
  ],
}

const SPLITTER_2: Router = {
  col: S2_COL,
  row: SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// Post-CI: 1 input west (from CI Passed), 3 outputs N/E/S — all
// unwired today. Three outputs leave room for the eventual
// "review continues / loop back / fast-merge" decision.
const POST_CI_SPLITTER: Router = {
  col: POST_CI_COL,
  row: POST_CI_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// Review-intake merger: 2 inputs (west = post-CI east, south =
// changes_requested loopback target — unwired today), 1 output
// (east → Review Requested).
const REVIEW_MERGER: Router = {
  col: RM_COL,
  row: RM_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Review-outcomes splitter: 1 input (west, from RR), 3 outputs
// fanning to the three review states.
const REVIEW_SPLITTER: Router = {
  col: REVIEW_COL,
  row: REVIEW_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// ─── Poles (90° turns) ────────────────────────────────────────────

// "Above" sits at high-row (row 19) — visually above S2. S2 is at
// lower row, so the belt arrives on the pole's SOUTH face and
// arcs east to MC.
const POLE_ABOVE: Pole = {
  col: S2_COL,
  row: POLE_ABOVE_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// "Below" sits at low-row (row 11). S2 is at higher row, so the
// belt arrives on the pole's NORTH face and arcs east to CI
// Failed.
const POLE_BELOW: Pole = {
  col: S2_COL,
  row: POLE_BELOW_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Review fan poles — mirror of CI fan poles but at REVIEW_COL.
const POLE_REVIEW_ABOVE: Pole = {
  col: REVIEW_COL,
  row: POLE_REVIEW_ABOVE_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

const POLE_REVIEW_BELOW: Pole = {
  col: REVIEW_COL,
  row: POLE_REVIEW_BELOW_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// ─── Loopback elements ───────────────────────────────────────────
//
// A merger and two corner poles wrap CI Failed.east (and the
// post-CI splitter's south output) around the bottom of the
// floor and back into the NC merger's south input.
//
// loop_merger: just east of CI Failed. West in (from CI Failed.east),
//   north in (from post-CI splitter.south), south out (toward the
//   bottom-east corner pole).
const LOOP_MERGER: Router = {
  col: LOOP_MERGER_COL,
  row: LOOP_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

// pole_2: bottom-east corner. North in (from loop_merger), west out.
const LOOP_POLE_2: Pole = {
  col: LOOP_POLE2_COL,
  row: LOOP_POLE2_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

// pole_3: bottom-west corner. East in (from pole_2), north out
// (toward merger.south).
const LOOP_POLE_3: Pole = {
  col: LOOP_POLE3_COL,
  row: LOOP_POLE3_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'north', kind: 'output' },
  ],
}

// ─── Ready-for-Review intake poles ────────────────────────────────
//
// rfr_pole_1: directly below S1. North in (from S1.south), east
//   out (toward rfr_pole_2 across row 11).
const RFR_POLE_1: Pole = {
  col: RFR_POLE1_COL,
  row: RFR_POLE1_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// rfr_pole_2: directly below RM. West in (from rfr_pole_1), north
//   out (climbing into RM.south).
const RFR_POLE_2: Pole = {
  col: RFR_POLE2_COL,
  row: RFR_POLE2_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
  ],
}

// ─── PR Opened intake poles ───────────────────────────────────────
//
// Z-shape wrapping under PR Opened. Conveyors leave PR Opened
// east, drop to row 23 below the station, run west under it, drop
// further to row 19, then run east into RFR.west.
const PRO_POLE_1: Pole = {
  col: PRO_POLE1_COL,
  row: PRO_POLE1_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

const PRO_POLE_2: Pole = {
  col: PRO_POLE2_COL,
  row: PRO_POLE2_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

const PRO_POLE_3: Pole = {
  col: PRO_POLE3_COL,
  row: PRO_POLE3_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

const PRO_POLE_4: Pole = {
  col: PRO_POLE4_COL,
  row: PRO_POLE4_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

export interface CameraStateForHUD {
  /** Polar angle from +z axis, in radians. 0 = top-down. */
  pitch: number
  /** Azimuth around +z axis, in radians. */
  yaw: number
  /** Zoom factor relative to the initial view. >1 = zoomed in. */
  zoom: number
}

export interface IsoDebugSceneHandle {
  destroy: () => void
  resetView: () => void
  /** Subscribe to camera state changes. The HUD uses this to render
   * pitch/yaw/zoom live. Returns an unsubscribe function. */
  onCameraChange: (cb: (s: CameraStateForHUD) => void) => () => void
}

export async function createIsoDebugScene(container: HTMLDivElement): Promise<IsoDebugSceneHandle> {
  const canvas = document.createElement('canvas')
  canvas.style.width = '100%'
  canvas.style.height = '100%'
  canvas.style.display = 'block'
  canvas.style.touchAction = 'none'
  container.appendChild(canvas)

  const initialRect = container.getBoundingClientRect()
  const dpr = window.devicePixelRatio || 1
  canvas.width = initialRect.width * dpr
  canvas.height = initialRect.height * dpr

  const renderer = new IsoScene(canvas)
  renderer.buildFloor(FLOOR_SIZE, FLOOR_CELL)

  // ─── Build geometry ────────────────────────────────────────────
  const prOpened = renderer.addStation(PR_OPENED)
  const readyForReview = renderer.addStation(READY_FOR_REVIEW)
  const newCommits = renderer.addStation(NEW_COMMITS)
  const mergeConflicts = renderer.addStation(MERGE_CONFLICTS)
  const ciPassed = renderer.addStation(CI_PASSED)
  const ciFailed = renderer.addStation(CI_FAILED)
  const reviewRequested = renderer.addStation(REVIEW_REQUESTED)
  const reviewApproved = renderer.addStation(REVIEW_APPROVED)
  const reviewCommented = renderer.addStation(REVIEW_COMMENTED)
  const changesRequested = renderer.addStation(CHANGES_REQUESTED)

  const intakeSplitter = renderer.addRouter(INTAKE_SPLITTER, FLOOR_CELL, {
    west: INTAKE_SPLITTER_WEST_OFFSET,
    east: INTAKE_SPLITTER_EAST_OFFSET,
    south: INTAKE_SPLITTER_SOUTH_OFFSET,
  })
  const merger = renderer.addRouter(MERGER, FLOOR_CELL, {
    west: MERGER_WEST_OFFSET,
    south: MERGER_SOUTH_OFFSET,
    east: MERGER_EAST_OFFSET,
  })
  const splitter1 = renderer.addRouter(SPLITTER_1, FLOOR_CELL, {
    west: S1_WEST_OFFSET,
    east: S1_EAST_OFFSET,
    south: S1_SOUTH_OFFSET,
  })
  const splitter2 = renderer.addRouter(SPLITTER_2, FLOOR_CELL, {
    west: S2_WEST_OFFSET,
    north: S2_NORTH_OFFSET,
    east: S2_EAST_OFFSET,
    south: S2_SOUTH_OFFSET,
  })
  const postCi = renderer.addRouter(POST_CI_SPLITTER, FLOOR_CELL, {
    west: POST_CI_WEST_OFFSET,
    north: POST_CI_OUTPUT_OFFSET,
    east: POST_CI_OUTPUT_OFFSET,
    south: POST_CI_OUTPUT_OFFSET,
  })
  const reviewMerger = renderer.addRouter(REVIEW_MERGER, FLOOR_CELL, {
    west: RM_WEST_OFFSET,
    south: RM_SOUTH_OFFSET,
    east: RM_EAST_OFFSET,
  })
  const reviewSplitter = renderer.addRouter(REVIEW_SPLITTER, FLOOR_CELL, {
    west: REVIEW_WEST_OFFSET,
    north: REVIEW_NORTH_OFFSET,
    east: REVIEW_EAST_OFFSET,
    south: REVIEW_SOUTH_OFFSET,
  })

  const poleAbove = renderer.addPole(POLE_ABOVE, FLOOR_CELL, POLE_ABOVE_OFFSET)
  const poleBelow = renderer.addPole(POLE_BELOW, FLOOR_CELL, POLE_BELOW_OFFSET)
  const poleReviewAbove = renderer.addPole(POLE_REVIEW_ABOVE, FLOOR_CELL, POLE_REVIEW_ABOVE_OFFSET)
  const poleReviewBelow = renderer.addPole(POLE_REVIEW_BELOW, FLOOR_CELL, POLE_REVIEW_BELOW_OFFSET)
  const loopMerger = renderer.addRouter(LOOP_MERGER, FLOOR_CELL, {
    west: LOOP_MERGER_WEST_OFFSET,
    north: LOOP_MERGER_NORTH_OFFSET,
    south: LOOP_MERGER_SOUTH_OFFSET,
  })
  const loopPole2 = renderer.addPole(LOOP_POLE_2, FLOOR_CELL, LOOP_POLE2_OFFSET)
  const loopPole3 = renderer.addPole(LOOP_POLE_3, FLOOR_CELL, LOOP_POLE3_OFFSET)
  const rfrPole1 = renderer.addPole(RFR_POLE_1, FLOOR_CELL, RFR_POLE1_OFFSET)
  const rfrPole2 = renderer.addPole(RFR_POLE_2, FLOOR_CELL, RFR_POLE2_OFFSET)
  const proPole1 = renderer.addPole(PRO_POLE_1, FLOOR_CELL, PRO_POLE1_OFFSET)
  const proPole2 = renderer.addPole(PRO_POLE_2, FLOOR_CELL, PRO_POLE2_OFFSET)
  const proPole3 = renderer.addPole(PRO_POLE_3, FLOOR_CELL, PRO_POLE3_OFFSET)
  const proPole4 = renderer.addPole(PRO_POLE_4, FLOOR_CELL, PRO_POLE4_OFFSET)

  // ─── Connecting belts ──────────────────────────────────────────
  // PR Opened.east → 4 corner poles → RFR.west.
  const beltPrOpenedToPole1 = renderer.addBelt(
    prOpened.ports[1], // PR Opened east output
    proPole1.ports.get('west')!,
    BELT_PRO_TO_POLE1,
    false,
    false,
  )
  const beltPole1ToPole2 = renderer.addBelt(
    proPole1.ports.get('south')!,
    proPole2.ports.get('north')!,
    BELT_POLE1_TO_POLE2,
    false,
    false,
  )
  const beltPole2ToPole3 = renderer.addBelt(
    proPole2.ports.get('west')!,
    proPole3.ports.get('east')!,
    BELT_POLE2_TO_POLE3,
    false,
    false,
  )
  const beltPole3ToPole4 = renderer.addBelt(
    proPole3.ports.get('south')!,
    proPole4.ports.get('north')!,
    BELT_POLE3_TO_POLE4,
    false,
    false,
  )
  const beltPole4ToRfr = renderer.addBelt(
    proPole4.ports.get('east')!,
    readyForReview.ports[0], // RFR west input
    BELT_POLE4_TO_RFR,
    false,
    false,
  )
  const beltRfrToIntakeSplitter = renderer.addBelt(
    readyForReview.ports[1], // RFR east output
    intakeSplitter.ports.get('west')!,
    BELT_RFR_TO_INTAKE_SPLITTER,
    false,
    false,
  )
  const beltIntakeSplitterToMerger = renderer.addBelt(
    intakeSplitter.ports.get('east')!,
    merger.ports.get('west')!,
    BELT_INTAKE_SPLITTER_TO_MERGER,
    false,
    false,
  )
  const beltMergerToNc = renderer.addBelt(
    merger.ports.get('east')!,
    newCommits.ports[0], // NC west input
    BELT_MERGER_TO_NC,
    false,
    false,
  )
  const beltNcToS1 = renderer.addBelt(
    newCommits.ports[1], // NC east output
    splitter1.ports.get('west')!,
    BELT_NC_TO_S1,
    false,
    false,
  )
  const beltS1ToS2 = renderer.addBelt(
    splitter1.ports.get('east')!,
    splitter2.ports.get('west')!,
    BELT_S1_TO_S2,
    false,
    false,
  )
  const beltS2ToCiPassed = renderer.addBelt(
    splitter2.ports.get('east')!,
    ciPassed.ports[0], // CI Passed west input
    BELT_S2_TO_CI_PASSED,
    false,
    false,
  )
  const beltCiPassedToPostCi = renderer.addBelt(
    ciPassed.ports[1], // CI Passed east output
    postCi.ports.get('west')!,
    BELT_CI_PASSED_TO_POST_CI,
    false,
    false,
  )
  // Above-S2 chain: S2.NORTH → pole_above.SOUTH → MC.
  const beltS2ToPoleAbove = renderer.addBelt(
    splitter2.ports.get('north')!,
    poleAbove.ports.get('south')!,
    BELT_S2_TO_POLE_ABOVE,
    false,
    false,
  )
  const beltPoleAboveToMc = renderer.addBelt(
    poleAbove.ports.get('east')!,
    mergeConflicts.ports[0],
    BELT_POLE_ABOVE_TO_MC,
    false,
    false,
  )
  // Below-S2 chain (mirror): S2.SOUTH → pole_below.NORTH → CI Failed.
  const beltS2ToPoleBelow = renderer.addBelt(
    splitter2.ports.get('south')!,
    poleBelow.ports.get('north')!,
    BELT_S2_TO_POLE_BELOW,
    false,
    false,
  )
  const beltPoleBelowToCiFailed = renderer.addBelt(
    poleBelow.ports.get('east')!,
    ciFailed.ports[0],
    BELT_POLE_BELOW_TO_CI_FAILED,
    false,
    false,
  )
  // Review chain: post-CI.east → review merger → RR → review splitter → 3 fan.
  const beltPostCiToRm = renderer.addBelt(
    postCi.ports.get('east')!,
    reviewMerger.ports.get('west')!,
    BELT_POSTCI_TO_RM,
    false,
    false,
  )
  const beltRmToRr = renderer.addBelt(
    reviewMerger.ports.get('east')!,
    reviewRequested.ports[0], // RR west input
    BELT_RM_TO_RR,
    false,
    false,
  )
  const beltRrToReview = renderer.addBelt(
    reviewRequested.ports[1], // RR east output
    reviewSplitter.ports.get('west')!,
    BELT_RR_TO_REVIEW,
    false,
    false,
  )
  const beltReviewToReviewCommented = renderer.addBelt(
    reviewSplitter.ports.get('east')!,
    reviewCommented.ports[0],
    BELT_REVIEW_TO_REVIEW_COMMENTED,
    false,
    false,
  )
  // Review-above chain: review.NORTH → pole.SOUTH → review_approved.
  const beltReviewToPoleReviewAbove = renderer.addBelt(
    reviewSplitter.ports.get('north')!,
    poleReviewAbove.ports.get('south')!,
    BELT_REVIEW_TO_POLE_REVIEW_ABOVE,
    false,
    false,
  )
  const beltPoleReviewAboveToRa = renderer.addBelt(
    poleReviewAbove.ports.get('east')!,
    reviewApproved.ports[0],
    BELT_POLE_REVIEW_ABOVE_TO_RA,
    false,
    false,
  )
  // Review-below chain (mirror): review.SOUTH → pole.NORTH → changes_requested.
  const beltReviewToPoleReviewBelow = renderer.addBelt(
    reviewSplitter.ports.get('south')!,
    poleReviewBelow.ports.get('north')!,
    BELT_REVIEW_TO_POLE_REVIEW_BELOW,
    false,
    false,
  )
  const beltPoleReviewBelowToCr = renderer.addBelt(
    poleReviewBelow.ports.get('east')!,
    changesRequested.ports[0],
    BELT_POLE_REVIEW_BELOW_TO_CR,
    false,
    false,
  )

  // CI Failed loopback: CIF.east + post-CI.south → loop_merger →
  // pole_2 → pole_3 → NC merger.south.
  const beltCifToLoopMerger = renderer.addBelt(
    ciFailed.ports[1], // CI Failed east output
    loopMerger.ports.get('west')!,
    BELT_CIF_TO_LOOP_MERGER,
    false,
    false,
  )
  const beltPostCiSouthToLoopMerger = renderer.addBelt(
    postCi.ports.get('south')!,
    loopMerger.ports.get('north')!,
    BELT_POSTCI_SOUTH_TO_LOOP_MERGER,
    false,
    false,
  )
  const beltLoopMergerToLoop2 = renderer.addBelt(
    loopMerger.ports.get('south')!,
    loopPole2.ports.get('north')!,
    BELT_LOOP_MERGER_TO_LOOP2,
    false,
    false,
  )
  const beltLoop2ToLoop3 = renderer.addBelt(
    loopPole2.ports.get('west')!,
    loopPole3.ports.get('east')!,
    BELT_LOOP2_TO_LOOP3,
    false,
    false,
  )
  const beltLoop3ToMerger = renderer.addBelt(
    loopPole3.ports.get('north')!,
    merger.ports.get('south')!,
    BELT_LOOP3_TO_MERGER,
    false,
    false,
  )
  // Ready-for-Review intake: intake_splitter.south → rfr_pole_1
  // → rfr_pole_2 → RM.south. Both vertical legs (col 1 and col 24)
  // sit clear of the loopback's horizontal belt (row 12, cols
  // 4–21), so the new chain doesn't cross any existing belts.
  const beltIntakeSplitterSouthToRfrPole1 = renderer.addBelt(
    intakeSplitter.ports.get('south')!,
    rfrPole1.ports.get('north')!,
    BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_POLE1,
    false,
    false,
  )
  const beltRfrPole1ToRfrPole2 = renderer.addBelt(
    rfrPole1.ports.get('east')!,
    rfrPole2.ports.get('west')!,
    BELT_RFR_POLE1_TO_RFR_POLE2,
    false,
    false,
  )
  const beltRfrPole2ToRm = renderer.addBelt(
    rfrPole2.ports.get('north')!,
    reviewMerger.ports.get('south')!,
    BELT_RFR_POLE2_TO_RM,
    false,
    false,
  )

  // ─── Item path graph ──────────────────────────────────────────
  // Spawn at intake splitter's west input. Every station the items
  // must transit (NC, CI Passed, CI Failed) is wired passthrough
  // so chips don't dead-end at every recess. MC remains a true
  // terminal — its west input has no `.next`, so chips entering
  // it despawn at the recess back.

  // Intake splitter: west input fans to east (→ merger) and south
  // (→ ready-for-review intake chain). Simulator picks next[0] =
  // east, so today every chip continues into the NC merger.
  // PR Opened (passthrough) → 4 corner poles → RFR (passthrough)
  // → belt → intake splitter.
  prOpened.ports[0].segment!.next = [prOpened.ports[1].segment!]
  prOpened.ports[1].segment!.next = [beltPrOpenedToPole1.segment]
  beltPrOpenedToPole1.segment.next = [proPole1.internalSegment]
  proPole1.internalSegment.next = [beltPole1ToPole2.segment]
  beltPole1ToPole2.segment.next = [proPole2.internalSegment]
  proPole2.internalSegment.next = [beltPole2ToPole3.segment]
  beltPole2ToPole3.segment.next = [proPole3.internalSegment]
  proPole3.internalSegment.next = [beltPole3ToPole4.segment]
  beltPole3ToPole4.segment.next = [proPole4.internalSegment]
  proPole4.internalSegment.next = [beltPole4ToRfr.segment]
  beltPole4ToRfr.segment.next = [readyForReview.ports[0].segment!]
  readyForReview.ports[0].segment!.next = [readyForReview.ports[1].segment!]
  readyForReview.ports[1].segment!.next = [beltRfrToIntakeSplitter.segment]
  beltRfrToIntakeSplitter.segment.next = [intakeSplitter.ports.get('west')!.segment!]
  intakeSplitter.ports.get('west')!.segment!.next = [
    intakeSplitter.ports.get('east')!.segment!,
    intakeSplitter.ports.get('south')!.segment!,
  ]
  intakeSplitter.ports.get('east')!.segment!.next = [beltIntakeSplitterToMerger.segment]
  beltIntakeSplitterToMerger.segment.next = [merger.ports.get('west')!.segment!]

  // Merger: both inputs converge to east output.
  merger.ports.get('west')!.segment!.next = [merger.ports.get('east')!.segment!]
  merger.ports.get('south')!.segment!.next = [merger.ports.get('east')!.segment!]
  merger.ports.get('east')!.segment!.next = [beltMergerToNc.segment]

  // intake_splitter.south → ready-for-review intake chain → RM.south.
  intakeSplitter.ports.get('south')!.segment!.next = [beltIntakeSplitterSouthToRfrPole1.segment]
  beltIntakeSplitterSouthToRfrPole1.segment.next = [rfrPole1.internalSegment]
  rfrPole1.internalSegment.next = [beltRfrPole1ToRfrPole2.segment]
  beltRfrPole1ToRfrPole2.segment.next = [rfrPole2.internalSegment]
  rfrPole2.internalSegment.next = [beltRfrPole2ToRm.segment]
  beltRfrPole2ToRm.segment.next = [reviewMerger.ports.get('south')!.segment!]

  // Merger → NC (passthrough) → S1.
  beltMergerToNc.segment.next = [newCommits.ports[0].segment!]
  newCommits.ports[0].segment!.next = [newCommits.ports[1].segment!]
  newCommits.ports[1].segment!.next = [beltNcToS1.segment]
  beltNcToS1.segment.next = [splitter1.ports.get('west')!.segment!]

  // S1: list both outputs in next[]; simulator picks index 0 = east.
  splitter1.ports.get('west')!.segment!.next = [
    splitter1.ports.get('east')!.segment!,
    splitter1.ports.get('south')!.segment!,
  ]
  splitter1.ports.get('east')!.segment!.next = [beltS1ToS2.segment]
  // S1.south stays a dead-end stub (reserved for the future
  // skip-build branch); no belt connects to it today.

  // S1 → S2.
  beltS1ToS2.segment.next = [splitter2.ports.get('west')!.segment!]
  splitter2.ports.get('west')!.segment!.next = [
    splitter2.ports.get('east')!.segment!,
    splitter2.ports.get('north')!.segment!,
    splitter2.ports.get('south')!.segment!,
  ]

  // S2.east → CI Passed (passthrough) → post-CI.
  splitter2.ports.get('east')!.segment!.next = [beltS2ToCiPassed.segment]
  beltS2ToCiPassed.segment.next = [ciPassed.ports[0].segment!]
  ciPassed.ports[0].segment!.next = [ciPassed.ports[1].segment!]
  ciPassed.ports[1].segment!.next = [beltCiPassedToPostCi.segment]
  beltCiPassedToPostCi.segment.next = [postCi.ports.get('west')!.segment!]
  postCi.ports.get('west')!.segment!.next = [
    postCi.ports.get('east')!.segment!,
    postCi.ports.get('north')!.segment!,
    postCi.ports.get('south')!.segment!,
  ]
  // Post-CI: east extends into the review chain below; south
  // feeds back into the loop merger (wired in the loopback
  // section); north stays dead-ended (staged for future wiring).

  // post-CI.east → review merger → RR (passthrough) → review splitter.
  postCi.ports.get('east')!.segment!.next = [beltPostCiToRm.segment]
  beltPostCiToRm.segment.next = [reviewMerger.ports.get('west')!.segment!]
  reviewMerger.ports.get('west')!.segment!.next = [reviewMerger.ports.get('east')!.segment!]
  // reviewMerger.south now receives the ready-for-review intake
  // chain. It will eventually also receive the changes_requested
  // loopback (multiple inputs → single east output).
  reviewMerger.ports.get('south')!.segment!.next = [reviewMerger.ports.get('east')!.segment!]
  reviewMerger.ports.get('east')!.segment!.next = [beltRmToRr.segment]
  beltRmToRr.segment.next = [reviewRequested.ports[0].segment!]
  reviewRequested.ports[0].segment!.next = [reviewRequested.ports[1].segment!]
  reviewRequested.ports[1].segment!.next = [beltRrToReview.segment]
  beltRrToReview.segment.next = [reviewSplitter.ports.get('west')!.segment!]
  reviewSplitter.ports.get('west')!.segment!.next = [
    reviewSplitter.ports.get('east')!.segment!,
    reviewSplitter.ports.get('north')!.segment!,
    reviewSplitter.ports.get('south')!.segment!,
  ]

  // review.east → review_commented (terminal).
  reviewSplitter.ports.get('east')!.segment!.next = [beltReviewToReviewCommented.segment]
  beltReviewToReviewCommented.segment.next = [reviewCommented.ports[0].segment!]
  // reviewCommented.west has no .next — terminal.

  // review.north → pole_review_above → review_approved (terminal).
  reviewSplitter.ports.get('north')!.segment!.next = [beltReviewToPoleReviewAbove.segment]
  beltReviewToPoleReviewAbove.segment.next = [poleReviewAbove.internalSegment]
  poleReviewAbove.internalSegment.next = [beltPoleReviewAboveToRa.segment]
  beltPoleReviewAboveToRa.segment.next = [reviewApproved.ports[0].segment!]

  // review.south → pole_review_below → changes_requested (terminal).
  reviewSplitter.ports.get('south')!.segment!.next = [beltReviewToPoleReviewBelow.segment]
  beltReviewToPoleReviewBelow.segment.next = [poleReviewBelow.internalSegment]
  poleReviewBelow.internalSegment.next = [beltPoleReviewBelowToCr.segment]
  beltPoleReviewBelowToCr.segment.next = [changesRequested.ports[0].segment!]
  // changes_requested.west has no .next — terminal until loopback wires up.

  // S2.north → pole_above → MC (terminal).
  splitter2.ports.get('north')!.segment!.next = [beltS2ToPoleAbove.segment]
  beltS2ToPoleAbove.segment.next = [poleAbove.internalSegment]
  poleAbove.internalSegment.next = [beltPoleAboveToMc.segment]
  beltPoleAboveToMc.segment.next = [mergeConflicts.ports[0].segment!]
  // MC.west has no .next — true terminal.

  // S2.south → pole_below → CI Failed (passthrough) → loopback.
  splitter2.ports.get('south')!.segment!.next = [beltS2ToPoleBelow.segment]
  beltS2ToPoleBelow.segment.next = [poleBelow.internalSegment]
  poleBelow.internalSegment.next = [beltPoleBelowToCiFailed.segment]
  beltPoleBelowToCiFailed.segment.next = [ciFailed.ports[0].segment!]
  ciFailed.ports[0].segment!.next = [ciFailed.ports[1].segment!]
  ciFailed.ports[1].segment!.next = [beltCifToLoopMerger.segment]
  beltCifToLoopMerger.segment.next = [loopMerger.ports.get('west')!.segment!]
  // Post-CI south output now also feeds the loop merger (the
  // "people commit even when CI passes" branch). Both inputs
  // converge to the merger's south output.
  postCi.ports.get('south')!.segment!.next = [beltPostCiSouthToLoopMerger.segment]
  beltPostCiSouthToLoopMerger.segment.next = [loopMerger.ports.get('north')!.segment!]
  loopMerger.ports.get('west')!.segment!.next = [loopMerger.ports.get('south')!.segment!]
  loopMerger.ports.get('north')!.segment!.next = [loopMerger.ports.get('south')!.segment!]
  loopMerger.ports.get('south')!.segment!.next = [beltLoopMergerToLoop2.segment]
  beltLoopMergerToLoop2.segment.next = [loopPole2.internalSegment]
  loopPole2.internalSegment.next = [beltLoop2ToLoop3.segment]
  beltLoop2ToLoop3.segment.next = [loopPole3.internalSegment]
  loopPole3.internalSegment.next = [beltLoop3ToMerger.segment]
  beltLoop3ToMerger.segment.next = [merger.ports.get('south')!.segment!]

  // ─── Spawner ──────────────────────────────────────────────────
  // Emit one item every 1.5s at PR Opened's west input. Items
  // appear at PR Opened's outside face, traverse the station, ride
  // through the 4-pole Z-chain into RFR, then continue east into
  // the intake splitter, merger, and NC chain.
  renderer.startItemSpawner(prOpened.ports[0].segment!, 1.5, {
    namespaces: ['triage-factory', 'claude-code', 'SKY', 'ENG'],
  })

  const ro = new ResizeObserver(() => {
    renderer.resize()
  })
  ro.observe(container)

  return {
    destroy: () => {
      ro.disconnect()
      renderer.destroy()
      canvas.remove()
    },
    resetView: () => renderer.resetView(),
    onCameraChange: (cb) => {
      // Throttle to one notification per animation frame — Babylon's
      // observable can fire on every input pixel, the HUD doesn't
      // need that resolution.
      let raf: number | null = null
      const snapshot = (): CameraStateForHUD => ({
        pitch: renderer.camera.beta,
        yaw: renderer.camera.alpha,
        zoom: INITIAL_ZOOM_RADIUS / renderer.camera.radius,
      })
      const wrapped = () => {
        if (raf != null) return
        raf = requestAnimationFrame(() => {
          raf = null
          cb(snapshot())
        })
      }
      const observer = renderer.camera.onViewMatrixChangedObservable.add(wrapped)
      cb(snapshot())
      return () => {
        if (raf != null) cancelAnimationFrame(raf)
        renderer.camera.onViewMatrixChangedObservable.remove(observer)
      }
    },
  }
}
