// The panel's own script, served from the binary like everything it loads.

function toast(msg) {
  const el = document.getElementById('toast');
  el.textContent = msg || 'Something went wrong';
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => el.hidden = true, 8000);
}
document.addEventListener('htmx:responseError', e => toast(e.detail.xhr.responseText));
document.addEventListener('htmx:sendError', () => toast('Network error'));

// Key/value editor over the "KEY=value" text the server stores, with a raw
// .env mode for pasting and optional suggestions from the repo's .env.example.
function envEditor(seed, suggestURL) {
  return {
    mode: 'rows', rows: [], raw: '', suggestFile: '', suggestKeys: [],
    init() {
      this.parse(seed);
      if (suggestURL) this.suggest(suggestURL);
    },
    parse(text) {
      this.rows = text.split('\n').map(l => l.trim()).filter(Boolean).map(line => {
        const i = line.indexOf('=');
        return i === -1 ? {key: line, value: '', show: false} : {key: line.slice(0, i).trim(), value: line.slice(i + 1), show: false};
      });
    },
    serialize() { return this.rows.filter(r => r.key.trim()).map(r => r.key.trim() + '=' + r.value).join('\n'); },
    get value() { return this.mode === 'raw' ? this.raw : this.serialize(); },
    toggle() {
      if (this.mode === 'rows') { this.raw = this.serialize(); this.mode = 'raw'; }
      else { this.parse(this.raw); this.mode = 'rows'; }
    },
    async suggest(url) {
      try {
        const data = await (await fetch(url)).json();
        const have = new Set(this.rows.map(r => r.key.trim()));
        this.suggestFile = data.file || '';
        this.suggestKeys = (data.keys || []).filter(k => !have.has(k));
      } catch (e) {}
    },
    add(key) {
      this.rows.push({key, value: '', show: false});
      this.suggestKeys = this.suggestKeys.filter(k => k !== key);
    },
  };
}

document.addEventListener('click', e => {
  if (e.target.id === 'toast') e.target.hidden = true;
});
