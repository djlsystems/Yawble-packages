// The watch documents Mailer writes: one per mailbox and folder (see the plugin's watch).
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

async function refresh() {
  try {
    render(await site.data.list('watch'));
  } catch (err) {
    document.getElementById('status').textContent = err.message;
  }
}

refresh();
setInterval(refresh, 10000);
addEventListener('focus', refresh);
