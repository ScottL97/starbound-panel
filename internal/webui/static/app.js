/* Starbound Panel single-page UI. No build step, no dependencies. */
(function () {
  'use strict';

  const MAX_LINES = 2000;
  const MINI_LINES = 200;

  const $ = (id) => document.getElementById(id);
  const esc = (v) =>
    String(v == null ? '' : v).replace(/[&<>"']/g, (c) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));

  // ---------------------------------------------------------------- i18n

  const I18N = window.SB_I18N || { en: {}, zh: {} };
  let lang = localStorage.getItem('sb.lang');
  if (lang !== 'en' && lang !== 'zh') {
    lang = (navigator.language || '').toLowerCase().startsWith('zh') ? 'zh' : 'en';
  }

  function t(key, vars) {
    const dict = I18N[lang] || {};
    const fallback = I18N.en || {};
    let out = dict[key] != null ? dict[key] : (fallback[key] != null ? fallback[key] : key);
    if (vars) {
      Object.keys(vars).forEach((k) => {
        out = out.split('{' + k + '}').join(vars[k]);
      });
    }
    return out;
  }

  function applyI18n() {
    document.querySelectorAll('[data-i18n]').forEach((el) => {
      const key = el.dataset.i18n;
      if (!key) return;
      const value = t(key);
      if (value !== '') el.textContent = value;
    });
    const sel = $('lang');
    if (sel) sel.value = lang;
  }

  // ---------------------------------------------------------------- state

  const state = {
    session: null,
    setup: false,
    status: null,
    players: [],
    lines: [],
    mods: [],
    modDetails: {},
    external: [],
    workshop: { items: [], total: 0, page: 1, query: '' },
    backups: [],
    settings: null,
    steam: null,
    engine: null,
    es: null,
    paused: false
  };

  // ---------------------------------------------------------------- utils

  function fmtTime(iso) {
    const d = new Date(iso);
    return isNaN(d.getTime()) ? '' : d.toLocaleTimeString();
  }

  function fmtDateTime(iso) {
    const d = new Date(iso);
    return isNaN(d.getTime()) ? '—' : d.toLocaleString();
  }

  function timeAgo(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '—';
    const sec = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000));
    if (sec < 60) return sec + 's';
    if (sec < 3600) return Math.round(sec / 60) + 'm';
    if (sec < 86400) return Math.round(sec / 3600) + 'h';
    return Math.round(sec / 86400) + 'd';
  }

  function fmtDuration(sec) {
    sec = Math.max(0, Math.round(sec || 0));
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d) return d + 'd ' + h + 'h';
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm';
    return sec + 's';
  }

  function fmtSize(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + ' B';
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + ' ' + units[i];
  }

  function fmtCount(n) {
    n = Number(n) || 0;
    if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
    return String(n);
  }

  let toastTimer = null;
  function toast(message, isError) {
    const el = $('toast');
    el.textContent = message;
    el.classList.toggle('error', !!isError);
    el.classList.remove('hidden');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.add('hidden'), 4000);
  }

  // ---------------------------------------------------------------- api

  async function api(path, options) {
    options = options || {};
    const opts = {
      method: options.method || 'GET',
      headers: Object.assign({ 'X-SB-Request': '1' }, options.headers || {})
    };
    if (options.body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(options.body);
    }

    const res = await fetch(path, opts);
    if (!res.ok) {
      let message = res.statusText || 'HTTP ' + res.status;
      try {
        const data = await res.json();
        if (data && data.error) message = data.error;
      } catch (_) { /* not JSON */ }
      if (res.status === 401 && path !== '/api/login' && path !== '/api/setup') {
        showLogin(false);
      }
      const err = new Error(message);
      err.status = res.status;
      throw err;
    }
    if (res.status === 204) return null;
    return res.json();
  }

  // ---------------------------------------------------------------- boot

  async function boot() {
    try {
      const session = await api('/api/session');
      state.session = session;
      $('versionLabel').textContent = 'v' + session.version;

      if (session.setupRequired) { showLogin(true); return; }
      if (!session.authenticated) { showLogin(false); return; }

      showApp();
      await refreshAll();
      connect();
    } catch (err) {
      showLogin(false);
    }
  }

  function showLogin(setup) {
    state.setup = !!setup;
    if (state.es) { state.es.close(); state.es = null; }
    $('app').classList.add('hidden');
    $('login').classList.remove('hidden');
    $('confirmWrap').classList.toggle('hidden', !setup);
    $('loginHint').textContent = t(setup ? 'login.setupHint' : 'login.hint');
    $('loginSubmit').textContent = t(setup ? 'login.setupSubmit' : 'login.submit');
    $('loginError').textContent = '';
    $('loginUser').value = setup ? 'admin' : ($('loginUser').value || 'admin');
    $('loginPass').value = '';
    $('loginPass2').value = '';
  }

  function showApp() {
    $('login').classList.add('hidden');
    $('app').classList.remove('hidden');
    applyI18n();
  }

  async function refreshAll() {
    const [status, mods, backups, settings, config, eng] = await Promise.all([
      api('/api/status'),
      api('/api/mods'),
      api('/api/backups'),
      api('/api/settings'),
      api('/api/config'),
      api('/api/engine')
    ]);

    applyStatus(status);

    await applyMods(mods);

    state.backups = backups.backups || [];
    renderBackups();

    state.settings = settings.settings || {};
    state.steam = settings.steam || {};
    renderSettings();

    state.engine = eng;
    renderEngine();

    renderConfig(config);
  }

  // ---------------------------------------------------------------- status

  function applyStatus(payload) {
    if (!payload || !payload.server) return;
    state.status = payload;
    state.players = payload.players || [];

    const server = payload.server;
    const phase = server.installed ? server.phase : 'notInstalled';
    const pill = $('statusPill');
    pill.className = 'pill ' + (phase === 'notInstalled' ? 'stopped' : phase);
    pill.textContent = t('phase.' + phase);

    $('playerPill').textContent = state.players.length;
    renderPlayers(state.players);

    $('statStatus').textContent = t('phase.' + phase);
    if (server.running) {
      $('statUptime').textContent = t('dash.uptime', { v: fmtDuration(server.uptimeSec) });
    } else if (server.phase === 'crashed' && server.lastExit) {
      $('statUptime').textContent = t('dash.lastExit', { v: server.lastExit.code });
    } else {
      $('statUptime').textContent = '';
    }

    $('statPlayers').textContent = String(state.players.length);
    $('statPlayerNames').textContent = state.players.map((p) => p.name || p.id).join(', ');

    $('statInstall').textContent = server.installed ? t('dash.installed') : t('dash.notInstalled');
    $('statPort').textContent = 'TCP ' + (payload.panel ? payload.panel.gamePort : '');

    const panel = payload.panel || {};
    const address = (panel.publicAddress || '').trim() ||
      (window.location.hostname + ':' + (panel.gamePort || 21025));
    state.address = address;
    $('statAddress').textContent = address;
    $('statAddressNote').textContent = server.running ? '' : t('dash.notRunning');

    const backup = payload.backup || {};
    $('statBackup').textContent = backup.newest ? timeAgo(backup.newest) : t('dash.never');
    $('statBackupSub').textContent = backup.newest ? fmtDateTime(backup.newest) : '';
    $('backupRunning').textContent = backup.running ? t('backups.running') : '';
    $('btnBackup').disabled = !!backup.running;
    $('btnBackup2').disabled = !!backup.running;

    const busy = !!(payload.panel && payload.panel.installing);
    $('btnStart').disabled = server.running || !server.installed || busy;
    $('btnStop').disabled = !server.running;
    $('btnRestart').disabled = !server.running;
    $('btnInstall').disabled = server.running || busy;

    // An install or engine switch just finished: refresh the engine card.
    if (state.lastInstalling === true && !busy) setTimeout(loadEngine, 500);
    state.lastInstalling = busy;

    $('installHint').textContent = server.installed ? '' : t('dash.installHint');
  }

  function renderPlayers(players) {
    const rows = $('playerRows');
    rows.innerHTML = '';
    (players || []).forEach((pl) => {
      const tr = document.createElement('tr');
      tr.innerHTML =
        '<td>' + esc(pl.name || '—') + '</td>' +
        '<td class="mono">' + esc(pl.id) + '</td>' +
        '<td class="mono">' + esc(pl.host || '—') + '</td>' +
        '<td>' + esc(fmtDateTime(pl.since)) + '</td>';
      rows.appendChild(tr);
    });
    $('playersEmpty').classList.toggle('hidden', (players || []).length > 0);
  }

  // ---------------------------------------------------------------- log

  function lineNode(line) {
    const div = document.createElement('div');
    div.className = 'line';
    const time = document.createElement('span');
    time.className = 'time';
    time.textContent = fmtTime(line.time);
    const src = document.createElement('span');
    src.className = 'src ' + (line.source || 'server');
    src.textContent = line.source || 'server';
    const text = document.createElement('span');
    text.className = 'text';
    text.textContent = line.text;
    div.append(time, src, text);
    return div;
  }

  function appendLine(line) {
    state.lines.push(line);
    if (state.lines.length > MAX_LINES) {
      state.lines.splice(0, state.lines.length - MAX_LINES);
    }
    if (state.paused) return;

    const log = $('log');
    log.appendChild(lineNode(line));
    while (log.childElementCount > MAX_LINES) log.removeChild(log.firstChild);

    const mini = $('miniLog');
    mini.appendChild(lineNode(line));
    while (mini.childElementCount > MINI_LINES) mini.removeChild(mini.firstChild);

    scrollLogs();
  }

  function renderAllLines() {
    const log = $('log');
    const mini = $('miniLog');
    log.innerHTML = '';
    mini.innerHTML = '';
    state.lines.forEach((line) => log.appendChild(lineNode(line)));
    state.lines.slice(-MINI_LINES).forEach((line) => mini.appendChild(lineNode(line)));
    scrollLogs();
  }

  function scrollLogs() {
    if (!$('followLog').checked) return;
    const log = $('log');
    const mini = $('miniLog');
    log.scrollTop = log.scrollHeight;
    mini.scrollTop = mini.scrollHeight;
  }

  // ---------------------------------------------------------------- mods

  function modCard(item, actions) {
    const preview = item.preview && /^https:\/\//.test(item.preview) ? item.preview : '';
    const title = item.title || item.id;
    const workshopURL = item.id && /^\d+$/.test(item.id)
      ? 'https://steamcommunity.com/sharedfiles/filedetails/?id=' + item.id
      : '';
    const open = '<a href="' + esc(workshopURL) + '" target="_blank" rel="noopener noreferrer">';
    const close = '</a>';

    const stats = [];
    if (item.favorited) stats.push('★ ' + fmtCount(item.favorited));
    if (typeof item.subscriptions === 'number') stats.push(fmtCount(item.subscriptions) + ' ' + t('mods.subs'));
    if (item.size) stats.push(fmtSize(item.size));

    const cover = preview
      ? '<img class="mod-thumb" alt="" loading="lazy" src="' + esc(preview) + '">'
      : '<span class="mod-noimg">' + esc(t('mods.noImage')) + '</span>';

    const article = document.createElement('article');
    article.className = 'mod-card' + (item.enabled === false ? ' disabled' : '');
    article.innerHTML =
      '<div class="mod-cover">' + (workshopURL ? open + cover + close : cover) + '</div>' +
      '<div class="mod-body">' +
      '<div class="mod-title" title="' + esc(title) + '">' +
      (workshopURL ? open + esc(title) + close : esc(title)) +
      '</div>' +
      (item.creator ? '<div class="mod-author">' + esc(t('mods.author')) + ' ' + esc(item.creator) + '</div>' : '') +
      '<div class="mod-stats">' + esc(stats.join(' · ')) + '</div>' +
      (item.description ? '<div class="mod-desc">' + esc(item.description) + '</div>' : '') +
      '<div class="mod-actions">' + actions +
      (workshopURL ? '<a class="mod-link" href="' + esc(workshopURL) + '" target="_blank" rel="noopener noreferrer">' + esc(t('mods.steamPage')) + ' ↗</a>' : '') +
      '</div>' +
      '</div>';

    const img = article.querySelector('img');
    if (img) {
      img.addEventListener('error', () => {
        img.removeAttribute('src');
        img.parentElement.innerHTML = '<span class="mod-noimg">' + esc(t('mods.noImage')) + '</span>';
      });
    }
    return article;
  }

  function renderMods() {
    const grid = $('modCards');
    grid.innerHTML = '';

    state.mods.forEach((mod) => {
      const details = state.modDetails[mod.id] || { id: mod.id };
      const stateLabel = mod.enabled
        ? '<span class="pill running">' + esc(t('mods.enabled')) + '</span>'
        : '<span class="pill stopped">' + esc(t('mods.disabled')) + '</span>';
      const toggle = mod.enabled
        ? '<button data-act="disable" data-id="' + esc(mod.id) + '">' + esc(t('mods.disable')) + '</button>'
        : '<button data-act="enable" data-id="' + esc(mod.id) + '">' + esc(t('mods.enable')) + '</button>';

      const card = modCard(details, stateLabel + toggle +
        '<button class="danger" data-act="remove" data-id="' + esc(mod.id) + '">' + esc(t('mods.unsubscribe')) + '</button>');
      card.dataset.id = mod.id;
      grid.appendChild(card);
    });

    $('modsEmpty').classList.toggle('hidden', state.mods.length > 0);
  }

  function renderWorkshop() {
    const grid = $('wsResults');
    grid.innerHTML = '';
    const items = state.workshop.items || [];

    items.forEach((item) => {
      let actions;
      if (item.installed) {
        actions = '<span class="pill running">' + esc(t('mods.subscribed')) + '</span>' +
          '<button class="danger" data-act="unsubscribe" data-id="' + esc(item.id) + '">' + esc(t('mods.unsubscribe')) + '</button>';
      } else {
        actions = '<button class="primary" data-act="subscribe" data-id="' + esc(item.id) + '">' + esc(t('mods.subscribe')) + '</button>';
      }
      grid.appendChild(modCard(item, actions));
    });

    $('wsEmpty').classList.toggle('hidden', items.length > 0);
    $('wsPrev').disabled = state.workshop.page <= 1;
    $('wsNext').disabled = state.workshop.page * 20 >= (state.workshop.total || 0);
  }

  // ---------------------------------------------------------------- backups

  function renderBackups() {
    const rows = $('backupRows');
    rows.innerHTML = '';
    state.backups.forEach((b) => {
      const tr = document.createElement('tr');
      tr.innerHTML =
        '<td class="mono">' + esc(b.name) + '</td>' +
        '<td>' + esc(fmtSize(b.size)) + '</td>' +
        '<td>' + esc(fmtDateTime(b.created)) + '</td>' +
        '<td class="actions">' +
        '<a href="/api/backups/' + encodeURIComponent(b.name) + '/download"><button>' + esc(t('backups.download')) + '</button></a> ' +
        '<button class="danger" data-act="delete-backup" data-name="' + esc(b.name) + '">' + esc(t('backups.delete')) + '</button>' +
        '</td>';
      rows.appendChild(tr);
    });
    $('backupsEmpty').classList.toggle('hidden', state.backups.length > 0);
  }

  // ---------------------------------------------------------------- settings

  function renderSettings() {
    const s = state.settings || {};
    $('backupEnabled').checked = !!s.backupEnabled;
    $('backupInterval').value = s.backupIntervalHours || 24;
    $('backupRetention').value = s.backupRetention || 7;
    $('autoRestart').checked = !!s.autoRestart;
    $('stopTimeout').value = s.stopTimeoutSec || 90;

    const steam = state.steam || {};
    $('steamUser').value = steam.username || '';
    $('steamState').textContent = steam.configured
      ? t('settings.credentialsSet', { v: steam.username || '' })
      : t('settings.credentialsMissing');
    $('webApiKeyState').textContent = steam.webApiKeySet
      ? t('settings.apiKeySet')
      : t('settings.apiKeyMissing');

    if (state.status && state.status.panel) {
      const p = state.status.panel;
      $('panelInfo').textContent = 'v' + p.version + ' · ' + p.serverDir;
    }
  }

  async function loadSettings() {
    const data = await api('/api/settings');
    state.settings = data.settings || {};
    state.steam = data.steam || {};
    renderSettings();
  }

  function renderConfig(config) {
    if (!config) return;
    $('configPath').textContent = config.path || '';
    $('configText').value = config.content || '';
    $('configText').placeholder = config.exists ? '' : t('settings.configMissing');
  }

  async function loadConfig() {
    try {
      renderConfig(await api('/api/config'));
    } catch (_) { /* ignore */ }
  }

  async function loadMods() {
    await applyMods(await api('/api/mods'));
  }

  async function applyMods(data) {
    state.mods = data.mods || [];
    state.external = data.external || [];

    // Enrich the installed mods (and hand-copied files with an id in their
    // name) with Workshop metadata.
    const ids = state.mods.map((m) => m.id)
      .concat(state.external.map((m) => m.id))
      .filter(Boolean);
    if (ids.length) {
      try {
        const details = await api('/api/workshop/details?ids=' + encodeURIComponent(ids.join(',')));
        state.modDetails = {};
        (details.items || []).forEach((item) => { state.modDetails[item.id] = item; });
      } catch (_) {
        state.modDetails = {};
      }
    } else {
      state.modDetails = {};
    }

    renderMods();
    renderExternal();
  }

  function renderExternal() {
    const grid = $('externalCards');
    grid.innerHTML = '';

    (state.external || []).forEach((item) => {
      const details = (item.id && state.modDetails[item.id]) || {};
      const display = {
        id: item.id || item.file,
        title: details.title || item.file,
        description: details.description || '',
        preview: details.preview || '',
        subscriptions: details.subscriptions,
        size: item.size,
        enabled: item.enabled
      };

      const toggle = item.enabled
        ? '<button data-act="disable" data-file="' + esc(item.file) + '">' + esc(t('mods.disable')) + '</button>'
        : '<button data-act="enable" data-file="' + esc(item.file) + '">' + esc(t('mods.enable')) + '</button>';

      const card = modCard(display,
        '<span class="pill ' + (item.enabled ? 'running' : 'stopped') + '">' +
        esc(item.enabled ? t('mods.enabled') : t('mods.disabled')) + '</span>' +
        toggle +
        '<button class="danger" data-act="delete" data-file="' + esc(item.file) + '">' + esc(t('mods.deleteFile')) + '</button>');
      grid.appendChild(card);
    });

    $('externalEmpty').classList.toggle('hidden', (state.external || []).length > 0);
  }

  async function searchWorkshop(query, page) {
    state.workshop.query = query || '';
    state.workshop.page = page || 1;
    $('wsNote').textContent = '';
    $('wsNote').classList.remove('error');

    try {
      const data = await api('/api/workshop/search?q=' + encodeURIComponent(state.workshop.query) +
        '&page=' + state.workshop.page);
      state.workshop.items = data.items || [];
      state.workshop.total = data.total || 0;
      renderWorkshop();
    } catch (err) {
      state.workshop.items = [];
      state.workshop.total = 0;
      renderWorkshop();
      $('wsNote').textContent = err.status === 400 ? t('mods.keyNeeded') : err.message;
      $('wsNote').classList.add('error');
    }
  }

  async function loadBackups() {
    const data = await api('/api/backups');
    state.backups = data.backups || [];
    renderBackups();
  }

  async function loadEngine() {
    try {
      state.engine = await api('/api/engine');
      renderEngine();
    } catch (_) { /* keep the previous view */ }
  }

  function renderEngine() {
    const e = state.engine;
    if (!e) return;

    const current = e.flavor === 'openstarbound'
      ? t('engine.installed', { v: e.installed || t('engine.unknown') })
      : t('engine.vanillaInstalled');
    $('engineCurrent').textContent = current;
    $('engineCurrent').className = 'pill ' + (e.flavor === 'openstarbound' ? 'running' : 'stopped');

    const wanted = e.tag || 'latest';
    const select = $('engineTag');
    select.innerHTML = '';

    const latest = document.createElement('option');
    latest.value = 'latest';
    latest.textContent = t('engine.tagLatest');
    select.appendChild(latest);

    (e.releases || []).forEach((rel) => {
      if (!rel || !rel.tag || rel.tag === 'latest') return;
      const opt = document.createElement('option');
      opt.value = rel.tag;
      opt.textContent = rel.tag;
      select.appendChild(opt);
    });

    if (wanted !== 'latest' && !Array.from(select.options).some((o) => o.value === wanted)) {
      const opt = document.createElement('option');
      opt.value = wanted;
      opt.textContent = wanted;
      select.appendChild(opt);
    }
    select.value = wanted;

    $('engineFlavor').value = e.flavor === 'openstarbound' ? 'openstarbound' : 'vanilla';
    select.disabled = $('engineFlavor').value !== 'openstarbound';

    let note = '';
    if (e.releaseError) note = t('engine.releasesFailed', { v: e.releaseError });
    else if (e.flavor === 'openstarbound' && e.installed) note = t('engine.installed', { v: e.installed });
    $('engineNote').textContent = note;
  }

  // ---------------------------------------------------------------- actions

  async function serverAction(action, okKey) {
    try {
      await api('/api/server/' + action, { method: 'POST' });
      toast(t(okKey));
    } catch (err) {
      toast(t('toast.failed', { v: err.message }), true);
    }
  }

  async function installServer() {
    if (!window.confirm(t('dash.installConfirm'))) return;
    const code = window.prompt(t('dash.guardPrompt'), '');
    if (code === null) return;
    try {
      await api('/api/server/install', { method: 'POST', body: { guardCode: code.trim() } });
      toast(t('toast.installStarted'));
      selectTab('console');
    } catch (err) {
      toast(t('toast.failed', { v: err.message }), true);
    }
  }

  async function backupNow() {
    try {
      await api('/api/backups', { method: 'POST' });
      toast(t('backups.started'));
      setTimeout(loadBackups, 1500);
    } catch (err) {
      toast(t('toast.failed', { v: err.message }), true);
    }
  }

  // ---------------------------------------------------------------- tabs

  function selectTab(name) {
    document.querySelectorAll('.nav').forEach((btn) => {
      btn.classList.toggle('active', btn.dataset.tab === name);
    });
    document.querySelectorAll('.tab').forEach((tab) => {
      tab.classList.toggle('active', tab.id === 'tab-' + name);
    });
    if (name === 'console') scrollLogs();
  }

  // ---------------------------------------------------------------- sse

  function connect() {
    if (state.es) state.es.close();
    const es = new EventSource('/api/stream');
    state.es = es;

    es.addEventListener('hello', (ev) => {
      const data = JSON.parse(ev.data);
      state.lines = (data.lines || []).slice(-MAX_LINES);
      renderAllLines();
      applyStatus(data.status);
    });
    es.addEventListener('log', (ev) => appendLine(JSON.parse(ev.data)));
    es.addEventListener('status', (ev) => applyStatus(JSON.parse(ev.data)));
    es.addEventListener('players', (ev) => {
      state.players = JSON.parse(ev.data) || [];
      $('playerPill').textContent = state.players.length;
      renderPlayers(state.players);
    });
    es.addEventListener('ping', (ev) => applyStatus(JSON.parse(ev.data)));
  }

  // ---------------------------------------------------------------- wiring

  function wire() {
    document.querySelectorAll('.nav').forEach((btn) => {
      btn.addEventListener('click', () => selectTab(btn.dataset.tab));
    });

    $('lang').addEventListener('change', (ev) => {
      lang = ev.target.value;
      localStorage.setItem('sb.lang', lang);
      applyI18n();
      renderPlayers(state.players);
      renderMods();
      renderBackups();
      if (state.status) applyStatus(state.status);
      if (state.settings) renderSettings();
      if (state.setup) showLogin(true);
    });

    $('logout').addEventListener('click', async () => {
      try { await api('/api/logout', { method: 'POST' }); } catch (_) { /* ignore */ }
      window.location.reload();
    });

    $('loginForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const username = $('loginUser').value.trim() || 'admin';
      const password = $('loginPass').value;
      $('loginError').textContent = '';

      if (state.setup && password !== $('loginPass2').value) {
        $('loginError').textContent = t('login.mismatch');
        return;
      }

      try {
        if (state.setup) {
          await api('/api/setup', { method: 'POST', body: { username: username, password: password } });
        }
        await api('/api/login', { method: 'POST', body: { username: username, password: password } });
        await boot();
      } catch (err) {
        $('loginError').textContent = err.message;
      }
    });

    $('btnStart').addEventListener('click', () => serverAction('start', 'toast.started'));
    $('btnStop').addEventListener('click', () => serverAction('stop', 'toast.stopped'));
    $('btnRestart').addEventListener('click', () => serverAction('restart', 'toast.restarted'));
    $('btnInstall').addEventListener('click', installServer);
    $('btnBackup').addEventListener('click', backupNow);
    $('btnBackup2').addEventListener('click', backupNow);

    $('cmdForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const command = $('cmdInput').value.trim();
      if (!command) return;
      try {
        await api('/api/server/command', { method: 'POST', body: { command: command } });
        $('cmdInput').value = '';
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    document.querySelectorAll('.chip').forEach((chip) => {
      chip.addEventListener('click', () => {
        $('cmdInput').value = chip.dataset.cmd || '';
        $('cmdInput').focus();
      });
    });

    $('clearLog').addEventListener('click', () => {
      state.lines = [];
      renderAllLines();
    });

    $('pauseLog').addEventListener('change', (ev) => {
      state.paused = ev.target.checked;
      if (!state.paused) renderAllLines();
    });

    $('followLog').addEventListener('change', scrollLogs);

    $('copyAddress').addEventListener('click', async () => {
      const address = state.address || $('statAddress').textContent || '';
      if (!address) return;
      try {
        await navigator.clipboard.writeText(address);
        toast(t('dash.copied'));
      } catch (_) {
        window.prompt(t('dash.copy'), address);
      }
    });

    $('modForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const id = $('modId').value.trim();
      if (!id) return;
      const code = window.prompt(t('mods.guardPrompt'), '');
      if (code === null) return;
      try {
        await api('/api/mods', { method: 'POST', body: { id: id, guardCode: code.trim() } });
        $('modId').value = '';
        toast(t('mods.added', { id: id }));
        selectTab('console');
        setTimeout(loadMods, 3000);
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('wsForm').addEventListener('submit', (ev) => {
      ev.preventDefault();
      searchWorkshop($('wsQuery').value.trim(), 1);
    });

    $('wsPopular').addEventListener('click', () => {
      $('wsQuery').value = '';
      searchWorkshop('', 1);
    });

    $('wsPrev').addEventListener('click', () => {
      if (state.workshop.page > 1) searchWorkshop(state.workshop.query, state.workshop.page - 1);
    });

    $('wsNext').addEventListener('click', () => {
      searchWorkshop(state.workshop.query, state.workshop.page + 1);
    });

    async function subscribeMod(id) {
      const code = window.prompt(t('mods.guardPrompt'), '');
      if (code === null) return;
      try {
        await api('/api/mods', { method: 'POST', body: { id: id, guardCode: code.trim() } });
        toast(t('mods.added', { id: id }));
        selectTab('console');
        setTimeout(() => { loadMods(); searchWorkshop(state.workshop.query, state.workshop.page); }, 3000);
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    }

    async function unsubscribeMod(id) {
      if (!window.confirm(t('mods.removeConfirm', { id: id }))) return;
      try {
        await api('/api/mods/' + encodeURIComponent(id), { method: 'DELETE' });
        await loadMods();
        if (state.workshop.items.length) searchWorkshop(state.workshop.query, state.workshop.page);
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    }

    $('modCards').addEventListener('click', async (ev) => {
      const btn = ev.target.closest('button[data-act]');
      if (!btn) return;
      const id = btn.dataset.id;
      const act = btn.dataset.act;
      if (act === 'remove') { await unsubscribeMod(id); return; }
      try {
        await api('/api/mods/' + encodeURIComponent(id) + '/' + act, { method: 'POST' });
        await loadMods();
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('externalCards').addEventListener('click', async (ev) => {
      const btn = ev.target.closest('button[data-act]');
      if (!btn) return;
      const file = btn.dataset.file;
      const act = btn.dataset.act;
      if (act === 'delete' && !window.confirm(t('mods.deleteFileConfirm', { file: file }))) return;
      try {
        await api('/api/modfiles/' + act, { method: 'POST', body: { file: file } });
        await loadMods();
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('uploadForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const input = $('uploadFiles');
      if (!input.files || !input.files.length) return;

      const form = new FormData();
      for (const file of input.files) form.append('files', file);
      $('uploadNote').textContent = t('mods.uploading');

      try {
        const res = await fetch('/api/mods/upload', {
          method: 'POST',
          body: form,
          headers: { 'X-SB-Request': '1' },
          credentials: 'same-origin'
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || res.statusText);

        input.value = '';
        const count = (data.installed || []).length;
        if (count) toast(t('mods.uploadDone', { n: count }));
        if (data.failed && data.failed.length) toast(data.failed.join('; '), true);
        await loadMods();
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      } finally {
        $('uploadNote').textContent = '';
      }
    });

    $('wsResults').addEventListener('click', async (ev) => {
      const btn = ev.target.closest('button[data-act]');
      if (!btn) return;
      if (btn.dataset.act === 'subscribe') await subscribeMod(btn.dataset.id);
      if (btn.dataset.act === 'unsubscribe') await unsubscribeMod(btn.dataset.id);
    });

    $('saveWebApiKey').addEventListener('click', async () => {
      try {
        await api('/api/settings/workshop', { method: 'POST', body: { apiKey: $('webApiKey').value.trim() } });
        toast(t('toast.saved'));
        $('webApiKey').value = '';
        await loadSettings();
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('backupRows').addEventListener('click', async (ev) => {
      const btn = ev.target.closest('button[data-act="delete-backup"]');
      if (!btn) return;
      const name = btn.dataset.name;
      if (!window.confirm(t('backups.deleteConfirm', { name: name }))) return;
      try {
        await api('/api/backups/' + encodeURIComponent(name), { method: 'DELETE' });
        await loadBackups();
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('scheduleForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      try {
        await api('/api/settings', {
          method: 'PUT',
          body: {
            backupEnabled: $('backupEnabled').checked,
            backupIntervalHours: Number($('backupInterval').value) || 24,
            backupRetention: Number($('backupRetention').value) || 7
          }
        });
        toast(t('toast.saved'));
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('engineFlavor').addEventListener('change', () => {
      $('engineTag').disabled = $('engineFlavor').value !== 'openstarbound';
    });

    $('engineForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const flavor = $('engineFlavor').value;
      const tag = flavor === 'openstarbound' ? ($('engineTag').value || 'latest') : '';
      const label = flavor === 'openstarbound'
        ? 'OpenStarbound ' + (tag === 'latest' ? t('engine.tagLatest') : tag)
        : t('engine.vanillaInstalled');

      if (!window.confirm(t('engine.confirm', { v: label }))) return;

      try {
        await api('/api/engine/switch', { method: 'POST', body: { flavor: flavor, tag: tag } });
        toast(t('engine.switched'));
        selectTab('console');
        setTimeout(loadEngine, 2000);
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('panelForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      try {
        await api('/api/settings', {
          method: 'PUT',
          body: {
            autoRestart: $('autoRestart').checked,
            stopTimeoutSec: Number($('stopTimeout').value) || 90
          }
        });
        toast(t('toast.saved'));
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    async function saveSteamCredentials() {
      await api('/api/settings/steam', {
        method: 'POST',
        body: {
          username: $('steamUser').value.trim(),
          password: $('steamPass').value,
          guardCode: $('steamGuard').value.trim()
        }
      });
      $('steamPass').value = '';
      $('steamGuard').value = '';
      state.steam = { configured: true, username: $('steamUser').value.trim() };
      renderSettings();
    }

    $('steamForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      try {
        await saveSteamCredentials();
        toast(t('toast.saved'));
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('steamTest').addEventListener('click', async () => {
      try {
        await saveSteamCredentials();
        await api('/api/steam/test', { method: 'POST' });
        toast(t('settings.testing'));
        selectTab('console');
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('saveConfig').addEventListener('click', async () => {
      try {
        await api('/api/config', { method: 'PUT', body: { content: $('configText').value } });
        toast(t('toast.saved'));
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });

    $('passwordForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      try {
        await api('/api/settings/password', {
          method: 'POST',
          body: { current: $('curPass').value, next: $('newPass').value }
        });
        toast(t('toast.passwordChanged'));
        $('curPass').value = '';
        $('newPass').value = '';
        showLogin(false);
      } catch (err) {
        toast(t('toast.failed', { v: err.message }), true);
      }
    });
  }

  // ---------------------------------------------------------------- start

  wire();
  boot();

  // Refresh the config editor when opening the settings tab.
  // Refresh the config editor and the engine card when opening the settings tab.
  document.querySelector('.nav[data-tab="settings"]').addEventListener('click', loadConfig);
  document.querySelector('.nav[data-tab="settings"]').addEventListener('click', loadEngine);
  document.querySelector('.nav[data-tab="backups"]').addEventListener('click', loadBackups);
})();
