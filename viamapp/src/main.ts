import { createRobotClient, GenericServiceClient } from "@viamrobotics/sdk";
import { Struct, type JsonValue } from "@viamrobotics/sdk";
import Cookies from "js-cookie";

// ── Types ──────────────────────────────────────────────────────────────────

interface MachineCookie {
  apiKey: { id: string; key: string };
  machineId: string;
  hostname: string;
}

interface MoveRecord {
  type: "move" | "go" | "reset" | "wipe" | "cache";
  label: string;
}

// ── State ──────────────────────────────────────────────────────────────────

const CHESS_SERVICE_NAME = "chess";
const PIECE_UNICODE: Record<string, string> = {
  K: "♔", Q: "♕", R: "♖", B: "♗", N: "♘", P: "♙",
  k: "♚", q: "♛", r: "♜", b: "♝", n: "♞", p: "♟",
};

let chessService: GenericServiceClient | null = null;
const history: MoveRecord[] = [];
let autoRefreshTimer: ReturnType<typeof setInterval> | null = null;

// ── Connection ─────────────────────────────────────────────────────────────

async function connect() {
  const cookieKey = window.location.pathname.split("/")[2];
  const raw = Cookies.get(cookieKey);
  if (!raw) throw new Error("Viam machine cookie not found — open this app from the Viam portal.");

  const cookie: MachineCookie = JSON.parse(raw);
  const { apiKey, hostname } = cookie;

  setStatus("Connecting...", "busy");

  const robot = await createRobotClient({
    host: hostname,
    signalingAddress: "https://app.viam.com:443",
    credentials: {
      type: "api-key",
      payload: apiKey.key,
      authEntity: apiKey.id,
    },
  });

  chessService = new GenericServiceClient(robot, CHESS_SERVICE_NAME);
  setStatus("Connected", "ok");
}

// ── DoCommand wrapper ──────────────────────────────────────────────────────

async function doCommand(cmd: Record<string, unknown>): Promise<Record<string, JsonValue>> {
  if (!chessService) throw new Error("Not connected");
  const result = await chessService.doCommand(Struct.fromJson(cmd as JsonValue));
  return (result ?? {}) as Record<string, JsonValue>;
}

// ── Board rendering ────────────────────────────────────────────────────────

function renderBoard(fen: string) {
  const boardEl = document.getElementById("board")!;
  const parts = fen.split(" ");
  const placement = parts[0];
  const activeColor = parts[1];
  const rows = placement.split("/");

  const table = document.createElement("table");
  table.className = "chess-table";

  rows.forEach((row, ri) => {
    const tr = document.createElement("tr");
    const rankTd = document.createElement("td");
    rankTd.className = "rank-label";
    rankTd.textContent = String(8 - ri);
    tr.appendChild(rankTd);

    let ci = 0;
    for (const ch of row) {
      if (ch >= "1" && ch <= "8") {
        for (let i = 0; i < parseInt(ch); i++) tr.appendChild(makeSquare(ri, ci++, null));
      } else {
        tr.appendChild(makeSquare(ri, ci++, ch));
      }
    }
    table.appendChild(tr);
  });

  const fileTr = document.createElement("tr");
  fileTr.appendChild(document.createElement("td"));
  for (const f of "abcdefgh") {
    const td = document.createElement("td");
    td.className = "file-label";
    td.textContent = f;
    fileTr.appendChild(td);
  }
  table.appendChild(fileTr);

  boardEl.innerHTML = "";
  boardEl.appendChild(table);

  const turn = document.getElementById("turn-indicator")!;
  turn.textContent = activeColor === "w" ? "White to move" : "Black to move";
}

function makeSquare(ri: number, ci: number, piece: string | null): HTMLTableCellElement {
  const td = document.createElement("td");
  td.className = "chess-square " + ((ri + ci) % 2 === 0 ? "light" : "dark");
  if (piece) {
    const span = document.createElement("span");
    span.className = "chess-piece " + (piece === piece.toUpperCase() ? "piece-white" : "piece-black");
    span.textContent = PIECE_UNICODE[piece] ?? "";
    td.appendChild(span);
  }
  return td;
}

// ── Camera board rendering ─────────────────────────────────────────────────

function renderCameraBoard(cameraBoard: Record<string, string>) {
  const el = document.getElementById("camera-board")!;
  const table = document.createElement("table");
  table.className = "chess-table";

  for (let rank = 8; rank >= 1; rank--) {
    const tr = document.createElement("tr");
    const rankTd = document.createElement("td");
    rankTd.className = "rank-label";
    rankTd.textContent = String(rank);
    tr.appendChild(rankTd);

    for (let fi = 0; fi < 8; fi++) {
      const file = "abcdefgh"[fi];
      const square = file + rank;
      const color = cameraBoard[square] ?? "0";
      const ri = 8 - rank;
      const td = document.createElement("td");
      td.className = "chess-square " + ((ri + fi) % 2 === 0 ? "light" : "dark");
      if (color !== "0") {
        const dot = document.createElement("span");
        dot.className = color === "1" ? "cam-white" : "cam-black";
        td.appendChild(dot);
      }
      tr.appendChild(td);
    }
    table.appendChild(tr);
  }

  const fileTr = document.createElement("tr");
  fileTr.appendChild(document.createElement("td"));
  for (const f of "abcdefgh") {
    const td = document.createElement("td");
    td.className = "file-label";
    td.textContent = f;
    fileTr.appendChild(td);
  }
  table.appendChild(fileTr);

  el.innerHTML = "";
  el.appendChild(table);
}

// ── History ────────────────────────────────────────────────────────────────

function addHistory(record: MoveRecord) {
  history.push(record);
  renderHistory();
}

function renderHistory() {
  const list = document.getElementById("history-list")!;
  list.innerHTML = "";
  [...history].reverse().forEach((h) => {
    const li = document.createElement("li");
    li.innerHTML = `<span class="tag tag-${h.type}">${h.type}</span>${h.label}`;
    list.appendChild(li);
  });
}

// ── Status ─────────────────────────────────────────────────────────────────

function setStatus(msg: string, cls: "ok" | "busy" | "err") {
  const el = document.getElementById("status")!;
  el.textContent = msg;
  el.className = `status ${cls}`;
}

function setBusy(busy: boolean) {
  document.getElementById("spinner")!.classList.toggle("hidden", !busy);
  document.querySelectorAll<HTMLButtonElement>("button").forEach((b) => (b.disabled = busy));
}

// ── Mode ───────────────────────────────────────────────────────────────────

function setMode(mode: string) {
  const label = document.getElementById("mode-label")!;
  if (mode === "human") {
    label.textContent = "Human vs Engine";
    label.className = "mode-label mode-human";
  } else {
    label.textContent = "Engine vs Engine";
    label.className = "mode-label mode-engine";
  }
}

// ── Refresh ────────────────────────────────────────────────────────────────

async function refreshState() {
  try {
    const res = await doCommand({ "board-snapshot": true });
    if (typeof res.fen === "string") renderBoard(res.fen);
    if (res.camera_board && typeof res.camera_board === "object") {
      renderCameraBoard(res.camera_board as Record<string, string>);
      document.getElementById("camera-board-section")!.classList.remove("hidden");
    }
    if (typeof res.mode === "string") setMode(res.mode);
  } catch (e) {
    console.error("refresh failed", e);
  }
}

// ── Actions ────────────────────────────────────────────────────────────────

async function withSpinner(label: string, fn: () => Promise<void>): Promise<void> {
  setBusy(true);
  setStatus(label, "busy");
  if (autoRefreshTimer) clearInterval(autoRefreshTimer);
  try {
    await fn();
    setStatus("Connected", "ok");
    await refreshState();
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    setStatus("Error: " + msg, "err");
    console.error(e);
  } finally {
    setBusy(false);
    startAutoRefresh();
  }
}

async function cmdGo() {
  const n = parseInt((document.getElementById("go-n") as HTMLInputElement).value) || 1;
  await withSpinner(`Running ${n} move(s)...`, async () => {
    const res = await doCommand({ go: n });
    const move = typeof res.move === "string" ? res.move : "";
    if (typeof res.mode === "string") setMode(res.mode);
    addHistory({ type: "go", label: `×${n}${move ? " → " + move : ""}` });
  });
}

async function cmdMove() {
  const from = (document.getElementById("move-from") as HTMLInputElement).value.trim().toLowerCase();
  const to   = (document.getElementById("move-to")   as HTMLInputElement).value.trim().toLowerCase();
  const n    = parseInt((document.getElementById("move-n") as HTMLInputElement).value) || 1;
  if (!from || !to) { alert("Enter from and to squares."); return; }
  await withSpinner(`Moving ${from}→${to}...`, async () => {
    await doCommand({ move: { from, to, n } });
    addHistory({ type: "move", label: `${from} → ${to}${n > 1 ? " ×" + n : ""}` });
  });
}

async function cmdReset() {
  if (!confirm("Physically reset the board?")) return;
  await withSpinner("Resetting board...", async () => {
    await doCommand({ reset: true });
    addHistory({ type: "reset", label: "board reset" });
  });
}

async function cmdWipe() {
  if (!confirm("Wipe game state?")) return;
  await withSpinner("Wiping state...", async () => {
    await doCommand({ wipe: true });
    addHistory({ type: "wipe", label: "state wiped" });
  });
}

async function cmdClearCache() {
  await withSpinner("Clearing cache...", async () => {
    await doCommand({ ClearCache: true });
    addHistory({ type: "cache", label: "square cache cleared" });
  });
}

async function cmdSnapshot() {
  await withSpinner("Capturing board snapshot...", async () => {});
}

async function cmdToggleMode() {
  setBusy(true);
  try {
    const res = await doCommand({ "toggle-mode": true });
    if (typeof res.mode === "string") setMode(res.mode);
    setStatus("Connected", "ok");
  } catch (e) {
    setStatus("Error: " + (e instanceof Error ? e.message : String(e)), "err");
  } finally {
    setBusy(false);
  }
}

// ── Auto-refresh ───────────────────────────────────────────────────────────

function startAutoRefresh() {
  if (autoRefreshTimer) clearInterval(autoRefreshTimer);
  autoRefreshTimer = setInterval(refreshState, 5000);
}

// ── Init ───────────────────────────────────────────────────────────────────

document.getElementById("btn-go")!.addEventListener("click", cmdGo);
document.getElementById("btn-move")!.addEventListener("click", cmdMove);
document.getElementById("btn-reset")!.addEventListener("click", cmdReset);
document.getElementById("btn-wipe")!.addEventListener("click", cmdWipe);
document.getElementById("btn-cache")!.addEventListener("click", cmdClearCache);
document.getElementById("btn-snapshot")!.addEventListener("click", cmdSnapshot);
document.getElementById("btn-toggle-mode")!.addEventListener("click", cmdToggleMode);
document.getElementById("btn-refresh")!.addEventListener("click", () => void refreshState());
document.getElementById("btn-clear-history")!.addEventListener("click", () => {
  history.length = 0;
  renderHistory();
});
document.getElementById("move-to")!.addEventListener("keydown", (e) => {
  if (e.key === "Enter") void cmdMove();
});

connect()
  .then(refreshState)
  .then(startAutoRefresh)
  .catch((e) => setStatus("Failed: " + (e instanceof Error ? e.message : String(e)), "err"));
