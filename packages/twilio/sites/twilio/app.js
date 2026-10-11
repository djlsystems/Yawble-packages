// The twilio site: the setup checklist (state/setup), recent texts (messages) and the
// consented-recipients list (recipients). Every value from the data is shown with textContent.

// Matched against the step names `check` prints, first match wins: secrets, credentials,
// account type, number / buy a number / choose fromNumber, capabilities, registration, mode,
// allowlist, first message. mode and allowlist are panel settings, so they get no link.
// An optional third pattern must also match the step's text: a registration line that names
// A2P 10DLC (a local number) opens A2P onboarding; any other one (toll-free) opens Active numbers.
const CONSOLE = [
  [/secret|credential/i, 'https://console.twilio.com/us1/account/keys-credentials/api-keys'],
  [/account type|upgrade/i, 'https://console.twilio.com/us1/billing/manage-billing/billing-overview'],
  [/buy a number/i, 'https://console.twilio.com/us1/develop/phone-numbers/manage/search'],
  [/number|capabilit/i, 'https://console.twilio.com/us1/develop/phone-numbers/manage/incoming'],
  [/registration/i, 'https://console.twilio.com/us1/develop/sms/regulatory-compliance/a2p-onboarding', /10DLC/],
  [/registration/i, 'https://console.twilio.com/us1/develop/phone-numbers/manage/incoming'],
  [/first message/i, 'https://console.twilio.com/us1/monitor/logs/sms'],
];

const E164 = /^\+[1-9][0-9]{6,14}$/;

function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined && text !== null) node.textContent = String(text);
  if (className) node.className = className;
  return node;
}

function showError(err) {
  const box = document.getElementById('error');
  box.textContent = err ? (err.message || String(err)) : '';
  box.hidden = !err;
}

function normalise(raw) {
  const digits = String(raw || '').replace(/[\s().-]/g, '');
  if (/^[0-9]{10}$/.test(digits)) return '+1' + digits;
  if (/^1[0-9]{10}$/.test(digits)) return '+' + digits;
  return digits;
}

function when(value) {
  if (!value) return '';
  const date = new Date(value);
  return isNaN(date) ? String(value) : date.toLocaleString();
}

// state/setup holds the last `check`: either steps [{step, state, text}] or lines
// ["Done: <step> — <evidence>", ...]. Both are read.
function setupSteps(doc) {
  if (!doc) return [];
  if (Array.isArray(doc.steps)) {
    return doc.steps.map(s => ({ state: String(s.state || s.status || ''), step: String(s.step || s.name || ''), text: String(s.text || s.evidence || s.detail || '') }));
  }
  const lines = Array.isArray(doc.lines) ? doc.lines : String(doc.text || doc.output || '').split('\n');
  return lines
    .map(line => /^(Done|Next|Waiting):\s*(.*?)(?:\s+[—-]\s+(.*))?$/.exec(String(line).trim()))
    .filter(Boolean)
    .map(m => ({ state: m[1], step: m[2], text: m[3] || '' }));
}

async function renderSetup() {
  const record = await site.data.get('state', 'setup');
  const steps = setupSteps(record && record.doc);
  const list = document.getElementById('setup');
  const template = document.getElementById('step-row');
  const rows = steps.map(s => {
    const row = template.content.firstElementChild.cloneNode(true);
    const state = s.state.toLowerCase();
    const badge = row.querySelector('.badge');
    badge.textContent = state === 'done' ? 'Done' : state === 'waiting' ? 'Waiting' : 'Next';
    badge.classList.add(state === 'done' || state === 'waiting' ? state : 'next');
    row.querySelector('.step').textContent = s.step;
    row.querySelector('.text').textContent = s.text;
    const link = row.querySelector('.link');
    const match = state === 'done' ? null : CONSOLE.find(([pattern, , text]) =>
      pattern.test(s.step) && (!text || text.test(s.text || '')));
    if (match) {
      link.href = match[1];
      link.textContent = 'Open in Twilio Console';
    } else {
      link.remove();
    }
    return row;
  });
  list.replaceChildren(...rows);
  const at = record && (record.doc.checkedAt || record.updatedAt);
  document.getElementById('setup-when').textContent = steps.length
    ? 'Last checked ' + when(at) + '. Ask the Texting Assistant to run check again.'
    : 'Waiting for the first check. It runs right after install and then with every poll.';
}

// The text exactly as it was sent (an outbound text's `body`, the business name and the opt-out
// line included), or the snippet the plugin kept; a setup test is a tag beside it, never in it.
function textCell(d) {
  const td = el('td');
  if (d.test) td.append(el('span', 'setup test', 'tag'), ' ');
  td.append(String(d.body || d.snippet || ''));
  return td;
}

async function renderMessages() {
  const items = await site.data.list('messages');
  items.sort((a, b) => String(b.doc.created || b.updatedAt).localeCompare(String(a.doc.created || a.updatedAt)));
  const rows = items.slice(0, 200).map(item => {
    const d = item.doc;
    const tr = document.createElement('tr');
    const inbound = d.direction === 'in' || String(d.direction || '').startsWith('inbound');
    const status = String(d.status || '') + (d.errorCode || d.error_code ? ' (' + (d.errorCode || d.error_code) + ')' : '');
    tr.append(
      el('td', when(d.created || item.updatedAt)),
      el('td', inbound ? 'in' : 'out'),
      el('td', inbound ? d.from : d.to),
      el('td', status, 'status-' + String(d.status || '').toLowerCase()),
      textCell(d),
      el('td', d.segments === undefined ? '' : d.segments));
    return tr;
  });
  document.getElementById('messages').replaceChildren(...rows);
  document.getElementById('messages-empty').hidden = rows.length > 0;
}

function evidenceCell(evidence) {
  const td = document.createElement('div');
  const text = String(evidence || '');
  if (/^[^\s:]+\/[^\s]*\.[A-Za-z0-9]+$|^[^\s:/]+\.[A-Za-z0-9]{2,5}$/.test(text)) {
    try {
      const a = el('a', text);
      a.href = site.files.url(text);
      td.append(a);
      return td;
    } catch (_) {
      // not a file path the site serves: show it as a note
    }
  }
  td.textContent = text;
  return td;
}

async function setStatus(item, status) {
  const doc = Object.assign({}, item.doc, { status });
  const me = await site.whoami();
  if (status === 'active') {
    doc.activatedAt = new Date().toISOString();
    doc.activatedBy = me.displayName;
  }
  if (status === 'opted-out') {
    doc.optedOutAt = new Date().toISOString();
    doc.optOutKeyword = 'by request';
  }
  await site.data.put('recipients', item.id, doc);
}

async function renderRecipients() {
  const items = await site.data.list('recipients');
  items.sort((a, b) => String(b.doc.addedAt || '').localeCompare(String(a.doc.addedAt || '')));
  const rows = items.map(item => {
    const d = item.doc;
    const consent = d.consent || {};
    const tr = document.createElement('tr');
    const statusText = d.status === 'opted-out'
      ? 'opted-out' + (d.optOutKeyword ? ' (' + d.optOutKeyword + (d.optedOutAt ? ', ' + when(d.optedOutAt) : '') + ')' : '')
      : String(d.status || '');
    const consentCell = el('td', [consent.method, consent.capturedAt].filter(Boolean).join(', '));
    const evidence = evidenceCell(consent.evidence);
    evidence.classList.add('evidence');
    consentCell.append(evidence);
    const actions = document.createElement('td');
    if (d.status !== 'active') {
      const activate = el('button', d.status === 'opted-out' ? 'Re-activate' : 'Activate');
      activate.type = 'button';
      if (d.status === 'opted-out') activate.title = 'Only after they texted START and Twilio lifted its block.';
      activate.addEventListener('click', () => setStatus(item, 'active').then(render, showError));
      actions.append(activate);
    }
    if (d.status !== 'opted-out') {
      const optout = el('button', 'Mark opted out');
      optout.type = 'button';
      optout.addEventListener('click', () => setStatus(item, 'opted-out').then(render, showError));
      actions.append(' ', optout);
    }
    tr.append(
      el('td', d.number),
      el('td', d.name),
      el('td', statusText, 'status-' + String(d.status || '')),
      consentCell,
      el('td', consent.purpose),
      el('td', [when(d.addedAt), d.addedBy].filter(Boolean).join(' by ')),
      actions);
    return tr;
  });
  document.getElementById('recipients').replaceChildren(...rows);
  document.getElementById('recipients-empty').hidden = rows.length > 0;
}

async function addRecipient(event) {
  event.preventDefault();
  const form = event.target;
  const result = document.getElementById('add-result');
  const value = name => String(form.elements[name].value || '').trim();
  const number = normalise(value('number'));
  const missing = ['name', 'method', 'capturedAt', 'purpose', 'evidence'].filter(n => !value(n));
  if (!E164.test(number)) {
    result.textContent = 'The number must be 10 US digits or +country format, such as +15551234567.';
    return;
  }
  if (missing.length) {
    result.textContent = 'Fill in every field: ' + missing.join(', ') + ' (time zone is optional).';
    return;
  }
  const id = number.slice(1);
  if (await site.data.get('recipients', id)) {
    result.textContent = number + ' is already on the list.';
    return;
  }
  const me = await site.whoami();
  const now = new Date().toISOString();
  const doc = {
    number,
    name: value('name'),
    status: 'pending',
    consent: {
      method: value('method'),
      capturedAt: value('capturedAt'),
      purpose: value('purpose'),
      evidence: value('evidence'),
      capturedBy: me.displayName,
    },
    addedAt: now,
    addedBy: me.displayName,
    firstMessageSent: false,
  };
  await site.data.put('recipients', id, doc);
  form.reset();
  result.textContent = number + ' added as pending. Press Activate when the consent record is complete.';
  await renderRecipients();
}

async function render() {
  try {
    await Promise.all([renderSetup(), renderMessages(), renderRecipients()]);
    showError(null);
  } catch (err) {
    showError(err);
  }
}

document.getElementById('add').addEventListener('submit', event => addRecipient(event).catch(showError));
render();
setInterval(render, 10000);
addEventListener('focus', render);
