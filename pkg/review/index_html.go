package review

const indexHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Gameplay Clipper — Review Queue</title>
<style>
  body { font-family: -apple-system, sans-serif; background: #111; color: #eee; margin: 0; padding: 24px; }
  h1 { font-size: 18px; font-weight: 600; }
  .hint { color: #888; font-size: 13px; margin-bottom: 20px; }
  .card { display: flex; gap: 16px; background: #1c1c1c; border: 2px solid #333; border-radius: 8px;
          padding: 16px; margin-bottom: 12px; }
  .card.active { border-color: #4a9eff; }
  .card img { width: 240px; height: 135px; object-fit: cover; background: #000; border-radius: 4px; }
  .meta { flex: 1; }
  .meta h3 { margin: 0 0 4px 0; }
  .badge { display: inline-block; background: #2a2a2a; border-radius: 4px; padding: 2px 8px; font-size: 12px; margin-right: 6px; }
  .signals { font-size: 12px; color: #999; margin-top: 6px; }
  .reason { font-size: 13px; color: #ccc; margin-top: 6px; }
  .formats { display: flex; gap: 16px; align-items: center; margin-top: 10px; font-size: 13px; }
  .formats label { display: flex; gap: 6px; align-items: center; cursor: pointer; }
  .formats .dim { color: #777; font-size: 12px; }
  .nofmt { color: #d08a3a; font-size: 12px; }
  .actions { display: flex; gap: 8px; margin-top: 10px; }
  button { background: #2a2a2a; color: #eee; border: 1px solid #444; border-radius: 4px; padding: 6px 14px; cursor: pointer; }
  button.approve { background: #1f5c2e; border-color: #2d7d40; }
  button.reject { background: #5c1f1f; border-color: #7d2d2d; }
  button:disabled { opacity: 0.4; cursor: not-allowed; }
  #finish { position: fixed; top: 20px; right: 24px; background: #4a9eff; border-color: #4a9eff; color: #111; font-weight: 600; }
  #empty { color: #888; margin-top: 40px; }
</style>
</head>
<body>
<h1>Review Queue</h1>
<div class="hint">J/K to move, A to approve, R to reject the active card. C/S toggle which files the active card renders.
Nothing is encoded until you approve it — a rejected card costs no render at all.</div>
<button id="finish">Finish review</button>
<div id="cards"></div>
<div id="empty" style="display:none">Nothing pending review.</div>

<script>
let candidates = [];
let activeIndex = 0;

// Which files each candidate should render, keyed by candidate id. Held in JS
// rather than read off the checkboxes at approve time because load() runs every
// 5s and rebuilds the card list from scratch — DOM state would be wiped mid-review.
const formatSel = {};

function formatsFor(id) {
  if (!formatSel[id]) formatSel[id] = { clip: true, short: true };
  return formatSel[id];
}

function selectedFormats(id) {
  const sel = formatsFor(id);
  return ['clip', 'short'].filter(k => sel[k]);
}

function toggleFormat(id, fmt, on) {
  formatsFor(id)[fmt] = on;
  render();
}

async function load() {
  const res = await fetch('/api/candidates');
  candidates = await res.json();
  render();
}

function render() {
  const container = document.getElementById('cards');
  const empty = document.getElementById('empty');
  container.innerHTML = '';
  if (candidates.length === 0) {
    empty.style.display = 'block';
    return;
  }
  empty.style.display = 'none';
  if (activeIndex >= candidates.length) activeIndex = candidates.length - 1;
  if (activeIndex < 0) activeIndex = 0;

  candidates.forEach((c, i) => {
    const card = document.createElement('div');
    card.className = 'card' + (i === activeIndex ? ' active' : '');
    // signals_fired arrives already parsed as an array (json.RawMessage is
    // emitted inline), so use it directly; tolerate a JSON string too, in
    // case an older row stored it that way. Calling JSON.parse on an array
    // throws ("[object Object]" is not valid JSON) and blanks the whole page.
    const rawSignals = c.signals_fired;
    const signals = (Array.isArray(rawSignals)
      ? rawSignals
      : (rawSignals ? JSON.parse(rawSignals) : [])) || [];
    const signalsText = signals.length
      ? signals.map(s => s.Kind + '@' + Math.round(s.T) + 's').join(', ')
      : 'none (generic detection only)';
    // The LLM hasn't run yet at review time — it only generates a title on
    // approval — so cards are driven by the deterministic evidence the reviewer
    // actually has: archetype, det score, timespan, signals, and the preview
    // frame. The llm_* fields are only shown if a prior run happened to fill
    // them (guarded, since llm_score is 0 / llm_title is '' when unjudged).
    const heading = c.llm_title || (c.archetype + ' · ' + c.start.toFixed(1) + 's → ' + c.end.toFixed(1) + 's');
    const llmScoreBadge = c.llm_score ? '<span class="badge">llm score ' + c.llm_score.toFixed(2) + '</span>' : '';
    const reasonBlock = c.llm_reason ? '<div class="reason">' + c.llm_reason + '</div>' : '';
    // Only the boxes ticked here get encoded on approval, so a clip you only
    // want vertically never pays for a full-resolution master.
    const sel = formatsFor(c.id);
    const chosen = selectedFormats(c.id);
    const checkbox = (key, label, note) =>
      '<label><input type="checkbox" ' + (sel[key] ? 'checked' : '') +
      ' onchange="toggleFormat(' + c.id + ', \'' + key + '\', this.checked)">' +
      label + ' <span class="dim">' + note + '</span></label>';
    const noneWarning = chosen.length ? '' : '<span class="nofmt">pick at least one file to render</span>';
    card.innerHTML =
      '<img src="/api/candidates/' + c.id + '/preview.jpg" onerror="this.style.opacity=0">' +
      '<div class="meta">' +
        '<h3>' + heading + '</h3>' +
        '<span class="badge">' + c.archetype + '</span>' +
        '<span class="badge">det score ' + c.det_score.toFixed(2) + '</span>' +
        llmScoreBadge +
        '<span class="badge">' + c.start.toFixed(1) + 's → ' + c.end.toFixed(1) + 's</span>' +
        '<div class="signals">signals fired: ' + signalsText + '</div>' +
        reasonBlock +
        '<div class="formats">' +
          checkbox('clip', 'clip.mp4', 'native resolution') +
          checkbox('short', 'short.mp4', '9:16 vertical') +
          noneWarning +
        '</div>' +
        '<div class="actions">' +
          '<button class="approve" onclick="approve(' + c.id + ')"' + (chosen.length ? '' : ' disabled') + '>Approve</button>' +
          '<button class="reject" onclick="reject(' + c.id + ')">Reject</button>' +
        '</div>' +
      '</div>';
    container.appendChild(card);
  });
}

async function approve(id) {
  const formats = selectedFormats(id);
  if (formats.length === 0) return; // approving with nothing ticked would render nothing
  const res = await fetch('/api/candidates/' + id + '/approve', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ formats: formats }),
  });
  if (!res.ok) {
    alert('Approve failed: ' + (await res.text()));
    return;
  }
  await load();
}

async function reject(id) {
  const reason = prompt('Reason (optional):', '') || '';
  await fetch('/api/candidates/' + id + '/reject', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({reason: reason}),
  });
  await load();
}

document.getElementById('finish').onclick = async () => {
  await fetch('/api/done', { method: 'POST' });
  document.body.innerHTML = '<h1>Review finished — you can close this tab.</h1>';
};

document.addEventListener('keydown', (e) => {
  if (candidates.length === 0) return;
  const active = candidates[activeIndex];
  if (e.key === 'j') { activeIndex = Math.min(activeIndex + 1, candidates.length - 1); render(); }
  if (e.key === 'k') { activeIndex = Math.max(activeIndex - 1, 0); render(); }
  if (e.key === 'a' && active) approve(active.id);
  if (e.key === 'r' && active) reject(active.id);
  if (e.key === 'c' && active) toggleFormat(active.id, 'clip', !formatsFor(active.id).clip);
  if (e.key === 's' && active) toggleFormat(active.id, 'short', !formatsFor(active.id).short);
});

load();
setInterval(load, 5000);
</script>
</body>
</html>`
