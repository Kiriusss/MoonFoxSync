'use strict';

const $ = id => document.getElementById(id);
const state = {view: 'units', unit: null, snapshot: '', path: '', cursor: null};
let generation = 0;
let previewRequest = 0;
let pageRequest = null;
let fileRequest = null;
const apiRoot = '/reader/api/units';
const previewLimit = 64 * 1024;
const status = message => { $('status').textContent = message; };

function hidePreview() {
  previewRequest++;
  fileRequest?.abort();
  $('preview').hidden = true;
  $('text').textContent = '';
}
function showPair() {
  generation++;
  pageRequest?.abort();
  hidePreview();
  $('library').hidden = true;
  $('logout').hidden = true;
  $('pair').hidden = false;
  $('list').replaceChildren();
}
function errors(code) {
  return {
    401:'配对码无效、已过期，或此设备授权已撤销。',
    403:'需要使用只读配对码。',
    404:'此项目已移除或不在当前授权范围。',
    409:'目录或文件正在变动，请刷新后重试。',
    410:'清单已变更，请刷新后重试。',
    412:'文件已变更，请刷新后重新打开。',
    429:'阅读请求较多，请稍后重试。',
    503:'NAS 数据卷暂时不可用。'
  }[code] || ('请求失败（HTTP ' + code + '）。');
}
async function request(url, options = {}, acceptEmpty = false) {
  const response = await fetch(url, {credentials:'same-origin', mode:'same-origin', redirect:'error', cache:'no-store', ...options});
  if (response.status === 401) showPair();
  if (acceptEmpty && response.status === 416 && response.headers.get('Content-Range') === 'bytes */0') return null;
  if (!response.ok) throw new Error(errors(response.status));
  return response;
}
function failure(e) {
  if (e.name !== 'AbortError') status(e.message || '无法连接梦枢天罗。');
}
function params(path, pairs) {
  const q = new URLSearchParams();
  Object.entries(pairs).forEach(([k,v]) => { if (v !== null && v !== undefined) q.set(k, v); });
  return path + '?' + q;
}
function browseURL(resource) {
  return apiRoot + '/' + encodeURIComponent(state.unit.unitId) + '/' + resource;
}
function fileURL(entry) {
  return params(browseURL('content'), {path:entry.path, snapshotId:state.snapshot || null});
}
function action(label, fn) {
  const b = document.createElement('button');
  b.type = 'button';
  b.textContent = label;
  b.addEventListener('click', fn);
  return b;
}
function row(name, detail, ...actions) {
  const r = document.createElement('div');
  r.className = 'row';
  const main = document.createElement('div');
  main.className = 'main';
  const title = document.createElement('div');
  title.className = 'name';
  title.textContent = name;
  const sub = document.createElement('div');
  sub.className = 'detail';
  sub.textContent = detail;
  main.append(title, sub);
  const buttons = document.createElement('div');
  buttons.className = 'actions';
  buttons.append(...actions);
  r.append(main, buttons);
  $('list').append(r);
}
function readableSize(bytes) {
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KiB';
  if (bytes < 1024 ** 3) return (bytes / (1024 ** 2)).toFixed(1) + ' MiB';
  return (bytes / (1024 ** 3)).toFixed(1) + ' GiB';
}
function navigate(view, unit = null, snapshot = '', path = '') {
  Object.assign(state, {view, unit, snapshot, path, cursor:null});
  renderPage(false);
}
function title() {
  const u = state.unit;
  $('pair').hidden = true;
  $('library').hidden = false;
  $('logout').hidden = false;
  $('history').hidden = !(u && u.kind === 'managed' && state.view !== 'history');
  const archive = $('archive');
  archive.hidden = !(u && u.kind === 'managed' && state.view === 'entries' && state.snapshot);
  if (archive.hidden) archive.removeAttribute('href');
  else archive.href = params(browseURL('archive'), {snapshotId:state.snapshot});
  $('heading').textContent = state.view === 'units' ? '资料库' :
    state.view === 'history' ? '版本历史' : state.path.split('/').pop() || u.name;
  $('meta').textContent = u ? (u.kind === 'external' ? 'NAS 只读目录' :
    (state.snapshot ? '快照 ' + state.snapshot : '受管资料库')) : '';
  const crumbs = $('crumbs');
  crumbs.replaceChildren();
  if (u) crumbs.append(action('资料库', () => navigate('units')));
  if (state.view === 'entries' && state.path) {
    crumbs.append(action(u.name, () => navigate('entries', u, state.snapshot)));
    const parts = state.path.split('/');
    parts.slice(0, -1).forEach((name, i) => crumbs.append(action(name, () => navigate('entries', u, state.snapshot, parts.slice(0, i + 1).join('/')))));
  }
}
function addItem(item) {
  if (state.view === 'units') {
    const managed = item.kind === 'managed';
    const open = () => navigate(managed && !item.head ? 'history' : 'entries', item, item.head || '');
    row(item.name, managed ? '受管资料库' : 'NAS 只读目录', action('打开', open));
    return;
  }
  if (state.view === 'history') {
    const date = new Date(item.committedAt).toLocaleString();
    row(item.label || date, [item.label ? date : '', item.outcome === 'fork' ? '分叉' : '主线', item.gitBranch, item.snapshotId.slice(-8)].filter(Boolean).join(' · '),
      action('查看', () => navigate('entries', state.unit, item.snapshotId)));
    return;
  }
  const name = item.path.split('/').pop();
  if (item.kind === 'directory') {
    row(name, '文件夹', action('打开', () => navigate('entries', state.unit, state.snapshot, item.path)));
  } else if (item.kind === 'file') {
    const link = document.createElement('a');
    link.className = 'download';
    link.href = fileURL(item);
    link.download = name;
    link.textContent = '下载';
    row(name, readableSize(item.size), action('预览', () => preview(item)), link);
  } else {
    row(name, '符号链接');
  }
}
async function renderPage(more) {
  const ticket = ++generation;
  hidePreview();
  pageRequest?.abort();
  pageRequest = new AbortController();
  if (!more) {
    state.cursor = null;
    $('list').replaceChildren();
  }
  $('more').disabled = true;
  $('more').hidden = true;
  status('正在读取…');
  title();
  const collection = state.view === 'units' ? apiRoot : browseURL(state.view === 'history' ? 'snapshots' : 'entries');
  const query = {cursor:state.cursor, limit:40};
  if (state.view === 'entries') {
    query.path = state.path;
    query.snapshotId = state.snapshot || null;
  }
  try {
    const response = await request(params(collection, query), {signal:pageRequest.signal});
    const page = await response.json();
    if (ticket !== generation) return;
    title();
    page.items.forEach(addItem);
    state.cursor = page.nextCursor;
    $('more').hidden = !state.cursor;
    $('more').disabled = false;
    status($('list').childElementCount ? '' : '暂无内容。');
  } catch (e) {
    if (e.name === 'AbortError') return;
    if (ticket === generation) {
      if (more && state.cursor) {
        $('more').hidden = false;
        $('more').disabled = false;
      }
      failure(e);
    } else if (!$('pair').hidden) {
      status('');
    }
  }
}
async function preview(item) {
  hidePreview();
  const ticket = previewRequest;
  fileRequest = new AbortController();
  const headers = {Range:'bytes=0-' + (previewLimit - 1)};
  if (item.digest) headers['If-Match'] = '"' + item.digest + '"';
  $('filename').textContent = item.path.split('/').pop();
  $('preview-note').textContent = '正在读取预览…';
  $('preview').hidden = false;
  try {
    const response = await request(fileURL(item), {headers, signal:fileRequest.signal}, true);
    if (!response) {
      if (ticket === previewRequest) $('preview-note').textContent = '空文件';
      return;
    }
    const length = Number(response.headers.get('Content-Length'));
    if (!Number.isSafeInteger(length) || length < 0 || length > previewLimit) throw new Error('预览超过大小限制。');
    const data = new Uint8Array(await response.arrayBuffer());
    if (ticket !== previewRequest) return;
    if (data.length > previewLimit) throw new Error('预览超过大小限制。');
    const range = response.headers.get('Content-Range');
    const actualSize = range ? Number(range.split('/').pop()) : data.length;
    const partial = actualSize > data.length;
    // Incomplete final UTF-8 characters in a partial range are omitted by stream mode.
    let decoded;
    try {
      decoded = new TextDecoder('utf-8', {fatal:true}).decode(data, {stream:partial});
    } catch {
      throw new Error('此文件无法按 UTF-8 文本预览，可下载原文件。');
    }
    if (Array.from(decoded).some(ch => { const n = ch.codePointAt(0); return (n < 32 && ![9,10,13].includes(n)) || n === 127; })) {
      throw new Error('此文件包含二进制数据，可下载原文件。');
    }
    $('preview-note').textContent = partial ? '仅显示开头 64 KiB。' : 'UTF-8 文本';
    $('text').textContent = decoded;
    $('preview').scrollIntoView({block:'nearest'});
  } catch (e) {
    if (e.name !== 'AbortError' && ticket === previewRequest) $('preview-note').textContent = e.message;
  }
}
$('pair-form').addEventListener('submit', async event => {
  event.preventDefault();
  const submit = event.currentTarget.querySelector('button');
  submit.disabled = true;
  try {
    await request('/reader/pair', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({code:$('code').value.trim()})});
    $('code').value = '';
    navigate('units');
  } catch (e) { failure(e); }
  finally { submit.disabled = false; }
});
$('logout').addEventListener('click', async () => {
  try {
    await request('/reader/logout', {method:'POST'});
    showPair();
    status('此浏览器的配对已撤销。');
  } catch (e) { failure(e); }
});
$('history').addEventListener('click', () => navigate('history', state.unit));
$('refresh').addEventListener('click', () => renderPage(false));
$('more').addEventListener('click', () => renderPage(true));
$('close-preview').addEventListener('click', hidePreview);
renderPage(false);
