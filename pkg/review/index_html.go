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
  .actions { display: flex; gap: 8px; margin-top: 10px; }
  button { background: #2a2a2a; color: #eee; border: 1px solid #444; border-radius: 4px; padding: 6px 14px; cursor: pointer; }
  button.approve { background: #1f5c2e; border-color: #2d7d40; }
  button.reject { background: #5c1f1f; border-color: #7d2d2d; }
  #finish { position: fixed; top: 20px; right: 24px; background: #4a9eff; border-color: #4a9eff; color: #111; font-weight: 600; }
  #empty { color: #888; margin-top: 40px; }
</style>
</head>
<body>
<h1>Review Queue</h1>
<div class="hint">J/K to move, A to approve, R to reject the active card. Nothing is cut until you approve it.</div>
<button id="finish">Finish review</button>
<div id="cards"></div>
<div id="empty" style="display:none">Nothing pending review.</div>

<script>
let candidates = [];
let activeIndex = 0;

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
    card.innerHTML =
      '<img src="/api/candidates/' + c.id + '/preview.jpg" onerror="this.style.opacity=0">' +
      '<div class="meta">' +
        '<h3>' + (c.llm_title || '(untitled)') + '</h3>' +
        '<span class="badge">' + c.archetype + '</span>' +
        '<span class="badge">det score ' + c.det_score.toFixed(2) + '</span>' +
        '<span class="badge">llm score ' + c.llm_score.toFixed(2) + '</span>' +
        '<span class="badge">' + c.start.toFixed(1) + 's → ' + c.end.toFixed(1) + 's</span>' +
        '<div class="signals">signals fired: ' + signalsText + '</div>' +
        '<div class="reason">' + (c.llm_reason || '') + '</div>' +
        '<div class="actions">' +
          '<button class="approve" onclick="approve(' + c.id + ')">Approve</button>' +
          '<button class="reject" onclick="reject(' + c.id + ')">Reject</button>' +
        '</div>' +
      '</div>';
    container.appendChild(card);
  });
}

async function approve(id) {
  await fetch('/api/candidates/' + id + '/approve', { method: 'POST' });
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
});

load();
setInterval(load, 5000);
</script>
</body>
</html>`
