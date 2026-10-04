'use strict';
(() => {
  const $ = id => document.getElementById(id);
  let key = '';
  let accounts = [];
  let policies = new Map();
  let states = new Map();
  let maxLimit = 1000000;
  let connected = false;
  let pendingUpdates = 0;
  const dirty = new Set();
  const message = (text, error = false) => { $('message').textContent = text; $('message').className = error ? 'error' : 'success'; };
  async function api(path, options = {}) {
    const response = await fetch('/v8/management/' + path, { ...options, cache: 'no-store', headers: { 'X-Management-Key': key, 'Content-Type': 'application/json' } });
    const text = await response.text();
    let data;
    try { data = JSON.parse(text); } catch { throw new Error('服务返回了非 JSON 响应，请检查连接。'); }
    if (!response.ok) {
      if (response.status === 409) throw new Error('配置已被其他页面修改，请刷新后重新设置。');
      if (response.status === 401 || response.status === 403) throw new Error('管理密码无效或访问受限，请检查后重新连接。');
      throw new Error(data.message || (typeof data.error === 'string' ? data.error : data.error?.message) || '请求失败：' + response.status);
    }
    return data;
  }
  function el(tag, cls, text) { const node = document.createElement(tag); if (cls) node.className = cls; if (text !== undefined) node.textContent = text; return node; }
  const idOf = account => account.id || account.auth_index;
  const policyOf = id => policies.get(id) || { max_in_flight: 0 };
  function updateSummary() {
    $('credential-count').textContent = accounts.length;
    $('limited-count').textContent = accounts.filter(a => { const p = policyOf(idOf(a)); return p.max_in_flight > 0; }).length;
    $('inflight-count').textContent = accounts.reduce((sum, a) => sum + (states.get(idOf(a))?.admitted_in_flight || 0), 0);
  }
  async function load() {
    const auths = await api('credentials');
    accounts = (auths.files || []).filter(a => idOf(a) && !a.runtime_only);
    if (accounts.some(a => !Object.hasOwn(a, 'max_in_flight'))) throw new Error('当前 CPA 尚未支持凭证并发限制，请升级服务后重试。');
    policies = new Map(accounts.map(a => [idOf(a), { max_in_flight: a.max_in_flight || 0 }]));
    states = new Map(accounts.map(a => [idOf(a), { admitted_in_flight: a.admitted_in_flight || 0 }]));
    dirty.clear(); render();
  }
  function parseLimit(raw) {
    const text = raw.trim();
    if (!text || text === '0') return null;
    if (!/^\d+$/.test(text)) throw new Error('并发上限必须为非负整数。');
    const number = Number(text);
    if (!Number.isSafeInteger(number) || number > maxLimit) throw new Error('并发上限不能超过 ' + maxLimit + '。');
    return number || null;
  }
  function filterRows() {
    const term = $('search').value.trim().toLowerCase();
    for (const node of $('accounts').children) node.hidden = !(node.dataset.search || '').includes(term);
  }
  function render() {
    $('accounts').replaceChildren();
    for (const account of accounts) {
      const id = idOf(account), policy = policyOf(id), state = states.get(id);
      const title = account.email || account.label || account.name || id;
      const row = el('article', 'account'); row.dataset.search = [title, account.name, account.provider].join(' ').toLowerCase();
      const main = el('div', 'account-main');
      const identity = el('div', 'identity');
      identity.append(el('h3', 'account-name', title), el('p', 'account-file', account.name || id));
      const stateBadge = el('span', account.disabled ? 'badge disabled' : 'badge', account.disabled ? '已停用' : '已启用');
      identity.append(el('span', 'badge', account.provider || account.type || '凭证'), stateBadge);
      const toggleWrap = el('div', 'credential-toggle');
      const toggle = el('button', 'toggle'); toggle.type = 'button'; toggle.setAttribute('role', 'switch');
      toggle.setAttribute('aria-label', '启用凭证 ' + title);
      const toggleLabel = el('span', 'toggle-label');
      const updateToggle = () => {
        toggle.setAttribute('aria-checked', String(!account.disabled));
        toggle.title = account.disabled ? '点击启用该凭证' : '点击停用该凭证';
        toggleLabel.textContent = account.disabled ? '已停用' : '已启用';
        stateBadge.className = account.disabled ? 'badge disabled' : 'badge';
        stateBadge.textContent = toggleLabel.textContent;
      };
      updateToggle(); toggleWrap.append(toggle, toggleLabel); identity.append(toggleWrap);
      const count = el('div', 'count', '已占用 ' + (state?.admitted_in_flight || 0) + ' / ' + (policy.max_in_flight || '不限'));
      const limitWrap = el('div', 'limit');
      const input = el('input'); input.type = 'number'; input.min = '0'; input.max = String(maxLimit); input.step = '1'; input.placeholder = '不限'; input.value = policy.max_in_flight || ''; input.id = 'limit-' + id;
      const label = el('label', '', '账号总并发上限'); label.htmlFor = input.id; limitWrap.append(label, input);
      const save = el('button', '', '保存'); save.type = 'button'; save.setAttribute('aria-label', '保存 ' + title + ' 的并发设置');
      const status = el('div', 'row-status'); status.setAttribute('role', 'status');
      const setRowBusy = busy => {
        save.disabled = busy; input.disabled = busy; toggle.disabled = busy;
        pendingUpdates += busy ? 1 : -1;
        $('refresh').disabled = pendingUpdates > 0;
        $('disconnect').disabled = pendingUpdates > 0;
      };
      toggle.addEventListener('click', async () => {
        const disabled = !account.disabled;
        setRowBusy(true); status.className = 'row-status'; status.textContent = disabled ? '正在停用…' : '正在启用…';
        try {
          const result = await api('credentials/status', { method: 'PATCH', body: JSON.stringify({ name: id, auth_index: account.auth_index, disabled }) });
          if (typeof result.disabled !== 'boolean') throw new Error('服务器未返回凭证状态，请刷新确认。');
          account.disabled = result.disabled; updateToggle();
          status.textContent = (account.disabled ? '已停用，不再调度新请求' : '已启用') + (dirty.has(id) ? ' · 并发修改尚未保存' : ' · ' + new Date().toLocaleTimeString());
        } catch (error) { status.textContent = error.message; status.className = 'row-status error'; }
        finally { setRowBusy(false); }
      });
      const markDirty = () => { dirty.add(id); status.textContent = '未保存'; status.className = 'row-status'; };
      input.addEventListener('input', markDirty);
      save.addEventListener('click', async () => {
        setRowBusy(true); status.className = 'row-status'; status.textContent = '正在保存…';
        try {
          const body = { name: id, max_in_flight: parseLimit(input.value) };
          await api('credentials/fields', { method: 'PATCH', body: JSON.stringify(body) });
          const updated = { max_in_flight: body.max_in_flight || 0 };
          policies.set(id, updated); dirty.delete(id); input.value = updated.max_in_flight || '';
          count.textContent = '已占用 ' + (state?.admitted_in_flight || 0) + ' / ' + (updated.max_in_flight || '不限');
          status.textContent = '已保存 · ' + new Date().toLocaleTimeString(); updateSummary();
        } catch (error) { status.textContent = error.message; status.className = 'row-status error'; }
        finally { setRowBusy(false); }
      });
      main.append(identity, count, limitWrap, save); row.append(main, status); $('accounts').append(row);
    }
    if (!accounts.length) $('accounts').append(el('p', 'empty', '暂无账号凭证，请先在 CPA 导入凭证。'));
    updateSummary(); filterRows();
  }
  $('login-form').addEventListener('submit', async event => {
    event.preventDefault(); key = $('management-key').value.trim(); if (!key) return;
    $('connect').disabled = true; message('正在连接…');
    try { await load(); connected = true; $('management-key').value = ''; $('login-panel').hidden = true; $('workspace').hidden = false; message('已连接 CPA，设置保存后对后续请求生效。'); }
    catch (error) { key = ''; message(error.message, true); }
    finally { $('connect').disabled = false; }
  });
  $('refresh').addEventListener('click', async () => {
    if (dirty.size) { message('存在未保存的修改，请先保存对应账号，再刷新数据。', true); return; }
    $('refresh').disabled = true;
    try { await load(); message('数据已刷新。'); } catch (error) { message(error.message, true); }
    finally { $('refresh').disabled = false; }
  });
  $('disconnect').addEventListener('click', () => { key = ''; connected = false; dirty.clear(); accounts = []; policies.clear(); states.clear(); $('accounts').replaceChildren(); $('workspace').hidden = true; $('login-panel').hidden = false; message('已断开连接。'); });
  $('search').addEventListener('input', filterRows);
  window.addEventListener('beforeunload', event => { if (connected && dirty.size) { event.preventDefault(); event.returnValue = ''; } });
})();
