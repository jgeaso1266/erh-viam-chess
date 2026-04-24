import { createRobotClient, GenericServiceClient } from "@viamrobotics/sdk";
import { Struct, type JsonValue } from "@viamrobotics/sdk";
import Cookies from "js-cookie";

// ── Types ──────────────────────────────────────────────────────────────────

interface MachineCookie {
  apiKey: { id: string; key: string };
  machineId: string;
  hostname: string;
  machineName?: string;
}

type EvtType = "go" | "reset" | "wipe" | "cache" | "refresh" | "snapshot" | "err";
interface MoveEntry {
  kind: "move";
  i: number;
  san: string;
  from: string;
  to: string;
  color: "w" | "b";
}
interface EvtEntry {
  kind: "evt";
  type: EvtType;
  label: string;
}
type TapeEntry = MoveEntry | EvtEntry;

type Mismatch = { sq: string; kind: "missing" | "phantom" | "wrongcolor" };

// ── Piece SVGs ─────────────────────────────────────────────────────────────

const pieceSvgs = import.meta.glob("../pieces/*.svg", {
  eager: true,
  query: "?url",
  import: "default",
}) as Record<string, string>;

const PIECE_FILE: Record<string, string> = {
  K: "white-king", Q: "white-queen", R: "white-rook",
  B: "white-bishop", N: "white-knight", P: "white-pawn",
  k: "black-king", q: "black-queen", r: "black-rook",
  b: "black-bishop", n: "black-knight", p: "black-pawn",
};

function pieceUrl(piece: string): string | undefined {
  const name = PIECE_FILE[piece];
  if (!name) return undefined;
  return pieceSvgs[`../pieces/${name}.svg`];
}

const PIECE_VALUE: Record<string, number> = { P: 1, N: 3, B: 3, R: 5, Q: 9, K: 0 };

// ── State ──────────────────────────────────────────────────────────────────

const CHESS_SERVICE_NAME = "chess";

let chessService: GenericServiceClient | null = null;
let currentFen: string | null = null;
let currentBoard: (string | null)[][] = emptyBoard();
let currentTurn: "w" | "b" = "w";
let cameraBoard: Record<string, string> | null = null;
let mismatches: Mismatch[] = [];
let whiteGraveyard: string[] = [];
let blackGraveyard: string[] = [];
let lastMove: { from: string; to: string; san?: string } | null = null;

let tapeMoves: MoveEntry[] = [];
let tapeEvents: EvtEntry[] = [];

let selectedSq: string | null = null;
let autoRefreshTimer: ReturnType<typeof setInterval> | null = null;
let refreshInFlight = false;
let busy = false;

// ── Helpers ────────────────────────────────────────────────────────────────

function emptyBoard(): (string | null)[][] {
  return Array.from({ length: 8 }, () => Array(8).fill(null));
}

function parseFENPlacement(fen: string): { board: (string | null)[][]; turn: "w" | "b" } {
  const [placement, turn] = fen.split(" ");
  const rows = placement.split("/");
  const board = rows.map((row) => {
    const out: (string | null)[] = [];
    for (const ch of row) {
      if (/\d/.test(ch)) for (let i = 0; i < parseInt(ch, 10); i++) out.push(null);
      else out.push(ch);
    }
    return out;
  });
  while (board.length < 8) board.push(Array(8).fill(null));
  return { board, turn: turn === "b" ? "b" : "w" };
}

function rcToSq(r: number, c: number): string {
  return String.fromCharCode(97 + c) + String(8 - r);
}

function diffCamera(board: (string | null)[][], cam: Record<string, string> | null): Mismatch[] {
  if (!cam) return [];
  const out: Mismatch[] = [];
  for (let r = 0; r < 8; r++) {
    for (let c = 0; c < 8; c++) {
      const sq = rcToSq(r, c);
      const p = board[r]?.[c] ?? null;
      const expected = p ? (p === p.toUpperCase() ? "white" : "black") : null;
      const reading = cam[sq] ?? "0";
      const detected = reading === "1" ? "white" : reading === "2" ? "black" : null;
      if (expected !== detected) {
        const kind: Mismatch["kind"] =
          expected && !detected ? "missing" : !expected && detected ? "phantom" : "wrongcolor";
        out.push({ sq, kind });
      }
    }
  }
  return out;
}

function sumValue(pieces: string[]): number {
  return pieces.reduce((a, p) => a + (PIECE_VALUE[p.toUpperCase()] ?? 0), 0);
}

function sortCaptured(pieces: string[]): string[] {
  const order = ["Q", "R", "B", "N", "P"];
  return [...pieces].sort(
    (a, b) => order.indexOf(a.toUpperCase()) - order.indexOf(b.toUpperCase())
  );
}

// ── Connection ─────────────────────────────────────────────────────────────

async function connect() {
  const cookieKey = window.location.pathname.split("/")[2];
  const raw = Cookies.get(cookieKey);
  if (!raw) throw new Error("Viam machine cookie not found — open this app from the Viam portal.");
  const cookie: MachineCookie = JSON.parse(raw);
  const { apiKey, hostname, machineName } = cookie;

  setStatus("Connecting", "warn");
  const machineEl = document.getElementById("machine-name");
  if (machineEl) machineEl.textContent = machineName || hostname.split(".")[0] || "—";

  const robot = await createRobotClient({
    host: hostname,
    signalingAddress: "https://app.viam.com:443",
    credentials: { type: "api-key", payload: apiKey.key, authEntity: apiKey.id },
  });
  chessService = new GenericServiceClient(robot, CHESS_SERVICE_NAME);
  setStatus("in sync", "ok");
}

async function doCommand(cmd: Record<string, unknown>): Promise<Record<string, JsonValue>> {
  if (!chessService) throw new Error("Not connected");
  const result = await chessService.doCommand(Struct.fromJson(cmd as JsonValue));
  return (result ?? {}) as Record<string, JsonValue>;
}

// ── Status pill ────────────────────────────────────────────────────────────

function setStatus(label: string, level: "ok" | "warn" | "err") {
  const pill = document.getElementById("status-pill");
  const labelEl = pill?.querySelector(".status-pill-label") as HTMLElement | null;
  if (!pill || !labelEl) return;
  pill.classList.remove("warn", "err");
  if (level === "warn") pill.classList.add("warn");
  if (level === "err") pill.classList.add("err");
  labelEl.textContent = label;
}

function updateStatusFromMismatches() {
  if (mismatches.length === 0) setStatus("in sync", "ok");
  else setStatus(`${mismatches.length} diff`, "warn");
}

// ── Board rendering ────────────────────────────────────────────────────────

function renderBoard() {
  const boardEl = document.getElementById("board");
  if (!boardEl) return;
  boardEl.innerHTML = "";

  const mmBySq = new Map<string, Mismatch>();
  mismatches.forEach((m) => mmBySq.set(m.sq, m));

  for (let r = 0; r < 8; r++) {
    for (let c = 0; c < 8; c++) {
      const sq = rcToSq(r, c);
      const isLight = (r + c) % 2 === 0;
      const piece = currentBoard[r]?.[c] ?? null;
      const mm = mmBySq.get(sq);

      const cell = document.createElement("div");
      cell.className = "chess-square " + (isLight ? "light" : "dark");
      cell.dataset.sq = sq;
      if (mm) cell.classList.add("mismatch-" + mm.kind);

      // last-move highlight
      if (lastMove && (lastMove.from === sq || lastMove.to === sq)) {
        const h = document.createElement("div");
        h.className = "last-highlight";
        cell.appendChild(h);
      }

      // selection ring
      if (selectedSq === sq) {
        const s = document.createElement("div");
        s.className = "selected-ring";
        cell.appendChild(s);
      }

      // mismatch tint + dot
      if (mm) {
        const tint = document.createElement("div");
        tint.className = "mismatch-tint";
        cell.appendChild(tint);
        const dot = document.createElement("div");
        dot.className = "mismatch-dot";
        cell.appendChild(dot);
      }

      // piece
      if (piece) {
        const url = pieceUrl(piece);
        if (url) {
          const img = document.createElement("img");
          img.className = "piece";
          img.src = url;
          img.alt = piece;
          img.draggable = true;
          img.dataset.sq = sq;
          img.addEventListener("dragstart", (e) => {
            if (busy) {
              e.preventDefault();
              return;
            }
            e.dataTransfer?.setData("text/plain", sq);
            selectedSq = sq;
          });
          cell.appendChild(img);
        }
      }

      // camera detection dot
      if (cameraBoard) {
        const reading = cameraBoard[sq];
        if (reading === "1" || reading === "2") {
          const dot = document.createElement("div");
          dot.className = "cam-dot " + (reading === "1" ? "white" : "black");
          cell.appendChild(dot);
        }
      }

      // coordinates
      const showFile = r === 7;
      const showRank = c === 0;
      if (showFile) {
        const f = document.createElement("span");
        f.className = "coord file";
        f.textContent = String.fromCharCode(97 + c);
        cell.appendChild(f);
      }
      if (showRank) {
        const ra = document.createElement("span");
        ra.className = "coord rank";
        ra.textContent = String(8 - r);
        cell.appendChild(ra);
      }

      // interactions
      cell.addEventListener("click", () => onSquareClick(sq));
      cell.addEventListener("dragover", (e) => {
        e.preventDefault();
        cell.classList.add("drag-over");
        if (!cell.querySelector(".drag-over-ring")) {
          const d = document.createElement("div");
          d.className = "drag-over-ring";
          cell.appendChild(d);
        }
      });
      cell.addEventListener("dragleave", () => {
        cell.classList.remove("drag-over");
        cell.querySelector(".drag-over-ring")?.remove();
      });
      cell.addEventListener("drop", (e) => {
        e.preventDefault();
        cell.classList.remove("drag-over");
        cell.querySelector(".drag-over-ring")?.remove();
        const from = e.dataTransfer?.getData("text/plain");
        if (from && from !== sq) void submitMove(from, sq);
        selectedSq = null;
      });

      boardEl.appendChild(cell);
    }
  }
}

function onSquareClick(sq: string) {
  if (busy) return;
  if (!selectedSq) {
    const [r, c] = [8 - parseInt(sq[1], 10), sq.charCodeAt(0) - 97];
    if (currentBoard[r]?.[c]) {
      selectedSq = sq;
      renderBoard();
    }
    return;
  }
  if (selectedSq === sq) {
    selectedSq = null;
    renderBoard();
    return;
  }
  const from = selectedSq;
  selectedSq = null;
  void submitMove(from, sq);
}

// ── Turn + last move + status ──────────────────────────────────────────────

function renderTopStatus() {
  const turnEl = document.getElementById("turn-indicator");
  if (turnEl) turnEl.textContent = currentTurn === "w" ? "White to move" : "Black to move";

  const lastMoveEl = document.getElementById("last-move");
  const lastMoveRule = document.getElementById("last-move-rule");
  if (lastMove && lastMoveEl && lastMoveRule) {
    lastMoveEl.classList.remove("hidden");
    lastMoveRule.classList.remove("hidden");
    (lastMoveEl.querySelector(".last-from") as HTMLElement).textContent = lastMove.from;
    (lastMoveEl.querySelector(".last-to") as HTMLElement).textContent = lastMove.to;
    (lastMoveEl.querySelector(".last-san") as HTMLElement).textContent = lastMove.san ?? "";
  } else if (lastMoveEl && lastMoveRule) {
    lastMoveEl.classList.add("hidden");
    lastMoveRule.classList.add("hidden");
  }
}

// ── Material + captured ────────────────────────────────────────────────────

function renderMaterial() {
  // captured.b (white lost) ↔ white_graveyard
  // captured.w (black lost) ↔ black_graveyard
  const wLost = whiteGraveyard;
  const bLost = blackGraveyard;
  const wPts = sumValue(wLost);
  const bPts = sumValue(bLost);
  const balance = wPts - bPts;

  const wEl = document.getElementById("scale-wpts");
  const bEl = document.getElementById("scale-bpts");
  if (wEl) wEl.textContent = String(wPts);
  if (bEl) bEl.textContent = String(bPts);

  const beam = document.getElementById("scale-beam");
  if (beam) {
    const tilt = Math.max(-12, Math.min(12, balance * 1.4));
    beam.style.transform = `translateX(-50%) rotate(${-tilt}deg)`;
  }

  const balLabel = document.getElementById("balance-label");
  if (balLabel) {
    if (balance === 0) {
      balLabel.textContent = "even";
      balLabel.classList.remove("nonzero");
    } else if (balance > 0) {
      balLabel.textContent = `white +${balance}`;
      balLabel.classList.add("nonzero");
    } else {
      balLabel.textContent = `black +${-balance}`;
      balLabel.classList.add("nonzero");
    }
  }

  renderCaptured("white-lost", wLost);
  renderCaptured("black-lost", bLost);
}

function renderCaptured(id: string, pieces: string[]) {
  const el = document.getElementById(id);
  if (!el) return;
  el.innerHTML = "";
  if (pieces.length === 0) {
    const empty = document.createElement("span");
    empty.className = "empty";
    empty.textContent = "— none —";
    el.appendChild(empty);
    return;
  }
  for (const p of sortCaptured(pieces)) {
    const url = pieceUrl(p);
    if (!url) continue;
    const img = document.createElement("img");
    img.className = "captured-piece";
    img.src = url;
    img.alt = p;
    el.appendChild(img);
  }
}

// ── Tape ───────────────────────────────────────────────────────────────────

function renderTape() {
  const tapeEl = document.getElementById("tape");
  const plyEl = document.getElementById("tape-ply");
  if (!tapeEl) return;
  if (plyEl) plyEl.textContent = `${tapeMoves.length} ply`;

  tapeEl.innerHTML = "";

  if (tapeMoves.length === 0 && tapeEvents.length === 0) {
    const e = document.createElement("div");
    e.className = "empty";
    e.textContent = "awaiting first move";
    tapeEl.appendChild(e);
    return;
  }

  // Render moves in order, then append events. Events are appended in arrival order.
  const items: TapeEntry[] = [...tapeMoves, ...tapeEvents];
  const lastMoveIdx = tapeMoves.length - 1;

  items.forEach((it) => {
    if (it.kind === "evt") {
      const row = document.createElement("div");
      row.className = "tape-evt";
      if (it.type === "err") row.classList.add("err");
      const tag = document.createElement("span");
      tag.className = "tape-evt-tag " + it.type;
      tag.textContent = it.type;
      const label = document.createElement("span");
      label.className = "tape-evt-label";
      label.textContent = it.label;
      row.appendChild(tag);
      row.appendChild(label);
      tapeEl.appendChild(row);
      return;
    }
    const row = document.createElement("div");
    row.className = "tape-row";
    if (it.i === lastMoveIdx) row.classList.add("last");
    const moveNum = Math.floor(it.i / 2) + 1;
    const isWhite = it.color === "w";
    const num = document.createElement("span");
    num.className = "tape-num";
    num.textContent = isWhite ? `${moveNum}.` : "";
    const sw = document.createElement("span");
    sw.className = "tape-swatch " + (isWhite ? "white" : "black");
    const san = document.createElement("span");
    san.className = "tape-san";
    san.textContent = it.san;
    const coord = document.createElement("span");
    coord.className = "tape-coord";
    coord.textContent = `${it.from}→${it.to}`;
    row.appendChild(num);
    row.appendChild(sw);
    row.appendChild(san);
    row.appendChild(coord);
    tapeEl.appendChild(row);
  });

  tapeEl.scrollTop = tapeEl.scrollHeight;
}

function pushEvent(type: EvtType, label: string) {
  tapeEvents.push({ kind: "evt", type, label });
  renderTape();
}

// ── Inline error popovers ──────────────────────────────────────────────────

function showInlineError(which: "go" | "move", msg: string) {
  const popId = which === "go" ? "go-error" : "move-error";
  const inputIds = which === "go" ? ["go-n"] : ["move-from", "move-to"];
  const pop = document.getElementById(popId);
  if (!pop) return;
  pop.classList.remove("hidden");
  (pop.querySelector(".inline-error-msg") as HTMLElement).textContent = msg;
  inputIds.forEach((id) => document.getElementById(id)?.classList.add("error"));
}
function dismissInlineError(which: "go" | "move") {
  const popId = which === "go" ? "go-error" : "move-error";
  const inputIds = which === "go" ? ["go-n"] : ["move-from", "move-to"];
  document.getElementById(popId)?.classList.add("hidden");
  inputIds.forEach((id) => document.getElementById(id)?.classList.remove("error"));
}

// ── State application ──────────────────────────────────────────────────────

function applySnapshot(res: Record<string, JsonValue>) {
  if (typeof res.fen === "string") {
    currentFen = res.fen;
    const { board, turn } = parseFENPlacement(currentFen);
    currentBoard = board;
    currentTurn = turn;
  }
  cameraBoard =
    res.camera_board && typeof res.camera_board === "object"
      ? (res.camera_board as Record<string, string>)
      : null;
  mismatches = diffCamera(currentBoard, cameraBoard);
  if (Array.isArray(res.white_graveyard)) whiteGraveyard = res.white_graveyard as string[];
  if (Array.isArray(res.black_graveyard)) blackGraveyard = res.black_graveyard as string[];

  renderBoard();
  renderMaterial();
  renderTopStatus();
  updateStatusFromMismatches();
}

function pushMoveToTape(from: string, to: string, san: string) {
  const i = tapeMoves.length;
  const color: "w" | "b" = i % 2 === 0 ? "w" : "b";
  tapeMoves.push({ kind: "move", i, from, to, san, color });
  lastMove = { from, to, san };
  renderTape();
  renderTopStatus();
}

// ── Refresh ────────────────────────────────────────────────────────────────

async function refreshState() {
  if (refreshInFlight || !chessService) return;
  refreshInFlight = true;
  try {
    const res = await doCommand({ "board-snapshot": true });
    applySnapshot(res);
  } catch (e) {
    console.error("refresh failed", e);
  } finally {
    refreshInFlight = false;
  }
}

function startAutoRefresh() {
  if (autoRefreshTimer) clearInterval(autoRefreshTimer);
  autoRefreshTimer = setInterval(refreshState, 1500);
}

// ── Commands ───────────────────────────────────────────────────────────────

function setBusy(next: boolean) {
  busy = next;
  document.querySelectorAll<HTMLButtonElement>("button").forEach((b) => (b.disabled = next));
}

async function withBusy(fn: () => Promise<void>) {
  setBusy(true);
  if (autoRefreshTimer) clearInterval(autoRefreshTimer);
  try {
    await fn();
    await refreshState();
  } finally {
    setBusy(false);
    startAutoRefresh();
  }
}

async function cmdGo() {
  dismissInlineError("go");
  const n = parseInt((document.getElementById("go-n") as HTMLInputElement).value, 10) || 1;
  try {
    await withBusy(async () => {
      const res = await doCommand({ go: n });
      const move = typeof res.move === "string" ? res.move : "";
      // Try to pick from/to out of move string like "e2e4" or "e2-e4"
      const m = move.match(/^([a-h][1-8])[-\s]?([a-h][1-8])/);
      if (m) pushMoveToTape(m[1], m[2], move);
      pushEvent("go", `robot ×${n}${move ? ` · ${move}` : ""}`);
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    showInlineError("go", msg);
    pushEvent("err", `go ${n}: ${msg}`);
  }
}

function applyLocalMove(from: string, to: string): boolean {
  const fr = 8 - parseInt(from[1], 10);
  const fc = from.charCodeAt(0) - 97;
  const tr = 8 - parseInt(to[1], 10);
  const tc = to.charCodeAt(0) - 97;
  const piece = currentBoard[fr]?.[fc];
  if (!piece) return false;
  const captured = currentBoard[tr]?.[tc] ?? null;
  currentBoard[tr][tc] = piece;
  currentBoard[fr][fc] = null;
  if (captured) {
    if (captured === captured.toUpperCase()) whiteGraveyard = [...whiteGraveyard, captured];
    else blackGraveyard = [...blackGraveyard, captured];
  }
  currentTurn = currentTurn === "w" ? "b" : "w";
  lastMove = { from, to, san: `${from}${to}` };
  mismatches = diffCamera(currentBoard, cameraBoard);
  return true;
}

async function submitMove(from: string, to: string) {
  dismissInlineError("move");
  if (!/^[a-h][1-8]$/.test(from) || !/^[a-h][1-8]$/.test(to)) {
    showInlineError("move", `invalid square: ${from}→${to}`);
    return;
  }
  // Snapshot for revert on failure.
  const prev = {
    board: currentBoard.map((row) => [...row]),
    white: whiteGraveyard,
    black: blackGraveyard,
    turn: currentTurn,
    lastMove,
    mismatches,
  };
  if (!applyLocalMove(from, to)) {
    showInlineError("move", `no piece on ${from}`);
    return;
  }
  renderBoard();
  renderMaterial();
  renderTopStatus();
  updateStatusFromMismatches();

  try {
    await withBusy(async () => {
      await doCommand({ move: { from, to, n: 1 } });
      pushMoveToTape(from, to, `${from}${to}`);
    });
  } catch (e) {
    currentBoard = prev.board;
    whiteGraveyard = prev.white;
    blackGraveyard = prev.black;
    currentTurn = prev.turn;
    lastMove = prev.lastMove;
    mismatches = prev.mismatches;
    renderBoard();
    renderMaterial();
    renderTopStatus();
    updateStatusFromMismatches();
    const msg = e instanceof Error ? e.message : String(e);
    showInlineError("move", msg);
    pushEvent("err", `direct ${from}→${to}: ${msg}`);
  }
}

async function cmdDirectMoveFromInputs() {
  const from = (document.getElementById("move-from") as HTMLInputElement).value.trim().toLowerCase();
  const to = (document.getElementById("move-to") as HTMLInputElement).value.trim().toLowerCase();
  if (!from || !to) {
    showInlineError("move", "enter both squares");
    return;
  }
  await submitMove(from, to);
  (document.getElementById("move-from") as HTMLInputElement).value = "";
  (document.getElementById("move-to") as HTMLInputElement).value = "";
}

async function cmdMaintenance(id: "refresh" | "snapshot" | "cache" | "wipe" | "reset") {
  if (id === "reset" && !confirm("Physically reset the board?")) return;
  if (id === "wipe" && !confirm("Wipe game state?")) return;
  try {
    await withBusy(async () => {
      if (id === "refresh") {
        // handled by the post-action refresh
      } else if (id === "snapshot") {
        // snapshot is implicit in refresh; just log
      } else if (id === "cache") {
        await doCommand({ ClearCache: true });
      } else if (id === "wipe") {
        await doCommand({ wipe: true });
        tapeMoves = [];
        lastMove = null;
      } else if (id === "reset") {
        await doCommand({ reset: true });
        tapeMoves = [];
        lastMove = null;
      }
      pushEvent(id, labelFor(id));
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    pushEvent("err", `${id}: ${msg}`);
  }
}

function labelFor(id: EvtType): string {
  switch (id) {
    case "refresh": return "state refreshed";
    case "snapshot": return "snapshot captured";
    case "cache": return "square cache cleared";
    case "wipe": return "state wiped";
    case "reset": return "board reset";
    default: return id;
  }
}

// ── Wire events ────────────────────────────────────────────────────────────

document.getElementById("btn-go")!.addEventListener("click", () => void cmdGo());
document.getElementById("btn-move")!.addEventListener("click", () => void cmdDirectMoveFromInputs());
document.getElementById("btn-refresh")!.addEventListener("click", () => void cmdMaintenance("refresh"));
document.getElementById("btn-snapshot")!.addEventListener("click", () => void cmdMaintenance("snapshot"));
document.getElementById("btn-cache")!.addEventListener("click", () => void cmdMaintenance("cache"));
document.getElementById("btn-wipe")!.addEventListener("click", () => void cmdMaintenance("wipe"));
document.getElementById("btn-reset")!.addEventListener("click", () => void cmdMaintenance("reset"));
document.getElementById("move-to")!.addEventListener("keydown", (e) => {
  if ((e as KeyboardEvent).key === "Enter") void cmdDirectMoveFromInputs();
});
document.getElementById("go-n")!.addEventListener("keydown", (e) => {
  if ((e as KeyboardEvent).key === "Enter") void cmdGo();
});
document.querySelectorAll(".inline-error-dismiss").forEach((b) => {
  b.addEventListener("click", (e) => {
    const parent = (e.currentTarget as HTMLElement).closest(".inline-error");
    if (parent?.id === "go-error") dismissInlineError("go");
    if (parent?.id === "move-error") dismissInlineError("move");
  });
});
(document.getElementById("go-n") as HTMLInputElement).addEventListener("input", (e) => {
  const input = e.target as HTMLInputElement;
  input.value = input.value.replace(/[^0-9]/g, "");
});
["move-from", "move-to"].forEach((id) => {
  (document.getElementById(id) as HTMLInputElement).addEventListener("input", (e) => {
    const input = e.target as HTMLInputElement;
    input.value = input.value.toLowerCase();
  });
});

// ── Init ───────────────────────────────────────────────────────────────────

renderBoard();
renderMaterial();
renderTape();

connect()
  .then(refreshState)
  .then(startAutoRefresh)
  .catch((e) => {
    const msg = e instanceof Error ? e.message : String(e);
    setStatus("offline", "err");
    pushEvent("err", msg);
  });
