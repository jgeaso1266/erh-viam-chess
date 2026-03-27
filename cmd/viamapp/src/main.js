import { createRobotClient, GenericServiceClient } from "@viamrobotics/sdk";
import { Struct } from "@viamrobotics/sdk";
import Cookies from "js-cookie";
// ── State ──────────────────────────────────────────────────────────────────
const CHESS_SERVICE_NAME = "chess";
const PIECE_UNICODE = {
    K: "♔", Q: "♕", R: "♖", B: "♗", N: "♘", P: "♙",
    k: "♚", q: "♛", r: "♜", b: "♝", n: "♞", p: "♟",
};
let chessService = null;
const history = [];
let autoRefreshTimer = null;
// ── Connection ─────────────────────────────────────────────────────────────
async function connect() {
    const cookieKey = window.location.pathname.split("/")[2];
    const raw = Cookies.get(cookieKey);
    if (!raw)
        throw new Error("Viam machine cookie not found — open this app from the Viam portal.");
    const cookie = JSON.parse(raw);
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
async function doCommand(cmd) {
    if (!chessService)
        throw new Error("Not connected");
    const result = await chessService.doCommand(Struct.fromJson(cmd));
    return (result ?? {});
}
// ── Board rendering ────────────────────────────────────────────────────────
function renderBoard(fen) {
    const boardEl = document.getElementById("board");
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
                for (let i = 0; i < parseInt(ch); i++)
                    tr.appendChild(makeSquare(ri, ci++, null));
            }
            else {
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
    const turn = document.getElementById("turn-indicator");
    turn.textContent = activeColor === "w" ? "White to move" : "Black to move";
}
function makeSquare(ri, ci, piece) {
    const td = document.createElement("td");
    td.className = "chess-square " + ((ri + ci) % 2 === 0 ? "light" : "dark");
    if (piece) {
        const span = document.createElement("span");
        span.className = "chess-piece";
        span.textContent = PIECE_UNICODE[piece] ?? "";
        td.appendChild(span);
    }
    return td;
}
// ── History ────────────────────────────────────────────────────────────────
function addHistory(record) {
    history.push(record);
    renderHistory();
}
function renderHistory() {
    const list = document.getElementById("history-list");
    list.innerHTML = "";
    [...history].reverse().forEach((h) => {
        const li = document.createElement("li");
        li.innerHTML = `<span class="tag tag-${h.type}">${h.type}</span>${h.label}`;
        list.appendChild(li);
    });
}
// ── Status ─────────────────────────────────────────────────────────────────
function setStatus(msg, cls) {
    const el = document.getElementById("status");
    el.textContent = msg;
    el.className = `status ${cls}`;
}
function setBusy(busy) {
    document.getElementById("spinner").classList.toggle("hidden", !busy);
    document.querySelectorAll("button").forEach((b) => (b.disabled = busy));
}
// ── Refresh ────────────────────────────────────────────────────────────────
async function refreshState() {
    try {
        const res = await doCommand({ info: true });
        if (typeof res.fen === "string")
            renderBoard(res.fen);
    }
    catch (e) {
        console.error("refresh failed", e);
    }
}
// ── Actions ────────────────────────────────────────────────────────────────
async function withSpinner(label, fn) {
    setBusy(true);
    setStatus(label, "busy");
    if (autoRefreshTimer)
        clearInterval(autoRefreshTimer);
    try {
        await fn();
        setStatus("Connected", "ok");
        await refreshState();
    }
    catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        setStatus("Error: " + msg, "err");
        console.error(e);
    }
    finally {
        setBusy(false);
        startAutoRefresh();
    }
}
async function cmdGo() {
    const n = parseInt(document.getElementById("go-n").value) || 1;
    await withSpinner(`Running ${n} move(s)...`, async () => {
        const res = await doCommand({ go: n });
        const move = typeof res.move === "string" ? res.move : "";
        addHistory({ type: "go", label: `×${n}${move ? " → " + move : ""}` });
    });
}
async function cmdMove() {
    const from = document.getElementById("move-from").value.trim().toLowerCase();
    const to = document.getElementById("move-to").value.trim().toLowerCase();
    const n = parseInt(document.getElementById("move-n").value) || 1;
    if (!from || !to) {
        alert("Enter from and to squares.");
        return;
    }
    await withSpinner(`Moving ${from}→${to}...`, async () => {
        await doCommand({ move: { from, to, n } });
        addHistory({ type: "move", label: `${from} → ${to}${n > 1 ? " ×" + n : ""}` });
    });
}
async function cmdReset() {
    if (!confirm("Physically reset the board?"))
        return;
    await withSpinner("Resetting board...", async () => {
        await doCommand({ reset: true });
        addHistory({ type: "reset", label: "board reset" });
    });
}
async function cmdWipe() {
    if (!confirm("Wipe game state?"))
        return;
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
// ── Auto-refresh ───────────────────────────────────────────────────────────
function startAutoRefresh() {
    if (autoRefreshTimer)
        clearInterval(autoRefreshTimer);
    autoRefreshTimer = setInterval(refreshState, 5000);
}
// ── Init ───────────────────────────────────────────────────────────────────
document.getElementById("btn-go").addEventListener("click", cmdGo);
document.getElementById("btn-move").addEventListener("click", cmdMove);
document.getElementById("btn-reset").addEventListener("click", cmdReset);
document.getElementById("btn-wipe").addEventListener("click", cmdWipe);
document.getElementById("btn-cache").addEventListener("click", cmdClearCache);
document.getElementById("btn-refresh").addEventListener("click", () => void refreshState());
document.getElementById("btn-clear-history").addEventListener("click", () => {
    history.length = 0;
    renderHistory();
});
document.getElementById("move-to").addEventListener("keydown", (e) => {
    if (e.key === "Enter")
        void cmdMove();
});
connect()
    .then(refreshState)
    .then(startAutoRefresh)
    .catch((e) => setStatus("Failed: " + (e instanceof Error ? e.message : String(e)), "err"));
