// Garry kiosk — gallery view.
// Single-page touchscreen UI for a gallery visitor to play against Garry.
// Read-only on-screen board (visitor plays on the physical board); board state
// is driven entirely by board-snapshot polling.

import {
  createRobotClient,
  GenericServiceClient,
  Struct,
  type JsonValue,
} from "@viamrobotics/sdk";
import Cookies from "js-cookie";

// ── Types ──────────────────────────────────────────────────────────────────

interface MachineCookie {
  apiKey: { id: string; key: string };
  hostname: string;
  machineName?: string;
}

interface Snapshot {
  fen: string;
  turn?: string;
  is_over?: boolean;
  outcome?: string;
  method?: string;
  in_check?: boolean;
  auto?: boolean;
  captured_at_ms?: number;
  event?: string;
  // Per-square camera detection: { "e4": "1", "e2": "0", ... }
  // "0" = empty, "1" = white piece, "2" = black piece.
  camera_board?: Record<string, string>;
}

type Outcome = "win" | "loss" | "draw";

type KioskState =
  | { name: "connecting" }
  | { name: "connection_lost" }
  | { name: "idle" }
  | { name: "onboarding" }
  | { name: "your_turn"; snap: Snapshot }
  | { name: "garry_turn"; snap: Snapshot }
  | { name: "game_over"; snap: Snapshot; outcome: Outcome };

// ── Constants ──────────────────────────────────────────────────────────────

const CHESS_SERVICE_NAME = "chess";
const ACTIVE_REFRESH_MS = 500;
const IDLE_RETURN_MS = 60_000;
const RECONNECT_DELAY_MS = 3_000;
const OPERATOR_LONG_PRESS_MS = 3_000;
const OPERATOR_AUTO_DISMISS_MS = 30_000;
const ONBOARDING_FLAG = "garry-onboarding-seen";

type Side = "white" | "black";
const VISITOR_SIDE: Side = "white"; // visitor plays white, Garry plays black

const STARTING_FEN_BOARD = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR";

// ── Module state ───────────────────────────────────────────────────────────

let chessService: GenericServiceClient | null = null;
let currentState: KioskState = { name: "connecting" };
let pollTimer: ReturnType<typeof setInterval> | null = null;
let lastActivityAt = Date.now();
let consecutivePollErrors = 0;
let operatorOpen = false;

const root = document.getElementById("root")!;

// ── Helpers ────────────────────────────────────────────────────────────────

function fenBoard(fen: string): string {
  return fen.split(" ")[0] ?? "";
}

// Normalize whose-turn-is-it across the two representations the server uses:
// the snapshot's `turn` field comes back as "white"/"black" from
// gameEventsResult, but if that field is missing we can still parse the FEN's
// w/b active-color marker.
function snapSide(snap: Snapshot): Side {
  if (snap.turn === "white" || snap.turn === "black") return snap.turn;
  const fenColor = (snap.fen.split(" ")[1] ?? "w").toLowerCase();
  return fenColor === "b" ? "black" : "white";
}

function isStartingPosition(fen: string): boolean {
  return fenBoard(fen) === STARTING_FEN_BOARD;
}

// Find the file+rank square ("e1", "c8", etc.) of the king of the given side
// in the FEN's board section. Returns null when the king isn't present (only
// happens in malformed positions; chess can't legally remove a king).
function findKingSquare(fen: string, side: Side): string | null {
  const target = side === "white" ? "K" : "k";
  const rows = fenBoard(fen).split("/");
  for (let rankIdx = 0; rankIdx < rows.length; rankIdx++) {
    const row = rows[rankIdx];
    let fileIdx = 0;
    for (const ch of row) {
      if (/[1-8]/.test(ch)) {
        fileIdx += +ch;
      } else {
        if (ch === target) {
          return FILES[fileIdx] + RANKS[rankIdx];
        }
        fileIdx++;
      }
    }
  }
  return null;
}

// Diff two FEN board strings and return the {from, to} most-recently-moved
// pair. Standard moves emptied one square ("from") and filled another ("to").
// Castling and en passant produce 3–4 diffs; in those cases we pick the
// king/pawn pair via simple heuristics. Returns null when we can't pin it.
// Diff the FEN's expected per-square color against the camera's reported
// per-square color. Returns the disagreement count plus best-effort from/to
// for surfacing as ring highlights. "from" = a square the FEN expects to
// have a piece but the camera sees empty; "to" = the inverse.
function diffCameraVsFen(snap: Snapshot): { from?: string; to?: string; count: number } {
  if (!snap.camera_board) return { count: 0 };
  const grid = parsePosition(snap.fen);
  let from: string | undefined;
  let to: string | undefined;
  let count = 0;
  for (let rankIdx = 0; rankIdx < 8; rankIdx++) {
    for (let fileIdx = 0; fileIdx < 8; fileIdx++) {
      const piece = grid[rankIdx]?.[fileIdx];
      const expected = piece ? (piece.color === "w" ? "1" : "2") : "0";
      const sqName = FILES[fileIdx] + RANKS[rankIdx];
      const actual = snap.camera_board[sqName];
      if (actual === undefined || actual === expected) continue;
      count++;
      if (expected !== "0" && actual === "0") from = sqName;
      else if (expected === "0") to = sqName;
    }
  }
  return { from, to, count };
}

function isShowingIllegal(): boolean {
  if (firstDisagreementAt == null) return false;
  return Date.now() - firstDisagreementAt > ILLEGAL_DEBOUNCE_MS;
}

function diffFenForLastMove(prevFen: string, nextFen: string): { from: string; to: string } | null {
  if (!prevFen || prevFen === nextFen) return null;
  const prev = parsePosition(prevFen);
  const next = parsePosition(nextFen);
  const emptied: string[] = [];
  const filled: string[] = [];
  const changed: string[] = [];
  for (let rankIdx = 0; rankIdx < 8; rankIdx++) {
    for (let fileIdx = 0; fileIdx < 8; fileIdx++) {
      const p = prev[rankIdx]?.[fileIdx] ?? null;
      const n = next[rankIdx]?.[fileIdx] ?? null;
      if (p === n) continue;
      const pType = p ? `${p.color}${p.type}` : "";
      const nType = n ? `${n.color}${n.type}` : "";
      if (pType === nType) continue;
      const sq = FILES[fileIdx] + RANKS[rankIdx];
      changed.push(sq);
      if (!n) emptied.push(sq);
      else if (!p) filled.push(sq);
      else filled.push(sq); // capture — piece replaced
    }
  }
  if (emptied.length === 1 && filled.length === 1) {
    return { from: emptied[0], to: filled[0] };
  }
  // Castling: 4 squares involved (king from/to + rook from/to). Highlight
  // the king's move so the eye lands on the more semantically meaningful pair.
  if (changed.length === 4) {
    const isKingSide = ["e1", "g1", "f1", "h1"].every((s) => changed.includes(s));
    const isKingSideBlack = ["e8", "g8", "f8", "h8"].every((s) => changed.includes(s));
    const isQueenSide = ["e1", "c1", "d1", "a1"].every((s) => changed.includes(s));
    const isQueenSideBlack = ["e8", "c8", "d8", "a8"].every((s) => changed.includes(s));
    if (isKingSide) return { from: "e1", to: "g1" };
    if (isKingSideBlack) return { from: "e8", to: "g8" };
    if (isQueenSide) return { from: "e1", to: "c1" };
    if (isQueenSideBlack) return { from: "e8", to: "c8" };
  }
  return null;
}

function classifyOutcome(snap: Snapshot): Outcome {
  // Server returns outcome as "white_won" / "black_won" / "draw" / "in_progress"
  // (do_command.go gameEventsResult). Map to visitor-frame outcome.
  const o = (snap.outcome ?? "").trim();
  if (o === "white_won") return VISITOR_SIDE === "white" ? "win" : "loss";
  if (o === "black_won") return VISITOR_SIDE === "white" ? "loss" : "win";
  return "draw";
}

// Map a fresh snapshot to whichever active game state it implies. Used by
// Begin / Play Garry / Play again when we explicitly want to leave a
// holding state. Does NOT have the "stay on attract" guard.
function snapshotToActiveState(snap: Snapshot): KioskState {
  if (snap.is_over) return { name: "game_over", snap, outcome: classifyOutcome(snap) };
  const turn = snapSide(snap);
  if (turn === VISITOR_SIDE) return { name: "your_turn", snap };
  return { name: "garry_turn", snap };
}

function deriveState(snap: Snapshot, previous: KioskState): KioskState {
  // Poll-protection: while the visitor is still on attract / onboarding,
  // snapshots from the loop shouldn't pull them into a game state. Explicit
  // user actions (Begin, Play Garry, Play again) call snapshotToActiveState
  // directly and bypass this guard.
  if (previous.name === "idle" || previous.name === "onboarding") return previous;
  return snapshotToActiveState(snap);
}

// stateKey returns a stable identity for a kiosk state so that snapshot polls
// that produce structurally-equivalent states don't trigger a re-render (and
// don't replay the 240ms state-fade-in animation, which manifests as a flicker
// twice per second when the snapshot poll runs).
function stateKey(s: KioskState): string {
  switch (s.name) {
    case "connecting":
    case "connection_lost":
    case "idle":
    case "onboarding":
      return s.name;
    case "your_turn":
      return `${s.name}|${s.snap.fen}|${s.snap.in_check ? "chk" : ""}|${isShowingIllegal() ? "ill" : ""}`;
    case "garry_turn":
      return `${s.name}|${s.snap.fen}|${s.snap.in_check ? "chk" : ""}`;
    case "game_over":
      return `${s.name}|${s.outcome}|${s.snap.fen}`;
  }
}

function setState(next: KioskState) {
  const prevKey = stateKey(currentState);
  const nextKey = stateKey(next);
  if (prevKey !== nextKey) {
    console.log(`[halcyon] state ${prevKey} → ${nextKey}`);
  }
  currentState = next;
  if (prevKey === nextKey) return;
  render();
}

function ev(tag: string, attrs: Record<string, string | null> = {}, ...children: (Node | string)[]) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on") && typeof v === "string") (el as any)[k] = v;
    else el.setAttribute(k, v);
  }
  for (const c of children) {
    el.append(c instanceof Node ? c : document.createTextNode(c));
  }
  return el;
}

function svgEl(tag: string, attrs: Record<string, string | number | null> = {}, ...children: SVGElement[]) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null) continue;
    el.setAttribute(k, String(v));
  }
  for (const c of children) el.append(c);
  return el as SVGElement;
}

// ── Board rendering ────────────────────────────────────────────────────────

const FILES = ["a", "b", "c", "d", "e", "f", "g", "h"];
const RANKS = ["8", "7", "6", "5", "4", "3", "2", "1"];

function parsePosition(fen: string): ({ type: string; color: "w" | "b" } | null)[][] {
  const board = fenBoard(fen);
  const rows = board.split("/");
  return rows.map((row) => {
    const out: ({ type: string; color: "w" | "b" } | null)[] = [];
    for (const ch of row) {
      if (/[1-8]/.test(ch)) {
        for (let i = 0; i < +ch; i++) out.push(null);
      } else {
        out.push({
          type: ch.toLowerCase(),
          color: ch === ch.toUpperCase() ? "w" : "b",
        });
      }
    }
    return out;
  });
}

// Real piece SVGs from ../pieces/. Vite glob import gives us URLs the
// <image> element can reference directly.
const pieceSvgs = import.meta.glob("../pieces/*.svg", {
  eager: true,
  query: "?url",
  import: "default",
}) as Record<string, string>;

const PIECE_FILE: Record<string, string> = {
  K: "white-king",
  Q: "white-queen",
  R: "white-rook",
  B: "white-bishop",
  N: "white-knight",
  P: "white-pawn",
  k: "black-king",
  q: "black-queen",
  r: "black-rook",
  b: "black-bishop",
  n: "black-knight",
  p: "black-pawn",
};

function pieceUrl(type: string, color: "w" | "b"): string | undefined {
  const fen = color === "w" ? type.toUpperCase() : type.toLowerCase();
  const name = PIECE_FILE[fen];
  if (!name) return undefined;
  return pieceSvgs[`../pieces/${name}.svg`];
}

interface BoardOptions {
  position: string;
  size: number;
  showCoordinates: boolean;
  thinking?: boolean;
  breathePiece?: string; // square name e.g. "e2"
  lastMove?: { from: string; to: string } | null;
  checkSquare?: string | null; // king-in-check ring
  illegalSquares?: string[]; // brick rings around the offending squares (illegal move attempt)
}

function renderBoard(opts: BoardOptions): HTMLElement {
  const { position, size, showCoordinates, thinking, breathePiece, lastMove, checkSquare, illegalSquares } = opts;
  const illegalSet = new Set(illegalSquares ?? []);
  const grid = parsePosition(position);
  const sq = size / 8;

  const wrap = ev("div", {
    class: "board",
    style: `width:${size}px;height:${size}px`,
  });

  if (thinking) wrap.append(ev("div", { class: "thinking-outline board-pulse" }));

  const svg = svgEl("svg", { viewBox: `0 0 ${size} ${size}`, width: size, height: size });
  for (let rankIdx = 0; rankIdx < 8; rankIdx++) {
    for (let fileIdx = 0; fileIdx < 8; fileIdx++) {
      const x = fileIdx * sq;
      const y = rankIdx * sq;
      const dark = (fileIdx + rankIdx) % 2 === 1;
      const sqName = FILES[fileIdx] + RANKS[rankIdx];
      const isLast = lastMove && (sqName === lastMove.from || sqName === lastMove.to);
      const piece = grid[rankIdx]?.[fileIdx];

      const cell = svgEl("g", { transform: `translate(${x} ${y})` });
      cell.append(
        svgEl("rect", {
          width: sq,
          height: sq,
          fill: dark ? "var(--board-dark)" : "var(--board-light)",
        }),
      );
      if (isLast) {
        cell.append(
          svgEl("rect", {
            width: sq,
            height: sq,
            fill: "var(--last-move)",
            opacity: dark ? 0.55 : 0.7,
          }),
        );
      }
      if (piece) {
        const url = pieceUrl(piece.type, piece.color);
        if (url) {
          const inset = sq * 0.08;
          const inner = sq - inset * 2;
          const image = svgEl("image", {
            href: url,
            x: inset,
            y: inset,
            width: inner,
            height: inner,
            preserveAspectRatio: "xMidYMid meet",
          });
          if (breathePiece === sqName) {
            (image as Element).setAttribute("class", "piece-breathe");
          }
          cell.append(image);
        }
      }
      // Warning rings (king-in-check, illegal-move squares) — drawn over the
      // piece so they're not occluded. Both use the loss tone; they don't
      // co-occur in practice (illegal would mean the move wasn't accepted).
      if (checkSquare === sqName || illegalSet.has(sqName)) {
        cell.append(
          svgEl("rect", {
            x: 2,
            y: 2,
            width: sq - 4,
            height: sq - 4,
            fill: "none",
            stroke: "var(--status-loss)",
            "stroke-width": 3,
          }),
        );
      }
      svg.append(cell);
    }
  }
  wrap.append(svg);

  if (showCoordinates) {
    FILES.forEach((f, i) => {
      const c = ev("div", {
        class: "board-coord",
        style: `left:${i * sq + sq - 14}px;bottom:4px`,
      });
      c.textContent = f;
      wrap.append(c);
    });
    RANKS.forEach((r, i) => {
      const c = ev("div", {
        class: "board-coord",
        style: `top:${i * sq + 4}px;left:4px`,
      });
      c.textContent = r;
      wrap.append(c);
    });
  }

  return wrap;
}

// ── Frame renderers ────────────────────────────────────────────────────────

function statusRow(tone: string, label: string, pulsing = false): HTMLElement {
  const row = ev("div", {
    class: "status-row",
    style: `color:var(--status-${tone})`,
  });
  if (pulsing) {
    row.append(
      ev("span", {
        class: "status-dot dot-pulse",
        style: `background:var(--status-${tone})`,
      }),
    );
  }
  row.append(label);
  return row;
}

function pedestal(name: string, dim = false): HTMLElement {
  const el = ev("div", { class: "pedestal" + (dim ? " dim" : "") });
  el.textContent = name;
  return el;
}

function footerNote(text: string): HTMLElement {
  const el = ev("div", { class: "footer-note" });
  el.textContent = text;
  return el;
}

function renderConnecting(): HTMLElement {
  const state = ev("div", { class: "state" });
  const center = ev("div", { class: "connecting-center" });
  center.append(textNode("div", "connecting-wordmark", "Garry · a chess robot"));

  const bar = ev("div", { class: "connecting-hairline" });
  bar.append(ev("div", { class: "connecting-bar" }));
  center.append(bar);

  center.append(textNode("div", "connecting-headline", "Connecting to the board…"));
  center.append(textNode("div", "connecting-sub", "This usually takes a moment."));
  state.append(operatorHotspot(), center);
  return state;
}

function renderConnectionLost(): HTMLElement {
  const state = ev("div", { class: "state" });

  const bg = ev("div", { class: "connection-lost-bg" });
  bg.append(
    renderBoard({
      position: STARTING_FEN_BOARD,
      size: 400,
      showCoordinates: false,
    }),
  );
  state.append(operatorHotspot(), bg);

  const center = ev("div", { class: "connection-lost-center" });

  const pauseLabel = ev("div", {
    class: "status-row",
    style: `color:var(--status-loss);margin-bottom:20px`,
  });
  pauseLabel.append(
    ev("span", {
      class: "status-dot",
      style: `background:var(--status-loss)`,
    }),
    "Pause",
  );
  center.append(pauseLabel);

  center.append(
    textNode(
      "div",
      "connection-lost-headline",
      "Garry has lost connection to the board.",
    ),
  );
  center.append(
    textNode(
      "div",
      "connection-lost-sub",
      "The game will resume on its own as soon as we’re back. Your position is saved.",
    ),
  );
  center.append(
    textNode(
      "div",
      "connection-lost-attendant",
      "If this lasts more than a minute, please ask a gallery attendant.",
    ),
  );
  state.append(center);
  return state;
}

function renderIdle(): HTMLElement {
  const state = ev("div", { class: "state" });
  const center = ev("div", { class: "idle-center" });
  center.append(textNode("div", "idle-wordmark", "Garry · a chess robot"));
  center.append(
    renderBoard({
      position: STARTING_FEN_BOARD,
      size: 480,
      showCoordinates: false,
      breathePiece: "e2",
    }),
  );

  const cta = ev("button", { class: "cta cta-breathe", type: "button" });
  cta.textContent = "Play Garry";
  cta.addEventListener("click", () => beginGame());
  cta.addEventListener("touchstart", () => beginGame(), { passive: true });
  center.append(cta);

  center.append(
    textNode(
      "div",
      "cta-hint",
      "Touch to begin · a full game takes about 20 minutes",
    ),
  );
  state.append(operatorHotspot(), center);
  return state;
}

function renderOnboarding(): HTMLElement {
  const state = ev("div", { class: "state" });

  const bg = ev("div", { class: "onboarding-bg" });
  bg.append(
    renderBoard({
      position: STARTING_FEN_BOARD,
      size: 480,
      showCoordinates: false,
    }),
  );
  state.append(operatorHotspot(), bg, ev("div", { class: "onboarding-wash" }));

  const center = ev("div", { class: "onboarding-center" });
  center.append(
    textNode(
      "div",
      "onboarding-headline",
      "You play on the board.\nGarry plays on the board too.",
    ),
  );

  const stepsWrap = ev("div", { class: "onboarding-steps" });
  const steps = [
    { n: "1", body: "Move pieces on the physical board." },
    { n: "2", body: "The board sees your move automatically." },
    { n: "3", body: "Garry considers, then the arm replies on the board." },
  ];
  for (const s of steps) {
    const col = ev("div", { class: "onboarding-step" });
    col.append(textNode("div", "step-label", `Step ${s.n}`));
    col.append(textNode("div", "step-body", s.body));
    stepsWrap.append(col);
  }
  center.append(stepsWrap);

  const begin = ev("button", { class: "cta-secondary", type: "button" });
  begin.textContent = "Begin";
  begin.addEventListener("click", () => dismissOnboarding());
  begin.addEventListener("touchstart", () => dismissOnboarding(), { passive: true });
  center.append(begin);
  center.append(textNode("div", "cta-hint", "You move first."));

  state.append(center);
  return state;
}

function renderPlay(
  snap: Snapshot,
  tone: "your-turn" | "thinking",
  baseLabel: string,
  pulsing: boolean,
  thinking: boolean,
  footerText: string,
  visitorActive: boolean,
): HTMLElement {
  const state = ev("div", { class: "state" });
  state.append(operatorHotspot());

  // Three competing status-row treatments, in priority order:
  //   1. Illegal move (visitor placed a piece somewhere the rules don't allow).
  //   2. Check (side to move is in check but a legal escape exists).
  //   3. Default ("Your move" / "Garry is thinking").
  const turn = snapSide(snap);
  const inCheck = snap.in_check === true;
  const showIllegal = visitorActive && isShowingIllegal();

  const checkSquare = inCheck ? findKingSquare(snap.fen, turn) : null;
  const illegalSquares: string[] = [];
  if (showIllegal && illegalAttempt) {
    if (illegalAttempt.from) illegalSquares.push(illegalAttempt.from);
    if (illegalAttempt.to) illegalSquares.push(illegalAttempt.to);
  }

  let label = baseLabel;
  let statusTone: string = tone;
  let footerCopy = footerText;
  if (showIllegal) {
    label = "Move not allowed";
    statusTone = "loss";
    footerCopy = "That move leaves your king in check — put the piece back and try again.";
  } else if (inCheck) {
    label = `${baseLabel} · check`;
    statusTone = "loss";
  }

  const stage = ev("div", { class: "stage" });
  stage.append(statusRow(statusTone, label, pulsing && !showIllegal));

  const middle = ev("div", { class: "stage-middle" });
  middle.append(
    renderBoard({
      position: snap.fen,
      size: 560,
      showCoordinates: true,
      thinking,
      lastMove: lastMoveCached,
      checkSquare,
      illegalSquares,
    }),
  );
  stage.append(middle);

  const footer = ev("div", { class: "stage-footer" });
  footer.append(pedestal("You · white", !visitorActive));
  footer.append(footerNote(footerCopy));
  footer.append(pedestal("Garry · black", visitorActive));
  stage.append(footer);

  state.append(stage);
  return state;
}

function renderYourTurn(snap: Snapshot): HTMLElement {
  return renderPlay(
    snap,
    "your-turn",
    "Your move",
    false,
    false,
    "The board is watching · move a piece",
    true,
  );
}

function renderGarryTurn(snap: Snapshot): HTMLElement {
  return renderPlay(
    snap,
    "thinking",
    "Garry is thinking",
    true,
    true,
    "The arm will move when ready",
    false,
  );
}

function renderGameOver(snap: Snapshot, outcome: Outcome): HTMLElement {
  const state = ev("div", { class: "state" });
  state.append(operatorHotspot());

  const presets: Record<
    Outcome,
    { label: string; headline: string; sub: string; tone: string }
  > = {
    win: {
      label: snap.method?.toLowerCase().includes("stalemate") ? "Stalemate" : "Checkmate",
      headline: "You won.",
      sub: "A clean game. Walk away with the win.",
      tone: "win",
    },
    loss: {
      label: "Checkmate",
      headline: "Garry won.",
      sub: "Played on this board. Try again whenever you like.",
      tone: "loss",
    },
    draw: {
      label: snap.method ? capitalize(snap.method) : "Draw",
      headline: "A draw.",
      sub: "No path to checkmate. The game stands.",
      tone: "draw",
    },
  };
  const o = presets[outcome];

  const center = ev("div", { class: "gameover-center" });
  center.append(
    textNode(
      "div",
      "gameover-eyebrow",
      o.label,
      `color:var(--status-${o.tone})`,
    ),
  );
  center.append(textNode("div", "gameover-headline", o.headline));
  center.append(textNode("div", "gameover-sub", o.sub));

  const mark = ev("div", { class: "gameover-mark" });
  mark.append(outcomeMark(outcome));
  center.append(mark);

  // Two CTAs: robot resets, or the visitor resets by hand. Equal-weight
  // visually so both feel like valid choices.
  const ctaRow = ev("div", { class: "gameover-cta-row" });

  const robotBtn = ev("button", { class: "cta-secondary", type: "button" });
  robotBtn.textContent = "Garry resets";
  robotBtn.addEventListener("click", () => playAgainWithRobot());
  robotBtn.addEventListener("touchstart", () => playAgainWithRobot(), { passive: true });

  const robotCol = ev("div", { class: "gameover-cta-col" });
  robotCol.append(robotBtn);
  robotCol.append(
    textNode("div", "gameover-cta-hint", "The arm returns the pieces home."),
  );
  ctaRow.append(robotCol);

  const manualBtn = ev("button", { class: "cta-outlined", type: "button" });
  manualBtn.textContent = "I'll reset";
  manualBtn.addEventListener("click", () => playAgainManual());
  manualBtn.addEventListener("touchstart", () => playAgainManual(), { passive: true });

  const manualCol = ev("div", { class: "gameover-cta-col" });
  manualCol.append(manualBtn);
  manualCol.append(
    textNode("div", "gameover-cta-hint", "Set the pieces yourself, then tap Play Garry."),
  );
  ctaRow.append(manualCol);

  center.append(ctaRow);
  state.append(center);
  return state;
}

function outcomeMark(outcome: Outcome): SVGElement {
  if (outcome === "win") {
    const svg = svgEl("svg", { width: 80, height: 80, viewBox: "0 0 80 80" });
    svg.append(
      svgEl("rect", {
        x: 0,
        y: 0,
        width: 80,
        height: 80,
        fill: "var(--last-move)",
        opacity: 0.5,
      }),
    );
    svg.append(svgEl("circle", { cx: 40, cy: 40, r: 14, fill: "var(--ink)" }));
    return svg;
  }
  if (outcome === "loss") {
    const svg = svgEl("svg", { width: 100, height: 80, viewBox: "0 0 100 80" });
    const tip = svgEl("g", { transform: "translate(50 50) rotate(-65)" });
    tip.append(svgEl("circle", { cx: 0, cy: 0, r: 14, fill: "var(--ink)" }));
    tip.append(svgEl("rect", { x: -2, y: -26, width: 4, height: 12, fill: "var(--ink)" }));
    tip.append(svgEl("rect", { x: -6, y: -23, width: 12, height: 4, fill: "var(--ink)" }));
    svg.append(tip);
    svg.append(
      svgEl("line", {
        x1: 20,
        y1: 70,
        x2: 80,
        y2: 70,
        stroke: "var(--hairline)",
        "stroke-width": 1,
      }),
    );
    return svg;
  }
  const svg = svgEl("svg", { width: 120, height: 40, viewBox: "0 0 120 40" });
  svg.append(
    svgEl("line", {
      x1: 0,
      y1: 20,
      x2: 120,
      y2: 20,
      stroke: "var(--hairline)",
      "stroke-width": 1,
    }),
  );
  svg.append(svgEl("circle", { cx: 40, cy: 20, r: 10, fill: "var(--ink)" }));
  svg.append(
    svgEl("circle", {
      cx: 80,
      cy: 20,
      r: 10,
      fill: "none",
      stroke: "var(--ink)",
      "stroke-width": 2,
    }),
  );
  return svg;
}

// ── Operator sheet ─────────────────────────────────────────────────────────

function operatorHotspot(): HTMLElement {
  const hot = ev("div", { class: "operator-hotspot" });
  attachLongPress(hot, openOperatorSheet);
  return hot;
}

function attachLongPress(target: HTMLElement, onLong: () => void) {
  let timer: ReturnType<typeof setTimeout> | null = null;
  const start = () => {
    timer = setTimeout(() => {
      onLong();
      timer = null;
    }, OPERATOR_LONG_PRESS_MS);
  };
  const cancel = () => {
    if (timer) clearTimeout(timer);
    timer = null;
  };
  target.addEventListener("mousedown", start);
  target.addEventListener("touchstart", start, { passive: true });
  target.addEventListener("mouseup", cancel);
  target.addEventListener("mouseleave", cancel);
  target.addEventListener("touchend", cancel);
  target.addEventListener("touchcancel", cancel);
}

let operatorDismissTimer: ReturnType<typeof setTimeout> | null = null;
let operatorEl: HTMLElement | null = null;

function openOperatorSheet() {
  if (operatorOpen) return;
  operatorOpen = true;
  bumpActivity();

  const overlay = ev("div", { class: "operator-overlay" });
  overlay.addEventListener("click", closeOperatorSheet);

  const sheet = ev("div", { class: "operator-sheet" });
  sheet.addEventListener("click", (e) => e.stopPropagation());

  const header = ev("div", { class: "operator-header" });
  const left = ev("div");
  left.append(textNode("div", "operator-eyebrow", "Operator"));
  left.append(textNode("div", "operator-title", "garry.kiosk · v1.0"));
  header.append(left);
  header.append(textNode("div", "operator-meta", "auto-closes in 30s"));
  sheet.append(header);
  sheet.append(ev("div", { class: "hairline" }));

  const grid = ev("div", { class: "operator-grid" });
  const actions = [
    {
      label: "Rescan board",
      sub: "Re-detect pieces · keep game state",
      do: () => sendCommand({ "clear-cache": true }),
    },
    {
      label: "Undo last move",
      sub: "Reverts Garry’s last play",
      do: () => sendCommand({ undo: 1 }),
    },
    {
      label: "Force new game",
      sub: "Visitor flow returns to idle",
      do: () => forceNewGame(),
    },
    {
      label: "Wipe state",
      sub: "Clear all game data",
      do: () => sendCommand({ wipe: true }),
      danger: true,
    },
  ];
  for (const a of actions) {
    const btn = ev("button", {
      class: "operator-action" + (a.danger ? " danger" : ""),
      type: "button",
    });
    btn.append(textNode("div", "operator-action-label", a.label));
    btn.append(textNode("div", "operator-action-sub", a.sub));
    btn.addEventListener("click", () => {
      void a.do();
      closeOperatorSheet();
    });
    grid.append(btn);
  }
  sheet.append(grid);
  sheet.append(ev("div", { class: "hairline" }));

  const footer = ev("div", { class: "operator-footer" });
  const left2 = ev("div");
  const dot = ev("span", { class: "operator-status-dot" });
  dot.textContent = "● robot online";
  left2.append(dot);
  const state = ev("span", {
    style: "margin-left:24px",
  });
  state.textContent = `visitor state: ${currentState.name.toUpperCase()}`;
  left2.append(state);
  footer.append(left2);

  const dismiss = ev("button", { class: "operator-dismiss", type: "button" });
  dismiss.textContent = "DISMISS";
  dismiss.addEventListener("click", closeOperatorSheet);
  footer.append(dismiss);
  sheet.append(footer);

  document.body.append(overlay, sheet);
  operatorEl = sheet;
  // Hold the overlay reference on the sheet so we can clean both up.
  (sheet as any)._overlay = overlay;

  operatorDismissTimer = setTimeout(closeOperatorSheet, OPERATOR_AUTO_DISMISS_MS);
}

function closeOperatorSheet() {
  if (!operatorOpen) return;
  operatorOpen = false;
  if (operatorDismissTimer) {
    clearTimeout(operatorDismissTimer);
    operatorDismissTimer = null;
  }
  if (operatorEl) {
    const overlay = (operatorEl as any)._overlay as HTMLElement | undefined;
    operatorEl.remove();
    overlay?.remove();
    operatorEl = null;
  }
}

async function forceNewGame() {
  lastMoveCached = null;
  suppressPollUntilFreshAt = Date.now();
  try {
    await sendCommand({ wipe: true });
  } catch {}
  setState({ name: "idle" });
}

// ── DOM utility ────────────────────────────────────────────────────────────

function textNode(tag: string, cls: string, text: string, extraStyle?: string): HTMLElement {
  const el = ev("div", { class: cls, style: extraStyle ?? null });
  // Honor newlines in copy as line breaks.
  text.split("\n").forEach((part, i, arr) => {
    el.append(document.createTextNode(part));
    if (i < arr.length - 1) el.append(document.createElement("br"));
  });
  // The first arg is hard-coded as div; use tag if caller passed something else.
  if (tag !== "div") {
    const swap = document.createElement(tag);
    swap.className = el.className;
    if (extraStyle) swap.setAttribute("style", extraStyle);
    while (el.firstChild) swap.append(el.firstChild);
    return swap;
  }
  return el;
}

function capitalize(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.slice(1);
}

// ── Dispatch + render ──────────────────────────────────────────────────────

function render() {
  root.innerHTML = "";
  let frame: HTMLElement;
  switch (currentState.name) {
    case "connecting":
      frame = renderConnecting();
      break;
    case "connection_lost":
      frame = renderConnectionLost();
      break;
    case "idle":
      frame = renderIdle();
      break;
    case "onboarding":
      frame = renderOnboarding();
      break;
    case "your_turn":
      frame = renderYourTurn(currentState.snap);
      break;
    case "garry_turn":
      frame = renderGarryTurn(currentState.snap);
      break;
    case "game_over":
      frame = renderGameOver(currentState.snap, currentState.outcome);
      break;
  }
  root.append(frame);
}

// ── Activity / idle return ─────────────────────────────────────────────────

function bumpActivity() {
  lastActivityAt = Date.now();
}

function checkIdle() {
  // Game-over is deliberately a sticky screen: it stays put until somebody
  // taps "Play again" (or the operator forces a new game via the long-press
  // sheet). That keeps the outcome readable from across the gallery and
  // gives a clean handoff to the next visitor. No auto-return to attract.
  void operatorOpen;
  void lastActivityAt;
}

["mousedown", "touchstart", "keydown"].forEach((evt) => {
  window.addEventListener(evt, bumpActivity, { passive: true });
});
setInterval(checkIdle, 1_000);

// ── Game-flow actions ──────────────────────────────────────────────────────

const STARTING_SNAP: Snapshot = { fen: STARTING_FEN_BOARD + " w - - 0 1" };

// The kiosk's whole UX assumes the engine will respond automatically when
// the visitor moves on the physical board. That only happens when auto
// mode is on server-side. We don't trust whatever the operator left it as;
// every game-flow entry point ensures it's enabled.
async function ensureAuto() {
  console.log("[halcyon] ensureAuto: enabling auto mode");
  try {
    const res = await sendCommand({ auto: true });
    console.log("[halcyon] ensureAuto: server replied", res);
  } catch (e) {
    console.warn("[halcyon] ensureAuto failed", e);
  }
}

function beginGame() {
  bumpActivity();
  void ensureAuto();
  const seen = (() => {
    try {
      return localStorage.getItem(ONBOARDING_FLAG) === "1";
    } catch {
      return false;
    }
  })();
  if (seen) {
    setState(snapshotToActiveState(lastSnap ?? STARTING_SNAP));
  } else {
    setState({ name: "onboarding" });
  }
}

function dismissOnboarding() {
  bumpActivity();
  void ensureAuto();
  try {
    localStorage.setItem(ONBOARDING_FLAG, "1");
  } catch {}
  setState(snapshotToActiveState(lastSnap ?? STARTING_SNAP));
}

// "Play again" with the robot resetting the board: physically restores the
// starting position via the arm + graveyards. The reset DoCommand can take
// several seconds (real arm motion), so we flip the UI optimistically and
// fire the command in the background — otherwise the tap reads as a dead
// button.
function playAgainWithRobot() {
  bumpActivity();
  lastMoveCached = null;
  suppressPollUntilFreshAt = Date.now();
  setState({ name: "your_turn", snap: STARTING_SNAP });
  void (async () => {
    try {
      await sendCommand({ reset: true });
    } catch (e) {
      console.warn("playAgainWithRobot: reset failed", e);
    }
    await ensureAuto();
  })();
}

// "Play again" with the visitor resetting the board by hand: wipe state, then
// the visitor physically restores the board and taps Play Garry from the
// attract screen. Same optimistic-UI pattern.
function playAgainManual() {
  bumpActivity();
  lastMoveCached = null;
  suppressPollUntilFreshAt = Date.now();
  setState({ name: "idle" });
  void (async () => {
    try {
      await sendCommand({ wipe: true });
    } catch (e) {
      console.warn("playAgainManual: wipe failed", e);
    }
  })();
}

// ── Polling ────────────────────────────────────────────────────────────────

let lastSnap: Snapshot | null = null;
let lastMoveCached: { from: string; to: string } | null = null;
// Illegal-move detection: when the camera shows a piece in a square that
// doesn't match the FEN-derived game state, and that disagreement persists
// across multiple polls, the visitor likely made a move the rules don't
// allow (e.g. own king left in check). Tracked here so we can debounce and
// reflect the state in stateKey().
const ILLEGAL_DEBOUNCE_MS = 1800;
let firstDisagreementAt: number | null = null;
let illegalAttempt: { from?: string; to?: string } | null = null;
// After Play again / Force new game we issue a wipe and immediately flip
// the UI to "your turn". The server's snapshot cache may still report the
// old is_over=true for ~half a second until runBoardLoop refreshes; ignore
// snapshots older than this until a fresh one arrives, so we don't snap
// back to the game-over screen.
let suppressPollUntilFreshAt: number | null = null;
let lastLoggedSnap: { fen: string; turn?: string; auto?: boolean; captured_at_ms?: number } | null = null;
let pollTickCount = 0;

function pollLog(snap: Snapshot) {
  pollTickCount++;
  const changed =
    !lastLoggedSnap ||
    lastLoggedSnap.fen !== snap.fen ||
    lastLoggedSnap.turn !== snap.turn ||
    lastLoggedSnap.auto !== snap.auto ||
    lastLoggedSnap.captured_at_ms !== snap.captured_at_ms;

  // Always log when anything changed; otherwise log once every ~10s so the
  // console isn't silent during a long stretch with no change.
  const heartbeat = pollTickCount % 20 === 0;
  if (!changed && !heartbeat) return;

  const ageS = snap.captured_at_ms ? ((Date.now() - snap.captured_at_ms) / 1000).toFixed(1) + "s" : "?";
  console.log(
    `[halcyon] snap fen=${snap.fen}  turn=${snap.turn ?? "?"}  auto=${snap.auto}  cache_age=${ageS}  is_over=${snap.is_over ?? false}  event=${snap.event ?? "-"}${changed ? "  (changed)" : "  (heartbeat)"}`,
  );
  lastLoggedSnap = {
    fen: snap.fen,
    turn: snap.turn,
    auto: snap.auto,
    captured_at_ms: snap.captured_at_ms,
  };

  // Surface the most common configuration mistake explicitly so it's obvious
  // in the logs rather than hidden as "nothing seems to be happening".
  if (snap.auto === false && (currentState.name === "your_turn" || currentState.name === "garry_turn")) {
    console.warn(
      "[halcyon] auto mode is OFF on the server during a game — visitor moves will not be detected. Halcyon should set this via ensureAuto(); check that the previous call succeeded.",
    );
  }
}

async function poll() {
  if (!chessService) return;
  try {
    const raw = await chessService.doCommand(Struct.fromJson({ "board-snapshot": true } as JsonValue));
    const snap = (raw ?? {}) as unknown as Snapshot;
    if (!snap.fen) {
      // Pre-first-tick from the server cache — leave state as-is.
      if (pollTickCount % 10 === 0) {
        console.log("[halcyon] poll: server cache not ready yet (no fen)");
      }
      pollTickCount++;
      return;
    }
    consecutivePollErrors = 0;
    pollLog(snap);

    // Suppress stale snapshots after Play again / Force new game so the UI
    // doesn't bounce back to the old game-over state while the server's
    // cache catches up.
    if (suppressPollUntilFreshAt != null) {
      const cachedAt = snap.captured_at_ms ?? 0;
      const isStale = cachedAt < suppressPollUntilFreshAt || snap.is_over === true;
      if (isStale) {
        if (pollTickCount % 4 === 0) {
          console.log(
            `[halcyon] poll: suppressing stale snap during new-game (is_over=${snap.is_over} cache_age=${cachedAt} need>${suppressPollUntilFreshAt})`,
          );
        }
        pollTickCount++;
        return;
      }
      console.log("[halcyon] poll: fresh snap received, clearing suppression");
      suppressPollUntilFreshAt = null;
    }

    // Update last-move cache before we replace lastSnap.
    if (isStartingPosition(snap.fen)) {
      // Either first snapshot or someone wiped — clear stale highlight.
      lastMoveCached = null;
    } else if (lastSnap && lastSnap.fen !== snap.fen) {
      const inferred = diffFenForLastMove(lastSnap.fen, snap.fen);
      if (inferred) lastMoveCached = inferred;
    }
    lastSnap = snap;

    // Illegal-move detection: camera_board vs FEN. Two or more disagreeing
    // squares that persist across polls = the visitor placed a piece somewhere
    // the rules don't allow (typically own king left in check).
    const camDiff = diffCameraVsFen(snap);
    if (camDiff.count >= 2) {
      if (firstDisagreementAt == null) firstDisagreementAt = Date.now();
      illegalAttempt = { from: camDiff.from, to: camDiff.to };
    } else {
      firstDisagreementAt = null;
      illegalAttempt = null;
    }

    // Promote to a game state if we were in connecting/connection_lost.
    if (currentState.name === "connecting" || currentState.name === "connection_lost") {
      if (isStartingPosition(snap.fen)) {
        setState({ name: "idle" });
      } else {
        setState(deriveState(snap, currentState));
      }
      return;
    }

    if (currentState.name === "idle" || currentState.name === "onboarding") {
      // The visitor hasn't said go yet; we stay here even if a game is
      // happening on the server (operator-driven). They'll see it when they
      // press "Play Garry".
      return;
    }

    setState(deriveState(snap, currentState));
  } catch (e) {
    consecutivePollErrors++;
    console.warn(`[halcyon] poll error (${consecutivePollErrors})`, e);
    if (consecutivePollErrors >= 3 && currentState.name !== "connection_lost") {
      console.warn("[halcyon] connection lost — switching to connection_lost frame");
      setState({ name: "connection_lost" });
    }
  }
}

async function sendCommand(cmd: Record<string, unknown>): Promise<Record<string, JsonValue>> {
  if (!chessService) throw new Error("Not connected");
  console.log("[halcyon] sendCommand", cmd);
  const result = await chessService.doCommand(Struct.fromJson(cmd as JsonValue));
  return (result ?? {}) as Record<string, JsonValue>;
}

// ── Bootstrap ──────────────────────────────────────────────────────────────

async function connect() {
  const cookieKey = window.location.pathname.split("/")[2];
  let raw = Cookies.get(cookieKey);
  if (!raw) {
    for (const val of Object.values(Cookies.get())) {
      try {
        if ((JSON.parse(val) as MachineCookie).apiKey) {
          raw = val;
          break;
        }
      } catch {}
    }
  }
  if (!raw) throw new Error("Viam machine cookie not found — open this app from the Viam portal.");
  const cookie: MachineCookie = JSON.parse(raw);
  const { apiKey, hostname, machineName } = cookie;
  console.log(`[halcyon] connect: host=${hostname} machine=${machineName ?? "?"}`);

  const robot = await createRobotClient({
    host: hostname,
    signalingAddress: "https://app.viam.com:443",
    credentials: { type: "api-key", payload: apiKey.key, authEntity: apiKey.id },
  });
  chessService = new GenericServiceClient(robot, CHESS_SERVICE_NAME);
  console.log(`[halcyon] connect: chess service client ready (resource name="${CHESS_SERVICE_NAME}")`);
}

async function main() {
  console.log("[halcyon] boot");
  render();
  try {
    await connect();
  } catch (e) {
    console.error("[halcyon] connect failed", e);
    setTimeout(main, RECONNECT_DELAY_MS);
    return;
  }
  await poll();
  pollTimer = setInterval(poll, ACTIVE_REFRESH_MS);
  console.log(`[halcyon] poll timer started at ${ACTIVE_REFRESH_MS}ms cadence`);
}

void main();
