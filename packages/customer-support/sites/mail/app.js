// The watch documents Mailer writes: one per mailbox and folder (see the plugin's watch), and the
// follows: one per followed message (see the plugin's follow).
// Every field may hold text from an email, so it is only ever set with textContent.

function describePosition(p) {
  if (!p) return '';
  if (p.imap) return 'IMAP UIDVALIDITY ' + p.imap.uidValidity + ', last UID ' + p.imap.lastUid;
  if (p.gmail) return 'Gmail historyId ' + p.gmail.historyId;
  if (p.graph) return 'Microsoft Graph delta, watching mail received since ' + p.graph.since;
  return '';
}

function render(docs) {
  const holder = document.getElementById('watches');
  const tpl = document.getElementById('watch');
  const sections = [];
  // The collection also holds Mailer's record of the messages it has shown (ids starting seen-),
  // which a reply checks against; it is not a watch.
  docs = docs.filter((item) => !String(item.id || '').startsWith('seen-') && !(item.doc && item.doc.shown));
  for (const item of docs) {
    const d = item.doc || {};
    const node = tpl.content.cloneNode(true);
    node.querySelector('.folder').textContent = d.folder || '';
    node.querySelector('.account').textContent = d.account || '';
    node.querySelector('.provider').textContent = d.provider || '';
    node.querySelector('.started').textContent = d.startedAt || '';
    node.querySelector('.last').textContent = (d.lastCheck || '') + (d.lastCheck ? ' (' + (d.lastFound || 0) + ' new)' : '');
    node.querySelector('.checks').textContent = String(d.checks || 0);
    node.querySelector('.position').textContent = describePosition(d.position);
    const pending = d.pending || [];
    const body = node.querySelector('tbody');
    for (const p of pending) {
      const tr = document.createElement('tr');
      const cells = [[p.id, 'id'], [p.from], [p.subject], [p.date], [String(p.listed || 0) + ' of 4']];
      for (const [text, cls] of cells) {
        const td = document.createElement('td');
        td.textContent = text || '';
        if (cls) td.className = cls;
        tr.append(td);
      }
      body.append(tr);
    }
    node.querySelector('.none').classList.toggle('hidden', pending.length > 0);
    node.querySelector('.pending').classList.toggle('hidden', pending.length === 0);
    sections.push(node);
  }
  holder.replaceChildren(...sections);
  document.getElementById('status').textContent = docs.length === 0
    ? 'The watch has not run yet: its first check, at install, starts it from now.'
    : 'Read ' + new Date().toLocaleTimeString() + '.';
}

function when(text) {
  if (!text) return '';
  const d = new Date(text);
  return isNaN(d) ? text : d.toLocaleString();
}

function renderFollows(docs) {
  const body = document.getElementById('follows');
  const rows = [];
  docs = docs.map((item) => item.doc || {}).sort((a, b) => String(a.since || '').localeCompare(String(b.since || '')));
  for (const f of docs) {
    const tr = document.createElement('tr');
    const cells = [f.from, f.subject, when(f.since), f.lastReminded ? when(f.lastReminded) : 'Not yet', 'Every ' + (f.every || 1) + ' h'];
    for (const text of cells) {
      const td = document.createElement('td');
      td.textContent = text || '';
      tr.append(td);
    }
    rows.push(tr);
  }
  body.replaceChildren(...rows);
  document.getElementById('follows-none').classList.toggle('hidden', rows.length > 0);
  document.getElementById('follows-table').classList.toggle('hidden', rows.length === 0);
}

async function refresh() {
  try {
    const [watches, follows] = await Promise.all([site.data.list('watch'), site.data.list('follows')]);
    render(watches);
    renderFollows(follows);
  } catch (err) {
    document.getElementById('status').textContent = err.message;
  }
}

refresh();
setInterval(refresh, 10000);
addEventListener('focus', refresh);
