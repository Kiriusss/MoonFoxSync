'use strict';
const $ = (id) => document.getElementById(id);
const icons = {
  download: 'M12 3v12 M7 10l5 5 5-5 M4 16v4h16v-4',
  archive: 'M4 5h16v4H4z M6 9v11h12V9 M10 12h4',
  library: 'M3 5h5v14H3z M10 5h4v14h-4z M16 6l3-1 3 13-3 1z',
  clock: 'M12 8v5l3 2 M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0',
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
  project:
    'M7 5v12 M17 5v3a5 5 0 0 1-5 5H7 M9 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0 M19 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0 M9 19a2 2 0 1 1-4 0 2 2 0 0 1 4 0',
  drive: 'M5 5h14l3 10v3a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1v-3z M2 15h20 M17 17h1',
  file: 'M6 3h8l4 4v14H6z M14 3v5h4 M9 12h6 M9 15h6',
  link: 'M10 14l4-4 M8 15l-1 1a3 3 0 0 1-4-4l4-4a3 3 0 0 1 5 0 M16 9l1-1a3 3 0 0 1 4 4l-4 4a3 3 0 0 1-5 0',
  logout: 'M10 4H4v16h6 M10 12h11 M17 8l4 4-4 4',
  refresh: 'M20 8a8 8 0 0 0-14-2L3 9 m0-5v5h5 M4 16a8 8 0 0 0 14 2l3-3 m0 5v-5h-5',
  close: 'M5 5l14 14 M19 5L5 19',
};
function el(tag, cls, value) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (value !== undefined) n.textContent = String(value);
  return n;
}
function ico(name) {
  const holder = el('span', 'ico');
  holder.setAttribute('aria-hidden', 'true');
  const ns = 'http://www.w3.org/2000/svg',
    svg = document.createElementNS(ns, 'svg'),
    path = document.createElementNS(ns, 'path');
  svg.setAttribute('viewBox', '0 0 24 24');
  path.setAttribute('d', icons[name]);
  svg.append(path);
  holder.append(svg);
  return holder;
}
document.querySelectorAll('[data-icon]').forEach((i) => i.replaceWith(ico(i.dataset.icon)));
const app = {
  paired: false,
  units: null,
  filter: 'all',
  query: '',
  controller: null,
  modalId: 0,
  locked: false,
};
const modal = $('modal'),
  body = $('modal-body'),
  actions = $('modal-actions');
const friendly = {
  400: '请求无效，请核对输入。',
  401: '配对码无效或设备授权已过期。',
  403: '此设备没有该操作的权限。',
  404: '所选资源已移除或不在授权范围内。',
  409: '版本已发生变化，请刷新后重新核对。',
  410: '目录清单已经变更，请刷新。',
  412: '文件已变化，请重新打开。',
  413: '提交内容超出限制。',
  429: '请求较多，请稍后重试。',
  503: 'NAS 数据卷暂时不可用。',
};
class APIError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}
function toast(message) {
  $('toast').textContent = message;
  $('toast').hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => ($('toast').hidden = true), 4600);
}
function btn(text, fn, cls = 'button') {
  const b = el('button', cls, text);
  b.type = 'button';
  b.addEventListener('click', async () => {
    if (b.disabled) return;
    b.disabled = true;
    try {
      await fn();
    } catch (err) {
      if (err.name !== 'AbortError') toast(err.message);
    } finally {
      b.disabled = false;
    }
  });
  return b;
}
function tag(text, strong = false) {
  return el('span', strong ? 'tag accent' : 'tag', text);
}
function block(parent, ...nodes) {
  parent.replaceChildren(...nodes.filter(Boolean));
}
function actionsBar(...nodes) {
  const a = el('div', 'actions');
  a.append(...nodes.filter(Boolean));
  return a;
}
function formField(text, input) {
  const box = el('div'),
    label = el('label', '', text);
  input.id = 'field-' + ++formField.counter;
  label.htmlFor = input.id;
  box.append(label, input);
  return box;
}
formField.counter = 0;
function getOpt(text, value) {
  const option = el('option', '', text);
  option.value = value;
  return option;
}
function route(view = 'libraries', unit = '', snapshot = '', path = '') {
  const params = new URLSearchParams();
  if (unit) params.set('unit', unit);
  if (snapshot) params.set('snapshot', snapshot);
  if (path) params.set('path', path);
  return '#' + view + (params.size ? '?' + params : '');
}
function go(...args) {
  const hash = route(...args);
  if (location.hash === hash) render();
  else location.hash = hash;
}
function getRoute() {
  const [view, q] = location.hash.slice(1).split('?');
  const p = new URLSearchParams(q);
  return {
    view: view || 'libraries',
    unit: p.get('unit') || '',
    snap: p.get('snapshot') || '',
    path: p.get('path') || '',
  };
}
function unitURL(u, part, values = {}) {
  const q = new URLSearchParams();
  Object.entries(values).forEach(([k, v]) => {
    if (v !== null && v !== undefined && v !== '') q.set(k, String(v));
  });
  return '/v1/units/' + encodeURIComponent(u.unitId) + '/' + part + (q.size ? '?' + q : '');
}
function showPair(msg = '') {
  app.paired = false;
  app.units = null;
  app.query = '';
  app.filter = 'all';
  app.controller?.abort();
  closeModal(true);
  $('workspace').hidden = true;
  $('pair-screen').hidden = false;
  block($('view'));
  $('pair-status').textContent = msg;
}
async function request(url, opts = {}) {
  const res = await fetch(url, {
    mode: 'same-origin',
    credentials: 'same-origin',
    cache: 'no-store',
    redirect: 'error',
    ...opts,
  });
  if (!res.ok) {
    if (
      opts.headers?.Range &&
      res.status === 416 &&
      res.headers.get('Content-Range') === 'bytes */0'
    )
      return null;
    let d = {};
    try {
      d = await res.json();
    } catch {}
    let message = friendly[res.status] || '请求失败（HTTP ' + res.status + '）。';
    if (d.message && /[\u3400-\u9fff]/.test(d.message)) message = d.message;
    if (res.status === 401 && app.paired) showPair(message);
    throw new APIError(res.status, message);
  }
  return res;
}
async function json(url, method = 'GET', payload, signal) {
  const opt = { method, signal };
  if (payload !== undefined) {
    opt.headers = { 'Content-Type': 'application/json' };
    opt.body = JSON.stringify(payload);
  }
  return (await request(url, opt)).json();
}
async function unitsList(signal) {
  const units = [];
  let cursor = '';
  const seen = new Set();
  do {
    const q = new URLSearchParams({ limit: '256' });
    if (cursor) q.set('cursor', cursor);
    const page = await json('/v1/units?' + q, 'GET', undefined, signal);
    units.push(...page.items);
    cursor = page.nextCursor;
    if (cursor && seen.has(cursor)) throw new Error('资料库分页未能继续，请刷新。');
    seen.add(cursor);
  } while (cursor);
  return units;
}
const date = (value) => {
  if (!value) return '';
  const d = new Date(value);
  return Number.isNaN(d.getTime())
    ? value
    : d.toLocaleString('zh-CN', {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      });
};
const short = (id) => (id ? id.slice(-8) : '');
const label = (s) => s.label || '自动快照';
const kind = (u) => (u.kind === 'external' ? '外部目录' : u.scopeId ? '共享项目' : '托管资料库');
const glyph = (u) => (u.kind === 'external' ? 'drive' : u.scopeId ? 'project' : 'library');
const write = (u) => u.kind === 'managed' && u.role === 'sync';
const size = (n) => {
  if (!Number.isFinite(n)) return '';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KiB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MiB';
  return (n / 1073741824).toFixed(1) + ' GiB';
};
function crumb(title, u) {
  const link = el('a', '', '梦枢天罗');
  link.href = route();
  const nodes = [link, el('span', '', '/'), el('span', '', title)];
  if (u) nodes.push(el('span', '', '/'), el('span', '', u.name));
  block($('crumbs'), ...nodes);
}
function heading(title, sub = '', right = null) {
  const row = el('div', 'view-head'),
    left = el('div');
  left.append(el('h1', '', title));
  if (sub) left.append(el('p', '', sub));
  row.append(left);
  if (right) row.append(right);
  return row;
}
function panel(title, tools = null) {
  const section = el('section', 'panel'),
    head = el('div', 'panel-head'),
    list = el('div');
  head.append(el('h2', '', title));
  if (tools) head.append(tools);
  section.append(head, list);
  return { section, list, head };
}
function item(iconName, title, description, open, tools) {
  const row = el('div', 'item'),
    mark = el('div', 'row-icon');
  mark.append(ico(iconName));
  const detail = el('div', 'item-main');
  detail.append(open ? btn(title, open, 'name link') : el('span', 'name', title));
  if (description) detail.append(el('div', 'detail', description));
  row.append(mark, detail);
  if (tools) row.append(tools);
  return row;
}
function catalog() {
  crumb('资料库');
  const tabs = el('div', 'tabs'),
    search = el('input', 'search');
  search.type = 'search';
  search.placeholder = '搜索资料库和项目';
  search.setAttribute('aria-label', search.placeholder);
  search.value = app.query;
  const collection = panel('项目列表', search);
  collection.head.after(tabs);
  tabs.classList.add('filter-tabs');
  const tabsByType = new Map();
  for (const [type, title] of [
    ['all', '全部'],
    ['library', '资料库'],
    ['project', '项目'],
    ['drive', '外部目录'],
  ]) {
    const b = btn(
      title,
      () => {
        app.filter = type;
        filter();
      },
      '',
    );
    const count = type === 'all' ? app.units.length : app.units.filter((u) => glyph(u) === type).length;
    b.append(el('span', 'filter-count', count));
    tabsByType.set(type, b);
    tabs.append(b);
  }
  function filter() {
    for (const [type, b] of tabsByType) b.setAttribute('aria-pressed', String(type === app.filter));
    const matched = app.units.filter(
      (u) =>
        (app.filter === 'all' || glyph(u) === app.filter) &&
        u.name.toLocaleLowerCase().includes(app.query.toLocaleLowerCase()),
    );
    const nodes = matched.map((u) => {
      const scope = app.units.find((g) => g.unitId === u.scopeId);
      const row = item(glyph(u), u.name, scope?.name || kind(u), null,
        tag(write(u) ? '可同步' : '只读'));
      row.querySelector('.name').replaceWith(repoLink(u.name, route('files', u.unitId)));
      return row;
    });
    if (!nodes.length)
      nodes.push(
        el(
          'p',
          'empty',
          app.units.length ? '没有符合条件的空间。' : '此设备当前没有获授权的资料库或项目。',
        ),
      );
    block(collection.list, ...nodes);
  }
  search.addEventListener('input', () => {
    app.query = search.value.trim();
    filter();
  });
  filter();
  block(
    $('view'),
    heading('项目与资料库'),
    collection.section,
  );
}
function unitPicker(units, selected, fn) {
  const s = el('select');
  s.setAttribute('aria-label', '选择资料库或项目');
  for (const u of units)
    s.append(getOpt(u.name + ' · ' + kind(u) + ' · ' + short(u.unitId), u.unitId));
  s.value = selected;
  s.addEventListener('change', () => fn(s.value));
  return s;
}
function unitNav(u, view, snap) {
  const tabs = el('div', 'tabs');
  const picks = [['files', '文件']];
  if (u.kind === 'managed') picks.push(['timeline', '版本时间线']);
  for (const [to, title] of picks) {
    const b = btn(title, () => go(to, u.unitId, to === 'files' ? snap : ''), '');
    b.setAttribute('aria-pressed', String(to === view));
    tabs.append(b);
  }
  return tabs;
}
function errorView(error, refresh) {
  const n = el('div', 'empty');
  n.append(el('p', '', error.message), btn('重新读取', refresh));
  return n;
}
async function render(force = false) {
  if (!app.paired) return;
  app.controller?.abort();
  const ctrl = new AbortController();
  app.controller = ctrl;
  const r = getRoute(),
    isTimeline = r.view === 'timeline';
  $('nav-libraries').setAttribute('aria-current', isTimeline ? 'false' : 'page');
  $('nav-timeline').setAttribute('aria-current', isTimeline ? 'page' : 'false');
  block($('view'), el('p', 'empty', '正在读取…'));
  let u;
  try {
    if (force || !app.units) {
      const fetched = await unitsList(ctrl.signal);
      if (ctrl.signal.aborted) return;
      app.units = fetched;
    }
    if (r.view === 'libraries' || !['files', 'timeline'].includes(r.view)) {
      catalog();
      return;
    }
    u = r.unit
      ? app.units.find((unit) => unit.unitId === r.unit)
      : app.units.find((unit) => unit.kind === 'managed');
    if (!u) {
      crumb(isTimeline ? '版本时间线' : '资料库');
      block(
        $('view'),
        heading(isTimeline ? '版本时间线' : '资料库'),
        el(
          'p',
          'empty',
          r.unit ? '此设备没有该资料库的访问权限。' : '暂无可查看时间线的托管资料库。',
        ),
      );
      return;
    }
    if (isTimeline && u.kind !== 'managed') {
      go('files', u.unitId);
      return;
    }
    if (isTimeline) await timeline(u, ctrl.signal);
    else await files(u, r.snap || u.head || '', r.path, ctrl.signal);
  } catch (e) {
    if (!ctrl.signal.aborted && app.paired) {
      crumb(isTimeline ? '版本时间线' : '资料库', u);
      block(
        $('view'),
        errorView(e, () => render(true)),
      );
    }
  }
}
function pageLoader(u, resource, query, list, signal, paint, emptyText) {
  let cursor = '',
    loading = false,
    footer = null;
  async function load() {
    if (loading || signal.aborted) return;
    loading = true;
    try {
      const response = await json(
        unitURL(u, resource, { ...query, cursor, limit: 70 }),
        'GET',
        undefined,
        signal,
      );
      if (signal.aborted) return;
      footer?.remove();
      for (const entry of response.items) list.append(paint(entry));
      if (!list.childElementCount) list.append(el('p', 'empty', emptyText));
      const next = response.nextCursor;
      if (next && next === cursor) throw new Error('分页未能继续，请刷新。');
      cursor = next;
      if (cursor) {
        footer = el('div', 'panel-foot');
        footer.append(btn('加载更多', load));
        list.append(footer);
      }
    } finally {
      loading = false;
    }
  }
  return load;
}
function repoLink(text, href) {
  const a = el('a', 'repo-link', text);
  a.href = href;
  return a;
}
function fileLink(u, snap, file) {
  const a = el('a', 'button file-download', '下载');
  a.download = file.path.split('/').pop();
  a.setAttribute('aria-label', '下载 ' + a.download);
  a.href = unitURL(u, 'content', { path: file.path, snapshotId: snap || null });
  return a;
}
function archiveLink(u, snap) {
  const a = el('a', 'button primary archive-download');
  a.href = unitURL(u, 'archive', { snapshotId: snap });
  a.download = '';
  a.title = '下载此版本的完整项目';
  a.append(ico('download'), document.createTextNode('下载 ZIP'));
  return a;
}
function repoFileRow(u, snap, file) {
  const row = el('div', 'repo-file-row');
  const name = file.path.split('/').pop();
  const directory = file.kind === 'directory';
  const link = file.kind === 'symlink';
  const title = el('div', 'repo-file-title');
  title.append(ico(directory ? 'folder' : link ? 'link' : 'file'));
  if (directory) title.append(repoLink(name, route('files', u.unitId, snap, file.path)));
  else if (link) title.append(el('span', '', name), el('small', 'detail', file.target || '符号链接'));
  else title.append(btn(name, () => previewFile(u, snap, file), 'repo-link'));
  row.append(title, el('span', 'file-size', size(file.size) || '—'),
    el('span', 'file-date', date(file.mtime) || '—'));
  if (!directory && !link) row.append(fileLink(u, snap, file));
  return row;
}
async function files(u, snap, path, signal) {
  crumb('项目与资料库', u);
  const scope = app.units.find((i) => i.unitId === u.scopeId);
  const head = heading(u.name, [scope?.name, kind(u), write(u) ? '可同步' : '只读'].filter(Boolean).join(' · '));
  head.classList.add('repo-heading');
  const tabs = unitNav(u, 'files', snap);
  tabs.classList.add('repo-tabs');
  const tabsBar = el('div', 'repo-tabs-bar');
  tabsBar.append(tabs);
  const nav = el('nav', 'repo-path');
  nav.setAttribute('aria-label', '项目文件路径');
  nav.append(repoLink(u.name, route('files', u.unitId, snap)));
  const parts = path ? path.split('/') : [];
  parts.forEach((part, i) => nav.append(el('span', 'slash', '/'),
    repoLink(part, route('files', u.unitId, snap, parts.slice(0, i + 1).join('/')))));
  nav.lastElementChild.setAttribute('aria-current', 'page');
  const dir = panel('文件');
  dir.section.classList.add('repo-files');
  const marker = el('span', 'version-chip');
  if (u.kind === 'managed' && snap) {
    marker.append(ico('project'), document.createTextNode(snap === u.head ? '当前主线' : '历史快照'),
      el('code', '', short(snap)));
  } else marker.textContent = u.kind === 'external' ? 'NAS 当前目录' : '尚无已发布版本';
  const bar = el('div', 'repo-toolbar');
  bar.append(marker);
  if (u.kind === 'managed' && snap) bar.append(archiveLink(u, snap));
  dir.head.replaceChildren(nav);
  block($('view'), head, tabsBar, bar);
  const children = app.units.filter((i) => i.scopeId === u.unitId);
  if (children.length && !path) {
    const projects = panel('子项目');
    for (const child of children) {
      const a = repoLink(child.name, route('files', child.unitId));
      const entry = el('div', 'item');
      entry.append(ico('project'), a);
      projects.list.append(entry);
    }
    $('view').append(projects.section);
  }
  $('view').append(dir.section);
  if (u.kind === 'managed' && !snap) {
    dir.list.append(el('p', 'empty', '此资料库尚无已发布的文件。'));
    return;
  }
  const columns = el('div', 'repo-file-row repo-columns');
  columns.setAttribute('aria-hidden', 'true');
  for (const title of ['名称', '大小', '修改时间', '']) columns.append(el('span', '', title));
  dir.head.after(columns);
  if (path) {
    const up = el('div', 'repo-up');
    up.append(repoLink('↑ 上一级', route('files', u.unitId, snap, parts.slice(0, -1).join('/'))));
    columns.after(up);
  }
  await pageLoader(u, 'entries', { path, snapshotId: snap, order: u.kind === 'managed' ? 'folders' : null }, dir.list, signal,
    (file) => repoFileRow(u, snap, file), '此目录为空。')();
}
async function timeline(u, signal) {
  crumb('版本时间线', u);
  const tools = el('div', 'bar'),
    units = app.units.filter((v) => v.kind === 'managed');
  tools.append(
    unitPicker(units, u.unitId, (id) => go('timeline', id)),
    btn('历史保存策略', () => retention(u)),
  );
  const history = panel('最近时间点');
  const view = heading(
    '版本时间线',
    u.name + ' · ' + (write(u) ? '可同步' : '只读'),
    unitNav(u, 'timeline', u.head),
  );
  block($('view'), view, tools, history.section);
  await pageLoader(
    u,
    'snapshots',
    {},
    history.list,
    signal,
    (s) => {
      const isHead = s.snapshotId === u.head,
        fork = s.outcome === 'fork';
      const open = () => go('files', u.unitId, s.snapshotId);
      const row = item(
        fork ? 'project' : 'clock',
        label(s),
        [date(s.committedAt || s.capturedAt), short(s.snapshotId)].filter(Boolean).join(' · '),
        open,
        tag(isHead ? '当前主线' : fork ? '分叉' : '历史版本', isHead),
      );
      const ops = actionsBar(
        btn('浏览文件', open),
        archiveLink(u, s.snapshotId),
        btn('版本详情', () => snapshotDetail(u, s)),
      );
      ops.classList.add('version-actions');
      row.querySelector('.item-main').append(ops);
      return row;
    },
    '此资料库尚无已发布的时间点。',
  )();
}
function closeModal(force = false) {
  if (app.locked && !force) return;
  app.modalId++;
  app.modalController?.abort();
  app.locked = false;
  if (modal.open) modal.close();
  body.replaceChildren();
  actions.replaceChildren();
  $('modal-status').textContent = '';
}
function dialog(title) {
  app.modalController?.abort();
  app.modalController = new AbortController();
  const id = ++app.modalId;
  app.locked = false;
  $('modal-close').disabled = false;
  $('modal-title').textContent = title;
  $('modal-status').textContent = '';
  body.replaceChildren();
  actions.replaceChildren();
  if (!modal.open) modal.showModal();
  return { id, signal: app.modalController.signal, live: () => id === app.modalId && modal.open };
}
function dialogError(ctx, err, reload) {
  if (!ctx.live() || err.name === 'AbortError') return;
  if (err.status === 409 || err.status === 410 || err.status === 404) {
    $('modal-status').textContent = err.message + ' 请重新读取并核对时间线。';
    block(
      actions,
      btn('关闭', () => closeModal()),
      btn(
        '刷新时间线',
        async () => {
          closeModal();
          await render(true);
        },
        'button primary',
      ),
    );
  } else {
    $('modal-status').textContent = err.message || '请求失败。';
    if (reload) actions.append(btn('重试', reload));
  }
}
function formButton(ctx, title, fn, cls = 'button primary', lock = false) {
  return btn(
    title,
    async () => {
      if (!ctx.live() || app.locked) return;
      if (lock) {
        app.locked = true;
        $('modal-close').disabled = true;
      }
      try {
        await fn();
      } catch (e) {
        dialogError(ctx, e);
      } finally {
        if (ctx.live()) {
          app.locked = false;
          $('modal-close').disabled = false;
        }
      }
    },
    cls,
  );
}
function detailRow(title, value, mono = false) {
  const box = el('div', 'member');
  box.append(el('h3', '', title), el('div', mono ? 'detail mono' : 'detail', value));
  return box;
}
function versionInfo(u, s) {
  return [
    detailRow('资料库', u.name),
    detailRow('所选时间点', label(s) + ' · ' + s.snapshotId),
    detailRow('当前主线', u.head || '尚无版本', true),
  ];
}
function snapshotDetail(u, s) {
  const ctx = dialog('版本详情');
  const parents =
    s.timelineParentSnapshotIds || [s.parentSnapshotId, s.mergeParentSnapshotId].filter(Boolean);
  body.append(
    ...versionInfo(u, s),
    detailRow(
      s.timelineRebuilt ? '重建后的父版本' : '父版本',
      parents.join('、') || '无父版本',
      true,
    ),
  );
  if (s.restoredFromSnapshotId) body.append(detailRow('恢复来源', s.restoredFromSnapshotId, true));
  if (write(u)) {
    if (s.outcome === 'fork')
      actions.append(formButton(ctx, '手动合并分叉', () => mergePlan(u, s), 'button'));
    if (s.snapshotId !== u.head && u.head)
      actions.append(formButton(ctx, '恢复为当前版本', () => restore(u, s), 'button'));
    if (u.head)
      actions.append(formButton(ctx, '删除并重建', () => deletePreview(u, s), 'button danger'));
  }
  actions.append(
    formButton(ctx, '浏览文件', () => {
      closeModal();
      go('files', u.unitId, s.snapshotId);
    }),
  );
}
async function previewFile(u, snap, file) {
  const ctx = dialog(file.path.split('/').pop()),
    a = fileLink(u, snap, file);
  actions.append(a);
  body.append(el('p', 'muted', '正在读取预览…'));
  try {
    const image = /\.(png|jpe?g|webp)$/i.test(file.path);
    const maxImage = 8 * 1024 * 1024;
    if (image && file.size > maxImage) {
      block(body, el('p', 'muted', '图片超过 8 MiB，可下载原图查看。'));
      return;
    }
    if (image && file.size > 0 && typeof createImageBitmap === 'function') {
      const headers = { Range: 'bytes=0-' + (file.size - 1) };
      if (file.digest) headers['If-Match'] = '"' + file.digest + '"';
      const res = await request(a.href, { headers, signal: ctx.signal });
      if (!ctx.live() || !res) return;
      if (Number(res.headers.get('Content-Length')) !== file.size) {
        await res.body?.cancel();
        throw new Error('文件大小已变化，请重新打开。');
      }
      const bitmap = await createImageBitmap(await res.blob());
      try {
        if (!ctx.live()) return;
        const canvas = el('canvas', 'image-preview');
        const scale = Math.min(1, 1440 / Math.max(bitmap.width, bitmap.height));
        canvas.width = Math.max(1, Math.round(bitmap.width * scale));
        canvas.height = Math.max(1, Math.round(bitmap.height * scale));
        canvas.setAttribute('role', 'img');
        canvas.setAttribute('aria-label', file.path);
        canvas.getContext('2d').drawImage(bitmap, 0, 0, canvas.width, canvas.height);
        block(body, el('p', 'detail', bitmap.width + ' × ' + bitmap.height + ' · ' + size(file.size)), canvas);
      } finally {
        bitmap.close();
      }
      return;
    }
    const headers = { Range: 'bytes=0-65535' };
    if (file.digest) headers['If-Match'] = '"' + file.digest + '"';
    const res = await request(a.href, { headers, signal: ctx.signal });
    if (!ctx.live()) return;
    if (!res) {
      block(body, el('p', 'muted', '空文件'));
      return;
    }
    const header = res.headers.get('Content-Length'),
      n = header === null ? NaN : Number(header);
    if (!Number.isSafeInteger(n) || n < 0 || n > 65536) {
      await res.body?.cancel();
      throw new Error('预览超过 64 KiB，请下载文件。');
    }
    const bytes = new Uint8Array(await res.arrayBuffer());
    if (!ctx.live()) return;
    const range = res.headers.get('Content-Range'),
      partial = range ? Number(range.split('/').pop()) > bytes.length : false;
    let text;
    try {
      text = new TextDecoder('utf-8', { fatal: true }).decode(bytes, { stream: partial });
    } catch {
      text = null;
    }
    if (text === null || /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(text)) {
      block(body, el('p', 'muted', '此文件无法作为 UTF-8 文本预览，可下载后打开。'));
      return;
    }
    block(
      body,
      el(
        'p',
        'detail',
        partial ? 'UTF-8 文本 · 预览前 64 KiB' : 'UTF-8 文本 · ' + size(bytes.length),
      ),
      el('pre', 'text-preview', text || '空文件'),
    );
  } catch (e) {
    if (ctx.live()) {
      body.replaceChildren();
      dialogError(ctx, e, () => previewFile(u, snap, file));
    }
  }
}
function newId() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let t = Date.now();
  for (let i = 5; i >= 0; i--) {
    bytes[i] = t % 256;
    t = Math.floor(t / 256);
  }
  const alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
  let bits = 2,
    buffer = 0,
    out = '';
  for (const b of bytes) {
    buffer = (buffer << 8) | b;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += alphabet[(buffer >>> bits) & 31];
    }
    buffer &= (1 << bits) - 1;
  }
  return out;
}
async function afterChange(message) {
  closeModal(true);
  toast(message);
  await render(true);
}
function restore(u, s) {
  const ctx = dialog('恢复为当前版本'),
    name = el('input');
  name.value = ('恢复 · ' + label(s)).slice(0, 480);
  name.maxLength = 500;
  body.append(
    ...versionInfo(u, s),
    el(
      'p',
      'info',
      '服务器将用所选时间点的内容创建新的 NAS 主线版本。原主线仍按本资料库的历史策略保存。',
    ),
    formField('新版本名称', name),
  );
  let frozen = null;
  actions.append(
    formButton(ctx, '返回', () => snapshotDetail(u, s), 'button'),
    formButton(
      ctx,
      '确认恢复为当前版本',
      async () => {
        const label = name.value.trim();
        if (!label) throw new Error('请填写新版本名称。');
        if (!frozen || frozen.label !== label)
          frozen = { snapshotId: s.snapshotId, expectedHead: u.head, requestId: newId(), label };
        const result = await json(unitURL(u, 'timeline/restore'), 'POST', frozen);
        if (!result.snapshotId || result.unitId !== u.unitId || result.outcome !== 'published')
          throw new Error('未收到正确的恢复回执，请重新核对。');
        await afterChange('已创建新的主线版本 · ' + short(result.snapshotId));
      },
      'button primary',
      true,
    ),
  );
}
async function deletePreview(u, s) {
  const ctx = dialog('删除并重建');
  body.append(el('p', 'muted', '正在计算受影响的时间点…'));
  try {
    const payload = { snapshotId: s.snapshotId, expectedHead: u.head };
    const p = await json(unitURL(u, 'timeline/delete/preview'), 'POST', payload, ctx.signal);
    if (!ctx.live()) return;
    if (
      p.snapshotId !== s.snapshotId ||
      p.expectedHead !== u.head ||
      !Array.isArray(p.rebuiltPoints)
    )
      throw new Error('删除预览与选择不符，请刷新后重新查看。');
    block(body, ...versionInfo(u, s));
    if (p.protectedReason) {
      body.append(el('p', 'info', '此时间点受保护：' + p.protectedReason));
      actions.append(formButton(ctx, '关闭', () => closeModal(), 'button'));
      return;
    }
    if (!p.planToken) throw new Error('预览缺少计划凭证。');
    body.append(
      el(
        'p',
        'info',
        '删除会隐藏所选时间点，重建其余时间点的父版本关系。保留下来的文件内容不变，物理空间不会立即释放。',
      ),
      el('h3', '', '需重建的时间点 · ' + p.rebuiltPoints.length),
    );
    if (!p.rebuiltPoints.length) body.append(el('p', 'muted', '其余时间点无需重建。'));
    for (const x of p.rebuiltPoints) {
      const section = el('div', 'member');
      section.append(
        el('h3', '', label(x)),
        el('div', 'identity mono', x.snapshotId),
        el('p', 'detail', '原父版本 · ' + (x.parentsBefore.join('、') || '无父版本')),
        el('p', 'detail', '重建后 · ' + (x.parentsAfter.join('、') || '无父版本')),
      );
      body.append(section);
    }
    actions.append(
      formButton(ctx, '取消', () => closeModal(), 'button'),
      formButton(
        ctx,
        '确认删除并重建',
        async () => {
          const ack = await json(unitURL(u, 'timeline/delete'), 'POST', {
            ...payload,
            planToken: p.planToken,
          });
          if (!ack.deleted || ack.snapshotId !== s.snapshotId)
            throw new Error('删除回执与所选时间点不符。');
          await afterChange(
            '时间点已删除，已重建 ' +
              (ack.rebuiltPoints?.length ?? p.rebuiltPoints.length) +
              ' 个时间点。',
          );
        },
        'button danger',
        true,
      ),
    );
  } catch (e) {
    dialogError(ctx, e, () => deletePreview(u, s));
  }
}
function policyText(p) {
  return p.mode === 'all'
    ? '保留全部时间点'
    : p.mode === 'latest'
      ? '保留最近 ' + p.value + ' 个时间点'
      : p.mode === 'days'
        ? '保留最近 ' + p.value + ' 天'
        : '未知策略';
}
async function retention(u) {
  const ctx = dialog('历史保存策略');
  body.append(el('p', 'muted', '正在读取保存策略…'));
  try {
    const old = await json(unitURL(u, 'history-policy'), 'GET', undefined, ctx.signal);
    if (!ctx.live()) return;
    block(
      body,
      detailRow('资料库', u.name),
      detailRow('当前策略', policyText(old)),
      el(
        'p',
        'info',
        '当前主线、未合并分叉和被副本持有的时间点受保护。超出保留范围的时间点将从历史隐藏，物理空间不会立即释放。',
      ),
    );
    if (!write(u)) {
      body.append(el('p', 'muted', '本设备有读取权限，可查看当前策略。'));
      return;
    }
    const mode = el('select');
    mode.append(
      getOpt('保留全部', 'all'),
      getOpt('按最近时间点数', 'latest'),
      getOpt('按天数', 'days'),
    );
    mode.value = old.mode;
    const value = el('input');
    value.type = 'number';
    value.min = '1';
    value.step = '1';
    value.value = String(old.value || 7);
    const field = formField('数值', value);
    const update = () => {
      field.hidden = mode.value === 'all';
      value.max = mode.value === 'days' ? '3650' : '10000';
      field.querySelector('label').textContent =
        mode.value === 'days' ? '保留天数（1–3650）' : '保留时间点数（1–10000）';
    };
    mode.onchange = update;
    update();
    body.append(formField('保存方式', mode), field);
    actions.append(
      formButton(ctx, '预览影响', async () => {
        const v = mode.value === 'all' ? 0 : value.valueAsNumber;
        if (mode.value !== 'all' && (!Number.isInteger(v) || v < 1 || v > Number(value.max)))
          throw new Error('请输入有效范围内的整数。');
        const choice = { mode: mode.value, value: v };
        if (choice.mode === old.mode && choice.value === old.value)
          throw new Error('当前已经使用此策略。');
        const preview = await json(
          unitURL(u, 'history-policy/preview'),
          'POST',
          choice,
          ctx.signal,
        );
        if (!ctx.live()) return;
        if (
          preview.mode !== choice.mode ||
          preview.value !== v ||
          !Number.isSafeInteger(preview.removeCount) ||
          !Number.isSafeInteger(preview.protectedCount)
        )
          throw new Error('服务器预览与选择不符。');
        const confirm = dialog('确认应用历史保存策略');
        body.append(
          detailRow('资料库', u.name),
          detailRow('原策略', policyText(old)),
          detailRow('新策略', policyText(choice)),
          el(
            'p',
            'info',
            '预计隐藏 ' +
              preview.removeCount +
              ' 个时间点，额外保护 ' +
              preview.protectedCount +
              ' 个。延长保留期限不会恢复已移除的时间点。',
          ),
        );
        actions.append(
          formButton(confirm, '返回', () => retention(u), 'button'),
          formButton(
            confirm,
            '确认应用策略',
            async () => {
              const data = { ...choice, expectedHead: u.head || '' };
              const result = await json(unitURL(u, 'history-policy'), 'PUT', data);
              if (result.mode !== choice.mode || result.value !== v)
                throw new Error('保存回执与预览不符。');
              await afterChange('历史保存策略已更新。');
            },
            'button primary',
            true,
          ),
        );
      }),
    );
  } catch (e) {
    dialogError(ctx, e, () => retention(u));
  }
}
async function mergePlan(u, s) {
  const ctx = dialog('手动合并分叉');
  body.append(el('p', 'muted', '正在比较主线、分叉和共同祖先…'));
  try {
    const plan = await json(
      unitURL(u, 'merge/plan'),
      'POST',
      { branchSnapshotId: s.snapshotId },
      ctx.signal,
    );
    if (!ctx.live()) return;
    if (plan.unitId !== u.unitId || plan.branchSnapshotId !== s.snapshotId || !plan.mainHead)
      throw new Error('合并计划与所选版本不符。');
    block(
      body,
      detailRow('当前主线', plan.mainHead, true),
      detailRow('分叉时间点', s.snapshotId, true),
      detailRow(
        '共同祖先',
        plan.baseHidden ? '原时间点已移除' : plan.baseSnapshotId || '空基线',
        true,
      ),
    );
    if (plan.alreadyMerged) {
      body.append(el('p', 'info', '所选分叉已经合并。'));
      return;
    }
    if (plan.baseAmbiguous) body.append(el('p', 'info', '存在多个共同祖先，请逐项核对全部差异。'));
    const automatic = el('details');
    automatic.append(
      el('summary', '', '自动合并 · ' + plan.autoPaths.length),
      el('pre', '', plan.autoPaths.join('\n') || '无自动变更。'),
    );
    body.append(automatic, el('h3', '', '需处理 · ' + plan.conflicts.length));
    const selected = new Map(),
      kinds = {
        file: '文件',
        symlink: '符号链接',
        directory: '目录',
        missing: '不存在',
        hidden: '原时间点已移除',
        text: '文本',
        binary: '二进制文件',
      };
    for (const c of plan.conflicts) {
      const part = el('details'),
        title = el('summary', '', c.path),
        select = el('select');
      select.append(
        getOpt('请选择处理方式', ''),
        getOpt('保留主线', 'main'),
        getOpt('采用分叉', 'branch'),
      );
      if (c.bothPath) select.append(getOpt('同时保留', 'both'));
      if (c.textAllowed) select.append(getOpt('手动编辑文本', 'text'));
      const editor = el('textarea');
      const textField = formField('合并后的 UTF-8 文本', editor);
      textField.hidden = true;
      const emptyLabel = el('label', 'empty-choice'),
        emptyAllowed = el('input');
      emptyAllowed.type = 'checkbox';
      emptyLabel.append(emptyAllowed, document.createTextNode('以空文件内容替换此路径'));
      textField.append(emptyLabel);
      const comparison = el('div'),
        info = el(
          'p',
          'detail',
          [
            ['主线', c.mainKind],
            ['分叉', c.branchKind],
            ['祖先', c.baseKind],
          ]
            .map(([t, k]) => t + ' · ' + (kinds[k] || k))
            .join('　'),
        );
      const state = { choice: '', text: '', empty: false };
      selected.set(c.path, state);
      emptyAllowed.addEventListener('change', () => (state.empty = emptyAllowed.checked));
      select.addEventListener('change', () => {
        state.choice = select.value;
        textField.hidden = select.value !== 'text';
        title.textContent = (state.choice ? '✓ ' : '') + c.path;
      });
      editor.addEventListener('input', () => {
        state.text = editor.value;
        emptyLabel.hidden = editor.value.length > 0;
      });
      const compare = btn('查看三方内容', async () => {
        comparison.textContent = '正在读取…';
        try {
          const sides = await Promise.all(
            [
              ['主线', plan.mainHead, c.mainKind],
              ['分叉', s.snapshotId, c.branchKind],
              ['祖先', plan.baseSnapshotId, c.baseKind],
            ].map(async ([name, id, type]) => {
              let v;
              if (name === '祖先' && plan.baseHidden) v = { kind: 'hidden' };
              else if (!id || type === 'missing') v = { kind: 'missing' };
              else
                v = await json(
                  unitURL(u, 'merge/preview', { snapshotId: id, path: c.path }),
                  'GET',
                  undefined,
                  ctx.signal,
                );
              return [name, v];
            }),
          );
          if (!ctx.live()) return;
          comparison.replaceChildren();
          for (const [name, v] of sides) {
            const d = el('details');
            d.append(
              el(
                'summary',
                '',
                name + ' · ' + (kinds[v.kind] || v.kind) + (v.truncated ? ' · 部分预览' : ''),
              ),
              el(
                'pre',
                '',
                v.kind === 'text'
                  ? v.text
                  : v.kind === 'symlink'
                    ? v.target
                    : kinds[v.kind] || v.kind,
              ),
            );
            comparison.append(d);
          }
        } catch (e) {
          if (ctx.live()) comparison.textContent = e.message;
        }
      });
      part.append(
        title,
        info,
        compare,
        comparison,
        formField('此冲突的处理方式', select),
        textField,
      );
      if (c.bothPath) part.append(el('p', 'detail', '同时保留时，分叉另存为 ' + c.bothPath));
      body.append(part);
    }
    const name = el('input');
    name.value = '合并分叉';
    name.maxLength = 500;
    body.append(formField('新版本名称', name));
    let requestKey, requestId;
    const next = formButton(
      ctx,
      '核对并发布…',
      async () => {
        const choices = [];
        for (const [path, value] of selected) {
          if (!value.choice) throw new Error('请先处理冲突：' + path);
          if (value.choice === 'text' && value.text === '' && !value.empty)
            throw new Error('请填写合并文本，或明确选择空文件：' + path);
          if (
            value.choice === 'text' &&
            (value.text.includes('\0') || new TextEncoder().encode(value.text).length > 131072)
          )
            throw new Error('合并文本须小于等于 128 KiB，且不含空字符。');
          const entry = { path, choice: value.choice };
          if (value.choice === 'text') entry.text = value.text;
          choices.push(entry);
        }
        const label = name.value.trim();
        if (!label) throw new Error('请输入新版本名称。');
        const frozen = { mainHead: plan.mainHead, branchSnapshotId: s.snapshotId, label, choices };
        const key = JSON.stringify(frozen);
        if (requestKey !== key) {
          requestKey = key;
          requestId = newId();
        }
        if (
          !window.confirm(
            '确认发布新的 NAS 主线版本「' +
              label +
              '」？\n当前主线：' +
              plan.mainHead +
              '\n来源分叉：' +
              s.snapshotId +
              '\n自动合并 ' +
              plan.autoPaths.length +
              ' 项，已处理冲突 ' +
              choices.length +
              ' 项。',
          )
        )
          return;
        const result = await json(unitURL(u, 'merge/commit'), 'POST', { ...frozen, requestId });
        if (result.unitId !== u.unitId || !result.snapshotId)
          throw new Error('合并回执与资料库不符，请核对后重试。');
        await afterChange('分叉已合并 · ' + short(result.snapshotId));
      },
      'button primary',
      true,
    );
    actions.append(next);
  } catch (e) {
    dialogError(ctx, e, () => mergePlan(u, s));
  }
}
$('modal-close').addEventListener('click', () => closeModal());
modal.addEventListener('cancel', (event) => {
  event.preventDefault();
  closeModal();
});
$('refresh').addEventListener('click', () => render(true));
window.addEventListener('hashchange', () => {
  if (!app.locked) closeModal();
  render();
});
$('pair-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const submit = $('pair-submit');
  if (submit.disabled) return;
  submit.disabled = true;
  $('pair-status').textContent = '正在配对…';
  try {
    await json('/console/pair', 'POST', {
      code: $('pair-code').value.trim(),
      deviceName: $('pair-name').value.trim(),
    });
    $('pair-code').value = '';
    await enter();
  } catch (e) {
    $('pair-status').textContent = e.message;
  } finally {
    submit.disabled = false;
  }
});
$('logout').addEventListener('click', async () => {
  if (!window.confirm('退出并解除此浏览器的配对？再次访问需要新的配对码。')) return;
  const b = $('logout');
  b.disabled = true;
  try {
    await json('/console/logout', 'POST', {});
    showPair('已退出此浏览器。');
    history.replaceState(null, '', '#libraries');
  } catch (e) {
    toast(e.message);
  } finally {
    b.disabled = false;
  }
});
async function enter() {
  app.paired = true;
  $('pair-screen').hidden = true;
  $('workspace').hidden = false;
  await render(true);
}
async function boot() {
  try {
    const state = await json('/console/session');
    if (state.paired) await enter();
    else showPair();
  } catch (e) {
    showPair(e.message);
    $('pair-status').append(' ', btn('重新检查', boot));
  }
}
boot();
